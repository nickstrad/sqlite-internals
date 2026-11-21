// Package relocate implements the seven-step coordinator that moves a database
// from a source node to a destination node without allowing two writable owners
// at once.
//
// Bumping the catalog epoch alone (step 1) does not stop the source node, which
// does not consult the catalog before writing; step 2 (SealSource) asks the
// cooperative source to refuse writes before step 3 (ShipFinal) drains local
// commits into the store, and step 6 (Activate) puts the destination's fence
// snapshot in the store to bind any uncooperative source.
//
// The coordinator holds no durable state of its own outside the catalog's
// relocations row. After a coordinator crash, node kill, or store fault, a
// fresh Coordinator reads that row and inspects the store's slot chain: if the
// destination's fence is already in the store, recovery can only roll forward
// (adopting the fence); otherwise it can either Resume forward or Rollback by
// draining and re-claiming on the source at a higher epoch.
package relocate

import (
	"context"
	"errors"
	"fmt"

	"github.com/nickstrad/walfleet/ch10-multi-database-platform-capstone/service/catalog"
	"github.com/nickstrad/walfleet/ch10-multi-database-platform-capstone/service/fence"
	"github.com/nickstrad/walfleet/ch10-multi-database-platform-capstone/service/node"
	"github.com/nickstrad/walfleet/internal/walship"
)

// Step numbers the seven phases of a relocation so progress can be persisted in
// the catalog and faults can be injected after each step.
type Step int

