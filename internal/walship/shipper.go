package walship

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"github.com/nickstrad/walfleet/internal/sqlitex"
	"github.com/nickstrad/walfleet/internal/walfile"
)

// Sentinel errors returned by Shipper operations.
var (
	// ErrMirrorDiverged reports that a periodic snapshot read from SQLite via
	// sqlite_dbpage does not hash to the shipper's locally replayed image.
	ErrMirrorDiverged = errors.New("the shipper's replayed state differs from SQLite's pages")
	// ErrNotWAL reports that Open was called for a non-WAL database configuration.
	ErrNotWAL = errors.New("walship needs a database in WAL mode")
)

// Config configures a Shipper: the target WAL database path, object Store,
// SQLite settings, periodic snapshot cadence (SnapshotEvery segments; 0
// disables periodic snapshots), and initial stream position (Origin).
type Config struct {
	Database      string
	Store         Store
	Options       sqlitex.Options
	SnapshotEvery int
	Origin        int64
}

// Shipper continuously captures committed WAL frames from a live database into
// a Store while owning checkpoints. It holds two SQLite connections: control
// (which takes BEGIN IMMEDIATE during each Cycle and reads snapshots via
// sqlite_dbpage) and pin (which holds a read transaction between cycles so no
// outside checkpoint can copy or recycle unshipped frames).
type Shipper struct {
	config          Config
	db              *sqlitex.DB
	control         *sql.Conn
	pin             *sql.Conn
	pinned          bool
	pageSize        int
	position        int64
	wal             WALPosition
	image           []byte
	state           string
	sinceSnapshot   int
	continuous      bool
	fullyBackfilled bool
	resumed         bool
	openSnapshot    *SnapshotMeta
	broken          error
}

// WALChange classifies how the on-disk -wal file relates to the shipper's last
// shipped WALPosition at the start of a Cycle.
type WALChange int

// Possible WAL transitions observed by Shipper.classify.
const (
	// WALUnchanged means the -wal keeps the same header and shipped prefix.
	WALUnchanged WALChange = iota
	// WALNewGeneration means the -wal restarted cleanly after the shipper's own
	// full checkpoint while the shipper held its read mark continuously.
	WALNewGeneration
	// WALDiscontinuity means continuity cannot be proven (for example, an
	// outside checkpoint or offline commit), forcing a new snapshot.
	WALDiscontinuity
)

// String returns a human-readable description of c for cycle logs and evidence.
func (c WALChange) String() string {
	switch c {
	case WALUnchanged:
		return "same WAL generation"
	case WALNewGeneration:
		return "new WAL generation after the shipper's full checkpoint"
	case WALDiscontinuity:
		return "discontinuity"
	}
	return fmt.Sprintf("WALChange(%d)", int(c))
}

// CycleReport summarizes what one Shipper.Cycle observed and published.
type CycleReport struct {
	Change       WALChange
	Reason       string
	Segment      *SegmentMeta
	Snapshot     *SnapshotMeta
	Checkpoint   sqlitex.CheckpointResult
	FullBackfill bool
}

// Lines formats r into deterministic, salt-free lines using name to label WAL
// generations in order of appearance.
func (r CycleReport) Lines(name func(Generation) string) []string {
	lines := []string{"wal: " + r.Change.String()}
	if r.Reason != "" {
		lines[0] += ": " + r.Reason
	}
	if r.Segment != nil {
		lines = append(lines, DescribeSegment(*r.Segment, name))
	} else {
		lines = append(lines, "segment: none")
	}
	lines = append(lines, fmt.Sprintf("checkpoint PASSIVE: %s; every shipped frame copied: %t", r.Checkpoint, r.FullBackfill))
	if r.Snapshot != nil {
		lines = append(lines, DescribeSnapshot(*r.Snapshot, name))
	}
	return lines
}

// DescribeSegment formats m as a single summary line without exposing random WAL salts.
func DescribeSegment(m SegmentMeta, name func(Generation) string) string {
	return fmt.Sprintf("segment %s: positions %d-%d, frames %d-%d of %s, user_version %d-%d, before %s after %s",
		SegmentKey(m.FirstPosition), m.FirstPosition, m.LastPosition, m.FirstFrame, m.LastFrame, name(m.Generation),
		m.Commits[0].UserVersion, m.Commits[len(m.Commits)-1].UserVersion, short(m.BeforeSHA256), short(m.AfterSHA256))
}

