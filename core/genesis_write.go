// Copyright 2014 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package core

import (
	"context"
	"crypto/ecdsa"
	"embed"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math/big"
	"sync"

	"github.com/c2h5oh/datasize"
	"github.com/ethereum-optimism/superchain-registry/superchain"
	"github.com/holiman/uint256"
	"golang.org/x/exp/slices"

	"github.com/ledgerwatch/erigon-lib/chain"
	"github.com/ledgerwatch/erigon-lib/chain/networkname"
	libcommon "github.com/ledgerwatch/erigon-lib/common"
	"github.com/ledgerwatch/erigon-lib/common/hexutil"
	"github.com/ledgerwatch/erigon-lib/kv"
	"github.com/ledgerwatch/erigon-lib/kv/kvcfg"
	"github.com/ledgerwatch/erigon-lib/kv/mdbx"
	"github.com/ledgerwatch/erigon-lib/kv/rawdbv3"

	"github.com/ledgerwatch/erigon/common"
	"github.com/ledgerwatch/erigon/consensus/ethash"
	"github.com/ledgerwatch/erigon/consensus/merge"
	"github.com/ledgerwatch/erigon/core/rawdb"
	"github.com/ledgerwatch/erigon/core/state"
	"github.com/ledgerwatch/erigon/core/types"
	"github.com/ledgerwatch/erigon/crypto"
	"github.com/ledgerwatch/erigon/eth/ethconfig"
	"github.com/ledgerwatch/erigon/params"
	"github.com/ledgerwatch/erigon/turbo/trie"
	"github.com/ledgerwatch/log/v3"
)

// CommitGenesisBlock writes or updates the genesis block in db.
// The block that will be used is:
//
//	                     genesis == nil       genesis != nil
//	                  +------------------------------------------
//	db has no genesis |  main-net          |  genesis
//	db has genesis    |  from DB           |  genesis (if compatible)
//
// The stored chain configuration will be updated if it is compatible (i.e. does not
// specify a fork block below the local head block). In case of a conflict, the
// error is a *params.ConfigCompatError and the new, unwritten config is returned.
//
// The returned chain configuration is never nil.
func CommitGenesisBlock(db kv.RwDB, genesis *types.Genesis, tmpDir string, logger log.Logger) (*chain.Config, *types.Block, error) {
	return CommitGenesisBlockWithOverride(db, genesis, nil, nil, nil, nil, tmpDir, logger)
}

func CommitGenesisBlockWithOverride(db kv.RwDB, genesis *types.Genesis, overrideCancunTime, overrideShanghaiTime, overrideOptimismCanyonTime, overrideOptimismEcotoneTime *big.Int, tmpDir string, logger log.Logger) (*chain.Config, *types.Block, error) {
	tx, err := db.BeginRw(context.Background())
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()
	c, b, err := WriteGenesisBlock(tx, genesis, overrideCancunTime, overrideShanghaiTime, overrideOptimismCanyonTime, overrideOptimismEcotoneTime, tmpDir, logger)
	if err != nil {
		return c, b, err
	}
	err = tx.Commit()
	if err != nil {
		return c, b, err
	}
	return c, b, nil
}

