// Copyright 2019 dfuse Platform Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package forkable

import (
	"fmt"

	"github.com/dfuse-io/bstream"
	"go.uber.org/zap"
)

type Forkable struct {
	logger        *zap.Logger
	handler       bstream.Handler
	forkDB        *ForkDB
	lastBlockSent *bstream.Block
	filterSteps   StepType

	ensureBlockFlows  bstream.BlockRef
	ensureBlockFlowed bool

	ensureAllBlocksTriggerLongestChain bool

	includeInitialLIB bool

	lastLongestChain []*Block
}

type ForkableObject struct {
	Step StepType

	HandoffCount int

	// The three following fields are filled when handling multi-block steps, like when passing Irreversibile segments, the whole segment is represented in here.
	StepCount  int                          // Total number of steps in multi-block steps.
	StepIndex  int                          // Index for the current block
	StepBlocks []*bstream.PreprocessedBlock // You can decide to process them when StepCount == StepIndex +1 or when StepIndex == 0 only.

	ForkDB *ForkDB // ForkDB is a reference to the `Forkable`'s ForkDB instance. Provided you don't use it in goroutines, it is safe for use in `ProcessBlock` calls.

	// Object that was returned by PreprocessBlock(). Could be nil
	Obj interface{}
}

type ForkableBlock struct {
	Block     *bstream.Block
	Obj       interface{}
	SentAsNew bool
}

func New(h bstream.Handler, opts ...Option) *Forkable {
	f := &Forkable{
		filterSteps:      StepsAll,
		handler:          h,
		forkDB:           NewForkDB(),
		ensureBlockFlows: bstream.BlockRefEmpty,
		logger:           zlog,
	}

	for _, opt := range opts {
		opt(f)
	}

	// Done afterwards so forkdb can get configured forkable logger from options
	f.forkDB.logger = f.logger

	return f
}

// NewWithLIB DEPRECATED, use `New(h, WithExclusiveLIB(libID))`.  Also use `EnsureBlockFlows`, `WithFilters`, `EnsureAllBlocksTriggerLongestChain` options
func NewWithLIB(libID bstream.BlockRef, h bstream.Handler) { return }

func (p *Forkable) targetChainBlock(blk *bstream.Block) bstream.BlockRef {
	if p.ensureBlockFlows.ID() != "" && !p.ensureBlockFlowed {
		return p.ensureBlockFlows
	}

	return blk
}

func (p *Forkable) matchFilter(filter StepType) bool {
	return p.filterSteps&filter != 0
}

func (p *Forkable) computeNewLongestChain(ppBlk *ForkableBlock) []*Block {
	longestChain := p.lastLongestChain
	blk := ppBlk.Block

	canSkipRecompute := false
	if len(longestChain) != 0 &&
		blk.PreviousID() == longestChain[len(longestChain)-1].BlockID && // optimize if adding block linearly
		p.forkDB.LIBNum()+1 == longestChain[0].BlockNum { // do not optimize if the lib moved (should truncate up to lib)
		canSkipRecompute = true
	}

	if canSkipRecompute {
		longestChain = append(longestChain, &Block{
			BlockID:  blk.ID(), // NOTE: we don't want "Previous" because ReversibleSegment does not give them
			BlockNum: blk.Num(),
			Object:   ppBlk,
		})
	} else {
		longestChain = p.forkDB.ReversibleSegment(p.targetChainBlock(blk))
	}
	p.lastLongestChain = longestChain
	return longestChain

}