// DescribeSnapshot formats m as a single summary line without exposing random WAL salts.
func DescribeSnapshot(m SnapshotMeta, name func(Generation) string) string {
	return fmt.Sprintf("snapshot %s (%s): position %d, %d pages, user_version %d, state %s, after frame %d of %s",
		SnapshotKey(m.Position), m.Reason, m.Position, m.Pages, m.UserVersion, short(m.StateSHA256), m.WAL.Frame, name(m.WAL.Generation))
}

// GenerationNames maps random WAL Generation headers to stable sequential
// labels ("WAL generation #1", "#2", ...) so EXACT experiment output does not
// depend on SQLite's random salts.
type GenerationNames struct {
	seen map[Generation]int
}

// Name returns the stable label for gen, assigning the next number on first sight.
func (g *GenerationNames) Name(gen Generation) string {
	if gen.None() {
		return "no WAL"
	}
	if g.seen == nil {
		g.seen = map[Generation]int{}
	}
	if _, ok := g.seen[gen]; !ok {
		g.seen[gen] = len(g.seen) + 1
	}
	return fmt.Sprintf("WAL generation #%d", g.seen[gen])
}

// Open starts a Shipper on config.Database against config.Store. If the store
// is empty, Open takes an initial snapshot at config.Origin under BEGIN
// IMMEDIATE and establishes its read pin; if the store already holds objects,
// Open resumes by replaying to the newest position (refusing damaged objects
// past that position) and marks continuity false until the first Cycle verifies
// the live WAL.
func Open(ctx context.Context, config Config) (*Shipper, error) {
	if config.Store == nil || config.Database == "" {
		return nil, errors.New("walship: Database and Store are required")
	}
	if config.SnapshotEvery < 0 {
		return nil, errors.New("walship: SnapshotEvery must not be negative")
	}
	if !strings.EqualFold(config.Options.File.JournalMode, sqlitex.JournalWAL) {
		return nil, fmt.Errorf("%w: journal mode %q", ErrNotWAL, config.Options.File.JournalMode)
	}
	// The shipper needs two pooled connections (control + pin), explicit
	// transaction control, and automatic checkpoints disabled so only the
	// shipper decides when frames are backfilled.
	options := config.Options
	options.SingleConnection = false
	options.ImmediateTx = false
	options.ReadOnly = false
	options.Conn.WALAutocheckpoint = 0
	db, err := sqlitex.Open(ctx, config.Database, options)
	if err != nil {
		return nil, err
	}
	s := &Shipper{config: config, db: db}
	if err := s.start(ctx); err != nil {
		return nil, errors.Join(err, s.closeConnections())
	}
	return s, nil
}

func (s *Shipper) start(ctx context.Context) error {
	var err error
	if s.control, err = s.db.Conn(ctx); err != nil {
		return err
	}
	if s.pin, err = s.db.Conn(ctx); err != nil {
		return err
	}
	if err := s.control.QueryRowContext(ctx, "PRAGMA page_size").Scan(&s.pageSize); err != nil {
		return err
	}
	catalog, err := LoadCatalog(ctx, s.config.Store)
	if err != nil {
		return err
	}
	if _, found := catalog.LastPosition(); found {
		if err := s.resume(catalog); err != nil {
			return err
		}
	} else {
		return s.withWriteLock(ctx, func() error {
			data, file, err := s.readWAL()
			if err != nil {
				return err
			}
			meta, err := s.snapshot(ctx, ReasonInitial, s.config.Origin, data, file)
			if err != nil {
				return err
			}
			s.openSnapshot = meta
			if err := s.takePin(ctx); err != nil {
				return err
			}
			s.continuous = true
			return nil
		})
	}
	return s.takePin(ctx)
}

func (s *Shipper) resume(catalog *Catalog) error {
	last, _ := catalog.LastPosition()
	// Refuse to rewind silently over a damaged snapshot or segment past the
	// last usable position, which would otherwise fork a second history.
	for _, d := range catalog.Damaged {
		if _, position, err := ParseKey(d.Key); err == nil && position > last {
			return fmt.Errorf("walship: resume: %w: the store holds a damaged object %s past the last usable position %d: %s", ErrBrokenChain, d.Key, last, d.Problem)
		}
	}
	plan, err := catalog.Plan(last)
	if err != nil {
		return fmt.Errorf("walship: resume: %w", err)
	}
	image, report, err := plan.Execute()
	if err != nil {
		return fmt.Errorf("walship: resume: %w", err)
	}
	s.image, s.state, s.position = image, report.StateSHA256, last
	s.wal = plan.Snapshot.Meta.WAL
	if n := len(plan.Segments); n > 0 {
		s.wal = plan.Segments[n-1].Meta.End()
	}
	s.sinceSnapshot = len(plan.Segments)
	s.resumed = true
	return nil
}