func WriteGenesisBlock(tx kv.RwTx, genesis *types.Genesis, overrideCancunTime, overrideShanghaiTime, overrideOptimismCanyonTime, overrideOptimismEcotoneTime *big.Int,
	tmpDir string, logger log.Logger) (*chain.Config, *types.Block, error) {
	var storedBlock *types.Block
	if genesis != nil && genesis.Config == nil {
		return params.AllProtocolChanges, nil, types.ErrGenesisNoConfig
	}
	// Just commit the new block if there is no stored genesis block.
	storedHash, storedErr := rawdb.ReadCanonicalHash(tx, 0)
	if storedErr != nil {
		return nil, nil, storedErr
	}

	applyOverrides := func(config *chain.Config) {
		if overrideShanghaiTime != nil {
			config.ShanghaiTime = overrideShanghaiTime
		}
		if overrideCancunTime != nil {
			config.CancunTime = overrideCancunTime
		}
		if config.IsOptimism() && overrideOptimismCanyonTime != nil {
			config.CanyonTime = overrideOptimismCanyonTime
			// Shanghai hardfork is included in canyon hardfork
			config.ShanghaiTime = overrideOptimismCanyonTime
			if config.Optimism.EIP1559DenominatorCanyon == 0 {
				logger.Warn("EIP1559DenominatorCanyon set to 0. Overriding to 250 to avoid divide by zero.")
				config.Optimism.EIP1559DenominatorCanyon = 250
			}
		}
		if overrideShanghaiTime != nil && config.IsOptimism() && overrideOptimismCanyonTime != nil {
			if overrideShanghaiTime.Cmp(overrideOptimismCanyonTime) != 0 {
				logger.Warn("Shanghai hardfork time is overridden by optimism canyon time",
					"shanghai", overrideShanghaiTime.String(), "canyon", overrideOptimismCanyonTime.String())
			}
		}
		if config.IsOptimism() && overrideOptimismEcotoneTime != nil {
			config.EcotoneTime = overrideOptimismEcotoneTime
			// Cancun hardfork is included in Ecotone hardfork
			config.CancunTime = overrideOptimismEcotoneTime
		}
		if overrideCancunTime != nil && config.IsOptimism() && overrideOptimismEcotoneTime != nil {
			if overrideCancunTime.Cmp(overrideOptimismEcotoneTime) != 0 {
				logger.Warn("Cancun hardfork time is overridden by optimism Ecotone time",
					"cancun", overrideCancunTime.String(), "ecotone", overrideOptimismEcotoneTime.String())
			}
		}
	}

	if (storedHash == libcommon.Hash{}) {
		custom := true
		if genesis == nil {
			logger.Info("Writing main-net genesis block")
			genesis = MainnetGenesisBlock()
			custom = false
		}
		applyOverrides(genesis.Config)
		block, _, err1 := write(tx, genesis, tmpDir)
		if err1 != nil {
			return genesis.Config, nil, err1
		}
		if custom {
			logger.Info("Writing custom genesis block", "hash", block.Hash().String())
		}
		return genesis.Config, block, nil
	}

	// Check whether the genesis block is already written.
	if genesis != nil {
		block, _, err1 := GenesisToBlock(genesis, tmpDir)
		if err1 != nil {
			return genesis.Config, nil, err1
		}
		hash := block.Hash()
		if hash != storedHash {
			return genesis.Config, block, &types.GenesisMismatchError{Stored: storedHash, New: hash}
		}
	}
	number := rawdb.ReadHeaderNumber(tx, storedHash)
	if number != nil {
		var err error
		storedBlock, _, err = rawdb.ReadBlockWithSenders(tx, storedHash, *number)
		if err != nil {
			return genesis.Config, nil, err
		}
	}
	// Get the existing chain configuration.
	newCfg := genesis.ConfigOrDefault(storedHash)
	applyOverrides(newCfg)
	if err := newCfg.CheckConfigForkOrder(); err != nil {
		return newCfg, nil, err
	}
	storedCfg, storedErr := rawdb.ReadChainConfig(tx, storedHash)
	if storedErr != nil && newCfg.Bor == nil {
		return newCfg, nil, storedErr
	}
	if storedCfg == nil {
		logger.Warn("Found genesis block without chain config")
		err1 := rawdb.WriteChainConfig(tx, storedHash, newCfg)
		if err1 != nil {
			return newCfg, nil, err1
		}
		return newCfg, storedBlock, nil
	}
	// Special case: don't change the existing config of a private chain if no new
	// config is supplied. This is useful, for example, to preserve DB config created by erigon init.
	// In that case, only apply the overrides.
	if genesis == nil && params.ChainConfigByGenesisHash(storedHash) == nil {
		newCfg = storedCfg
		applyOverrides(newCfg)
	}

	// Special case: temporal fix for optimism mainnet database which contains incorrect chainspec.
	// If london block number is set to previous wrong chainspec, overwrite with correct chainspec.
	// Following code will be removed after we are confident that previous dbs are all corrected.
	// https://github.com/testinprod-io/op-erigon/issues/71
	if storedCfg.ChainName == networkname.OPMainnetChainName &&
		newCfg.ChainName == networkname.OPMainnetChainName &&
		storedCfg.LondonBlock.Uint64() == 3950000 &&
		newCfg.LondonBlock.Uint64() == 105235063 {
		log.Warn("Override chainconfig for Optimism Mainnet chainspec correction")
		if err := rawdb.WriteChainConfig(tx, storedHash, newCfg); err != nil {
			return newCfg, nil, err
		}
		return newCfg, storedBlock, nil
	}

	// Check config compatibility and write the config. Compatibility errors
	// are returned to the caller unless we're already at block zero.
	height := rawdb.ReadHeaderNumber(tx, rawdb.ReadHeadHeaderHash(tx))
	if height != nil {
		compatibilityErr := storedCfg.CheckCompatible(newCfg, *height)
		if compatibilityErr != nil && *height != 0 && compatibilityErr.RewindTo != 0 {
			return newCfg, storedBlock, compatibilityErr
		}
	}
	if err := rawdb.WriteChainConfig(tx, storedHash, newCfg); err != nil {
		return newCfg, nil, err
	}
	return newCfg, storedBlock, nil
}

