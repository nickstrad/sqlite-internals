package walship

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
)

// Sentinel errors returned while planning, executing, or pruning restores.
var (
	// ErrNoSnapshot reports that no usable snapshot exists at or before the target position.
	ErrNoSnapshot = errors.New("no usable snapshot at or before the target")
	// ErrBrokenChain reports a missing, damaged, or non-contiguous segment or a post-replay state-hash mismatch.
	ErrBrokenChain = errors.New("segment chain is broken")
	// ErrNoCommit reports that a target position does not name any commit point inside its covering segment.
	ErrNoCommit = errors.New("no commit point at that position")
)

// Snapshot is a validated snapshot object loaded from a Store.
type Snapshot struct {
	Key   string
	Meta  SnapshotMeta
	Image []byte
}

// Segment is a validated WAL segment object loaded from a Store.
type Segment struct {
	Key    string
	Meta   SegmentMeta
	Frames []byte
}

// Damaged records an object in a Store that failed key parsing, decoding, body
// hashing, or WAL checksum-chain verification during LoadCatalog.
type Damaged struct {
	Key     string
	Kind    string
	Problem string
}

// Catalog holds all usable snapshots and segments loaded from a Store, sorted
// by position, alongside any objects quarantined as Damaged.
type Catalog struct {
	Snapshots []Snapshot
	Segments  []Segment
	Damaged   []Damaged
}

// LoadCatalog lists, fetches, decodes, and verifies every snapshot and segment
// object in store. Objects that fail key/metadata/body/checksum checks are
// recorded in Catalog.Damaged instead of aborting the load, so undamaged
// snapshots and segments remain usable for planning and diagnostics.
func LoadCatalog(ctx context.Context, store Store) (*Catalog, error) {
	catalog := &Catalog{}
	for _, prefix := range []string{SnapshotPrefix, SegmentPrefix} {
		keys, err := store.List(ctx, prefix)
		if err != nil {
			return nil, err
		}
		// Sort keys explicitly; Store.List makes no ordering guarantee.
		sort.Strings(keys)
		for _, key := range keys {
			data, err := store.Get(ctx, key)
			if err != nil {
				return nil, err
			}
			catalog.add(key, data)
		}
	}
	sort.Slice(catalog.Snapshots, func(i, j int) bool { return catalog.Snapshots[i].Meta.Position < catalog.Snapshots[j].Meta.Position })
	sort.Slice(catalog.Segments, func(i, j int) bool {
		return catalog.Segments[i].Meta.FirstPosition < catalog.Segments[j].Meta.FirstPosition
	})
	return catalog, nil
}

func (c *Catalog) add(key string, data []byte) {
	kind, position, err := ParseKey(key)
	if err != nil {
		c.Damaged = append(c.Damaged, Damaged{Key: key, Kind: "unknown", Problem: err.Error()})
		return
	}
	switch kind {
	case "snapshot":
		meta, image, err := DecodeSnapshot(data)
		if err == nil && meta.Position != position {
			err = fmt.Errorf("%w: key names position %d, metadata says %d", ErrMalformedObject, position, meta.Position)
		}
		if err != nil {
			c.Damaged = append(c.Damaged, Damaged{Key: key, Kind: kind, Problem: err.Error()})
			return
		}
		c.Snapshots = append(c.Snapshots, Snapshot{Key: key, Meta: meta, Image: image})
	case "segment":
		meta, frames, err := DecodeSegment(data)
		if err == nil && meta.FirstPosition != position {
			err = fmt.Errorf("%w: key names first position %d, metadata says %d", ErrMalformedObject, position, meta.FirstPosition)
		}
		// Verify the WAL header, commit frames, and checksum chain up front so
		// damaged segments are rejected during LoadCatalog and Plan before any
		// page is applied.
		if err == nil {
			err = VerifySegmentFrames(meta, frames)
		}
		if err != nil {
			c.Damaged = append(c.Damaged, Damaged{Key: key, Kind: kind, Problem: err.Error()})
			return
		}
		c.Segments = append(c.Segments, Segment{Key: key, Meta: meta, Frames: frames})
	}
}

// LastPosition returns the highest usable stream position present in c (across
// both snapshots and segments), or (0, false) if c has no usable objects.
func (c *Catalog) LastPosition() (int64, bool) {
	last, found := int64(0), false
	for _, s := range c.Snapshots {
		if !found || s.Meta.Position > last {
			last, found = s.Meta.Position, true
		}
	}
	for _, s := range c.Segments {
		if !found || s.Meta.LastPosition > last {
			last, found = s.Meta.LastPosition, true
		}
	}
	return last, found
}