func (p *Forkable) ProcessBlock(blk *bstream.Block, obj interface{}) error {
	if blk.Num() < p.forkDB.LIBNum() && p.lastBlockSent != nil {
		return nil
	}

	zlogBlk := p.logger.With(zap.Stringer("block", blk))

	// TODO: consider an `initialHeadBlockID`, triggerNewLongestChain also when the initialHeadBlockID's BlockNum == blk.Num()
	triggersNewLongestChain := p.triggersNewLongestChain(blk)
	zlogBlk.Debug("processing block", zap.Bool("new_longest_chain", triggersNewLongestChain))

	ppBlk := &ForkableBlock{Block: blk, Obj: obj}

	if p.includeInitialLIB && p.lastBlockSent == nil && blk.ID() == p.forkDB.LIBID() {
		return p.processInitialInclusiveIrreversibleBlock(blk, obj)
	}
	// special case to send the LIB if we receive it on an initlib'ed empty forkdb. Easier in some contexts.
	// ex: I have block 00000004a in my hands, and I know it is irreversible.
	//     I initLIB with 00000003a, then I send the block 00000003a in, I want it pushed :D
	// if p.lastBlockSent == nil && blk.ID() == p.forkDB.LIBID() {
	// 	zlogBlk.Debug("sending block through, it is our lib", zap.String("blk_id", blk.ID()), zap.Uint64("blk_num", blk.Num()))
	// 	return p.handler.ProcessBlock(blk, &ForkableObject{
	// 		Step:   StepNew,
	// 		ForkDB: p.forkDB,
	// 		Obj:    obj,
	// 	})
	// }

	var undos, redos []*ForkableBlock
	if p.matchFilter(StepUndo | StepRedo) {
		if triggersNewLongestChain && p.lastBlockSent != nil {
			undos, redos = p.sentChainSwitchSegments(zlogBlk, p.lastBlockSent.ID(), blk.PreviousID())
		}
	}

	previousRef := bstream.NewBlockRef(blk.PreviousID(), blk.Num()-1)
	if exists := p.forkDB.AddLink(blk, previousRef, ppBlk); exists {
		return nil
	}

	if !p.forkDB.HasLIB() { // always skip processing until LIB is set
		p.forkDB.TrySetLIB(blk, previousRef, blk.LIBNum())
	}

	if !p.forkDB.HasLIB() {
		return nil
	}

	// All this code isn't reachable unless a LIB is set in the ForkDB

	longestChain := p.computeNewLongestChain(ppBlk)
	if !triggersNewLongestChain || len(longestChain) == 0 {
		return nil
	}

	zlogBlk.Debug("got longest chain", zap.Int("chain_length", len(longestChain)), zap.Int("undos_length", len(undos)), zap.Int("redos_length", len(redos)))
	if p.matchFilter(StepUndo) {
		if err := p.processBlockIDs(blk.ID(), undos, StepUndo); err != nil {
			return err
		}
	}

	if p.matchFilter(StepRedo) {
		if err := p.processBlockIDs(blk.ID(), redos, StepRedo); err != nil {
			return err
		}
	}

	if err := p.processNewBlocks(longestChain); err != nil {
		return err
	}

	if p.lastBlockSent == nil {
		return nil
	}

	newLIBNum := p.lastBlockSent.LIBNum()
	newHeadBlock := p.lastBlockSent

	libRef := p.forkDB.BlockInCurrentChain(newHeadBlock, newLIBNum)
	if libRef.ID() == "" {
		// TODO: this is quite an error condition, if we've reached
		// this place and have links down to the `LIB` (which we're
		// assured by the `TrySetLIB` check up there ^^)
		zlogBlk.Debug("missing links to reach lib_num", zap.Stringer("new_head_block", newHeadBlock), zap.Uint64("new_lib_num", newLIBNum))
		return nil
	}

	// TODO: check preconditions here, and decide on whether we
	// continue or not early return would be perfect if there's no
	// `irreversibleSegment` or `stalledBlocks` to process.
	hasNew, irreversibleSegment, stalledBlocks := p.forkDB.HasNewIrreversibleSegment(libRef)
	if !hasNew {
		return nil
	}

	zlogBlk.Debug("moving lib", zap.String("lib_id", libRef.ID()), zap.Uint64("lib_num", libRef.Num()))
	_ = p.forkDB.MoveLIB(libRef)

	if err := p.processIrreversibleSegment(irreversibleSegment); err != nil {
		return err
	}

	if err := p.processStalledSegment(stalledBlocks); err != nil {
		return err
	}

	return nil
}

func ids(blocks []*ForkableBlock) (ids []string) {
	ids = make([]string, len(blocks))
	for i, obj := range blocks {
		ids[i] = obj.Block.ID()
	}

	return
}

func (p *Forkable) sentChainSwitchSegments(zlogger *zap.Logger, currentHeadBlockID string, newHeadsPreviousID string) (undos []*ForkableBlock, redos []*ForkableBlock) {
	if currentHeadBlockID == newHeadsPreviousID {
		return
	}

	undoIDs, redoIDs := p.forkDB.ChainSwitchSegments(currentHeadBlockID, newHeadsPreviousID)

	undos = p.sentChainSegment(undoIDs, false)
	redos = p.sentChainSegment(redoIDs, true)
	return
}

func (p *Forkable) sentChainSegment(ids []string, doingRedos bool) (ppBlocks []*ForkableBlock) {
	for _, blockID := range ids {
		blkObj := p.forkDB.BlockForID(blockID)
		if blkObj == nil {
			panic(fmt.Errorf("block for id returned nil for id %q, this would panic later on", blockID))
		}

		ppBlock := blkObj.Object.(*ForkableBlock)
		if doingRedos && !ppBlock.SentAsNew {
			continue
		}

		ppBlocks = append(ppBlocks, ppBlock)
	}
	return
}

