package eth1

import (
	"context"
	"errors"
	"fmt"

	libcommon "github.com/ledgerwatch/erigon-lib/common"
	"github.com/ledgerwatch/erigon-lib/gointerfaces"
	"github.com/ledgerwatch/erigon-lib/kv"

	"github.com/ledgerwatch/erigon-lib/gointerfaces/execution"
	types2 "github.com/ledgerwatch/erigon-lib/gointerfaces/types"

	"github.com/ledgerwatch/erigon/core/rawdb"
)

func (e *EthereumExecutionModule) parseSegmentRequest(ctx context.Context, tx kv.Tx, req *execution.GetSegmentRequest) (blockHash libcommon.Hash, blockNumber uint64, err error) {
	switch {
	// Case 1: Only hash is given.
	case req.BlockHash != nil && req.BlockNumber == nil:
		blockHash = gointerfaces.ConvertH256ToHash(req.BlockHash)
		blockNumberPtr := rawdb.ReadHeaderNumber(tx, blockHash)
		if blockNumberPtr == nil {
			err = fmt.Errorf("ethereumExecutionModule.parseSegmentRequest: could not read block: non existent index")
			return
		}
		blockNumber = *blockNumberPtr
	case req.BlockHash == nil && req.BlockNumber != nil:
		blockNumber = *req.BlockNumber
		blockHash, err = e.canonicalHash(ctx, tx, blockNumber)
		if err != nil {
			err = fmt.Errorf("ethereumExecutionModule.parseSegmentRequest: could not read block %d: %s", blockNumber, err)
			return
		}
	case req.BlockHash != nil && req.BlockNumber != nil:
		blockHash = gointerfaces.ConvertH256ToHash(req.BlockHash)
		blockNumber = *req.BlockNumber
	}
	return
}

func (e *EthereumExecutionModule) GetBody(ctx context.Context, req *execution.GetSegmentRequest) (*execution.GetBodyResponse, error) {
	// Invalid case: request is invalid.
	if req == nil || (req.BlockHash == nil && req.BlockNumber == nil) {
		return nil, errors.New("ethereumExecutionModule.GetBody: bad request")
	}
	tx, err := e.db.BeginRo(ctx)
	if err != nil {
		return nil, fmt.Errorf("ethereumExecutionModule.GetHeader: could not open database: %s", err)
	}
	defer tx.Rollback()

	blockHash, blockNumber, err := e.parseSegmentRequest(ctx, tx, req)
	if err != nil {
		return nil, fmt.Errorf("ethereumExecutionModule.GetBody: %s", err)
	}
	body, err := e.getBody(ctx, tx, blockHash, blockNumber)
	if err != nil {
		return nil, fmt.Errorf("ethereumExecutionModule.GetBody: coild not read body: %s", err)
	}
	if body == nil {
		return &execution.GetBodyResponse{Body: nil}, nil
	}
	rawBody := body.RawBody()

	return &execution.GetBodyResponse{Body: ConvertRawBlockBodyToRpc(rawBody, blockNumber, blockHash)}, nil
}

func (e *EthereumExecutionModule) GetHeader(ctx context.Context, req *execution.GetSegmentRequest) (*execution.GetHeaderResponse, error) {
	// Invalid case: request is invalid.
	if req == nil || (req.BlockHash == nil && req.BlockNumber == nil) {
		return nil, errors.New("ethereumExecutionModule.GetHeader: bad request")
	}
	tx, err := e.db.BeginRo(ctx)
	if err != nil {
		return nil, fmt.Errorf("ethereumExecutionModule.GetHeader: could not open database: %s", err)
	}
	defer tx.Rollback()

	blockHash, blockNumber, err := e.parseSegmentRequest(ctx, tx, req)
	if err != nil {
		return nil, fmt.Errorf("ethereumExecutionModule.GetHeader: %s", err)
	}
	header, err := e.getHeader(ctx, tx, blockHash, blockNumber)
	if err != nil {
		return nil, fmt.Errorf("ethereumExecutionModule.GetHeader: coild not read body: %s", err)
	}
	if header == nil {
		return &execution.GetHeaderResponse{Header: nil}, nil
	}

	return &execution.GetHeaderResponse{Header: HeaderToHeaderRPC(header)}, nil
}

func (e *EthereumExecutionModule) GetHeaderHashNumber(ctx context.Context, req *types2.H256) (*execution.GetHeaderHashNumberResponse, error) {
	tx, err := e.db.BeginRo(ctx)
	if err != nil {
		return nil, fmt.Errorf("ethereumExecutionModule.GetBody: could not open database: %s", err)
	}
	defer tx.Rollback()
	blockNumber := rawdb.ReadHeaderNumber(tx, gointerfaces.ConvertH256ToHash(req))
	if blockNumber == nil {
		return nil, fmt.Errorf("ethereumExecutionModule.parseSegmentRequest: could not read block: non existent index")
	}
	return &execution.GetHeaderHashNumberResponse{BlockNumber: blockNumber}, nil
}

func (e *EthereumExecutionModule) CanonicalHash(ctx context.Context, req *types2.H256) (*execution.IsCanonicalResponse, error) {
	tx, err := e.db.BeginRo(ctx)
	if err != nil {
		return nil, fmt.Errorf("ethereumExecutionModule.CanonicalHash: could not open database: %s", err)
	}
	defer tx.Rollback()
	blockHash := gointerfaces.ConvertH256ToHash(req)
	blockNumber := rawdb.ReadHeaderNumber(tx, blockHash)
	if blockNumber == nil {
		return nil, fmt.Errorf("ethereumExecutionModule.CanonicalHash: could not read block: non existent index")
	}
	expectedHash, err := e.canonicalHash(ctx, tx, *blockNumber)
	if err != nil {
		return nil, fmt.Errorf("ethereumExecutionModule.CanonicalHash: could not read canonical hash")
	}
	return &execution.IsCanonicalResponse{Canonical: expectedHash == blockHash}, nil
}
