// Package walship implements Rebuild Ladder rung R6: continuous point-in-time
// capture and restore for a live WAL-mode SQLite database. A Shipper captures
// a base snapshot through sqlite_dbpage, holds a read mark so unshipped WAL
// frames cannot be overwritten, copies newly committed frames from the -wal
// file into numbered hash-chained segments in an object Store, and performs
// shipper-owned PASSIVE checkpoints only after shipping. A Catalog and Plan
// verify every object, WAL checksum chain, and state hash before replaying
// frames through internal/walfile to rebuild any commit position byte for byte.
package walship

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Sentinel errors returned by Store implementations.
var (
	// ErrExists reports that PutIfAbsent refused to overwrite an existing key,
	// or that WriteDatabase refused to write over an existing file or sidecar.
	ErrExists = errors.New("object already exists")
	// ErrNotFound reports that Get requested a key absent from the store.
	ErrNotFound = errors.New("object not found")
	// ErrInvalidKey reports that a key or prefix violates ValidateKey's syntax.
	ErrInvalidKey = errors.New("invalid object key")
)

// Store is the minimal object-store contract that walship requires and that
// Chapter 10 implements against S3-compatible storage (SeaweedFS).
//
// Implementations must guarantee:
//   - PutIfAbsent is atomic and exclusive: readers observe either the whole
//     object or none of it, and an existing key fails with ErrExists unchanged.
//   - Read-after-write and list-after-write visibility once PutIfAbsent returns nil.
//   - Delete is idempotent (deleting an absent key returns nil).
//
// List may return keys in any order; walship sorts keys itself.
type Store interface {
	PutIfAbsent(ctx context.Context, key string, data []byte) error
	Get(ctx context.Context, key string) ([]byte, error)
	List(ctx context.Context, prefix string) ([]string, error)
	Delete(ctx context.Context, key string) error
}

// keyPattern restricts keys to lowercase ASCII slash-separated segments with
// no leading/trailing slashes, empty segments, or dot-prefixed entries (preventing
// path traversal and hiding DirStore's .tmp staging directory).
var keyPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*(/[a-z0-9][a-z0-9_-]*)*$`)

// ValidateKey checks that key is a safe, normalized object-store key.
func ValidateKey(key string) error {
	if !keyPattern.MatchString(key) {
		return fmt.Errorf("%w: %q", ErrInvalidKey, key)
	}
	return nil
}

func validatePrefix(prefix string) error {
	if prefix == "" {
		return nil
	}
	return ValidateKey(strings.TrimSuffix(prefix, "/"))
}

const dirStoreTemp = ".tmp"

// DirStore implements Store over a local filesystem directory using fsynced
// temporary files and hard links (os.Link) for atomic PutIfAbsent.
type DirStore struct {
	Root string
}

// NewDirStore initializes a DirStore rooted at root, creating the root and its
// hidden .tmp staging directory if needed.
func NewDirStore(root string) (*DirStore, error) {
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(absolute, dirStoreTemp), 0o755); err != nil {
		return nil, err
	}
	return &DirStore{Root: absolute}, nil
}

func (s *DirStore) path(key string) string {
	return filepath.Join(s.Root, filepath.FromSlash(key))
}

// PutIfAbsent writes data to an fsynced temp file under .tmp, links it
// atomically to key's path (returning ErrExists if already present), unlinks
// the temp entry, and fsyncs the target directory.
func (s *DirStore) PutIfAbsent(ctx context.Context, key string, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := ValidateKey(key); err != nil {
		return err
	}
	final := s.path(key)
	if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil {
		return err
	}
	temp, err := s.writeTemp(data)
	if err != nil {
		return err
	}
	defer os.Remove(temp)
	// os.Link fails atomically with EEXIST if final already exists, unlike
	// os.Rename which silently overwrites an existing target on Unix.
	if err := os.Link(temp, final); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("%w: %s", ErrExists, key)
		}
		return err
	}
	return syncDir(filepath.Dir(final))
}

func (s *DirStore) writeTemp(data []byte) (string, error) {
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", err
	}
	name := filepath.Join(s.Root, dirStoreTemp, "put-"+hex.EncodeToString(suffix[:]))
	file, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return "", err
	}
	_, writeErr := file.Write(data)
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		os.Remove(name)
		return "", err
	}
	return name, nil
}

func syncDir(dir string) error {
	handle, err := os.Open(dir)
	if err != nil {
		return err
	}
	syncErr := handle.Sync()
	closeErr := handle.Close()
	return errors.Join(syncErr, closeErr)
}

// Get reads and returns the complete bytes stored under key, or ErrNotFound.
func (s *DirStore) Get(ctx context.Context, key string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := ValidateKey(key); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(s.path(key))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, key)
	}
	return data, err
}

// List returns all valid keys starting with prefix in sorted lexical order,
// skipping dot-prefixed directories such as .tmp.
func (s *DirStore) List(ctx context.Context, prefix string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validatePrefix(prefix); err != nil {
		return nil, err
	}
	var keys []string
	err := filepath.WalkDir(s.Root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if path != s.Root && strings.HasPrefix(entry.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		relative, err := filepath.Rel(s.Root, path)
		if err != nil {
			return err
		}
		key := filepath.ToSlash(relative)
		if strings.HasPrefix(key, prefix) && ValidateKey(key) == nil {
			keys = append(keys, key)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(keys)
	return keys, nil
}

// Delete removes key from the store and fsyncs its parent directory. Deleting
// a key that does not exist returns nil.
func (s *DirStore) Delete(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := ValidateKey(key); err != nil {
		return err
	}
	err := os.Remove(s.path(key))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return syncDir(filepath.Dir(s.path(key)))
}
