package freezeblocks

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"reflect"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/holiman/uint256"
	"github.com/ledgerwatch/erigon-lib/chain"
	common2 "github.com/ledgerwatch/erigon-lib/common"
	"github.com/ledgerwatch/erigon-lib/common/dbg"
	"github.com/ledgerwatch/erigon-lib/common/hexutility"
	"github.com/ledgerwatch/erigon-lib/downloader/snaptype"
	"github.com/ledgerwatch/erigon-lib/kv"
	types2 "github.com/ledgerwatch/erigon-lib/types"
	"github.com/ledgerwatch/erigon/core/rawdb"
	coresnaptype "github.com/ledgerwatch/erigon/core/snaptype"
	"github.com/ledgerwatch/erigon/core/types"
	"github.com/ledgerwatch/erigon/eth/ethconfig/estimate"
	"github.com/ledgerwatch/erigon/rlp"
	"github.com/ledgerwatch/erigon/turbo/services"
	"github.com/ledgerwatch/log/v3"
	"golang.org/x/sync/errgroup"
)

func (br *BlockRetire) dbHasEnoughDataForPreBedrockRetire(ctx context.Context) (bool, error) {
	return true, nil
}

func (br *BlockRetire) retirePreBedrockBlocks(ctx context.Context, minBlockNum uint64, maxBlockNum uint64, lvl log.Lvl, seedNewSnapshots func(downloadRequest []services.DownloadRequest) error, onDelete func(l []string) error) (bool, error) {
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	default:
	}
	notifier, logger, blockReader, tmpDir, db, workers := br.notifier, br.logger, br.blockReader, br.tmpDir, br.db, br.workers
	snapshots := br.snapshots()

	blockFrom, blockTo, ok := CanRetire(maxBlockNum, minBlockNum, snaptype.Unknown, br.chainConfig)

	if ok {
		if has, err := br.dbHasEnoughDataForPreBedrockRetire(ctx); err != nil {
			return false, err
		} else if !has {
			return false, nil
		}

		logger.Log(lvl, "[prebedrock snapshots] Retire PreBedrock Blocks", "range", fmt.Sprintf("%dk-%dk", blockFrom/1000, blockTo/1000))
		// in future we will do it in background
		if err := DumpPreBedrockBlocks(ctx, blockFrom, blockTo, br.chainConfig, tmpDir, snapshots.Dir(), db, workers, lvl, logger, blockReader); err != nil {
			return ok, fmt.Errorf("DumpBlocks: %w", err)
		}

		if err := snapshots.ReopenFolder(); err != nil {
			return ok, fmt.Errorf("reopen: %w", err)
		}
		snapshots.LogStat("blocks:retire")
		if notifier != nil && !reflect.ValueOf(notifier).IsNil() { // notify about new snapshots of any size
			notifier.OnNewSnapshot()
		}

	}

	return true, nil
}

func DumpPreBedrockBlocks(ctx context.Context, blockFrom, blockTo uint64, chainConfig *chain.Config, tmpDir, snapDir string, chainDB kv.RoDB, workers int, lvl log.Lvl, logger log.Logger, blockReader services.FullBlockReader) error {

	firstTxNum := blockReader.FirstTxnNumNotInSnapshots()
	for i := blockFrom; i < blockTo; i = chooseSegmentEnd(i, blockTo, coresnaptype.Enums.Headers, chainConfig) {
		lastTxNum, err := dumpPreBedrockBlocksRange(ctx, i, chooseSegmentEnd(i, blockTo, coresnaptype.Enums.Headers, chainConfig), tmpDir, snapDir, firstTxNum, chainDB, chainConfig, workers, lvl, logger)
		if err != nil {
			return err
		}
		firstTxNum = lastTxNum + 1
	}
	return nil
}

