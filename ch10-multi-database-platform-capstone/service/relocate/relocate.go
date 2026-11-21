package relocate

import (
	"context"
	"errors"
	"fmt"

	"github.com/nickstrad/sqlite-internals/ch10-multi-database-platform-capstone/service/catalog"
	"github.com/nickstrad/sqlite-internals/ch10-multi-database-platform-capstone/service/fence"
	"github.com/nickstrad/sqlite-internals/ch10-multi-database-platform-capstone/service/node"
	"github.com/nickstrad/sqlite-internals/internal/walship"
)

type Step int

const (
	FenceEpoch Step = iota + 1
	SealSource
	ShipFinal
	Rebuild
	Verify
	Activate
	RetireSource
)

var stepNames = map[Step]string{
	FenceEpoch:   "fence the source: bump the epoch in the catalog",
	SealSource:   "seal the source",
	ShipFinal:    "ship the final segment and close the source at rest",
	Rebuild:      "rebuild at the destination from the store",
	Verify:       "verify the rebuilt hash against the source's",
	Activate:     "activate: put the destination's fence in the store",
	RetireSource: "retire the source",
}

func (s Step) String() string {
	if name, ok := stepNames[s]; ok {
		return fmt.Sprintf("%d (%s)", int(s), name)
	}
	return fmt.Sprintf("Step(%d)", int(s))
}

func Steps() []Step {
	return []Step{FenceEpoch, SealSource, ShipFinal, Rebuild, Verify, Activate, RetireSource}
}

var (
	ErrCrashed       = errors.New("relocate: the coordinator stopped (injected crash)")
	ErrVerifyFailed  = errors.New("relocate: the rebuilt database is not the source's state at the fence point")
	ErrNotRunning    = errors.New("relocate: the relocation is not running")
	ErrFenceInStore  = errors.New("relocate: the destination's fence is in the store; the relocation can only roll forward")
	ErrSupersededBy  = errors.New("relocate: a newer epoch than the relocation's is in the store")
	ErrMissingRecord = errors.New("relocate: the catalog has no drained position for this relocation")
)

type Participant interface {
	Seal(ctx context.Context, id string) error
	Drain(ctx context.Context, id string) (node.Drained, error)
	Stage(ctx context.Context, id string, epoch uint64) (node.Staged, error)
	Activate(ctx context.Context, id string, epoch uint64, position int64, digest string, fenceStore bool) (node.Activated, error)
	Retire(ctx context.Context, id string) error
	Discard(ctx context.Context, id string) error
	Claim(ctx context.Context, id string) (node.ClaimResult, error)
}

type Coordinator struct {
	Catalog    *catalog.Catalog
	Store      walship.Store
	Node       func(name string) Participant
	CrashAfter Step
	SkipFence  bool
	AfterStep  func(step Step)
}

type Outcome string

const (
	Done          Outcome = "done"
	RolledBack    Outcome = "rolled back"
	RolledForward Outcome = "rolled forward"
	Superseded    Outcome = "superseded"
	NothingToDo   Outcome = "nothing to do"
)

type Result struct {
	Outcome    Outcome
	Position   int64
	SHA256     string
	Fence      int64
	Epoch      uint64
	DiscardErr error
}

type StepError struct {
	Step Step
	Err  error
}

func (e *StepError) Error() string { return fmt.Sprintf("relocate: step %s: %v", e.Step, e.Err) }

func (e *StepError) Unwrap() error { return e.Err }

func (c *Coordinator) after(step Step) {
	if c.AfterStep != nil {
		c.AfterStep(step)
	}
}

func (c *Coordinator) Run(ctx context.Context, id, source, destination string) (Result, error) {
	r, err := c.Catalog.BeginRelocation(ctx, id, source, destination)
	if err != nil {
		return Result{}, &StepError{Step: FenceEpoch, Err: err}
	}
	if c.CrashAfter == FenceEpoch {
		return Result{}, ErrCrashed
	}
	c.after(FenceEpoch)
	return c.forward(ctx, r)
}

