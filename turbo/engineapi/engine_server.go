package engineapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"math/big"
	"reflect"
	"sync"
	"time"

	libstate "github.com/ledgerwatch/erigon-lib/state"

	"github.com/holiman/uint256"
	"github.com/ledgerwatch/erigon-lib/chain"
	libcommon "github.com/ledgerwatch/erigon-lib/common"
	"github.com/ledgerwatch/erigon-lib/common/hexutility"
	"github.com/ledgerwatch/erigon-lib/gointerfaces/txpool"
	types2 "github.com/ledgerwatch/erigon-lib/gointerfaces/types"
	"github.com/ledgerwatch/erigon-lib/kv"
	"github.com/ledgerwatch/erigon-lib/kv/kvcache"
	"github.com/ledgerwatch/log/v3"

	"github.com/ledgerwatch/erigon-lib/gointerfaces"
	"github.com/ledgerwatch/erigon/cl/clparams"
	"github.com/ledgerwatch/erigon/cmd/rpcdaemon/cli"
	"github.com/ledgerwatch/erigon/cmd/rpcdaemon/cli/httpcfg"
	"github.com/ledgerwatch/erigon/common"
	"github.com/ledgerwatch/erigon/common/hexutil"
	"github.com/ledgerwatch/erigon/consensus"
	"github.com/ledgerwatch/erigon/consensus/merge"
	"github.com/ledgerwatch/erigon/core"
	"github.com/ledgerwatch/erigon/core/rawdb"
	"github.com/ledgerwatch/erigon/core/types"
	"github.com/ledgerwatch/erigon/rpc"
	"github.com/ledgerwatch/erigon/turbo/builder"
	"github.com/ledgerwatch/erigon/turbo/engineapi/engine_helpers"
	"github.com/ledgerwatch/erigon/turbo/engineapi/engine_types"
	"github.com/ledgerwatch/erigon/turbo/jsonrpc"
	"github.com/ledgerwatch/erigon/turbo/rpchelper"
	"github.com/ledgerwatch/erigon/turbo/services"
	"github.com/ledgerwatch/erigon/turbo/stages/headerdownload"
)

// Configure network related parameters for the config.
type EngineServerConfig struct {
	// Authentication
	JwtSecret []byte
	// Network related
	Address string
	Port    int
}

type EngineServer struct {
	hd     *headerdownload.HeaderDownload
	config *chain.Config
	// Block proposing for proof-of-stake
	payloadId      uint64
	lastParameters *core.BlockBuilderParameters
	builders       map[uint64]*builder.BlockBuilder
	builderFunc    builder.BlockBuilderFunc
	proposing      bool

	ctx    context.Context
	lock   sync.Mutex
	logger log.Logger

	db          kv.RoDB
	blockReader services.FullBlockReader
}

func NewEngineServer(ctx context.Context, logger log.Logger, config *chain.Config, builderFunc builder.BlockBuilderFunc,
	db kv.RoDB, blockReader services.FullBlockReader, hd *headerdownload.HeaderDownload, proposing bool) *EngineServer {
	return &EngineServer{
		ctx:         ctx,
		logger:      logger,
		config:      config,
		builderFunc: builderFunc,
		db:          db,
		blockReader: blockReader,
		proposing:   proposing,
		hd:          hd,
		builders:    make(map[uint64]*builder.BlockBuilder),
	}
}

func (e *EngineServer) Start(httpConfig httpcfg.HttpCfg,
	filters *rpchelper.Filters, stateCache kvcache.Cache, agg *libstate.AggregatorV3, engineReader consensus.EngineReader,
	eth rpchelper.ApiBackend, txPool txpool.TxpoolClient, mining txpool.MiningClient) {
	base := jsonrpc.NewBaseApi(filters, stateCache, e.blockReader, agg, httpConfig.WithDatadir, httpConfig.EvmCallTimeout, engineReader, httpConfig.Dirs)

	ethImpl := jsonrpc.NewEthAPI(base, e.db, eth, txPool, mining, httpConfig.Gascap, httpConfig.ReturnDataLimit, e.logger)
	// engineImpl := NewEngineAPI(base, db, engineBackend)

	apiList := []rpc.API{
		{
			Namespace: "eth",
			Public:    true,
			Service:   jsonrpc.EthAPI(ethImpl),
			Version:   "1.0",
		}, {
			Namespace: "engine",
			Public:    true,
			Service:   EngineAPI(e),
			Version:   "1.0",
		}}

	if err := cli.StartRpcServerWithJwtAuthentication(e.ctx, httpConfig, apiList, e.logger); err != nil {
		e.logger.Error(err.Error())
	}
}