func WriteGenesisState(g *types.Genesis, tx kv.RwTx, tmpDir string) (*types.Block, *state.IntraBlockState, error) {
	block, statedb, err := GenesisToBlock(g, tmpDir)
	if err != nil {
		return nil, nil, err
	}
	histV3, err := kvcfg.HistoryV3.Enabled(tx)
	if err != nil {
		panic(err)
	}

	var stateWriter state.StateWriter
	if ethconfig.EnableHistoryV4InTest {
		panic("implement me")
		//tx.(*temporal.Tx).Agg().SetTxNum(0)
		//stateWriter = state.NewWriterV4(tx.(kv.TemporalTx))
		//defer tx.(*temporal.Tx).Agg().StartUnbufferedWrites().FinishWrites()
	} else {
		for addr, account := range g.Alloc {
			if len(account.Code) > 0 || len(account.Storage) > 0 {
				// Special case for weird tests - inaccessible storage
				var b [8]byte
				binary.BigEndian.PutUint64(b[:], state.FirstContractIncarnation)
				if err := tx.Put(kv.IncarnationMap, addr[:], b[:]); err != nil {
					return nil, nil, err
				}
			}
		}
		stateWriter = state.NewPlainStateWriter(tx, tx, 0)
	}

	if block.Number().Sign() != 0 {
		return nil, statedb, fmt.Errorf("can't commit genesis block with number > 0")
	}

	if err := statedb.CommitBlock(&chain.Rules{}, stateWriter); err != nil {
		return nil, statedb, fmt.Errorf("cannot write state: %w", err)
	}
	if !histV3 {
		if csw, ok := stateWriter.(state.WriterWithChangeSets); ok {
			if err := csw.WriteChangeSets(); err != nil {
				return nil, statedb, fmt.Errorf("cannot write change sets: %w", err)
			}
			if err := csw.WriteHistory(); err != nil {
				return nil, statedb, fmt.Errorf("cannot write history: %w", err)
			}
		}
	}
	return block, statedb, nil
}
func MustCommitGenesis(g *types.Genesis, db kv.RwDB, tmpDir string) *types.Block {
	tx, err := db.BeginRw(context.Background())
	if err != nil {
		panic(err)
	}
	defer tx.Rollback()
	block, _, err := write(tx, g, tmpDir)
	if err != nil {
		panic(err)
	}
	err = tx.Commit()
	if err != nil {
		panic(err)
	}
	return block
}