func dumpPreBedrockBlocksRange(ctx context.Context, blockFrom, blockTo uint64, tmpDir, snapDir string, firstTxNum uint64, chainDB kv.RoDB, chainConfig *chain.Config, workers int, lvl log.Lvl, logger log.Logger) (lastTxNum uint64, err error) {
	logEvery := time.NewTicker(20 * time.Second)
	defer logEvery.Stop()

	if _, err = dumpRange(ctx, coresnaptype.Headers.FileInfo(snapDir, blockFrom, blockTo),
		DumpPreBedrockHeaders, nil, chainDB, chainConfig, tmpDir, workers, lvl, logger); err != nil {
		return 0, err
	}

	if lastTxNum, err = dumpRange(ctx, coresnaptype.Bodies.FileInfo(snapDir, blockFrom, blockTo),
		DumpPreBedrockBodies, func(context.Context) uint64 { return firstTxNum }, chainDB, chainConfig, tmpDir, workers, lvl, logger); err != nil {
		return lastTxNum, err
	}

	if _, err = dumpRange(ctx, coresnaptype.Transactions.FileInfo(snapDir, blockFrom, blockTo),
		DumpPreBedrockTxs, func(context.Context) uint64 { return firstTxNum }, chainDB, chainConfig, tmpDir, workers, lvl, logger); err != nil {
		return lastTxNum, err
	}

	return lastTxNum, nil
}

