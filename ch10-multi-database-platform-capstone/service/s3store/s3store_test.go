package s3store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/nickstrad/sqlite-internals/internal/rangefs"
	"github.com/nickstrad/sqlite-internals/internal/walship"
)

func TestS3StoreSemantics(t *testing.T) {
	ctx := context.Background()
	s, err := Connect(ctx, rangefs.S3ConfigFromEnv(), "course-ch10a", "test-s3store")
	if errors.Is(err, ErrUnreachable) {
		t.Skipf("S3 store not running: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Wipe(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.PutIfAbsent(ctx, "a/k1", []byte("one")); err != nil {
		t.Fatal(err)
	}
	if err := s.PutIfAbsent(ctx, "a/k1", []byte("two")); !errors.Is(err, walship.ErrExists) {
		t.Fatalf("second put: %v", err)
	}
	if got, err := s.Get(ctx, "a/k1"); err != nil || string(got) != "one" {
		t.Fatalf("%q %v", got, err)
	}
	if _, err := s.Get(ctx, "a/none"); !errors.Is(err, walship.ErrNotFound) {
		t.Fatalf("missing get: %v", err)
	}
	if err := s.PutIfAbsent(ctx, "b/k2", nil); err != nil {
		t.Fatal(err)
	}
	keys, err := s.List(ctx, "a/")
	if err != nil || strings.Join(keys, ",") != "a/k1" {
		t.Fatalf("%v %v", keys, err)
	}
	if err := s.PutIfAbsent(ctx, "../x", nil); !errors.Is(err, walship.ErrInvalidKey) {
		t.Fatalf("invalid key: %v", err)
	}
	if err := s.Delete(ctx, "a/k1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, "a/k1"); err != nil {
		t.Fatalf("second delete: %v", err)
	}
	if n, err := s.Wipe(ctx); n != 1 || err != nil {
		t.Fatalf("wipe %d %v", n, err)
	}
	if _, err := Connect(ctx, rangefs.S3Config{Endpoint: "http://127.0.0.1:1", Region: "us-east-1", AccessKey: "x", SecretKey: "y"}, "course-ch10a", "p"); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("closed port: %v", err)
	}
}