// Resumed reports whether Open resumed from existing objects in the store
// rather than taking a fresh initial snapshot.
func (s *Shipper) Resumed() bool { return s.resumed }

// OpenSnapshot returns the initial SnapshotMeta taken during Open, or nil if
// the shipper resumed from an existing store.
func (s *Shipper) OpenSnapshot() *SnapshotMeta { return s.openSnapshot }

// Position returns the shipper's latest shipped stream position.
func (s *Shipper) Position() int64 { return s.position }

// StateSHA256 returns the SHA-256 digest of the database image at Position.
func (s *Shipper) StateSHA256() string { return s.state }

// WALPosition returns the WAL generation, frame, and checksum at Position.
func (s *Shipper) WALPosition() WALPosition { return s.wal }

func (s *Shipper) withWriteLock(ctx context.Context, work func() error) error {
	if _, err := s.control.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("walship: take the write lock: %w", err)
	}
	if err := work(); err != nil {
		_, rollbackErr := s.control.ExecContext(ctx, "ROLLBACK")
		return errors.Join(err, rollbackErr)
	}
	_, err := s.control.ExecContext(ctx, "COMMIT")
	return err
}

// takePin starts a real read transaction on s.pin so SQLite acquires a WAL read
// mark (walTryBeginRead, src/wal.c:3020-3272): slot >= 1 caps every checkpoint
// at mxSafeFrame (src/wal.c:2233-2251), and slot 0 (when nBackfill == mxFrame)
// blocks any backfill because backfilling needs an exclusive lock on slot 0
// (src/wal.c:2259-2260).
func (s *Shipper) takePin(ctx context.Context) error {
	if _, err := s.pin.ExecContext(ctx, "BEGIN"); err != nil {
		return err
	}
	var tables int
	if err := s.pin.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_schema").Scan(&tables); err != nil {
		_, rollbackErr := s.pin.ExecContext(ctx, "ROLLBACK")
		return errors.Join(err, rollbackErr)
	}
	s.pinned = true
	return nil
}

func (s *Shipper) dropPin(ctx context.Context) error {
	if !s.pinned {
		return nil
	}
	s.pinned = false
	_, err := s.pin.ExecContext(ctx, "COMMIT")
	return err
}

// readWAL reads and parses the live -wal file directly. This is POSIX-lock-safe
// inside the owning process because SQLite places advisory locks on the .db and
// -shm files, never on the -wal file.
func (s *Shipper) readWAL() ([]byte, *walfile.File, error) {
	data, err := os.ReadFile(s.config.Database + "-wal")
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	file, err := walfile.Parse(data)
	if errors.Is(err, walfile.ErrWALIgnored) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	if file.Size > 0 && file.PageSize() != s.pageSize {
		return nil, nil, fmt.Errorf("WAL page size %d, database page size %d", file.PageSize(), s.pageSize)
	}
	return data, file, nil
}

func generationOf(data []byte, file *walfile.File) Generation {
	if file == nil || file.Size == 0 {
		return Generation{}
	}
	return GenerationOf(data[:walfile.HeaderSize])
}

func chainAt(file *walfile.File, frame int) [2]uint32 {
	if file == nil || file.Size == 0 {
		return [2]uint32{}
	}
	if frame == 0 {
		return file.Header.Checksum
	}
	return file.Frames[frame-1].StoredChecksum
}

func lastCommit(file *walfile.File) int {
	if file == nil {
		return 0
	}
	return file.LastCommitFrame
}