// Write writes the block and state of a genesis specification to the database.
// The block is committed as the canonical head block.
func write(tx kv.RwTx, g *types.Genesis, tmpDir string) (*types.Block, *state.IntraBlockState, error) {
	block, statedb, err2 := WriteGenesisState(g, tx, tmpDir)
	if err2 != nil {
		return block, statedb, err2
	}
	config := g.Config
	if config == nil {
		config = params.AllProtocolChanges
	}
	if err := config.CheckConfigForkOrder(); err != nil {
		return nil, nil, err
	}

	if err := rawdb.WriteBlock(tx, block); err != nil {
		return nil, nil, err
	}
	if err := rawdb.WriteTd(tx, block.Hash(), block.NumberU64(), g.Difficulty); err != nil {
		return nil, nil, err
	}
	if err := rawdbv3.TxNums.WriteForGenesis(tx, 1); err != nil {
		return nil, nil, err
	}
	if err := rawdb.WriteReceipts(tx, block.NumberU64(), nil); err != nil {
		return nil, nil, err
	}

	if err := rawdb.WriteCanonicalHash(tx, block.Hash(), block.NumberU64()); err != nil {
		return nil, nil, err
	}

	rawdb.WriteHeadBlockHash(tx, block.Hash())
	if err := rawdb.WriteHeadHeaderHash(tx, block.Hash()); err != nil {
		return nil, nil, err
	}
	if err := rawdb.WriteChainConfig(tx, block.Hash(), config); err != nil {
		return nil, nil, err
	}

	// We support ethash/merge for issuance (for now)
	if g.Config.Consensus != chain.EtHashConsensus {
		return block, statedb, nil
	}
	// Issuance is the sum of allocs
	genesisIssuance := big.NewInt(0)
	for _, account := range g.Alloc {
		genesisIssuance.Add(genesisIssuance, account.Balance)
	}

	// BlockReward can be present at genesis
	if block.Header().Difficulty.Cmp(merge.ProofOfStakeDifficulty) != 0 {
		blockReward, _ := ethash.AccumulateRewards(g.Config, block.Header(), nil)
		// Set BlockReward
		genesisIssuance.Add(genesisIssuance, blockReward.ToBig())
	}
	if err := rawdb.WriteTotalIssued(tx, 0, genesisIssuance); err != nil {
		return nil, nil, err
	}
	return block, statedb, rawdb.WriteTotalBurnt(tx, 0, libcommon.Big0)
}

// GenesisBlockForTesting creates and writes a block in which addr has the given wei balance.
func GenesisBlockForTesting(db kv.RwDB, addr libcommon.Address, balance *big.Int, tmpDir string) *types.Block {
	g := types.Genesis{Alloc: types.GenesisAlloc{addr: {Balance: balance}}, Config: params.TestChainConfig}
	block := MustCommitGenesis(&g, db, tmpDir)
	return block
}

type GenAccount struct {
	Addr    libcommon.Address
	Balance *big.Int
}

func GenesisWithAccounts(db kv.RwDB, accs []GenAccount, tmpDir string) *types.Block {
	g := types.Genesis{Config: params.TestChainConfig}
	allocs := make(map[libcommon.Address]types.GenesisAccount)
	for _, acc := range accs {
		allocs[acc.Addr] = types.GenesisAccount{Balance: acc.Balance}
	}
	g.Alloc = allocs
	block := MustCommitGenesis(&g, db, tmpDir)
	return block
}

// MainnetGenesisBlock returns the Ethereum main net genesis block.
func MainnetGenesisBlock() *types.Genesis {
	return &types.Genesis{
		Config:     params.MainnetChainConfig,
		Nonce:      66,
		ExtraData:  hexutil.MustDecode("0x11bbe8db4e347b4e8c937c1c8370e4b5ed33adb3db69cbdb7a38e1e50b1b82fa"),
		GasLimit:   5000,
		Difficulty: big.NewInt(17179869184),
		Alloc:      readPrealloc("allocs/mainnet.json"),
	}
}

// HoleskyGenesisBlock returns the Holesky main net genesis block.
func HoleskyGenesisBlock() *types.Genesis {
	return &types.Genesis{
		Config:     params.HoleskyChainConfig,
		Nonce:      4660,
		GasLimit:   25000000,
		Difficulty: big.NewInt(1),
		Timestamp:  1695902100,
		Alloc:      readPrealloc("allocs/holesky.json"),
	}
}

// SepoliaGenesisBlock returns the Sepolia network genesis block.
func SepoliaGenesisBlock() *types.Genesis {
	return &types.Genesis{
		Config:     params.SepoliaChainConfig,
		Nonce:      0,
		ExtraData:  []byte("Sepolia, Athens, Attica, Greece!"),
		GasLimit:   30000000,
		Difficulty: big.NewInt(131072),
		Timestamp:  1633267481,
		Alloc:      readPrealloc("allocs/sepolia.json"),
	}
}