func (c *Catalog) damagedAt(kind string, position int64) (Damaged, bool) {
	for _, d := range c.Damaged {
		k, p, err := ParseKey(d.Key)
		if err == nil && k == kind && p == position {
			return d, true
		}
	}
	return Damaged{}, false
}

func (c *Catalog) segmentStartingAt(position int64) (Segment, error) {
	for _, s := range c.Segments {
		if s.Meta.FirstPosition == position {
			return s, nil
		}
	}
	if d, ok := c.damagedAt("segment", position); ok {
		return Segment{}, fmt.Errorf("%w: the segment starting at position %d is damaged: %s", ErrBrokenChain, position, d.Problem)
	}
	return Segment{}, fmt.Errorf("%w: no segment starts at position %d", ErrBrokenChain, position)
}

// Plan is a verified restore plan from one base Snapshot through zero or more
// contiguous Segments up to Target's CommitPoint.
type Plan struct {
	Target   int64
	Snapshot Snapshot
	Segments []Segment
	Commit   CommitPoint
}

// Commits returns the number of shipped commits between p.Snapshot and p.Target.
func (p Plan) Commits() int64 { return p.Target - p.Snapshot.Meta.Position }

// PlanFrom builds a restore Plan starting at the specific snapshot at
// snapshotPosition and advancing through contiguous segments to target. Every
// link's BeforeSHA256, WAL generation, frame number, and seed checksum are
// verified before Plan.Execute applies any page.
func (c *Catalog) PlanFrom(snapshotPosition, target int64) (Plan, error) {
	var snapshot *Snapshot
	for i := range c.Snapshots {
		if c.Snapshots[i].Meta.Position == snapshotPosition {
			snapshot = &c.Snapshots[i]
		}
	}
	if snapshot == nil {
		if d, ok := c.damagedAt("snapshot", snapshotPosition); ok {
			return Plan{}, fmt.Errorf("%w: the snapshot at position %d is damaged: %s", ErrNoSnapshot, snapshotPosition, d.Problem)
		}
		return Plan{}, fmt.Errorf("%w: no snapshot at position %d", ErrNoSnapshot, snapshotPosition)
	}
	if snapshot.Meta.Position > target {
		return Plan{}, fmt.Errorf("%w: the snapshot at position %d is after target %d", ErrNoSnapshot, snapshot.Meta.Position, target)
	}
	plan := Plan{Target: target, Snapshot: *snapshot, Commit: CommitPoint{
		Position: snapshot.Meta.Position, Frame: snapshot.Meta.WAL.Frame, Pages: snapshot.Meta.Pages,
		UserVersion: snapshot.Meta.UserVersion, StateSHA256: snapshot.Meta.StateSHA256,
	}}
	position, state, end := snapshot.Meta.Position, snapshot.Meta.StateSHA256, snapshot.Meta.WAL
	for position < target {
		segment, err := c.segmentStartingAt(position + 1)
		if err != nil {
			return Plan{}, err
		}
		if err := continues(end, state, segment.Meta); err != nil {
			return Plan{}, fmt.Errorf("%w: segment %s: %v", ErrBrokenChain, segment.Key, err)
		}
		plan.Segments = append(plan.Segments, segment)
		if segment.Meta.LastPosition >= target {
			commit, ok := segment.Meta.Commit(target)
			if !ok {
				return Plan{}, fmt.Errorf("%w: %d in segment %s", ErrNoCommit, target, segment.Key)
			}
			plan.Commit = commit
			return plan, nil
		}
		position, state, end = segment.Meta.LastPosition, segment.Meta.AfterSHA256, segment.Meta.End()
	}
	return plan, nil
}

// continues verifies that next chains cleanly from the previous state hash and
// WAL position: within the same generation it must start at Frame+1 with
// SeedChecksum == end.Checksum; in a new generation it must start at frame 1
// seeded from the new 32-byte WAL header's checksum.
func continues(end WALPosition, state string, next SegmentMeta) error {
	if next.BeforeSHA256 != state {
		return fmt.Errorf("its before hash %s is not the previous state %s", short(next.BeforeSHA256), short(state))
	}
	if next.Generation == end.Generation {
		if next.FirstFrame != end.Frame+1 {
			return fmt.Errorf("it starts at frame %d of the same WAL generation, want %d", next.FirstFrame, end.Frame+1)
		}
		if next.SeedChecksum != end.Checksum {
			return fmt.Errorf("its seed checksum does not continue the previous frame's checksum")
		}
		return nil
	}
	if next.FirstFrame != 1 {
		return fmt.Errorf("it starts a new WAL generation at frame %d, want 1", next.FirstFrame)
	}
	header, err := next.Generation.Bytes()
	if err != nil {
		return err
	}
	if seed := [2]uint32{be32(header[24:]), be32(header[28:])}; next.SeedChecksum != seed {
		return fmt.Errorf("it starts a new WAL generation but its seed is not the header checksum")
	}
	return nil
}

