package walship

import (
	"context"
	"fmt"
)

func (s *Shipper) AdoptAtRest(ctx context.Context) (bool, error) {
	if s.broken != nil {
		return false, fmt.Errorf("walship: an earlier cycle failed; reopen the shipper to resume from the store: %w", s.broken)
	}
	adopted := false
	err := s.withWriteLock(ctx, func() error {
		_, file, err := s.readWAL()
		if err != nil || lastCommit(file) > 0 {
			return err
		}
		image, err := s.readPages(ctx)
		if err != nil {
			return err
		}
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