// GoerliGenesisBlock returns the Görli network genesis block.
func GoerliGenesisBlock() *types.Genesis {
	return &types.Genesis{
		Config:     params.GoerliChainConfig,
		Timestamp:  1548854791,
		ExtraData:  hexutil.MustDecode("0x22466c6578692069732061207468696e6722202d204166726900000000000000e0a2bd4258d2768837baa26a28fe71dc079f84c70000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000"),
		GasLimit:   10485760,
		Difficulty: big.NewInt(1),
		Alloc:      readPrealloc("allocs/goerli.json"),
	}
}

func MumbaiGenesisBlock() *types.Genesis {
	return &types.Genesis{
		Config:     params.MumbaiChainConfig,
		Nonce:      0,
		Timestamp:  1558348305,
		GasLimit:   10000000,
		Difficulty: big.NewInt(1),
		Mixhash:    libcommon.HexToHash("0x0000000000000000000000000000000000000000000000000000000000000000"),
		Coinbase:   libcommon.HexToAddress("0x0000000000000000000000000000000000000000"),
		Alloc:      readPrealloc("allocs/mumbai.json"),
	}
}

// BorMainnetGenesisBlock returns the Bor Mainnet network genesis block.
func BorMainnetGenesisBlock() *types.Genesis {
	return &types.Genesis{
		Config:     params.BorMainnetChainConfig,
		Nonce:      0,
		Timestamp:  1590824836,
		GasLimit:   10000000,
		Difficulty: big.NewInt(1),
		Mixhash:    libcommon.HexToHash("0x0000000000000000000000000000000000000000000000000000000000000000"),
		Coinbase:   libcommon.HexToAddress("0x0000000000000000000000000000000000000000"),
		Alloc:      readPrealloc("allocs/bor_mainnet.json"),
	}
}

func BorDevnetGenesisBlock() *types.Genesis {
	return &types.Genesis{
		Config:     params.BorDevnetChainConfig,
		Nonce:      0,
		Timestamp:  1558348305,
		GasLimit:   10000000,
		Difficulty: big.NewInt(1),
		Mixhash:    libcommon.HexToHash("0x0000000000000000000000000000000000000000000000000000000000000000"),
		Coinbase:   libcommon.HexToAddress("0x0000000000000000000000000000000000000000"),
		Alloc:      readPrealloc("allocs/bor_devnet.json"),
	}
}

func OptimismDevnetGenesisBlock() *types.Genesis {
	// copy of optimism goerli spec
	return &types.Genesis{
		Config:     params.OPDevnetChainConfig,
		Difficulty: big.NewInt(0),
		Mixhash:    libcommon.HexToHash("0x0000000000000000000000000000000000000000000000000000000000000000"),
		ExtraData:  hexutil.MustDecode("0x424544524f434b"), // BEDROCK
		GasLimit:   30000000,
		GasUsed:    0,
		Alloc:      readPrealloc("allocs/optimism_devnet.json"),
	}
}

func GnosisGenesisBlock() *types.Genesis {
	return &types.Genesis{
		Config:     params.GnosisChainConfig,
		Timestamp:  0,
		AuRaSeal:   common.FromHex("0x0000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000"),
		GasLimit:   0x989680,
		Difficulty: big.NewInt(0x20000),
		Alloc:      readPrealloc("allocs/gnosis.json"),
	}
}

func ChiadoGenesisBlock() *types.Genesis {
	return &types.Genesis{
		Config:     params.ChiadoChainConfig,
		Timestamp:  0,
		AuRaSeal:   common.FromHex("0x0000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000"),
		GasLimit:   0x989680,
		Difficulty: big.NewInt(0x20000),
		Alloc:      readPrealloc("allocs/chiado.json"),
	}
}

// Pre-calculated version of:
//
//	DevnetSignPrivateKey = crypto.HexToECDSA(sha256.Sum256([]byte("erigon devnet key")))
//	DevnetEtherbase=crypto.PubkeyToAddress(DevnetSignPrivateKey.PublicKey)
var DevnetSignPrivateKey, _ = crypto.HexToECDSA("26e86e45f6fc45ec6e2ecd128cec80fa1d1505e5507dcd2ae58c3130a7a97b48")
var DevnetEtherbase = libcommon.HexToAddress("67b1d87101671b127f5f8714789c7192f7ad340e")

