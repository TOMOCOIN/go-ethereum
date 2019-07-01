// Copyright 2015 The go-ethereum Authors
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
	"encoding/json"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/posv"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/tomox"
	sdktypes "github.com/tomochain/tomox-sdk/types"
)

// BlockValidator is responsible for validating block headers, uncles and
// processed state.
//
// BlockValidator implements Validator.
type BlockValidator struct {
	config *params.ChainConfig // Chain configuration options
	bc     *BlockChain         // Canonical block chain
	engine consensus.Engine    // Consensus engine used for validating
}

// NewBlockValidator returns a new block validator which is safe for re-use
func NewBlockValidator(config *params.ChainConfig, blockchain *BlockChain, engine consensus.Engine) *BlockValidator {
	validator := &BlockValidator{
		config: config,
		engine: engine,
		bc:     blockchain,
	}
	return validator
}

// ValidateBody validates the given block's uncles and verifies the the block
// header's transaction and uncle roots. The headers are assumed to be already
// validated at this point.
func (v *BlockValidator) ValidateBody(block *types.Block) error {
	// Check whether the block's known, and if not, that it's linkable
	if v.bc.HasBlockAndState(block.Hash(), block.NumberU64()) {
		return ErrKnownBlock
	}
	if !v.bc.HasBlockAndState(block.ParentHash(), block.NumberU64()-1) {
		if !v.bc.HasBlock(block.ParentHash(), block.NumberU64()-1) {
			return consensus.ErrUnknownAncestor
		}
		return consensus.ErrPrunedAncestor
	}
	// Header validity is known at this point, check the uncles and transactions
	header := block.Header()
	if err := v.engine.VerifyUncles(v.bc, block); err != nil {
		return err
	}
	if hash := types.CalcUncleHash(block.Uncles()); hash != header.UncleHash {
		return fmt.Errorf("uncle root hash mismatch: have %x, want %x", hash, header.UncleHash)
	}
	if hash := types.DeriveSha(block.Transactions()); hash != header.TxHash {
		return fmt.Errorf("transaction root hash mismatch: have %x, want %x", hash, header.TxHash)
	}

	engine, _ := v.engine.(*posv.Posv)
	tomoXService := engine.GetTomoXService()
	if tomoXService == nil {
		log.Error("tomox not found")
		return tomox.ErrTomoXServiceNotFound
	}

	currentState, err := v.bc.State()
	if err != nil {
		return err
	}
	processedData := []map[string][]byte{}

	// validate matchedOrder txs
	for _, tx := range block.Transactions() {
		if tx.IsMatchingTransaction() {
			log.Debug("process tx match")
			order := &tomox.OrderItem{}
			ol := &tomox.OrderList{}

			order, ol, err = v.validateMatchedOrder(tomoXService, currentState, tx)
			if order != nil {
				var (
					encodedOrderItem []byte
					encodedOrderList []byte
				)
				encodedOrderItem, err = tomox.EncodeBytesItem(order)
				if err != nil {
					break
				}
				if ol != nil {
					encodedOrderList, err = tomox.EncodeBytesItem(ol)
					if err != nil {
						break
					}
				}

				processedData = append(processedData, map[string][]byte{
					"orderItem": encodedOrderItem,
					"orderList": encodedOrderList,
				})
			}
			if err != nil {
				break
			}
		}
	}
	if err != nil {
		// rollback
		if err := tomoXService.Rollback(processedData); err != nil {
			return fmt.Errorf("validateMatchedOrder failed. Failed to rollback. %s", err.Error())
		}
		return err
	}
	return nil
}

// ValidateState validates the various changes that happen after a state
// transition, such as amount of used gas, the receipt roots and the state root
// itself. ValidateState returns a database batch if the validation was a success
// otherwise nil and an error is returned.
func (v *BlockValidator) ValidateState(block, parent *types.Block, statedb *state.StateDB, receipts types.Receipts, usedGas uint64) error {
	header := block.Header()
	if block.GasUsed() != usedGas {
		return fmt.Errorf("invalid gas used (remote: %d local: %d)", block.GasUsed(), usedGas)
	}
	// Validate the received block's bloom with the one derived from the generated receipts.
	// For valid blocks this should always validate to true.
	rbloom := types.CreateBloom(receipts)
	if rbloom != header.Bloom {
		return fmt.Errorf("invalid bloom (remote: %x  local: %x)", header.Bloom, rbloom)
	}
	// Tre receipt Trie's root (R = (Tr [[H1, R1], ... [Hn, R1]]))
	receiptSha := types.DeriveSha(receipts)
	if receiptSha != header.ReceiptHash {
		return fmt.Errorf("invalid receipt root hash (remote: %x local: %x)", header.ReceiptHash, receiptSha)
	}
	// Validate the state root against the received state root and throw
	// an error if they don't match.
	if root := statedb.IntermediateRoot(v.config.IsEIP158(header.Number)); header.Root != root {
		return fmt.Errorf("invalid merkle root (remote: %x local: %x)", header.Root, root)
	}
	return nil
}

