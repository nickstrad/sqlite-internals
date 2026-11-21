package walship

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestDirStoreSemantics(t *testing.T) {
	ctx := context.Background()
	store, err := NewDirStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PutIfAbsent(ctx, "segments/000000000001", []byte("one")); err != nil {
		t.Fatal(err)
	}
	if err := store.PutIfAbsent(ctx, "segments/000000000001", []byte("two")); !errors.Is(err, ErrExists) {
		t.Fatalf("second put: %v", err)
	}
	data, err := store.Get(ctx, "segments/000000000001")
	if err != nil || string(data) != "one" {
		t.Fatalf("get %q %v", data, err)
	}
	if _, err := store.Get(ctx, "segments/000000000002"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing get: %v", err)
	}
	if err := store.PutIfAbsent(ctx, "snapshots/000000000000", nil); err != nil {
		t.Fatal(err)
	}
	keys, err := store.List(ctx, SegmentPrefix)
	if err != nil || !reflect.DeepEqual(keys, []string{"segments/000000000001"}) {
		t.Fatalf("list %v %v", keys, err)
	}
	all, err := store.List(ctx, "")
	if err != nil || len(all) != 2 {
		t.Fatalf("list all %v %v", all, err)
	}
	temps, err := os.ReadDir(filepath.Join(store.Root, dirStoreTemp))
	if err != nil || len(temps) != 0 {
		t.Fatalf("temporary files left: %v %v", temps, err)
	}
	if err := store.Delete(ctx, "segments/000000000001"); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(ctx, "segments/000000000001"); err != nil {
		t.Fatalf("deleting a missing key: %v", err)
	}
	if err := store.PutIfAbsent(ctx, "segments/000000000001", []byte("three")); err != nil {
		t.Fatalf("put after delete: %v", err)
	}
	for _, bad := range []string{"", "/x", "x/", "../x", "a//b", ".tmp/x", "A", "x/.y", "x y"} {
		if err := store.PutIfAbsent(ctx, bad, nil); !errors.Is(err, ErrInvalidKey) {
			t.Fatalf("key %q: %v", bad, err)
		}
		if _, err := store.Get(ctx, bad); !errors.Is(err, ErrInvalidKey) {
			t.Fatalf("get %q: %v", bad, err)
		}
	}
	if _, err := store.List(ctx, "../"); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("list ../: %v", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := store.PutIfAbsent(cancelled, "segments/000000000009", nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled put: %v", err)
	}
}

func TestParseKey(t *testing.T) {
	kind, position, err := ParseKey(SegmentKey(42))
	if err != nil || kind != "segment" || position != 42 {
		t.Fatalf("%s %d %v", kind, position, err)
	}
	kind, position, err = ParseKey(SnapshotKey(7))
	if err != nil || kind != "snapshot" || position != 7 {
		t.Fatalf("%s %d %v", kind, position, err)
	}
	for _, bad := range []string{"segments/42", "segments/00000000004x", "other/000000000001", "snapshots/-00000000001", "segments/0000000000001"} {
		if _, _, err := ParseKey(bad); !errors.Is(err, ErrMalformedObject) {
			t.Fatalf("%q: %v", bad, err)
		}
	}
}

func TestObjectEncodingIsStrict(t *testing.T) {
	image := bytes.Repeat([]byte{7}, 1024)
	meta := SnapshotMeta{Position: 3, Reason: ReasonInitial, PageSize: 512, Pages: 2, StateSHA256: SHA256Hex(image)}
	object, err := EncodeSnapshot(meta, image)
	if err != nil {
		t.Fatal(err)
	}
	decoded, body, err := DecodeSnapshot(object)
	if err != nil || decoded.Position != 3 || !bytes.Equal(body, image) {
		t.Fatalf("round trip %+v %v", decoded, err)
	}
	corrupt := func(change func([]byte) []byte) error {
		_, _, err := DecodeSnapshot(change(append([]byte(nil), object...)))
		return err
	}
	cases := map[string]func([]byte) []byte{
		"body byte":  func(b []byte) []byte { b[len(b)-1] ^= 1; return b },
		"short body": func(b []byte) []byte { return b[:len(b)-1] },
		"magic":      func(b []byte) []byte { b[0] = 'W'; return b },
		"no newline": func(b []byte) []byte { return []byte(snapshotMagic) },
		"unknown field": func(b []byte) []byte {
			return bytes.Replace(b, []byte(`{"position"`), []byte(`{"extra":1,"position"`), 1)
		},
		"two values": func(b []byte) []byte { return bytes.Replace(b, []byte("}\n"), []byte("} {}\n"), 1) },
		"empty":      func(b []byte) []byte { return nil },
	}
	for name, change := range cases {
		if err := corrupt(change); !errors.Is(err, ErrMalformedObject) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	wrongState := meta
	wrongState.StateSHA256 = SHA256Hex([]byte("other"))
	object, err = EncodeSnapshot(wrongState, image)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := DecodeSnapshot(object); !errors.Is(err, ErrMalformedObject) {
		t.Fatalf("state hash: %v", err)
	}
	wrongPages := meta
	wrongPages.Pages = 3
	object, err = EncodeSnapshot(wrongPages, image)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := DecodeSnapshot(object); !errors.Is(err, ErrMalformedObject) {
		t.Fatalf("pages: %v", err)
	}
}