// DevnetSignKey is defined like this to allow the devnet process to pre-allocate keys
// for nodes and then pass the address via --miner.etherbase - the function will be called
// to retieve the mining key
var DevnetSignKey = func(address libcommon.Address) *ecdsa.PrivateKey {
	return DevnetSignPrivateKey
}

// DeveloperGenesisBlock returns the 'geth --dev' genesis block.
func DeveloperGenesisBlock(period uint64, faucet libcommon.Address) *types.Genesis {
	// Override the default period to the user requested one
	config := *params.AllCliqueProtocolChanges
	config.Clique.Period = period

	// Assemble and return the genesis with the precompiles and faucet pre-funded
	return &types.Genesis{
		Config:     &config,
		ExtraData:  append(append(make([]byte, 32), faucet[:]...), make([]byte, crypto.SignatureLength)...),
		GasLimit:   11500000,
		Difficulty: big.NewInt(1),
		Alloc:      readPrealloc("allocs/dev.json"),
	}
}

// ToBlock creates the genesis block and writes state of a genesis specification
// to the given database (or discards it if nil).
func GenesisToBlock(g *types.Genesis, tmpDir string) (*types.Block, *state.IntraBlockState, error) {
	_ = g.Alloc //nil-check

	head := &types.Header{
		Number:        new(big.Int).SetUint64(g.Number),
		Nonce:         types.EncodeNonce(g.Nonce),
		Time:          g.Timestamp,
		ParentHash:    g.ParentHash,
		Extra:         g.ExtraData,
		GasLimit:      g.GasLimit,
		GasUsed:       g.GasUsed,
		Difficulty:    g.Difficulty,
		MixDigest:     g.Mixhash,
		Coinbase:      g.Coinbase,
		BaseFee:       g.BaseFee,
		BlobGasUsed:   g.BlobGasUsed,
		ExcessBlobGas: g.ExcessBlobGas,
		AuRaStep:      g.AuRaStep,
		AuRaSeal:      g.AuRaSeal,
	}
	if g.GasLimit == 0 {
		head.GasLimit = params.GenesisGasLimit
	}
	if g.Difficulty == nil {
		head.Difficulty = params.GenesisDifficulty
	}
	if g.Config != nil && g.Config.IsLondon(0) {
		if g.BaseFee != nil {
			head.BaseFee = g.BaseFee
		} else {
			head.BaseFee = new(big.Int).SetUint64(params.InitialBaseFee)
		}
	}

	var withdrawals []*types.Withdrawal
	if g.Config != nil && g.Config.IsShanghai(g.Timestamp) {
		withdrawals = []*types.Withdrawal{}
	}

	if g.Config != nil && g.Config.IsCancun(g.Timestamp) {
		if g.BlobGasUsed != nil {
			head.BlobGasUsed = g.BlobGasUsed
		} else {
			head.BlobGasUsed = new(uint64)
		}
		if g.ExcessBlobGas != nil {
			head.ExcessBlobGas = g.ExcessBlobGas
		} else {
			head.ExcessBlobGas = new(uint64)
		}
		if g.ParentBeaconBlockRoot != nil {
			head.ParentBeaconBlockRoot = g.ParentBeaconBlockRoot
		} else {
			head.ParentBeaconBlockRoot = &libcommon.Hash{}
		}
	}

	var root libcommon.Hash
	var statedb *state.IntraBlockState
	wg := sync.WaitGroup{}
	wg.Add(1)

	var err error
	go func() { // we may run inside write tx, can't open 2nd write tx in same goroutine
		// TODO(yperbasis): use memdb.MemoryMutation instead
		defer wg.Done()
		genesisTmpDB := mdbx.NewMDBX(log.New()).InMem(tmpDir).MapSize(2 * datasize.GB).GrowthStep(1 * datasize.MB).MustOpen()
		defer genesisTmpDB.Close()
		var tx kv.RwTx
		if tx, err = genesisTmpDB.BeginRw(context.Background()); err != nil {
			return
		}
		defer tx.Rollback()

		r, w := state.NewDbStateReader(tx), state.NewDbStateWriter(tx, 0)
		statedb = state.New(r)

		hasConstructorAllocation := false
		for _, account := range g.Alloc {
			if len(account.Constructor) > 0 {
				hasConstructorAllocation = true
				break
			}
		}
		// See https://github.com/NethermindEth/nethermind/blob/master/src/Nethermind/Nethermind.Consensus.AuRa/InitializationSteps/LoadGenesisBlockAuRa.cs
		if hasConstructorAllocation && g.Config.Aura != nil {
			statedb.CreateAccount(libcommon.Address{}, false)
		}

		keys := sortedAllocKeys(g.Alloc)
		for _, key := range keys {
			addr := libcommon.BytesToAddress([]byte(key))
			account := g.Alloc[addr]

			balance, overflow := uint256.FromBig(account.Balance)
			if overflow {
				panic("overflow at genesis allocs")
			}
			statedb.AddBalance(addr, balance)
			statedb.SetCode(addr, account.Code)
			statedb.SetNonce(addr, account.Nonce)
			for key, value := range account.Storage {
				key := key
				val := uint256.NewInt(0).SetBytes(value.Bytes())
				statedb.SetState(addr, &key, *val)
			}

			if len(account.Constructor) > 0 {
				if _, err = SysCreate(addr, account.Constructor, *g.Config, statedb, head); err != nil {
					return
				}
			}

			if len(account.Code) > 0 || len(account.Storage) > 0 || len(account.Constructor) > 0 {
				statedb.SetIncarnation(addr, state.FirstContractIncarnation)
			}
		}
		if err = statedb.FinalizeTx(&chain.Rules{}, w); err != nil {
			return
		}
		if root, err = trie.CalcRoot("genesis", tx); err != nil {
			return
		}
	}()
	wg.Wait()
	if err != nil {
		return nil, nil, err
	}

	if g.StateHash != nil {
		if len(g.Alloc) > 0 {
			panic(fmt.Errorf("cannot both have genesis hash %s "+
				"and non-empty state-allocation", *g.StateHash))
		}
		root = *g.StateHash
	}
	head.Root = root

	return types.NewBlock(head, nil, nil, nil, withdrawals), statedb, nil
}