// Plan chooses the newest usable snapshot at or before target and returns
// PlanFrom that snapshot to target, minimizing the number of segments replayed.
func (c *Catalog) Plan(target int64) (Plan, error) {
	for i := len(c.Snapshots) - 1; i >= 0; i-- {
		if c.Snapshots[i].Meta.Position <= target {
			return c.PlanFrom(c.Snapshots[i].Meta.Position, target)
		}
	}
	return Plan{}, fmt.Errorf("%w: target %d", ErrNoSnapshot, target)
}

// RestoreReport summarizes the segments and frames replayed by Plan.Execute and
// the final restored database image's SHA-256.
type RestoreReport struct {
	Plan            Plan
	SegmentsApplied int
	FramesApplied   int
	StateSHA256     string
}

// Execute clones p.Snapshot.Image, applies each planned segment in order through
// p.Commit.Frame using ApplySegment, and verifies the whole-image SHA-256 after
// every segment and at the target commit point.
func (p Plan) Execute() ([]byte, RestoreReport, error) {
	image := append([]byte(nil), p.Snapshot.Image...)
	report := RestoreReport{Plan: p}
	for _, segment := range p.Segments {
		through := segment.Meta.LastFrame
		want := segment.Meta.AfterSHA256
		if segment.Meta.LastPosition > p.Target {
			through, want = p.Commit.Frame, p.Commit.StateSHA256
		}
		out, replay, err := ApplySegment(image, segment.Meta, segment.Frames, through)
		if err != nil {
			return nil, report, fmt.Errorf("segment %s: %w", segment.Key, err)
		}
		if got := SHA256Hex(out); got != want {
			return nil, report, fmt.Errorf("%w: after segment %s the state hash is %s, want %s", ErrBrokenChain, segment.Key, short(got), short(want))
		}
		image = out
		report.SegmentsApplied++
		report.FramesApplied += replay.AppliedFrames
	}
	report.StateSHA256 = SHA256Hex(image)
	if report.StateSHA256 != p.Commit.StateSHA256 {
		return nil, report, fmt.Errorf("%w: restored state %s, the commit point says %s", ErrBrokenChain, short(report.StateSHA256), short(p.Commit.StateSHA256))
	}
	return image, report, nil
}

func be32(b []byte) uint32 {
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}

// Prune deletes snapshots and segments that are no longer needed to restore any
// position at or after keepFrom. It keeps the newest usable snapshot at or
// before keepFrom and everything after it, returning only the keys actually
// deleted before any error.
func Prune(ctx context.Context, store Store, keepFrom int64) ([]string, error) {
	catalog, err := LoadCatalog(ctx, store)
	if err != nil {
		return nil, err
	}
	base := int64(-1)
	for _, s := range catalog.Snapshots {
		if s.Meta.Position <= keepFrom {
			base = s.Meta.Position
		}
	}
	if base < 0 {
		return nil, fmt.Errorf("%w: nothing to keep restores from at or before %d", ErrNoSnapshot, keepFrom)
	}
	var deleted []string
	for _, s := range catalog.Snapshots {
		if s.Meta.Position < base {
			deleted = append(deleted, s.Key)
		}
	}
	for _, s := range catalog.Segments {
		if s.Meta.LastPosition <= base {
			deleted = append(deleted, s.Key)
		}
	}
	for i, key := range deleted {
		if err := store.Delete(ctx, key); err != nil {
			return deleted[:i], err
		}
	}
	return deleted, nil
}

// WriteDatabase writes a restored database image to path and fsyncs it. It
// refuses to overwrite an existing database file or write beside an existing
// -wal, -shm, or -journal sidecar that SQLite would apply on open.
func WriteDatabase(path string, image []byte) error {
	for _, candidate := range []string{path, path + "-wal", path + "-shm", path + "-journal"} {
		if _, err := os.Lstat(candidate); err == nil {
			return fmt.Errorf("%w: %s", ErrExists, candidate)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(image)
	syncErr := file.Sync()
	closeErr := file.Close()
	return errors.Join(writeErr, syncErr, closeErr)
}

// CopyStore copies every object from from into to using PutIfAbsent in sorted
// key order, returning the number of objects copied.
func CopyStore(ctx context.Context, from, to Store) (int, error) {
	keys, err := from.List(ctx, "")
	if err != nil {
		return 0, err
	}
	sort.Strings(keys)
	for i, key := range keys {
		data, err := from.Get(ctx, key)
		if err != nil {
			return i, err
		}
		if err := to.PutIfAbsent(ctx, key, data); err != nil {
			return i, err
		}
	}
	return len(keys), nil
}
