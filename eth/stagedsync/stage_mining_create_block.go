package stagedsync

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	mapset "github.com/deckarep/golang-set"
	"github.com/ledgerwatch/erigon-lib/kv"
	"github.com/ledgerwatch/erigon-lib/txpool"
	"github.com/ledgerwatch/erigon/common"
	"github.com/ledgerwatch/erigon/common/debug"
	"github.com/ledgerwatch/erigon/consensus"
	"github.com/ledgerwatch/erigon/core"
	"github.com/ledgerwatch/erigon/core/rawdb"
	"github.com/ledgerwatch/erigon/core/types"
	"github.com/ledgerwatch/erigon/eth/ethutils"
	"github.com/ledgerwatch/erigon/params"
	"github.com/ledgerwatch/log/v3"
)

type MiningBlock struct {
	Header   *types.Header
	Uncles   []*types.Header
	Txs      []types.Transaction
	Receipts types.Receipts

	LocalTxs  types.TransactionsStream
	RemoteTxs types.TransactionsStream
}

type MiningState struct {
	MiningConfig    *params.MiningConfig
	PendingResultCh chan *types.Block
	MiningResultCh  chan *types.Block
	MiningBlock     *MiningBlock
}

func NewMiningState(cfg *params.MiningConfig) MiningState {
	return MiningState{
		MiningConfig:    cfg,
		PendingResultCh: make(chan *types.Block, 1),
		MiningResultCh:  make(chan *types.Block, 1),
		MiningBlock:     &MiningBlock{},
	}
}

type MiningCreateBlockCfg struct {
	db          kv.RwDB
	miner       MiningState
	chainConfig params.ChainConfig
	engine      consensus.Engine
	txPool      *core.TxPool
	txPool2     *txpool.TxPool
	txPool2DB   kv.RoDB
	tmpdir      string
}

func StageMiningCreateBlockCfg(
	db kv.RwDB,
	miner MiningState,
	chainConfig params.ChainConfig,
	engine consensus.Engine,
	txPool *core.TxPool,
	txPool2 *txpool.TxPool,
	txPool2DB kv.RoDB,
	tmpdir string,
) MiningCreateBlockCfg {
	return MiningCreateBlockCfg{
		db:          db,
		miner:       miner,
		chainConfig: chainConfig,
		engine:      engine,
		txPool:      txPool,
		txPool2:     txPool2,
		txPool2DB:   txPool2DB,
		tmpdir:      tmpdir,
	}
}