// DumpTxs - [from, to)
// Format: hash[0]_1byte + sender_address_2bytes + txnRlp
func DumpPreBedrockTxs(ctx context.Context, db kv.RoDB, chainConfig *chain.Config, blockFrom, blockTo uint64, _ firstKeyGetter, collect func([]byte) error, workers int, lvl log.Lvl, logger log.Logger) (lastTx uint64, err error) {
	logEvery := time.NewTicker(20 * time.Second)
	defer logEvery.Stop()
	warmupCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	chainID, _ := uint256.FromBig(chainConfig.ChainID)

	numBuf := make([]byte, 8)

	parse := func(ctx *types2.TxParseContext, v, valueBuf []byte, senders []common2.Address, j int) ([]byte, error) {
		var sender [20]byte
		slot := types2.TxSlot{}

		if _, err := ctx.ParseTransaction(v, 0, &slot, sender[:], false /* hasEnvelope */, false /* wrappedWithBlobs */, nil); err != nil {
			return valueBuf, err
		}
		if len(senders) > 0 {
			sender = senders[j]
		}

		valueBuf = valueBuf[:0]
		valueBuf = append(valueBuf, slot.IDHash[:1]...)
		valueBuf = append(valueBuf, sender[:]...)
		valueBuf = append(valueBuf, v...)
		return valueBuf, nil
	}

	addSystemTx := func(ctx *types2.TxParseContext, tx kv.Tx, txId uint64) error {
		binary.BigEndian.PutUint64(numBuf, txId)
		tv, err := tx.GetOne(kv.EthTx, numBuf)
		if err != nil {
			return err
		}
		if tv == nil {
			if err := collect(nil); err != nil {
				return err
			}
			return nil
		}

		ctx.WithSender(false)

		valueBuf := bufPool.Get().(*[16 * 4096]byte)
		defer bufPool.Put(valueBuf)

		parsed, err := parse(ctx, tv, valueBuf[:], nil, 0)
		if err != nil {
			return err
		}
		if err := collect(parsed); err != nil {
			return err
		}
		return nil
	}

	doWarmup, warmupTxs, warmupSenders := blockTo-blockFrom >= 100_000 && workers > 4, &atomic.Bool{}, &atomic.Bool{}
	from := hexutility.EncodeTs(blockFrom)
	if err := kv.BigChunks(db, kv.HeaderCanonical, from, func(tx kv.Tx, k, v []byte) (bool, error) {
		blockNum := binary.BigEndian.Uint64(k)
		if blockNum >= blockTo { // [from, to)
			return false, nil
		}

		h := common2.BytesToHash(v)
		dataRLP := rawdb.ReadStorageBodyRLP(tx, h, blockNum)
		if dataRLP == nil {
			return false, fmt.Errorf("body not found: %d, %x", blockNum, h)
		}
		var body types.BodyForStorage
		if e := rlp.DecodeBytes(dataRLP, &body); e != nil {
			return false, e
		}
		if body.TxAmount == 0 {
			return true, nil
		}

		if doWarmup && !warmupSenders.Load() && blockNum%1_000 == 0 {
			clean := kv.ReadAhead(warmupCtx, db, warmupSenders, kv.Senders, hexutility.EncodeTs(blockNum), 10_000)
			defer clean()
		}
		if doWarmup && !warmupTxs.Load() && blockNum%1_000 == 0 {
			clean := kv.ReadAhead(warmupCtx, db, warmupTxs, kv.EthTx, hexutility.EncodeTs(body.BaseTxId), 100*10_000)
			defer clean()
		}
		senders, err := rawdb.ReadSenders(tx, h, blockNum)
		if err != nil {
			return false, err
		}

		workers := estimate.AlmostAllCPUs()

		if workers > 3 {
			workers = workers / 3 * 2
		}

		if workers > int(body.TxAmount-2) {
			if int(body.TxAmount-2) > 1 {
				workers = int(body.TxAmount - 2)
			} else {
				workers = 1
			}
		}

		parsers := errgroup.Group{}
		parsers.SetLimit(workers)

		valueBufs := make([][]byte, workers)
		parseCtxs := make([]*types2.TxParseContext, workers)

		for i := 0; i < workers; i++ {
			valueBuf := bufPool.Get().(*[16 * 4096]byte)
			defer bufPool.Put(valueBuf)
			valueBufs[i] = valueBuf[:]
			parseCtxs[i] = types2.NewTxParseContext(*chainID)
		}

		if err := addSystemTx(parseCtxs[0], tx, body.BaseTxId); err != nil {
			return false, err
		}

		binary.BigEndian.PutUint64(numBuf, body.BaseTxId+1)

		collected := -1
		collectorLock := sync.Mutex{}
		collections := sync.NewCond(&collectorLock)

		var j int

		if err := tx.ForAmount(kv.EthTx, numBuf, body.TxAmount-2, func(_, tv []byte) error {
			tx := j
			j++

			parsers.Go(func() error {
				parseCtx := parseCtxs[tx%workers]

				parseCtx.WithSender(len(senders) == 0)
				parseCtx.WithAllowPreEip2s(blockNum <= chainConfig.HomesteadBlock.Uint64())

				valueBuf, err := parse(parseCtx, tv, valueBufs[tx%workers], senders, tx)

				if err != nil {
					return fmt.Errorf("%w, block: %d", err, blockNum)
				}

				collectorLock.Lock()
				defer collectorLock.Unlock()

				for collected < tx-1 {
					collections.Wait()
				}

				// first tx byte => sender adress => tx rlp
				if err := collect(valueBuf); err != nil {
					return err
				}

				collected = tx
				collections.Broadcast()

				return nil
			})

			return nil
		}); err != nil {
			return false, fmt.Errorf("ForAmount: %w", err)
		}

		if err := parsers.Wait(); err != nil {
			return false, fmt.Errorf("ForAmount parser: %w", err)
		}

		if err := addSystemTx(parseCtxs[0], tx, body.BaseTxId+uint64(body.TxAmount)-1); err != nil {
			return false, err
		}

		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-logEvery.C:
			var m runtime.MemStats
			if lvl >= log.LvlInfo {
				dbg.ReadMemStats(&m)
			}
			logger.Log(lvl, "[snapshots] Dumping txs", "block num", blockNum,
				"alloc", common2.ByteCount(m.Alloc), "sys", common2.ByteCount(m.Sys),
			)
		default:
		}
		return true, nil
	}); err != nil {
		return 0, fmt.Errorf("BigChunks: %w", err)
	}
	return 0, nil
}