func (s *EngineServer) stageLoopIsBusy() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	wait, ok := s.hd.BeaconRequestList.WaitForWaiting(ctx)
	if !ok {
		select {
		case <-wait:
			return false
		case <-ctx.Done():
			return true
		}
	}
	return false
}

func (s *EngineServer) checkWithdrawalsPresence(time uint64, withdrawals []*types.Withdrawal) error {
	if !s.config.IsShanghai(time) && withdrawals != nil {
		return &rpc.InvalidParamsError{Message: "withdrawals before shanghai"}
	}
	if s.config.IsShanghai(time) && withdrawals == nil {
		return &rpc.InvalidParamsError{Message: "missing withdrawals list"}
	}
	return nil
}

// EngineNewPayload validates and possibly executes payload
func (s *EngineServer) newPayload(ctx context.Context, req *engine_types.ExecutionPayload, version clparams.StateVersion) (*engine_types.PayloadStatus, error) {
	var bloom types.Bloom
	copy(bloom[:], req.LogsBloom)

	txs := [][]byte{}
	for _, transaction := range req.Transactions {
		txs = append(txs, transaction)
	}

	header := types.Header{
		ParentHash:  req.ParentHash,
		Coinbase:    req.FeeRecipient,
		Root:        req.StateRoot,
		Bloom:       bloom,
		BaseFee:     (*big.Int)(req.BaseFeePerGas),
		Extra:       req.ExtraData,
		Number:      big.NewInt(int64(req.BlockNumber)),
		GasUsed:     uint64(req.GasUsed),
		GasLimit:    uint64(req.GasLimit),
		Time:        uint64(req.Timestamp),
		MixDigest:   req.PrevRandao,
		UncleHash:   types.EmptyUncleHash,
		Difficulty:  merge.ProofOfStakeDifficulty,
		Nonce:       merge.ProofOfStakeNonce,
		ReceiptHash: req.ReceiptsRoot,
		TxHash:      types.DeriveSha(types.BinaryTransactions(txs)),
	}
	var withdrawals []*types.Withdrawal
	if version >= clparams.CapellaVersion {
		withdrawals = req.Withdrawals
	}

	if withdrawals != nil {
		wh := types.DeriveSha(types.Withdrawals(withdrawals))
		header.WithdrawalsHash = &wh
	}

	if err := s.checkWithdrawalsPresence(header.Time, withdrawals); err != nil {
		return nil, err
	}

	if version >= clparams.DenebVersion {
		header.DataGasUsed = (*uint64)(req.DataGasUsed)
		header.ExcessDataGas = (*uint64)(req.ExcessDataGas)
	}

	if !s.config.IsCancun(header.Time) && (header.DataGasUsed != nil || header.ExcessDataGas != nil) {
		return nil, &rpc.InvalidParamsError{Message: "dataGasUsed/excessDataGas present before Cancun"}
	}

	if s.config.IsCancun(header.Time) && (header.DataGasUsed == nil || header.ExcessDataGas == nil) {
		return nil, &rpc.InvalidParamsError{Message: "dataGasUsed/excessDataGas missing"}
	}

	blockHash := req.BlockHash
	if header.Hash() != blockHash {
		s.logger.Error("[NewPayload] invalid block hash", "stated", blockHash, "actual", header.Hash())
		return &engine_types.PayloadStatus{
			Status:          engine_types.InvalidStatus,
			ValidationError: engine_types.NewStringifiedErrorFromString("invalid block hash"),
		}, nil
	}

	for _, txn := range req.Transactions {
		if types.TypedTransactionMarshalledAsRlpString(txn) {
			s.logger.Warn("[NewPayload] typed txn marshalled as RLP string", "txn", common.Bytes2Hex(txn))
			return &engine_types.PayloadStatus{
				Status:          engine_types.InvalidStatus,
				ValidationError: engine_types.NewStringifiedErrorFromString("typed txn marshalled as RLP string"),
			}, nil
		}
	}

	transactions, err := types.DecodeTransactions(txs)
	if err != nil {
		s.logger.Warn("[NewPayload] failed to decode transactions", "err", err)
		return &engine_types.PayloadStatus{
			Status:          engine_types.InvalidStatus,
			ValidationError: engine_types.NewStringifiedError(err),
		}, nil
	}
	block := types.NewBlockFromStorage(blockHash, &header, transactions, nil /* uncles */, withdrawals)

	possibleStatus, err := s.getQuickPayloadStatusIfPossible(blockHash, uint64(req.BlockNumber), header.ParentHash, nil, true)
	if err != nil {
		return nil, err
	}
	if possibleStatus != nil {
		return possibleStatus, nil
	}

	s.lock.Lock()
	defer s.lock.Unlock()

	s.logger.Debug("[NewPayload] sending block", "height", header.Number, "hash", blockHash)
	s.hd.BeaconRequestList.AddPayloadRequest(block)

	payloadStatus := <-s.hd.PayloadStatusCh
	s.logger.Debug("[NewPayload] got reply", "payloadStatus", payloadStatus)

	if payloadStatus.CriticalError != nil {
		return nil, payloadStatus.CriticalError
	}

	return &payloadStatus, nil
}