// SpawnMiningCreateBlockStage
//TODO:
// - resubmitAdjustCh - variable is not implemented
func SpawnMiningCreateBlockStage(s *StageState, tx kv.RwTx, cfg MiningCreateBlockCfg, quit <-chan struct{}) (err error) {
	current := cfg.miner.MiningBlock
	txPoolLocals := cfg.txPool.Locals()
	coinbase := cfg.miner.MiningConfig.Etherbase

	const (
		// staleThreshold is the maximum depth of the acceptable stale block.
		staleThreshold = 7
	)

	if cfg.miner.MiningConfig.Etherbase == (common.Address{}) {
		return fmt.Errorf("refusing to mine without etherbase")
	}

	logPrefix := s.LogPrefix()
	executionAt, err := s.ExecutionAt(tx)
	if err != nil {
		return fmt.Errorf("getting last executed block: %w", err)
	}
	parent := rawdb.ReadHeaderByNumber(tx, executionAt)
	if parent == nil { // todo: how to return error and don't stop Erigon?
		return fmt.Errorf(fmt.Sprintf("[%s] Empty block", logPrefix), "blocknum", executionAt)
	}
	log.Info(fmt.Sprintf("[%s] Start mine", logPrefix), "block", executionAt+1)

	blockNum := executionAt + 1
	if cfg.txPool2 != nil {
		txSlots := txpool.TxsRlp{}
		if err = cfg.txPool2DB.View(context.Background(), func(tx kv.Tx) error {
			if err := cfg.txPool2.Best(200, &txSlots, tx); err != nil {
				return err
			}
			for i := 0; i < len(txSlots.Txs); i++ {
				txSlots.Txs[i] = common.CopyBytes(txSlots.Txs[i]) // because we need this data outside of tx
			}
			return nil
		}); err != nil {
			return err
		}
		txs, err := types.UnmarshalTransactionsFromBinary(txSlots.Txs)
		if err != nil {
			return fmt.Errorf("decode rlp of pending txs: %w", err)
		}
		var sender common.Address
		for i := range txs {
			copy(sender[:], txSlots.Senders.At(i))
			txs[i].SetSender(sender)
		}
		current.RemoteTxs = types.NewTransactionsFixedOrder(txs)
		// txpool v2 - doesn't prioritise local txs over remote
		current.LocalTxs = types.NewTransactionsFixedOrder(nil)
	} else {
		pendingTxs, err := cfg.txPool.Pending()
		if err != nil {
			return err
		}
		// Split the pending transactions into locals and remotes
		localTxs, remoteTxs := types.TransactionsGroupedBySender{}, types.TransactionsGroupedBySender{}
		signer := types.MakeSigner(&cfg.chainConfig, blockNum)
		for _, txs := range pendingTxs {
			if len(txs) == 0 {
				continue
			}
			from, _ := txs[0].Sender(*signer)
			isLocal := false
			for _, local := range txPoolLocals {
				if local == from {
					isLocal = true
					break
				}
			}

			if isLocal {
				localTxs = append(localTxs, txs)
			} else {
				remoteTxs = append(remoteTxs, txs)
			}
		}

		current.LocalTxs = types.NewTransactionsByPriceAndNonce(*signer, localTxs)
		current.RemoteTxs = types.NewTransactionsByPriceAndNonce(*signer, remoteTxs)
	}
	localUncles, remoteUncles, err := readNonCanonicalHeaders(tx, blockNum, cfg.engine, coinbase, txPoolLocals)
	if err != nil {
		return err
	}
	chain := ChainReader{Cfg: cfg.chainConfig, Db: tx}
	var GetBlocksFromHash = func(hash common.Hash, n int) (blocks []*types.Block) {
		number := rawdb.ReadHeaderNumber(tx, hash)
		if number == nil {
			return nil
		}
		for i := 0; i < n; i++ {
			block := rawdb.ReadBlock(tx, hash, *number)
			if block == nil {
				break
			}
			blocks = append(blocks, block)
			hash = block.ParentHash()
			*number--
		}
		return
	}

	type envT struct {
		signer    *types.Signer
		ancestors mapset.Set // ancestor set (used for checking uncle parent validity)
		family    mapset.Set // family set (used for checking uncle invalidity)
		uncles    mapset.Set // uncle set
	}
	env := &envT{
		signer:    types.MakeSigner(&cfg.chainConfig, blockNum),
		ancestors: mapset.NewSet(),
		family:    mapset.NewSet(),
		uncles:    mapset.NewSet(),
	}

	// re-written miner/worker.go:commitNewWork
	timestamp := time.Now().Unix()
	if parent.Time >= uint64(timestamp) {
		timestamp = int64(parent.Time + 1)
	}
	num := parent.Number
	header := &types.Header{
		ParentHash: parent.Hash(),
		Number:     num.Add(num, common.Big1),
		GasLimit:   core.CalcGasLimit(parent.GasUsed, parent.GasLimit, cfg.miner.MiningConfig.GasFloor, cfg.miner.MiningConfig.GasCeil),
		Extra:      cfg.miner.MiningConfig.ExtraData,
		Time:       uint64(timestamp),
	}

	// Only set the coinbase if our consensus engine is running (avoid spurious block rewards)
	//if w.isRunning() {
	header.Coinbase = coinbase
	//}

	if err = cfg.engine.Prepare(chain, header); err != nil {
		log.Error("Failed to prepare header for mining",
			"err", err,
			"headerNumber", header.Number.Uint64(),
			"headerRoot", header.Root.String(),
			"headerParentHash", header.ParentHash.String(),
			"parentNumber", parent.Number.Uint64(),
			"parentHash", parent.Hash().String(),
			"callers", debug.Callers(10))
		return err
	}

	// If we are care about TheDAO hard-fork check whether to override the extra-data or not
	if daoBlock := cfg.chainConfig.DAOForkBlock; daoBlock != nil {
		// Check whether the block is among the fork extra-override range
		limit := new(big.Int).Add(daoBlock, params.DAOForkExtraRange)
		if header.Number.Cmp(daoBlock) >= 0 && header.Number.Cmp(limit) < 0 {
			// Depending whether we support or oppose the fork, override differently
			if cfg.chainConfig.DAOForkSupport {
				header.Extra = common.CopyBytes(params.DAOForkBlockExtra)
			} else if bytes.Equal(header.Extra, params.DAOForkBlockExtra) {
				header.Extra = []byte{} // If miner opposes, don't let it use the reserved extra-data
			}
		}
	}

	// analog of miner.Worker.updateSnapshot
	var makeUncles = func(proposedUncles mapset.Set) []*types.Header {
		var uncles []*types.Header
		proposedUncles.Each(func(item interface{}) bool {
			hash, ok := item.(common.Hash)
			if !ok {
				return false
			}

			uncle, exist := localUncles[hash]
			if !exist {
				uncle, exist = remoteUncles[hash]
			}
			if !exist {
				return false
			}
			uncles = append(uncles, uncle)
			return false
		})
		return uncles
	}
	// when 08 is processed ancestors contain 07 (quick block)
	for _, ancestor := range GetBlocksFromHash(parent.Hash(), 7) {
		for _, uncle := range ancestor.Uncles() {
			env.family.Add(uncle.Hash())
		}
		env.family.Add(ancestor.Hash())
		env.ancestors.Add(ancestor.Hash())
	}
	commitUncle := func(env *envT, uncle *types.Header) error {
		hash := uncle.Hash()
		if env.uncles.Contains(hash) {
			return errors.New("uncle not unique")
		}
		if parent.Hash() == uncle.ParentHash {
			return errors.New("uncle is sibling")
		}
		if !env.ancestors.Contains(uncle.ParentHash) {
			return errors.New("uncle's parent unknown")
		}
		if env.family.Contains(hash) {
			return errors.New("uncle already included")
		}
		env.uncles.Add(uncle.Hash())
		return nil
	}

	// Accumulate the miningUncles for the env block
	// Prefer to locally generated uncle
	uncles := make([]*types.Header, 0, 2)
	for _, blocks := range []map[common.Hash]*types.Header{localUncles, remoteUncles} {
		// Clean up stale uncle blocks first
		for hash, uncle := range blocks {
			if uncle.Number.Uint64()+staleThreshold <= header.Number.Uint64() {
				delete(blocks, hash)
			}
		}
		for hash, uncle := range blocks {
			if len(uncles) == 2 {
				break
			}
			if err = commitUncle(env, uncle); err != nil {
				log.Trace("Possible uncle rejected", "hash", hash, "reason", err)
			} else {
				log.Debug("Committing new uncle to block", "hash", hash)
				uncles = append(uncles, uncle)
			}
		}
	}

	current.Header = header
	current.Uncles = makeUncles(env.uncles)
	return nil
}

func readNonCanonicalHeaders(tx kv.Tx, blockNum uint64, engine consensus.Engine, coinbase common.Address, txPoolLocals []common.Address) (localUncles, remoteUncles map[common.Hash]*types.Header, err error) {
	localUncles, remoteUncles = map[common.Hash]*types.Header{}, map[common.Hash]*types.Header{}
	nonCanonicalBlocks, err := rawdb.ReadHeadersByNumber(tx, blockNum)
	if err != nil {
		return
	}
	for _, u := range nonCanonicalBlocks {
		if ethutils.IsLocalBlock(engine, coinbase, txPoolLocals, u) {
			localUncles[u.Hash()] = u
		} else {
			remoteUncles[u.Hash()] = u
		}

	}
	return
}