// Ordered relocation steps from catalog epoch bump through source retirement.
const (
	// FenceEpoch bumps the catalog epoch to destination and inserts the running
	// relocation row in one SQLite transaction.
	FenceEpoch Step = iota + 1
	// SealSource writes <id>.sealed on the source so later writes are refused.
	SealSource
	// ShipFinal drains unshipped commits from the source, closes it at rest,
	// and records the drained position and sha256 in the catalog.
	ShipFinal
	// Rebuild lists the store at the new epoch on the destination and restores
	// the chain end into <id>.staged.
	Rebuild
	// Verify checks that the staged position and sha256 equal the drained state.
	Verify
	// Activate puts the destination's fence snapshot in the store, installs the
	// local database and .owner file, and updates the catalog.
	Activate
	// RetireSource evicts and deletes the database file and sidecars on the source.
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

// String formats s as "<number> (<description>)".
func (s Step) String() string {
	if name, ok := stepNames[s]; ok {
		return fmt.Sprintf("%d (%s)", int(s), name)
	}
	return fmt.Sprintf("Step(%d)", int(s))
}

// Steps returns all seven relocation steps in execution order.
func Steps() []Step {
	return []Step{FenceEpoch, SealSource, ShipFinal, Rebuild, Verify, Activate, RetireSource}
}

// Sentinel errors returned by Coordinator.Run, Resume, and Rollback.
var (
	// ErrCrashed is returned when the Coordinator.CrashAfter injection hook fires.
	ErrCrashed = errors.New("relocate: the coordinator stopped (injected crash)")
	// ErrVerifyFailed reports that the destination's staged position or sha256
	// did not match the source's drained state at step 5.
	ErrVerifyFailed = errors.New("relocate: the rebuilt database is not the source's state at the fence point")
	// ErrNotRunning reports that the relocation row is already in a terminal state.
	ErrNotRunning = errors.New("relocate: the relocation is not running")
	// ErrFenceInStore is joined with the roll-forward result when Rollback
	// discovers the destination's fence snapshot already landed in the store.
	ErrFenceInStore = errors.New("relocate: the destination's fence is in the store; the relocation can only roll forward")
	// ErrSupersededBy reports that a newer epoch than the relocation's ToEpoch
	// (owned by another node) already holds a slot in the store.
	ErrSupersededBy = errors.New("relocate: a newer epoch than the relocation's is in the store")
	// ErrMissingRecord reports that the store holds the destination's fence
	// snapshot but the catalog row lacks a matching drained position and sha256.
	ErrMissingRecord = errors.New("relocate: the catalog has no drained position for this relocation")
)

// Participant is the set of operations the coordinator invokes on a source or
// destination node, satisfied by both an in-process *node.Node and node.Remote.
type Participant interface {
	Seal(ctx context.Context, id string) error
	Drain(ctx context.Context, id string) (node.Drained, error)
	Stage(ctx context.Context, id string, epoch uint64) (node.Staged, error)
	Activate(ctx context.Context, id string, epoch uint64, position int64, digest string, fenceStore bool) (node.Activated, error)
	Retire(ctx context.Context, id string) error
	Discard(ctx context.Context, id string) error
	Claim(ctx context.Context, id string) (node.ClaimResult, error)
}

// Coordinator drives a database relocation against Catalog, Store, and
// Participant handles. CrashAfter, SkipFence (the Experiment 10.3 mutant), and
// AfterStep are test and experiment hooks.
type Coordinator struct {
	Catalog    *catalog.Catalog
	Store      walship.Store
	Node       func(name string) Participant
	CrashAfter Step
	SkipFence  bool
	AfterStep  func(step Step)
}

// Outcome names how a Run, Resume, or Rollback call finished.
type Outcome string

// Terminal outcomes returned in Result.Outcome.
const (
	// Done means Run completed all seven steps without interruption.
	Done Outcome = "done"
	// RolledBack means Rollback returned ownership to the source at a new epoch.
	RolledBack Outcome = "rolled back"
	// RolledForward means Resume (or Rollback after a landed fence) finished
	// moving ownership to the destination.
	RolledForward Outcome = "rolled forward"
	// Superseded means another node's higher epoch already took over in the store.
	Superseded Outcome = "superseded"
	// NothingToDo means the relocation row was already finished.
	NothingToDo Outcome = "nothing to do"
)

// Result summarizes the outcome, drained fence-point Position and SHA256,
// accepted Fence position, resulting Epoch, and any best-effort destination
// DiscardErr left when rolling back while the destination is unreachable.
type Result struct {
	Outcome    Outcome
	Position   int64
	SHA256     string
	Fence      int64
	Epoch      uint64
	DiscardErr error
}

// StepError tags a failure returned during a specific relocation Step.
type StepError struct {
	Step Step
	Err  error
}

// Error formats e with the failing step number and description.
func (e *StepError) Error() string { return fmt.Sprintf("relocate: step %s: %v", e.Step, e.Err) }

// Unwrap exposes the underlying error for errors.Is and errors.As.
func (e *StepError) Unwrap() error { return e.Err }

func (c *Coordinator) after(step Step) {
	if c.AfterStep != nil {
		c.AfterStep(step)
	}
}

// Run starts a new relocation of id from source to destination (step 1 in the
// catalog) and executes steps 2 through 7 in order.
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
			// SkipFence is the Experiment 10.3 mutant: skipping seal, drain,
			// store fence, and retire proves the boundary check catches dual writers.
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

// Resume finishes an interrupted relocation using only the catalog's
// relocations row and the store's slot chain. If a higher epoch already holds a
// slot, it marks the row superseded; if ToEpoch's fence is already in the store,
// it adopts the fence and retires the source; otherwise it replays idempotent
// steps 2 through 7 to completion.
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

// Rollback cancels an interrupted relocation before the destination's fence
// lands in the store: it seals and drains the source (preserving every local
// acknowledgement), lets the source Claim a higher epoch and fence the store
// against any zombie destination, marks the relocation rolled-back in the
// catalog, and then asks the destination to Discard its staged copy best-effort
// (recording any unreachable error in Result.DiscardErr without wedging the
// relocation). If the destination's fence is already in the store, Rollback
// rolls forward instead and returns ErrFenceInStore.
func (c *Coordinator) Rollback(ctx context.Context, id string) (Result, error) {
	r, newest, _, err := c.running(ctx, id)
	if errors.Is(err, ErrNotRunning) {
		return Result{Outcome: NothingToDo}, nil
	}
	if err != nil {
		return Result{}, err
	}
	switch {
	// A prior Rollback may have completed source.Claim before crashing prior to
	// FinishRelocation; recognize the source's own higher epoch and finish as
	// rolled-back rather than reporting Superseded.
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
	// Finish the catalog record before contacting the destination: once the
	// source's higher-epoch fence is in the store, a leftover .staged file on a
	// down destination cannot activate and must not keep relocations.state running.
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
