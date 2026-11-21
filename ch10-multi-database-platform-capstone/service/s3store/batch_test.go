package s3store

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/nickstrad/sqlite-internals/internal/rangefs"
)

// TestWipeInBatchesDeletesMoreThanOneBatch puts 1,003 objects into live SeaweedFS and verifies WipeInBatches deletes across two S3 DeleteObjects requests.
func TestWipeInBatchesDeletesMoreThanOneBatch(t *testing.T) {
	ctx := context.Background()
	s, err := Connect(ctx, rangefs.S3ConfigFromEnv(), "course-ch10b", "test-wipe-batches")
	if errors.Is(err, ErrUnreachable) {
		t.Skipf("S3 store not running: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.WipeInBatches(ctx); err != nil {
		t.Fatal(err)
	}
	for i := range deleteBatch + 3 {
		if err := s.PutIfAbsent(ctx, fmt.Sprintf("k/%05d", i), []byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	deleted, err := s.WipeInBatches(ctx)
	if err != nil || deleted != deleteBatch+3 {
		t.Fatalf("deleted %d: %v", deleted, err)
	}
	keys, err := s.List(ctx, "")
	if err != nil || len(keys) != 0 {
		t.Fatalf("%v %v", keys, err)
	}
}
