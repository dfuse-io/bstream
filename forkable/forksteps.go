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
	"strings"

	pbbstream "github.com/dfuse-io/pbgo/dfuse/bstream/v1"
)

type StepType int

const (
	StepNew          = StepType(1 << iota) //1  First time we're seeing this block
	StepUndo                               //2  We are undoing this block (it was done previously)
	StepRedo                               //4  We are redoing this block (it was done previously)
	StepHandoff                            //8  The block passed a handoff from one producer to another
	StepIrreversible                       //16 This block passed the LIB barrier and is in chain
	StepStalled                            //32 This block passed the LIB and is definitely forked out
	StepsAll         = StepType(StepNew | StepUndo | StepRedo | StepHandoff | StepIrreversible | StepStalled)
)

func (t StepType) String() string {
	var el []string
	if t&StepNew != 0 {
		el = append(el, "new")
	}
	if t&StepUndo != 0 {
		el = append(el, "undo")
	}
	if t&StepRedo != 0 {
		el = append(el, "redo")
	}
	if t&StepHandoff != 0 {
		el = append(el, "handoff")
	}
	if t&StepIrreversible != 0 {
		el = append(el, "irreversible")
	}
	if t&StepStalled != 0 {
		el = append(el, "stalled")
	}
	if len(el) == 0 {
		return "none"
	}
	out := strings.Join(el, ",")
	if out == "new,undo,redo,handoff,irreversible,stalled" {
		return "all"
	}
	return out
}

func (t StepType) IsSingleStep() bool {
	switch t {
	case StepNew,
		StepUndo,
		StepRedo,
		StepHandoff,
		StepIrreversible,
		StepStalled:
		return true
	}
	return false
}

func StepsFromProto(steps []pbbstream.ForkStep) StepType {
	if len(steps) <= 0 {
		return StepNew | StepRedo | StepUndo | StepIrreversible
	}

	var filter StepType
	var containsNew bool
	var containsUndo bool
	for _, step := range steps {
		if step == pbbstream.ForkStep_STEP_NEW {
			containsNew = true
		}
		if step == pbbstream.ForkStep_STEP_UNDO {
			containsUndo = true
		}
		filter |= StepFromProto(step)
	}

	// Redo is output into 'new' and has no proto equivalent
	if containsNew && containsUndo {
		filter |= StepRedo
	}

	return filter
}

func StepFromProto(step pbbstream.ForkStep) StepType {
	switch step {
	case pbbstream.ForkStep_STEP_NEW:
		return StepNew
	case pbbstream.ForkStep_STEP_UNDO:
		return StepUndo
	case pbbstream.ForkStep_STEP_IRREVERSIBLE:
		return StepIrreversible
	}
	return StepType(0)
}
