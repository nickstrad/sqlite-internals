package walship

import (
	"context"
	"fmt"
)

// AdoptAtRest resumes a closed, fully checkpointed database without uploading a
// discontinuity snapshot. When the last connection closes, sqlite3WalClose
// (src/wal.c:2507-2571) checkpoints and deletes the -wal file; on reopen the
// shipper's saved WALPosition still names the deleted WAL generation, so a
// plain Cycle would see a WAL header mismatch with no held read mark and upload
// a full snapshot.
//
// Under BEGIN IMMEDIATE on the control connection, AdoptAtRest checks that the
// -wal holds no committed frames and that every page read through sqlite_dbpage
// hashes to the shipper's last shipped state. When both hold, it clears the
// saved WALPosition and marks the shipper continuous and fully backfilled: the
// pin taken in Open sits on read slot 0 while no WAL exists, which blocks any
// backfill (src/wal.c:2259-2260), and the next commit starts a fresh WAL
// generation at frame 1 that Plan accepts.
func (s *Shipper) AdoptAtRest(ctx context.Context) (bool, error) {
	if s.broken != nil {
		return false, fmt.Errorf("walship: an earlier cycle failed; reopen the shipper to resume from the store: %w", s.broken)
	}
	adopted := false
	err := s.withWriteLock(ctx, func() error {
		_, file, err := s.readWAL()
		// Any committed frame in -wal means the database is not at rest; leave
		// the shipper unchanged so the next Cycle handles it normally.
		if err != nil || lastCommit(file) > 0 {
			return err
		}
		image, err := s.readPages(ctx)
		if err != nil {
			return err
		}
		// Refuse to adopt if the file bytes diverge from the last shipped state
		// (for example, an offline write while closed).
		if SHA256Hex(image) != s.state {
			return nil
		}
		s.wal = WALPosition{}
		s.continuous, s.fullyBackfilled, adopted = true, true, true
		return nil
	})
	if err != nil {
		s.broken = err
	}
	return adopted, err
}