func (p *Forkable) processBlockIDs(currentBlockID string, blocks []*ForkableBlock, step StepType) error {
	var objs []*bstream.PreprocessedBlock
	for _, block := range blocks {
		objs = append(objs, &bstream.PreprocessedBlock{
			Block: block.Block,
			Obj:   block.Obj,
		})
	}

	for idx, block := range blocks {
		err := p.handler.ProcessBlock(block.Block, &ForkableObject{
			Step:   step,
			ForkDB: p.forkDB,
			Obj:    block.Obj,

			StepIndex:  idx,
			StepCount:  len(blocks),
			StepBlocks: objs,
		})

		p.logger.Debug("sent block", zap.Stringer("block", block.Block), zap.Stringer("step_type", step))
		if err != nil {
			return fmt.Errorf("process block [%s] step=%q: %w", block.Block, step, err)
		}
	}
	return nil
}

func (p *Forkable) processNewBlocks(longestChain []*Block) (err error) {
	for _, b := range longestChain {
		ppBlk := b.Object.(*ForkableBlock)
		if ppBlk.SentAsNew {
			// Sadly, there was a debug log line here, but it's so a pain to have when debug, since longuest
			// chain is iterated over and over again generating tons of this (now gone) log line. For this,
			// it was removed to make it easier to track what happen.
			continue
		}

		if p.matchFilter(StepNew) {
			err = p.handler.ProcessBlock(ppBlk.Block, &ForkableObject{
				Step:   StepNew,
				ForkDB: p.forkDB,
				Obj:    ppBlk.Obj,
			})
			if err != nil {
				return
			}
		}

		p.logger.Debug("sending block as new to consumer", zap.Stringer("block", ppBlk.Block))

		p.blockFlowed(ppBlk.Block)
		ppBlk.SentAsNew = true
		p.lastBlockSent = ppBlk.Block
	}

	return
}

func (p *Forkable) processInitialInclusiveIrreversibleBlock(blk *bstream.Block, obj interface{}) error {
	// Normally extracted from ForkDB, we create it here:
	singleBlock := &Block{
		// Other fields not needed by `processNewBlocks`
		Object: &ForkableBlock{
			// WARN: this ForkDB doesn't have a reference to the current block, hopefully downstream doesn't need that (!)
			Block: blk,
			Obj:   obj,
		},
	}

	tinyChain := []*Block{singleBlock}

	if err := p.processNewBlocks(tinyChain); err != nil {
		return err
	}

	if err := p.processIrreversibleSegment(tinyChain); err != nil {
		return err
	}

	return nil
}

func (p *Forkable) processIrreversibleSegment(irreversibleSegment []*Block) error {
	if p.matchFilter(StepIrreversible) {
		var irrGroup []*bstream.PreprocessedBlock
		for _, irrBlock := range irreversibleSegment {
			preprocBlock := irrBlock.Object.(*ForkableBlock)
			irrGroup = append(irrGroup, &bstream.PreprocessedBlock{
				Block: preprocBlock.Block,
				Obj:   preprocBlock.Obj,
			})
		}

		for idx, irrBlock := range irreversibleSegment {
			preprocBlock := irrBlock.Object.(*ForkableBlock)

			objWrap := &ForkableObject{
				Step:   StepIrreversible,
				ForkDB: p.forkDB,
				Obj:    preprocBlock.Obj,

				StepIndex:  idx,
				StepCount:  len(irreversibleSegment),
				StepBlocks: irrGroup,
			}

			if err := p.handler.ProcessBlock(preprocBlock.Block, objWrap); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *Forkable) processStalledSegment(stalledBlocks []*Block) error {
	if p.matchFilter(StepStalled) {
		var stalledGroup []*bstream.PreprocessedBlock
		for _, staleBlock := range stalledBlocks {
			preprocBlock := staleBlock.Object.(*ForkableBlock)
			stalledGroup = append(stalledGroup, &bstream.PreprocessedBlock{
				Block: preprocBlock.Block,
				Obj:   preprocBlock.Obj,
			})
		}

		for idx, staleBlock := range stalledBlocks {
			preprocBlock := staleBlock.Object.(*ForkableBlock)

			objWrap := &ForkableObject{
				Step:   StepStalled,
				ForkDB: p.forkDB,
				Obj:    preprocBlock.Obj,

				StepIndex:  idx,
				StepCount:  len(stalledBlocks),
				StepBlocks: stalledGroup,
			}

			if err := p.handler.ProcessBlock(preprocBlock.Block, objWrap); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *Forkable) blockFlowed(blockRef bstream.BlockRef) {
	if p.ensureBlockFlows.ID() == "" {
		return
	}

	if p.ensureBlockFlowed {
		return
	}

	if blockRef.ID() == p.ensureBlockFlows.ID() {
		p.ensureBlockFlowed = true
	}
}

func (p *Forkable) triggersNewLongestChain(blk *bstream.Block) bool {
	if p.ensureAllBlocksTriggerLongestChain {
		return true
	}

	if p.lastBlockSent == nil {
		return true
	}

	if blk.Num() > p.lastBlockSent.Num() {
		return true
	}

	return false
}