func sortedAllocKeys(m types.GenesisAlloc) []string {
	keys := make([]string, len(m))
	i := 0
	for k := range m {
		keys[i] = string(k.Bytes())
		i++
	}
	slices.Sort(keys)
	return keys
}

//go:embed allocs
var allocs embed.FS

func readPrealloc(filename string) types.GenesisAlloc {
	f, err := allocs.Open(filename)
	if err != nil {
		panic(fmt.Sprintf("Could not open genesis preallocation for %s: %v", filename, err))
	}
	defer f.Close()
	decoder := json.NewDecoder(f)
	ga := make(types.GenesisAlloc)
	err = decoder.Decode(&ga)
	if err != nil {
		panic(fmt.Sprintf("Could not parse genesis preallocation for %s: %v", filename, err))
	}
	return ga
}

func GenesisBlockByChainName(chain string) *types.Genesis {
	genesis, err := loadOPStackGenesisByChainName(chain)
	if err != nil {
		panic(err)
	}
	if genesis != nil {
		return genesis
	}
	switch chain {
	case networkname.MainnetChainName:
		return MainnetGenesisBlock()
	case networkname.HoleskyChainName:
		return HoleskyGenesisBlock()
	case networkname.SepoliaChainName:
		return SepoliaGenesisBlock()
	case networkname.GoerliChainName:
		return GoerliGenesisBlock()
	case networkname.MumbaiChainName:
		return MumbaiGenesisBlock()
	case networkname.BorMainnetChainName:
		return BorMainnetGenesisBlock()
	case networkname.BorDevnetChainName:
		return BorDevnetGenesisBlock()
	case networkname.OPDevnetChainName:
		return OptimismDevnetGenesisBlock()
	case networkname.GnosisChainName:
		return GnosisGenesisBlock()
	case networkname.ChiadoChainName:
		return ChiadoGenesisBlock()
	default:
		return nil
	}
}