// DumpHeaders - [from, to)
func DumpPreBedrockHeaders(ctx context.Context, db kv.RoDB, _ *chain.Config, blockFrom, blockTo uint64, _ firstKeyGetter, collect func([]byte) error, workers int, lvl log.Lvl, logger log.Logger) (uint64, error) {
	logEvery := time.NewTicker(20 * time.Second)
	defer logEvery.Stop()

	key := make([]byte, 8+32)
	from := hexutility.EncodeTs(blockFrom)
	if err := kv.BigChunks(db, kv.HeaderCanonical, from, func(tx kv.Tx, k, v []byte) (bool, error) {
		blockNum := binary.BigEndian.Uint64(k)
		if blockNum >= blockTo {
			return false, nil
		}
		copy(key, k)
		copy(key[8:], v)
		dataRLP, err := tx.GetOne(kv.Headers, key)
		if err != nil {
			return false, err
		}
		if dataRLP == nil {
			return false, fmt.Errorf("header missed in db: block_num=%d,  hash=%x", blockNum, v)
		}
		h := types.Header{}
		if err := rlp.DecodeBytes(dataRLP, &h); err != nil {
			return false, err
		}

		value := make([]byte, len(dataRLP)+1) // first_byte_of_header_hash + header_rlp
		value[0] = h.Hash()[0]
		copy(value[1:], dataRLP)
		if err := collect(value); err != nil {
			return false, err
		}

		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-logEvery.C:
			var m runtime.MemStats
			if lvl >= log.LvlInfo {
				dbg.ReadMemStats(&m)
			}
			logger.Log(lvl, "[snapshots] Dumping headers", "block num", blockNum,
				"alloc", common2.ByteCount(m.Alloc), "sys", common2.ByteCount(m.Sys),
			)
		default:
		}
		return true, nil
	}); err != nil {
		return 0, err
	}
	return 0, nil
}

// DumpBodies - [from, to)
func DumpPreBedrockBodies(ctx context.Context, db kv.RoDB, _ *chain.Config, blockFrom, blockTo uint64, firstTxNum firstKeyGetter, collect func([]byte) error, workers int, lvl log.Lvl, logger log.Logger) (uint64, error) {
	logEvery := time.NewTicker(20 * time.Second)
	defer logEvery.Stop()

	blockNumByteLength := 8
	blockHashByteLength := 32
	key := make([]byte, blockNumByteLength+blockHashByteLength)
	from := hexutility.EncodeTs(blockFrom)

	lastTxNum := firstTxNum(ctx)

	if err := kv.BigChunks(db, kv.HeaderCanonical, from, func(tx kv.Tx, k, v []byte) (bool, error) {
		blockNum := binary.BigEndian.Uint64(k)
		if blockNum >= blockTo {
			return false, nil
		}
		copy(key, k)
		copy(key[8:], v)

		// Important: DB does store canonical and non-canonical txs in same table. And using same body.BaseTxID
		// But snapshots using canonical TxNum in field body.BaseTxID
		// So, we manually calc this field here and serialize again.
		//
		// FYI: we also have other table to map canonical BlockNum->TxNum: kv.MaxTxNum
		body, err := rawdb.ReadBodyForStorageByKey(tx, key)
		if err != nil {
			return false, err
		}
		if body == nil {
			logger.Warn("body missed", "block_num", blockNum, "hash", hex.EncodeToString(v))
			return true, nil
		}

		body.BaseTxId = lastTxNum
		lastTxNum += uint64(body.TxAmount)

		dataRLP, err := rlp.EncodeToBytes(body)
		if err != nil {
			return false, err
		}

		if err := collect(dataRLP); err != nil {
			return false, err
		}

		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-logEvery.C:
			var m runtime.MemStats
			if lvl >= log.LvlInfo {
				dbg.ReadMemStats(&m)
			}
			logger.Log(lvl, "[snapshots] Wrote into file", "block num", blockNum,
				"alloc", common2.ByteCount(m.Alloc), "sys", common2.ByteCount(m.Sys),
			)
		default:
		}
		return true, nil
	}); err != nil {
		return lastTxNum, err
	}

	return lastTxNum, nil
}