// Check if we can quickly determine the status of a newPayload or forkchoiceUpdated.
func (s *EngineServer) getQuickPayloadStatusIfPossible(blockHash libcommon.Hash, blockNumber uint64, parentHash libcommon.Hash, forkchoiceMessage *engine_types.ForkChoiceState, newPayload bool) (*engine_types.PayloadStatus, error) {
	// Determine which prefix to use for logs
	var prefix string
	if newPayload {
		prefix = "NewPayload"
	} else {
		prefix = "ForkChoiceUpdated"
	}
	if s.config.TerminalTotalDifficulty == nil {
		s.logger.Error(fmt.Sprintf("[%s] not a proof-of-stake chain", prefix))
		return nil, fmt.Errorf("not a proof-of-stake chain")
	}

	if s.hd == nil {
		return nil, fmt.Errorf("headerdownload is nil")
	}

	tx, err := s.db.BeginRo(s.ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	// Some Consensus layer clients sometimes sends us repeated FCUs and make Erigon print a gazillion logs.
	// E.G teku sometimes will end up spamming fcu on the terminal block if it has not synced to that point.
	if forkchoiceMessage != nil &&
		forkchoiceMessage.FinalizedBlockHash == rawdb.ReadForkchoiceFinalized(tx) &&
		forkchoiceMessage.HeadHash == rawdb.ReadForkchoiceHead(tx) &&
		forkchoiceMessage.SafeBlockHash == rawdb.ReadForkchoiceSafe(tx) {
		return &engine_types.PayloadStatus{Status: engine_types.ValidStatus, LatestValidHash: &blockHash}, nil
	}

	header, err := rawdb.ReadHeaderByHash(tx, blockHash)
	if err != nil {
		return nil, err
	}
	// Retrieve parent and total difficulty.
	var parent *types.Header
	var td *big.Int
	if newPayload {
		parent, err = rawdb.ReadHeaderByHash(tx, parentHash)
		if err != nil {
			return nil, err
		}
		td, err = rawdb.ReadTdByHash(tx, parentHash)
	} else {
		td, err = rawdb.ReadTdByHash(tx, blockHash)
	}
	if err != nil {
		return nil, err
	}

	if td != nil && td.Cmp(s.config.TerminalTotalDifficulty) < 0 {
		s.logger.Warn(fmt.Sprintf("[%s] Beacon Chain request before TTD", prefix), "hash", blockHash)
		return &engine_types.PayloadStatus{Status: engine_types.InvalidStatus, LatestValidHash: &libcommon.Hash{}}, nil
	}

	if !s.hd.POSSync() {
		s.logger.Info(fmt.Sprintf("[%s] Still in PoW sync", prefix), "hash", blockHash)
		return &engine_types.PayloadStatus{Status: engine_types.SyncingStatus}, nil
	}

	var canonicalHash libcommon.Hash
	if header != nil {
		canonicalHash, err = s.blockReader.CanonicalHash(context.Background(), tx, header.Number.Uint64())
	}
	if err != nil {
		return nil, err
	}

	if newPayload && parent != nil && blockNumber != parent.Number.Uint64()+1 {
		s.logger.Warn(fmt.Sprintf("[%s] Invalid block number", prefix), "headerNumber", blockNumber, "parentNumber", parent.Number.Uint64())
		s.hd.ReportBadHeaderPoS(blockHash, parent.Hash())
		parentHash := parent.Hash()
		return &engine_types.PayloadStatus{
			Status:          engine_types.InvalidStatus,
			LatestValidHash: &parentHash,
			ValidationError: engine_types.NewStringifiedErrorFromString("invalid block number"),
		}, nil
	}
	// Check if we already determined if the hash is attributed to a previously received invalid header.
	bad, lastValidHash := s.hd.IsBadHeaderPoS(blockHash)
	if bad {
		s.logger.Warn(fmt.Sprintf("[%s] Previously known bad block", prefix), "hash", blockHash)
	} else if newPayload {
		bad, lastValidHash = s.hd.IsBadHeaderPoS(parentHash)
		if bad {
			s.logger.Warn(fmt.Sprintf("[%s] Previously known bad block", prefix), "hash", blockHash, "parentHash", parentHash)
		}
	}
	if bad {
		s.hd.ReportBadHeaderPoS(blockHash, lastValidHash)
		return &engine_types.PayloadStatus{Status: engine_types.InvalidStatus, LatestValidHash: &lastValidHash}, nil
	}

	// If header is already validated or has a missing parent, you can either return VALID or SYNCING.
	if newPayload {
		if header != nil && canonicalHash == blockHash {
			return &engine_types.PayloadStatus{Status: engine_types.ValidStatus, LatestValidHash: &blockHash}, nil
		}

		if parent == nil && s.hd.PosStatus() != headerdownload.Idle {
			s.logger.Debug(fmt.Sprintf("[%s] Downloading some other PoS blocks", prefix), "hash", blockHash)
			return &engine_types.PayloadStatus{Status: engine_types.SyncingStatus}, nil
		}
	} else {
		if header == nil && s.hd.PosStatus() != headerdownload.Idle {
			s.logger.Debug(fmt.Sprintf("[%s] Downloading some other PoS stuff", prefix), "hash", blockHash)
			return &engine_types.PayloadStatus{Status: engine_types.SyncingStatus}, nil
		}
		// Following code ensures we skip the fork choice state update if if forkchoiceState.headBlockHash references an ancestor of the head of canonical chain
		headHash := rawdb.ReadHeadBlockHash(tx)
		if err != nil {
			return nil, err
		}

		// We add the extra restriction blockHash != headHash for the FCU case of canonicalHash == blockHash
		// because otherwise (when FCU points to the head) we want go to stage headers
		// so that it calls writeForkChoiceHashes.
		if blockHash != headHash && canonicalHash == blockHash {
			return &engine_types.PayloadStatus{Status: engine_types.ValidStatus, LatestValidHash: &blockHash}, nil
		}
	}

	// If another payload is already commissioned then we just reply with syncing
	if s.stageLoopIsBusy() {
		s.logger.Debug(fmt.Sprintf("[%s] stage loop is busy", prefix))
		return &engine_types.PayloadStatus{Status: engine_types.SyncingStatus}, nil
	}

	return nil, nil
}

// The expected value to be received by the feeRecipient in wei
func blockValue(br *types.BlockWithReceipts, baseFee *uint256.Int) *uint256.Int {
	blockValue := uint256.NewInt(0)
	txs := br.Block.Transactions()
	for i := range txs {
		gas := new(uint256.Int).SetUint64(br.Receipts[i].GasUsed)
		effectiveTip := txs[i].GetEffectiveGasTip(baseFee)
		txValue := new(uint256.Int).Mul(gas, effectiveTip)
		blockValue.Add(blockValue, txValue)
	}
	return blockValue
}

// EngineGetPayload retrieves previously assembled payload (Validators only)
func (s *EngineServer) getPayload(ctx context.Context, payloadId uint64) (*engine_types.GetPayloadResponse, error) {
	if !s.proposing {
		return nil, fmt.Errorf("execution layer not running as a proposer. enable proposer by taking out the --proposer.disable flag on startup")
	}

	if s.config.TerminalTotalDifficulty == nil {
		return nil, fmt.Errorf("not a proof-of-stake chain")
	}

	s.logger.Debug("[GetPayload] acquiring lock")
	s.lock.Lock()
	defer s.lock.Unlock()
	s.logger.Debug("[GetPayload] lock acquired")

	builder, ok := s.builders[payloadId]
	if !ok {
		log.Warn("Payload not stored", "payloadId", payloadId)
		return nil, &engine_helpers.UnknownPayloadErr
	}

	blockWithReceipts, err := builder.Stop()
	if err != nil {
		s.logger.Error("Failed to build PoS block", "err", err)
		return nil, err
	}
	block := blockWithReceipts.Block
	header := block.Header()

	baseFee := new(uint256.Int)
	baseFee.SetFromBig(header.BaseFee)

	encodedTransactions, err := types.MarshalTransactionsBinary(block.Transactions())
	if err != nil {
		return nil, err
	}
	txs := []hexutility.Bytes{}
	for _, tx := range encodedTransactions {
		txs = append(txs, tx)
	}

	payload := &engine_types.ExecutionPayload{
		ParentHash:    header.ParentHash,
		FeeRecipient:  header.Coinbase,
		Timestamp:     hexutil.Uint64(header.Time),
		PrevRandao:    header.MixDigest,
		StateRoot:     block.Root(),
		ReceiptsRoot:  block.ReceiptHash(),
		LogsBloom:     block.Bloom().Bytes(),
		GasLimit:      hexutil.Uint64(block.GasLimit()),
		GasUsed:       hexutil.Uint64(block.GasUsed()),
		BlockNumber:   hexutil.Uint64(block.NumberU64()),
		ExtraData:     block.Extra(),
		BaseFeePerGas: (*hexutil.Big)(header.BaseFee),
		BlockHash:     block.Hash(),
		Transactions:  txs,
	}
	if block.Withdrawals() != nil {
		payload.Withdrawals = block.Withdrawals()
	}

	if header.DataGasUsed != nil && header.ExcessDataGas != nil {
		payload.DataGasUsed = (*hexutil.Uint64)(header.DataGasUsed)
		payload.ExcessDataGas = (*hexutil.Uint64)(header.ExcessDataGas)
	}

	blockValue := blockValue(blockWithReceipts, baseFee)

	blobsBundle := &engine_types.BlobsBundleV1{}
	for i, tx := range block.Transactions() {
		if tx.Type() != types.BlobTxType {
			continue
		}
		blobTx, ok := tx.(*types.BlobTxWrapper)
		if !ok {
			return nil, fmt.Errorf("expected blob transaction to be type BlobTxWrapper, got: %T", blobTx)
		}
		versionedHashes, commitments, proofs, blobs := blobTx.GetDataHashes(), blobTx.Commitments, blobTx.Proofs, blobTx.Blobs
		lenCheck := len(versionedHashes)
		if lenCheck != len(commitments) || lenCheck != len(proofs) || lenCheck != len(blobs) {
			return nil, fmt.Errorf("tx %d in block %s has inconsistent commitments (%d) / proofs (%d) / blobs (%d) / "+
				"versioned hashes (%d)", i, block.Hash(), len(commitments), len(proofs), len(blobs), lenCheck)
		}
		for _, commitment := range commitments {
			blobsBundle.Commitments = append(blobsBundle.Commitments, commitment)
		}
		for _, proof := range proofs {
			blobsBundle.Proofs = append(blobsBundle.Proofs, proof)
		}
		for _, blob := range blobs {
			blobsBundle.Blobs = append(blobsBundle.Blobs, blob)
		}
	}

	return &engine_types.GetPayloadResponse{
		ExecutionPayload: payload,
		BlockValue:       (*hexutil.Big)(blockValue.ToBig()),
		BlobsBundle:      blobsBundle,
	}, nil
}

// engineForkChoiceUpdated either states new block head or request the assembling of a new block
func (s *EngineServer) forkchoiceUpdated(ctx context.Context, forkchoiceState *engine_types.ForkChoiceState, payloadAttributes *engine_types.PayloadAttributes, version clparams.StateVersion,
) (*engine_types.ForkChoiceUpdatedResponse, error) {
	forkChoice := &engine_types.ForkChoiceState{
		HeadHash:           forkchoiceState.HeadHash,
		SafeBlockHash:      forkchoiceState.SafeBlockHash,
		FinalizedBlockHash: forkchoiceState.FinalizedBlockHash,
	}

	status, err := s.getQuickPayloadStatusIfPossible(forkChoice.HeadHash, 0, libcommon.Hash{}, forkChoice, false)
	if err != nil {
		return nil, err
	}

	s.lock.Lock()
	defer s.lock.Unlock()

	if status == nil {
		s.logger.Debug("[ForkChoiceUpdated] sending forkChoiceMessage", "head", forkChoice.HeadHash)
		s.hd.BeaconRequestList.AddForkChoiceRequest(forkChoice)

		statusDeref := <-s.hd.PayloadStatusCh
		status = &statusDeref
		s.logger.Debug("[ForkChoiceUpdated] got reply", "payloadStatus", status)

		if status.CriticalError != nil {
			return nil, status.CriticalError
		}
	}

	// No need for payload building
	if payloadAttributes == nil || status.Status != engine_types.ValidStatus {
		return &engine_types.ForkChoiceUpdatedResponse{PayloadStatus: status}, nil
	}

	if !s.proposing {
		return nil, fmt.Errorf("execution layer not running as a proposer. enable proposer by taking out the --proposer.disable flag on startup")
	}

	tx2, err := s.db.BeginRo(ctx)
	if err != nil {
		return nil, err
	}
	defer tx2.Rollback()
	headHash := rawdb.ReadHeadBlockHash(tx2)
	headNumber := rawdb.ReadHeaderNumber(tx2, headHash)
	headHeader := rawdb.ReadHeader(tx2, headHash, *headNumber)
	tx2.Rollback()

	if headHeader.Hash() != forkChoice.HeadHash {
		// Per Item 2 of https://github.com/ethereum/execution-apis/blob/v1.0.0-alpha.9/src/engine/specification.md#specification-1:
		// Client software MAY skip an update of the forkchoice state and
		// MUST NOT begin a payload build process if forkchoiceState.headBlockHash doesn't reference a leaf of the block tree.
		// That is, the block referenced by forkchoiceState.headBlockHash is neither the head of the canonical chain nor a block at the tip of any other chain.
		// In the case of such an event, client software MUST return
		// {payloadStatus: {status: VALID, latestValidHash: forkchoiceState.headBlockHash, validationError: null}, payloadId: null}.

		s.logger.Warn("Skipping payload building because forkchoiceState.headBlockHash is not the head of the canonical chain",
			"forkChoice.HeadBlockHash", forkChoice.HeadHash, "headHeader.Hash", headHeader.Hash())
		return &engine_types.ForkChoiceUpdatedResponse{PayloadStatus: status}, nil
	}

	if headHeader.Time >= uint64(payloadAttributes.Timestamp) {
		return nil, &engine_helpers.InvalidPayloadAttributesErr
	}

	param := core.BlockBuilderParameters{
		ParentHash:            forkChoice.HeadHash,
		Timestamp:             uint64(payloadAttributes.Timestamp),
		PrevRandao:            payloadAttributes.PrevRandao,
		SuggestedFeeRecipient: payloadAttributes.SuggestedFeeRecipient,
		PayloadId:             s.payloadId,
	}
	if version >= clparams.CapellaVersion {
		param.Withdrawals = payloadAttributes.Withdrawals
	}
	if err := s.checkWithdrawalsPresence(uint64(payloadAttributes.Timestamp), param.Withdrawals); err != nil {
		return nil, err
	}

	// First check if we're already building a block with the requested parameters
	if reflect.DeepEqual(s.lastParameters, &param) {
		s.logger.Info("[ForkChoiceUpdated] duplicate build request")
		return &engine_types.ForkChoiceUpdatedResponse{
			PayloadStatus: &engine_types.PayloadStatus{
				Status:          engine_types.ValidStatus,
				LatestValidHash: &headHash,
			},
			PayloadId: convertPayloadId(s.payloadId),
		}, nil
	}

	// Initiate payload building
	s.evictOldBuilders()

	s.payloadId++
	param.PayloadId = s.payloadId
	s.lastParameters = &param

	s.builders[s.payloadId] = builder.NewBlockBuilder(s.builderFunc, &param)
	s.logger.Info("[ForkChoiceUpdated] BlockBuilder added", "payload", s.payloadId)

	return &engine_types.ForkChoiceUpdatedResponse{
		PayloadStatus: &engine_types.PayloadStatus{
			Status:          engine_types.ValidStatus,
			LatestValidHash: &headHash,
		},
		PayloadId: convertPayloadId(s.payloadId),
	}, nil
}

func convertPayloadId(payloadId uint64) *hexutility.Bytes {
	encodedPayloadId := make([]byte, 8)
	binary.BigEndian.PutUint64(encodedPayloadId, payloadId)
	ret := hexutility.Bytes(encodedPayloadId)
	return &ret
}

func (s *EngineServer) getPayloadBodiesByHash(ctx context.Context, request []libcommon.Hash, _ clparams.StateVersion) ([]*engine_types.ExecutionPayloadBodyV1, error) {
	tx, err := s.db.BeginRo(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	bodies := make([]*engine_types.ExecutionPayloadBodyV1, len(request))

	for hashIdx, hash := range request {
		block, err := s.blockReader.BlockByHash(ctx, tx, hash)
		if err != nil {
			return nil, err
		}
		body, err := extractPayloadBodyFromBlock(block)
		if err != nil {
			return nil, err
		}
		bodies[hashIdx] = body
	}

	return bodies, nil
}

func (s *EngineServer) getPayloadBodiesByRange(ctx context.Context, start, count uint64, _ clparams.StateVersion) ([]*engine_types.ExecutionPayloadBodyV1, error) {
	tx, err := s.db.BeginRo(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	bodies := make([]*engine_types.ExecutionPayloadBodyV1, 0, count)

	for i := uint64(0); i < count; i++ {
		hash, err := rawdb.ReadCanonicalHash(tx, start+i)
		if err != nil {
			return nil, err
		}
		if hash == (libcommon.Hash{}) {
			// break early if beyond the last known canonical header
			break
		}

		block, _, err := s.blockReader.BlockWithSenders(ctx, tx, hash, start+i)
		if err != nil {
			return nil, err
		}
		body, err := extractPayloadBodyFromBlock(block)
		if err != nil {
			return nil, err
		}
		bodies = append(bodies, body)
	}

	return bodies, nil
}

func extractPayloadBodyFromBlock(block *types.Block) (*engine_types.ExecutionPayloadBodyV1, error) {
	if block == nil {
		return nil, nil
	}

	txs := block.Transactions()
	bdTxs := make([]hexutility.Bytes, len(txs))
	for idx, tx := range txs {
		var buf bytes.Buffer
		if err := tx.MarshalBinary(&buf); err != nil {
			return nil, err
		} else {
			bdTxs[idx] = buf.Bytes()
		}
	}

	return &engine_types.ExecutionPayloadBodyV1{Transactions: bdTxs, Withdrawals: block.Withdrawals()}, nil
}

func (s *EngineServer) evictOldBuilders() {
	ids := common.SortedKeys(s.builders)

	// remove old builders so that at most MaxBuilders - 1 remain
	for i := 0; i <= len(s.builders)-engine_helpers.MaxBuilders; i++ {
		delete(s.builders, ids[i])
	}
}

func ConvertWithdrawalsFromRpc(in []*types2.Withdrawal) []*types.Withdrawal {
	if in == nil {
		return nil
	}
	out := make([]*types.Withdrawal, 0, len(in))
	for _, w := range in {
		out = append(out, &types.Withdrawal{
			Index:     w.Index,
			Validator: w.ValidatorIndex,
			Address:   gointerfaces.ConvertH160toAddress(w.Address),
			Amount:    w.Amount,
		})
	}
	return out
}

func ConvertWithdrawalsToRpc(in []*types.Withdrawal) []*types2.Withdrawal {
	if in == nil {
		return nil
	}
	out := make([]*types2.Withdrawal, 0, len(in))
	for _, w := range in {
		out = append(out, &types2.Withdrawal{
			Index:          w.Index,
			ValidatorIndex: w.Validator,
			Address:        gointerfaces.ConvertAddressToH160(w.Address),
			Amount:         w.Amount,
		})
	}
	return out
}

func (e *EngineServer) GetPayloadV1(ctx context.Context, payloadId hexutility.Bytes) (*engine_types.ExecutionPayload, error) {

	decodedPayloadId := binary.BigEndian.Uint64(payloadId)
	log.Info("Received GetPayloadV1", "payloadId", decodedPayloadId)

	response, err := e.getPayload(ctx, decodedPayloadId)
	if err != nil {
		return nil, err
	}

	return response.ExecutionPayload, nil
}

func (e *EngineServer) GetPayloadV2(ctx context.Context, payloadID hexutility.Bytes) (*engine_types.GetPayloadResponse, error) {
	decodedPayloadId := binary.BigEndian.Uint64(payloadID)
	log.Info("Received GetPayloadV2", "payloadId", decodedPayloadId)

	return e.getPayload(ctx, decodedPayloadId)
}

func (e *EngineServer) GetPayloadV3(ctx context.Context, payloadID hexutility.Bytes) (*engine_types.GetPayloadResponse, error) {
	decodedPayloadId := binary.BigEndian.Uint64(payloadID)
	log.Info("Received GetPayloadV3", "payloadId", decodedPayloadId)

	return e.getPayload(ctx, decodedPayloadId)
}

func (e *EngineServer) ForkchoiceUpdatedV1(ctx context.Context, forkChoiceState *engine_types.ForkChoiceState, payloadAttributes *engine_types.PayloadAttributes) (*engine_types.ForkChoiceUpdatedResponse, error) {
	return e.forkchoiceUpdated(ctx, forkChoiceState, payloadAttributes, clparams.BellatrixVersion)
}

func (e *EngineServer) ForkchoiceUpdatedV2(ctx context.Context, forkChoiceState *engine_types.ForkChoiceState, payloadAttributes *engine_types.PayloadAttributes) (*engine_types.ForkChoiceUpdatedResponse, error) {
	return e.forkchoiceUpdated(ctx, forkChoiceState, payloadAttributes, clparams.CapellaVersion)
}

// NewPayloadV1 processes new payloads (blocks) from the beacon chain without withdrawals.
// See https://github.com/ethereum/execution-apis/blob/main/src/engine/paris.md#engine_newpayloadv1
func (e *EngineServer) NewPayloadV1(ctx context.Context, payload *engine_types.ExecutionPayload) (*engine_types.PayloadStatus, error) {
	return e.newPayload(ctx, payload, clparams.BellatrixVersion)
}

// NewPayloadV2 processes new payloads (blocks) from the beacon chain with withdrawals.
// See https://github.com/ethereum/execution-apis/blob/main/src/engine/shanghai.md#engine_newpayloadv2
func (e *EngineServer) NewPayloadV2(ctx context.Context, payload *engine_types.ExecutionPayload) (*engine_types.PayloadStatus, error) {
	return e.newPayload(ctx, payload, clparams.CapellaVersion)
}

// NewPayloadV3 processes new payloads (blocks) from the beacon chain with withdrawals & excess data gas.
// See https://github.com/ethereum/execution-apis/blob/main/src/engine/specification.md#engine_newpayloadv3
func (e *EngineServer) NewPayloadV3(ctx context.Context, payload *engine_types.ExecutionPayload) (*engine_types.PayloadStatus, error) {
	return e.newPayload(ctx, payload, clparams.DenebVersion)
}

// Receives consensus layer's transition configuration and checks if the execution layer has the correct configuration.
// Can also be used to ping the execution layer (heartbeats).
// See https://github.com/ethereum/execution-apis/blob/v1.0.0-beta.1/src/engine/specification.md#engine_exchangetransitionconfigurationv1
func (e *EngineServer) ExchangeTransitionConfigurationV1(ctx context.Context, beaconConfig *engine_types.TransitionConfiguration) (*engine_types.TransitionConfiguration, error) {
	tx, err := e.db.BeginRo(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	terminalTotalDifficulty := e.config.TerminalTotalDifficulty

	if terminalTotalDifficulty == nil {
		return nil, fmt.Errorf("the execution layer doesn't have a terminal total difficulty. expected: %v", beaconConfig.TerminalTotalDifficulty)
	}

	if terminalTotalDifficulty.Cmp((*big.Int)(beaconConfig.TerminalTotalDifficulty)) != 0 {
		return nil, fmt.Errorf("the execution layer has a wrong terminal total difficulty. expected %v, but instead got: %d", beaconConfig.TerminalTotalDifficulty, terminalTotalDifficulty)
	}

	return &engine_types.TransitionConfiguration{
		TerminalTotalDifficulty: (*hexutil.Big)(terminalTotalDifficulty),
		TerminalBlockHash:       libcommon.Hash{},
		TerminalBlockNumber:     (*hexutil.Big)(libcommon.Big0),
	}, nil
}

func (e *EngineServer) GetPayloadBodiesByHashV1(ctx context.Context, hashes []libcommon.Hash) ([]*engine_types.ExecutionPayloadBodyV1, error) {
	if len(hashes) > 1024 {
		return nil, &engine_helpers.TooLargeRequestErr
	}

	return e.getPayloadBodiesByHash(ctx, hashes, clparams.DenebVersion)
}

func (e *EngineServer) GetPayloadBodiesByRangeV1(ctx context.Context, start, count hexutil.Uint64) ([]*engine_types.ExecutionPayloadBodyV1, error) {
	if start == 0 || count == 0 {
		return nil, &rpc.InvalidParamsError{Message: fmt.Sprintf("invalid start or count, start: %v count: %v", start, count)}
	}
	if count > 1024 {
		return nil, &engine_helpers.TooLargeRequestErr
	}

	return e.getPayloadBodiesByRange(ctx, uint64(start), uint64(count), clparams.CapellaVersion)
}

var ourCapabilities = []string{
	"engine_forkchoiceUpdatedV1",
	"engine_forkchoiceUpdatedV2",
	"engine_newPayloadV1",
	"engine_newPayloadV2",
	// "engine_newPayloadV3",
	"engine_getPayloadV1",
	"engine_getPayloadV2",
	// "engine_getPayloadV3",
	"engine_exchangeTransitionConfigurationV1",
	"engine_getPayloadBodiesByHashV1",
	"engine_getPayloadBodiesByRangeV1",
}

func (e *EngineServer) ExchangeCapabilities(fromCl []string) []string {
	missingOurs := compareCapabilities(fromCl, ourCapabilities)
	missingCl := compareCapabilities(ourCapabilities, fromCl)

	if len(missingCl) > 0 || len(missingOurs) > 0 {
		log.Debug("ExchangeCapabilities mismatches", "cl_unsupported", missingCl, "erigon_unsupported", missingOurs)
	}

	return ourCapabilities
}

func compareCapabilities(from []string, to []string) []string {
	result := make([]string, 0)
	for _, f := range from {
		found := false
		for _, t := range to {
			if f == t {
				found = true
				break
			}
		}
		if !found {
			result = append(result, f)
		}
	}

	return result
}