func (c *Coordinator) forward(ctx context.Context, r catalog.Relocation) (Result, error) {
	source, destination := c.Node(r.Source), c.Node(r.Destination)
	var staged node.Staged
	result := Result{Epoch: r.ToEpoch}
	for _, step := range Steps()[1:] {
		var position int64
		var digest string
		var err error
		switch step {
		case SealSource:
			if !c.SkipFence {
				err = source.Seal(ctx, r.ID)
			}
		case ShipFinal:
			var drained node.Drained
			if c.SkipFence {
				drained, err = c.storeEnd(ctx, r.ID)
			} else {
				drained, err = source.Drain(ctx, r.ID)
			}
			position, digest = drained.Position, drained.SHA256
			r.Position, r.SHA256 = position, digest
		case Rebuild:
			staged, err = destination.Stage(ctx, r.ID, r.ToEpoch)
		case Verify:
			if staged.Position != r.Position || staged.SHA256 != r.SHA256 {
				err = fmt.Errorf("%w: staged position %d sha256 %s, drained position %d sha256 %s", ErrVerifyFailed, staged.Position, staged.SHA256, r.Position, r.SHA256)
			}
		case Activate:
			var activated node.Activated
			activated, err = destination.Activate(ctx, r.ID, r.ToEpoch, r.Position, r.SHA256, !c.SkipFence)
			result.Fence = activated.Fence
		case RetireSource:
			if !c.SkipFence {
				err = source.Retire(ctx, r.ID)
			}
		}
		if err != nil {
			return result, &StepError{Step: step, Err: err}
		}
		if c.CrashAfter == step {
			return result, ErrCrashed
		}
		if err := c.Catalog.AdvanceRelocation(ctx, r.ID, int(step), position, digest); err != nil {
			return result, &StepError{Step: step, Err: err}
		}
		c.after(step)
	}
	result.Outcome, result.Position, result.SHA256 = Done, r.Position, r.SHA256
	return result, c.Catalog.FinishRelocation(ctx, r.ID, catalog.RelocationDone)
}

func (c *Coordinator) storeEnd(ctx context.Context, id string) (node.Drained, error) {
	_, restorable, err := fence.Audit(ctx, c.Store, id)
	if err != nil {
		return node.Drained{}, err
	}
	last, found := restorable.LastPosition()
	if !found {
		return node.Drained{}, fmt.Errorf("relocate: the store holds nothing for %s", id)
	}
	image, err := fence.RestoreAt(restorable, last)
	return node.Drained{Position: last, SHA256: walship.SHA256Hex(image)}, err
}

func newestEpoch(chain []fence.Slot) (uint64, int64) {
	var epoch uint64
	var first int64
	for _, slot := range chain {
		if slot.Epoch > epoch {
			epoch, first = slot.Epoch, slot.First
		}
	}
	return epoch, first
}

func (c *Coordinator) running(ctx context.Context, id string) (catalog.Relocation, uint64, int64, error) {
	r, err := c.Catalog.GetRelocation(ctx, id)
	if err != nil {
		return r, 0, 0, err
	}
	if r.State != catalog.RelocationRunning {
		return r, 0, 0, fmt.Errorf("%w: %s", ErrNotRunning, r)
	}
	chain, err := fence.Chain(ctx, c.Store, id)
	if err != nil {
		return r, 0, 0, err
	}
	epoch, first := newestEpoch(chain)
	return r, epoch, first, nil
}

func (c *Coordinator) Resume(ctx context.Context, id string) (Result, error) {
	r, newest, first, err := c.running(ctx, id)
	if errors.Is(err, ErrNotRunning) {
		return Result{Outcome: NothingToDo, Epoch: r.ToEpoch, Position: r.Position, SHA256: r.SHA256}, nil
	}
	if err != nil {
		return Result{}, err
	}
	switch {
	case newest > r.ToEpoch:
		return Result{Outcome: Superseded}, errors.Join(fmt.Errorf("%w: epoch %d, relocation epoch %d", ErrSupersededBy, newest, r.ToEpoch), c.Catalog.FinishRelocation(ctx, id, catalog.RelocationSuperseded))
	case newest == r.ToEpoch:
		return c.adopt(ctx, r, first)
	}
	result, err := c.forward(ctx, r)
	if err == nil {
		result.Outcome = RolledForward
	}
	return result, err
}

