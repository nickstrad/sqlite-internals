package walship

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
	"github.com/nickstrad/sqlite-internals/internal/walfile"
)

var (
	ErrMirrorDiverged = errors.New("the shipper's replayed state differs from SQLite's pages")
	ErrNotWAL         = errors.New("walship needs a database in WAL mode")
)

type Config struct {
	Database      string
	Store         Store
	Options       sqlitex.Options
	SnapshotEvery int
	Origin        int64
}

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

type CycleReport struct {
	WALChange    string
	Segment      *SegmentMeta
	Snapshot     *SnapshotMeta
	Checkpoint   sqlitex.CheckpointResult
	FullBackfill bool
}

func (r CycleReport) Lines() []string {
	var lines []string
	if r.WALChange != "" {
		lines = append(lines, "wal: "+r.WALChange)
	}
	if r.Segment != nil {
		lines = append(lines, DescribeSegment(*r.Segment))
	} else {
		lines = append(lines, "segment: none (no new commit)")
	}
	lines = append(lines, fmt.Sprintf("checkpoint PASSIVE: %s; every shipped frame copied: %t", r.Checkpoint, r.FullBackfill))
	if r.Snapshot != nil {
		lines = append(lines, DescribeSnapshot(*r.Snapshot))
	}
	return lines
}

func DescribeSegment(m SegmentMeta) string {
	return fmt.Sprintf("segment %s: positions %d-%d, frames %d-%d of WAL %s, user_version %d-%d, before %s after %s",
		SegmentKey(m.FirstPosition), m.FirstPosition, m.LastPosition, m.FirstFrame, m.LastFrame, m.Generation,
		m.Commits[0].UserVersion, m.Commits[len(m.Commits)-1].UserVersion, short(m.BeforeSHA256), short(m.AfterSHA256))
}

func DescribeSnapshot(m SnapshotMeta) string {
	return fmt.Sprintf("snapshot %s (%s): position %d, %d pages, user_version %d, state %s, WAL frame %d of %s",
		SnapshotKey(m.Position), m.Reason, m.Position, m.Pages, m.UserVersion, short(m.StateSHA256), m.WAL.Frame, m.WAL.Generation)
}

func Open(ctx context.Context, config Config) (*Shipper, error) {
	if config.Store == nil || config.Database == "" {
		return nil, errors.New("walship: Database and Store are required")
	}
	if config.SnapshotEvery < 0 {
		return nil, errors.New("walship: SnapshotEvery must not be negative")
	}
	if config.Options.File.JournalMode != sqlitex.JournalWAL {
		return nil, fmt.Errorf("%w: journal mode %q", ErrNotWAL, config.Options.File.JournalMode)
	}
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

func (s *Shipper) Resumed() bool { return s.resumed }

func (s *Shipper) OpenSnapshot() *SnapshotMeta { return s.openSnapshot }

func (s *Shipper) Position() int64 { return s.position }

func (s *Shipper) StateSHA256() string { return s.state }

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

func (s *Shipper) readPages(ctx context.Context) ([]byte, error) {
	rows, err := s.control.QueryContext(ctx, "SELECT pgno, data FROM sqlite_dbpage('main') ORDER BY pgno")
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
		if pgno != expected || len(data) != s.pageSize {
			return nil, fmt.Errorf("sqlite_dbpage returned page %d with %d bytes, want page %d with %d", pgno, len(data), expected, s.pageSize)
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

func (s *Shipper) snapshot(ctx context.Context, reason string, position int64, data []byte, file *walfile.File) (*SnapshotMeta, error) {
	image, err := s.readPages(ctx)
	if err != nil {
		return nil, err
	}
	digest := SHA256Hex(image)
	frame := lastCommit(file)
	if reason == ReasonPeriodic {
		if digest != s.state {
			return nil, fmt.Errorf("%w: SQLite's pages hash to %s, the replayed state to %s", ErrMirrorDiverged, short(digest), short(s.state))
		}
		if frame != s.wal.Frame || generationOf(data, file) != s.wal.Generation {
			return nil, fmt.Errorf("%w: the WAL moved under the write lock", ErrMirrorDiverged)
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
		start, change, discontinuity := s.classify(data, file)
		report.WALChange = change
		if discontinuity {
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

func (s *Shipper) classify(data []byte, file *walfile.File) (WALPosition, string, bool) {
	current := generationOf(data, file)
	if current == s.wal.Generation {
		if lastCommit(file) < s.wal.Frame {
			return s.wal, fmt.Sprintf("discontinuity: the WAL holds %d committed frames, %d were already shipped", lastCommit(file), s.wal.Frame), true
		}
		if s.wal.Frame > 0 && chainAt(file, s.wal.Frame) != s.wal.Checksum {
			return s.wal, fmt.Sprintf("discontinuity: frame %d no longer carries the shipped checksum", s.wal.Frame), true
		}
		return s.wal, "", false
	}
	if !s.continuous {
		return s.wal, fmt.Sprintf("discontinuity: WAL header changed from %s to %s while the shipper held no read mark", s.wal.Generation, current), true
	}
	if !s.wal.Generation.None() && !s.fullyBackfilled {
		return s.wal, fmt.Sprintf("discontinuity: WAL header changed from %s to %s without a full checkpoint by the shipper", s.wal.Generation, current), true
	}
	start := WALPosition{Generation: current, Frame: 0, Checksum: chainAt(file, 0)}
	return start, fmt.Sprintf("new WAL generation %s after the shipper's full checkpoint", current), false
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
	image := s.image
	for _, frame := range file.Frames[first-1 : last] {
		if !frame.IsCommit() {
			continue
		}
		out, _, err := ApplySegment(s.image, meta, body, frame.Number)
		if err != nil {
			return nil, err
		}
		image = out
		meta.Commits = append(meta.Commits, CommitPoint{
			Position: s.position + int64(len(meta.Commits)) + 1, Frame: frame.Number, Pages: frame.CommitSize,
			UserVersion: userVersion(out), StateSHA256: SHA256Hex(out),
		})
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
