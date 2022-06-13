// Copyright (C) 2019-2021, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package stateful

import (
	"fmt"

	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/snow/choices"
	"github.com/ava-labs/avalanchego/snow/consensus/snowman"
	"github.com/ava-labs/avalanchego/vms/platformvm/blocks/stateless"
	"github.com/ava-labs/avalanchego/vms/platformvm/state"
	"github.com/ava-labs/avalanchego/vms/platformvm/status"
	"github.com/ava-labs/avalanchego/vms/platformvm/transactions/signed"
)

var _ Block = &ProposalBlock{}

// ProposalBlock is a proposal to change the chain's state.
//
// A proposal may be to:
// 	1. Advance the chain's timestamp (*AdvanceTimeTx)
//  2. Remove a staker from the staker set (*RewardStakerTx)
//  3. Add a new staker to the set of pending (future) stakers
//     (*AddValidatorTx, *AddDelegatorTx, *AddSubnetValidatorTx)
//
// The proposal will be enacted (change the chain's state) if the proposal block
// is accepted and followed by an accepted Commit block
type ProposalBlock struct {
	*stateless.ProposalBlock
	*commonBlock

	// The state that the chain will have if this block's proposal is committed
	onCommitState state.Versioned
	// The state that the chain will have if this block's proposal is aborted
	onAbortState state.Versioned
}

// NewProposalBlock creates a new block that proposes to issue a transaction.
// The parent of this block has ID [parentID].
// The parent must be a decision block.
func NewProposalBlock(
	verifier Verifier,
	parentID ids.ID,
	height uint64,
	tx signed.Tx,
) (*ProposalBlock, error) {
	statelessBlk, err := stateless.NewProposalBlock(parentID, height, tx)
	if err != nil {
		return nil, err
	}

	return toStatefulProposalBlock(statelessBlk, verifier, choices.Processing)
}

func toStatefulProposalBlock(
	statelessBlk *stateless.ProposalBlock,
	verifier Verifier,
	status choices.Status,
) (*ProposalBlock, error) {
	pb := &ProposalBlock{
		ProposalBlock: statelessBlk,
		commonBlock: &commonBlock{
			baseBlk:  &statelessBlk.CommonBlock,
			status:   status,
			verifier: verifier,
		},
	}

	pb.Tx.Unsigned.InitCtx(pb.verifier.Ctx())
	return pb, nil
}

func (pb *ProposalBlock) free() {
	pb.commonBlock.free()
	pb.onCommitState = nil
	pb.onAbortState = nil
}

func (pb *ProposalBlock) Accept() error {
	blkID := pb.ID()
	pb.verifier.Ctx().Log.Verbo(
		"Accepting Proposal Block %s at height %d with parent %s",
		blkID,
		pb.Height(),
		pb.Parent(),
	)

	pb.status = choices.Accepted
	pb.verifier.SetLastAccepted(blkID)
	return nil
}

func (pb *ProposalBlock) Reject() error {
	pb.verifier.Ctx().Log.Verbo(
		"Rejecting Proposal Block %s at height %d with parent %s",
		pb.ID(),
		pb.Height(),
		pb.Parent(),
	)

	pb.onCommitState = nil
	pb.onAbortState = nil

	if err := pb.verifier.Add(&pb.Tx); err != nil {
		pb.verifier.Ctx().Log.Verbo(
			"failed to reissue tx %q due to: %s",
			pb.Tx.ID(),
			err,
		)
	}

	defer pb.reject()
	pb.verifier.AddStatelessBlock(pb.ProposalBlock, pb.Status())
	return pb.verifier.Commit()
}

func (pb *ProposalBlock) setBaseState() {
	pb.onCommitState.SetBase(pb.verifier.GetMutableState())
	pb.onAbortState.SetBase(pb.verifier.GetMutableState())
}

// Verify this block is valid.
//
// The parent block must either be a Commit or an Abort block.
//
// If this block is valid, this function also sets pas.onCommit and pas.onAbort.
func (pb *ProposalBlock) Verify() error {
	if err := pb.verify(); err != nil {
		return err
	}

	parentIntf, err := pb.parentBlock()
	if err != nil {
		return err
	}

	// The parent of a proposal block (ie this block) must be a decision block
	parent, ok := parentIntf.(Decision)
	if !ok {
		return fmt.Errorf("expected Decision block but got %T", parentIntf)
	}

	// parentState is the state if this block's parent is accepted
	parentState := parent.OnAccept()
	pb.onCommitState, pb.onAbortState, err = pb.verifier.ExecuteProposal(&pb.Tx, parentState)
	if err != nil {
		txID := pb.Tx.ID()
		pb.verifier.MarkDropped(txID, err.Error()) // cache tx as dropped
		return err
	}
	pb.onCommitState.AddTx(&pb.Tx, status.Committed)
	pb.onAbortState.AddTx(&pb.Tx, status.Aborted)

	pb.timestamp = parentState.GetTimestamp()

	pb.verifier.RemoveProposalTx(&pb.Tx)
	pb.verifier.CacheVerifiedBlock(pb)
	parentIntf.addChild(pb)
	return nil
}

// Options returns the possible children of this block in preferential order.
func (pb *ProposalBlock) Options() ([2]snowman.Block, error) {
	blkID := pb.ID()
	nextHeight := pb.Height() + 1
	prefersCommit, err := pb.verifier.InitiallyPrefersCommit(pb.Tx.Unsigned)
	if err != nil {
		return [2]snowman.Block{}, fmt.Errorf(
			"failed to create options, err %w",
			err,
		)
	}

	commit, err := NewCommitBlock(pb.verifier, blkID, nextHeight, prefersCommit)
	if err != nil {
		return [2]snowman.Block{}, fmt.Errorf(
			"failed to create commit block: %w",
			err,
		)
	}
	abort, err := NewAbortBlock(pb.verifier, blkID, nextHeight, !prefersCommit)
	if err != nil {
		return [2]snowman.Block{}, fmt.Errorf(
			"failed to create abort block: %w",
			err,
		)
	}

	if prefersCommit {
		return [2]snowman.Block{commit, abort}, nil
	}
	return [2]snowman.Block{abort, commit}, nil
}