func (c *Coordinator) adopt(ctx context.Context, r catalog.Relocation, fencePosition int64) (Result, error) {
	if r.Position == 0 || r.SHA256 == "" || r.Position+1 != fencePosition {
		return Result{}, fmt.Errorf("%w: %s, fence at %d", ErrMissingRecord, r, fencePosition)
	}
	activated, err := c.Node(r.Destination).Activate(ctx, r.ID, r.ToEpoch, r.Position, r.SHA256, true)
	if err != nil {
		return Result{}, &StepError{Step: Activate, Err: err}
	}
	if err := c.Catalog.AdvanceRelocation(ctx, r.ID, int(Activate), 0, ""); err != nil {
		return Result{}, &StepError{Step: Activate, Err: err}
	}
	if err := c.Node(r.Source).Retire(ctx, r.ID); err != nil {
		return Result{}, &StepError{Step: RetireSource, Err: err}
	}
	if err := c.Catalog.AdvanceRelocation(ctx, r.ID, int(RetireSource), 0, ""); err != nil {
		return Result{}, &StepError{Step: RetireSource, Err: err}
	}
	result := Result{Outcome: RolledForward, Position: r.Position, SHA256: r.SHA256, Fence: activated.Fence, Epoch: r.ToEpoch}
	return result, c.Catalog.FinishRelocation(ctx, r.ID, catalog.RelocationDone)
}

func (c *Coordinator) Rollback(ctx context.Context, id string) (Result, error) {
	r, newest, _, err := c.running(ctx, id)
	if errors.Is(err, ErrNotRunning) {
		return Result{Outcome: NothingToDo}, nil
	}
	if err != nil {
		return Result{}, err
	}
	switch {
	case newest > r.ToEpoch && c.reclaimedBySource(ctx, r, newest):
		result := Result{Outcome: RolledBack, Epoch: newest}
		if err := c.Catalog.FinishRelocation(ctx, id, catalog.RelocationRolledBack); err != nil {
			return result, err
		}
		result.DiscardErr = c.Node(r.Destination).Discard(ctx, id)
		return result, nil
	case newest > r.ToEpoch:
		return Result{Outcome: Superseded}, errors.Join(fmt.Errorf("%w: epoch %d, relocation epoch %d", ErrSupersededBy, newest, r.ToEpoch), c.Catalog.FinishRelocation(ctx, id, catalog.RelocationSuperseded))
	case newest == r.ToEpoch:
		result, err := c.Resume(ctx, id)
		return result, errors.Join(ErrFenceInStore, err)
	}
	source := c.Node(r.Source)
	if err := source.Seal(ctx, id); err != nil {
		return Result{}, &StepError{Step: SealSource, Err: err}
	}
	drained, err := source.Drain(ctx, id)
	if err != nil {
		return Result{}, &StepError{Step: ShipFinal, Err: err}
	}
	claimed, err := source.Claim(ctx, id)
	if err != nil {
		return Result{}, fmt.Errorf("relocate: roll back %s: the source's claim: %w", id, err)
	}
	result := Result{Outcome: RolledBack, Position: drained.Position, SHA256: drained.SHA256, Fence: claimed.Fence, Epoch: claimed.Epoch}
	if err := c.Catalog.FinishRelocation(ctx, id, catalog.RelocationRolledBack); err != nil {
		return result, err
	}
	result.DiscardErr = c.Node(r.Destination).Discard(ctx, id)
	return result, nil
}

func (c *Coordinator) reclaimedBySource(ctx context.Context, r catalog.Relocation, newest uint64) bool {
	record, err := c.Catalog.Get(ctx, r.ID)
	return err == nil && record.Owner == r.Source && record.Epoch == newest
}