// loadOPStackGenesisByChainName loads genesis block corresponding to the chain name from superchain regsitry.
// This implementation is based on op-geth(https://github.com/ethereum-optimism/op-geth/blob/c7871bc4454ffc924eb128fa492975b30c9c46ad/core/superchain.go#L13)
func loadOPStackGenesisByChainName(name string) (*types.Genesis, error) {
	opStackChainCfg := params.OPStackChainConfigByName(name)
	if opStackChainCfg == nil {
		return nil, nil
	}

	cfg := params.LoadSuperChainConfig(opStackChainCfg)
	if cfg == nil {
		return nil, nil
	}

	gen, err := superchain.LoadGenesis(opStackChainCfg.ChainID)
	if err != nil {
		return nil, fmt.Errorf("failed to load genesis definition for chain %d: %w", opStackChainCfg.ChainID, err)
	}

	genesis := &types.Genesis{
		Config:     cfg,
		Nonce:      gen.Nonce,
		Timestamp:  gen.Timestamp,
		ExtraData:  gen.ExtraData,
		GasLimit:   gen.GasLimit,
		Difficulty: (*big.Int)(gen.Difficulty),
		Mixhash:    libcommon.Hash(gen.Mixhash),
		Coinbase:   libcommon.Address(gen.Coinbase),
		Alloc:      make(types.GenesisAlloc),
		Number:     gen.Number,
		GasUsed:    gen.GasUsed,
		ParentHash: libcommon.Hash(gen.ParentHash),
		BaseFee:    (*big.Int)(gen.BaseFee),
	}

	for addr, acc := range gen.Alloc {
		var code []byte
		if acc.CodeHash != ([32]byte{}) {
			dat, err := superchain.LoadContractBytecode(acc.CodeHash)
			if err != nil {
				return nil, fmt.Errorf("failed to load bytecode %s of address %s in chain %d: %w", acc.CodeHash, addr, opStackChainCfg.ChainID, err)
			}
			code = dat
		}
		var storage map[libcommon.Hash]libcommon.Hash
		if len(acc.Storage) > 0 {
			storage = make(map[libcommon.Hash]libcommon.Hash)
			for k, v := range acc.Storage {
				storage[libcommon.Hash(k)] = libcommon.Hash(v)
			}
		}
		bal := libcommon.Big0
		if acc.Balance != nil {
			bal = (*big.Int)(acc.Balance)
		}
		genesis.Alloc[libcommon.Address(addr)] = types.GenesisAccount{
			Code:    code,
			Storage: storage,
			Balance: bal,
			Nonce:   acc.Nonce,
		}
	}
	if gen.StateHash != nil {
		if len(gen.Alloc) > 0 {
			return nil, fmt.Errorf("chain definition unexpectedly contains both allocation (%d) and state-hash %s", len(gen.Alloc), *gen.StateHash)
		}
		genesis.StateHash = (*libcommon.Hash)(gen.StateHash)
	}

	genesisBlock, _, err := GenesisToBlock(genesis, "")
	if err != nil {
		return nil, fmt.Errorf("failed to build genesis block: %w", err)
	}
	genesisBlockHash := genesisBlock.Hash()
	expectedHash := libcommon.Hash([32]byte(opStackChainCfg.Genesis.L2.Hash))

	// Verify we correctly produced the genesis config by recomputing the genesis-block-hash,
	// and check the genesis matches the chain genesis definition.
	if opStackChainCfg.Genesis.L2.Number != genesisBlock.NumberU64() {
		switch opStackChainCfg.ChainID {
		case params.OPMainnetChainID:
			expectedHash = params.OPMainnetGenesisHash
		case params.OPGoerliChainID:
			expectedHash = params.OPGoerliGenesisHash
		default:
			return nil, fmt.Errorf("unknown stateless genesis definition for chain %d", opStackChainCfg.ChainID)
		}
	}
	if expectedHash != genesisBlockHash {
		return nil, fmt.Errorf("produced genesis with hash %s but expected %s", genesisBlockHash, expectedHash)
	}
	return genesis, nil
}