// an order (type *tomox.OrderItem) is returned to let us know which orders has been processed
// it's important information for rolling back in case of failure
func (v *BlockValidator) validateMatchedOrder(tomoXService *tomox.TomoX, currentState *state.StateDB, tx *types.Transaction) (*tomox.OrderItem, *tomox.OrderList, error) {
	txMatch := &tomox.TxDataMatch{}
	if err := json.Unmarshal(tx.Data(), txMatch); err != nil {
		return nil, nil, fmt.Errorf("transaction match is corrupted. Failed unmarshal. Error: %s", err.Error())
	}

	// verify orderItem
	order, err := txMatch.DecodeOrder()
	if err != nil {
		return nil, nil, fmt.Errorf("transaction match is corrupted. Failed decode order. Error: %s ", err.Error())
	}
	if err := order.VerifyMatchedOrder(currentState); err != nil {
		return nil, nil, err
	}

	// verify old state: orderbook hash, bidTree hash, askTree hash
	ob, err := tomoXService.GetOrderBook(order.PairName)
	if err != nil {
		return nil, nil, err
	}
	if err := txMatch.VerifyOldTomoXState(ob); err != nil {
		return nil, nil, err
	}

	var ol *tomox.OrderList
	if len(txMatch.GetTrades()) > 0 {
		if order.Side == tomox.Bid {
			ol = ob.Asks.MinPriceList()
		} else {
			ol = ob.Bids.MaxPriceList()
		}
	}

	// process Matching Engine
	if _, _, err := ob.ProcessOrder(order, true); err != nil {
		return nil, nil, err
	}

	// update pending hash, processedHash
	if err := tomoXService.MarkOrderAsProcessed(order.Hash); err != nil {
		return order, ol, err
	}
	// verify new state
	if err := txMatch.VerifyNewTomoXState(ob); err != nil {
		return order, ol, err
	}

	trades := txMatch.GetTrades()
	if err := logTrades(tomoXService, tx.Hash(), order, trades); err != nil {
		return order, ol, err
	}

	return order, ol, nil
}

// CalcGasLimit computes the gas limit of the next block after parent.
// This is miner strategy, not consensus protocol.
func CalcGasLimit(parent *types.Block) uint64 {
	// contrib = (parentGasUsed * 3 / 2) / 1024
	contrib := (parent.GasUsed() + parent.GasUsed()/2) / params.GasLimitBoundDivisor

	// decay = parentGasLimit / 1024 -1
	decay := parent.GasLimit()/params.GasLimitBoundDivisor - 1

	/*
		strategy: gasLimit of block-to-mine is set based on parent's
		gasUsed value.  if parentGasUsed > parentGasLimit * (2/3) then we
		increase it, otherwise lower it (or leave it unchanged if it's right
		at that usage) the amount increased/decreased depends on how far away
		from parentGasLimit * (2/3) parentGasUsed is.
	*/
	limit := parent.GasLimit() - decay + contrib
	if limit < params.MinGasLimit {
		limit = params.MinGasLimit
	}
	// however, if we're now below the target (TargetGasLimit) we increase the
	// limit as much as we can (parentGasLimit / 1024 -1)
	if limit < params.TargetGasLimit {
		limit = parent.GasLimit() + decay
		if limit > params.TargetGasLimit {
			limit = params.TargetGasLimit
		}
	}
	return limit
}

func logTrades(tomoXService *tomox.TomoX, txHash common.Hash, order *tomox.OrderItem, trades []map[string]string) error {
	log.Debug("Got trades", "number", len(trades), "trades", trades)
	for _, trade := range trades {
		tradeSDK := &sdktypes.Trade{}
		if q, ok := trade["quantity"]; ok {
			tradeSDK.Amount = new(big.Int)
			tradeSDK.Amount.SetString(q, 10)
		}
		tradeSDK.PricePoint = order.Price
		tradeSDK.PairName = order.PairName
		tradeSDK.BaseToken = order.BaseToken
		tradeSDK.QuoteToken = order.QuoteToken
		tradeSDK.Status = sdktypes.TradeStatusSuccess
		tradeSDK.Maker = order.UserAddress
		tradeSDK.MakerOrderHash = order.Hash
		if u, ok := trade["uAddr"]; ok {
			tradeSDK.Taker = common.Address{}
			tradeSDK.Taker.SetString(u)
		}
		tradeSDK.TakerOrderHash = order.Hash //FIXME: will update txMatch to include TakerOrderHash = headOrder.Item.Hash
		tradeSDK.TxHash = txHash
		tradeSDK.Hash = tradeSDK.ComputeHash()
		log.Debug("TRADE history", "order", order, "trade", tradeSDK)
		// put tradeSDK to mongodb on SDK node
		if tomoXService.IsSDKNode() {
			db := tomoXService.GetDB()
			if err := db.Put(tomox.EmptyKey(), tradeSDK); err != nil {
				return fmt.Errorf("failed to store tradeSDK %s", err.Error())
			}
		}
	}
	return nil
}