// RowsQuerier is the query interface required by ReadPages (implemented by
// *sql.DB, *sql.Conn, *sql.Tx, and *sqlitex.DB).
type RowsQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// ReadPages reads every page of schema "main" in 1..N order through the
// sqlite_dbpage virtual table, refusing gaps, wrong page sizes, or empty
// results. Called inside a read or write transaction, it yields a consistent
// E1 snapshot image.
func ReadPages(ctx context.Context, q RowsQuerier, pageSize int) ([]byte, error) {
	rows, err := q.QueryContext(ctx, "SELECT pgno, data FROM sqlite_dbpage('main') ORDER BY pgno")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var image []byte
	expected := int64(1)
	for rows.Next() {
		var pgno int64
		var data []byte
		if err := rows.Scan(&pgno, &data); err != nil {
			return nil, err
		}
		if pgno != expected || len(data) != pageSize {
			return nil, fmt.Errorf("sqlite_dbpage returned page %d with %d bytes, want page %d with %d", pgno, len(data), expected, pageSize)
		}
		image = append(image, data...)
		expected++
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(image) == 0 {
		return nil, errors.New("sqlite_dbpage returned no pages")
	}
	return image, nil
}

func (s *Shipper) readPages(ctx context.Context) ([]byte, error) {
	return ReadPages(ctx, s.control, s.pageSize)
}

func (s *Shipper) snapshot(ctx context.Context, reason string, position int64, data []byte, file *walfile.File) (*SnapshotMeta, error) {
	image, err := s.readPages(ctx)
	if err != nil {
		return nil, err
	}
	digest := SHA256Hex(image)
	frame := lastCommit(file)
	// Every periodic snapshot cross-checks SQLite's live pages against the
	// shipper's own replayed mirror image; any divergence stops shipping.
	if reason == ReasonPeriodic {
		if digest != s.state {
			return nil, fmt.Errorf("%w: SQLite's pages hash to %s, the replayed state to %s", ErrMirrorDiverged, short(digest), short(s.state))
		}
	}
	meta := SnapshotMeta{
		Position: position, Reason: reason, PageSize: s.pageSize, Pages: uint32(len(image) / s.pageSize),
		UserVersion: userVersion(image), StateSHA256: digest,
		WAL: WALPosition{Generation: generationOf(data, file), Frame: frame, Checksum: chainAt(file, frame)},
	}
	object, err := EncodeSnapshot(meta, image)
	if err != nil {
		return nil, err
	}
	if err := s.config.Store.PutIfAbsent(ctx, SnapshotKey(position), object); err != nil {
		return nil, err
	}
	s.image, s.state, s.position, s.wal = image, digest, position, meta.WAL
	s.sinceSnapshot = 0
	return &meta, nil
}

// Cycle runs one atomic shipping step under BEGIN IMMEDIATE on the control
// connection: read and classify -wal, publish either a discontinuity snapshot
// or one segment covering all newly committed frames, drop the pin to run a
// shipper-owned PASSIVE checkpoint, re-acquire the pin, and take a periodic
// snapshot if SnapshotEvery segments have been shipped since the last one.
// If any error occurs, the shipper marks itself broken until reopened.
func (s *Shipper) Cycle(ctx context.Context) (CycleReport, error) {
	var report CycleReport
	if s.broken != nil {
		return report, fmt.Errorf("walship: an earlier cycle failed; reopen the shipper to resume from the store: %w", s.broken)
	}
	err := s.withWriteLock(ctx, func() error {
		data, file, err := s.readWAL()
		if err != nil {
			return err
		}
		start, change, reason, err := s.classify(ctx, data, file)
		if err != nil {
			return err
		}
		report.Change, report.Reason = change, reason
		if change == WALDiscontinuity {
			meta, err := s.snapshot(ctx, ReasonDiscontinuity, s.position+1, data, file)
			if err != nil {
				return err
			}
			report.Snapshot = meta
		} else {
			s.wal = start
			if lastCommit(file) > s.wal.Frame {
				meta, err := s.ship(ctx, data, file)
				if err != nil {
					return err
				}
				report.Segment = meta
			}
		}
		if err := s.dropPin(ctx); err != nil {
			return err
		}
		result, err := sqlitex.Checkpoint(ctx, s.pin, "", sqlitex.CheckpointPassive)
		if err != nil {
			return err
		}
		report.Checkpoint = result
		s.fullyBackfilled = !result.Busy && result.LogFrames == result.Checkpointed && result.LogFrames == s.wal.Frame
		report.FullBackfill = s.fullyBackfilled
		if err := s.takePin(ctx); err != nil {
			return err
		}
		if report.Snapshot == nil && s.config.SnapshotEvery > 0 && s.sinceSnapshot >= s.config.SnapshotEvery {
			meta, err := s.snapshot(ctx, ReasonPeriodic, s.position, data, file)
			if err != nil {
				return err
			}
			report.Snapshot = meta
		}
		return nil
	})
	if err != nil {
		s.broken = err
		return report, err
	}
	s.continuous = true
	return report, nil
}

// classify decides whether the live -wal continues s.wal. Because SQLite's
// nCkpt and salt-1 are per-connection (src/wal.c:2152-2166, 4095-4096) and
// cannot prove how many restarts occurred, a header change is accepted as
// WALNewGeneration only when the shipper held its pin continuously and its last
// checkpoint backfilled every shipped frame; otherwise it forces a snapshot.
func (s *Shipper) classify(ctx context.Context, data []byte, file *walfile.File) (WALPosition, WALChange, string, error) {
	current := generationOf(data, file)
	if current == s.wal.Generation {
		// When both the last shipped position and the live database have no WAL
		// and the shipper was closed in between, offline commits could have
		// been checkpointed and the -wal deleted; verify the full page hash.
		if current.None() && !s.continuous {
			image, err := s.readPages(ctx)
			if err != nil {
				return s.wal, WALUnchanged, "", err
			}
			if SHA256Hex(image) != s.state {
				return s.wal, WALDiscontinuity, "there is no WAL and the database pages differ from the last shipped state", nil
			}
		}
		if lastCommit(file) < s.wal.Frame {
			return s.wal, WALDiscontinuity, fmt.Sprintf("the WAL holds %d committed frames, %d were already shipped", lastCommit(file), s.wal.Frame), nil
		}
		if s.wal.Frame > 0 && chainAt(file, s.wal.Frame) != s.wal.Checksum {
			return s.wal, WALDiscontinuity, fmt.Sprintf("frame %d no longer carries the shipped checksum", s.wal.Frame), nil
		}
		return s.wal, WALUnchanged, "", nil
	}
	if !s.continuous {
		return s.wal, WALDiscontinuity, "the WAL header changed while the shipper held no read mark", nil
	}
	if !s.wal.Generation.None() && !s.fullyBackfilled {
		return s.wal, WALDiscontinuity, "the WAL header changed although the shipper's last checkpoint did not copy every frame", nil
	}
	return WALPosition{Generation: current, Frame: 0, Checksum: chainAt(file, 0)}, WALNewGeneration, "", nil
}

func (s *Shipper) ship(ctx context.Context, data []byte, file *walfile.File) (*SegmentMeta, error) {
	first, last := s.wal.Frame+1, file.LastCommitFrame
	frameSize := file.FrameSize()
	start := file.Frames[first-1].Offset
	body := append([]byte(nil), data[start:file.Frames[last-1].Offset+int64(frameSize)]...)
	meta := SegmentMeta{
		FirstPosition: s.position + 1, Generation: s.wal.Generation, FirstFrame: first, LastFrame: last,
		SeedChecksum: s.wal.Checksum, LastChecksum: chainAt(file, last), PageSize: s.pageSize, BeforeSHA256: s.state,
	}
	// Enumerate every commit frame first so VerifySegmentFrames inside
	// ApplySegment can cross-check the complete commit list before replay.
	for _, frame := range file.Frames[first-1 : last] {
		if frame.IsCommit() {
			meta.Commits = append(meta.Commits, CommitPoint{
				Position: s.position + int64(len(meta.Commits)) + 1, Frame: frame.Number, Pages: frame.CommitSize,
			})
		}
	}
	image := s.image
	for i := range meta.Commits {
		out, _, err := ApplySegment(s.image, meta, body, meta.Commits[i].Frame)
		if err != nil {
			return nil, err
		}
		image = out
		meta.Commits[i].UserVersion, meta.Commits[i].StateSHA256 = userVersion(out), SHA256Hex(out)
	}
	meta.LastPosition = meta.Commits[len(meta.Commits)-1].Position
	meta.AfterSHA256 = meta.Commits[len(meta.Commits)-1].StateSHA256
	object, err := EncodeSegment(meta, body)
	if err != nil {
		return nil, err
	}
	if err := s.config.Store.PutIfAbsent(ctx, SegmentKey(meta.FirstPosition), object); err != nil {
		return nil, err
	}
	s.image, s.state, s.position, s.wal = image, meta.AfterSHA256, meta.LastPosition, meta.End()
	s.sinceSnapshot++
	return &meta, nil
}

// Close releases the held read pin and closes the shipper's SQLite connections.
func (s *Shipper) Close() error {
	var pinErr error
	if s.pinned {
		pinErr = s.dropPin(context.Background())
	}
	return errors.Join(pinErr, s.closeConnections())
}

func (s *Shipper) closeConnections() error {
	var errs []error
	if s.pin != nil {
		errs = append(errs, s.pin.Close())
	}
	if s.control != nil {
		errs = append(errs, s.control.Close())
	}
	errs = append(errs, s.db.Close())
	return errors.Join(errs...)
}
