package connmgr

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

type Status struct {
	Path     string
	AtRest   bool
	Problems []string
	SHA256   string
	Size     int64
}

func (s Status) String() string {
	if s.AtRest {
		return fmt.Sprintf("at rest, %d bytes, sha256 %s", s.Size, s.SHA256)
	}
	return "not at rest: " + strings.Join(s.Problems, "; ")
}

func CheckAtRest(path string) (Status, error) {
	status := Status{Path: path}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		status.Problems = append(status.Problems, "database file missing")
		return status, nil
	} else if err != nil {
		return status, err
	}
	wal, err := os.Stat(path + "-wal")
	switch {
	case err == nil && wal.Size() > 0:
		status.Problems = append(status.Problems, fmt.Sprintf("-wal holds %d bytes: the database file alone may lack committed transactions", wal.Size()))
	case err != nil && !errors.Is(err, os.ErrNotExist):
		return status, err
	}
	hot, journalSize, err := hotJournal(path + "-journal")
	if err != nil {
		return status, err
	}
	if hot {
		status.Problems = append(status.Problems, fmt.Sprintf("hot -journal (%d bytes): the next open rolls back an interrupted transaction", journalSize))
	}
	shm, err := os.Stat(path + "-shm")
	switch {
	case err == nil:
		status.Problems = append(status.Problems, fmt.Sprintf("-shm exists (%d bytes): a connection is open or did not close cleanly", shm.Size()))
	case !errors.Is(err, os.ErrNotExist):
		return status, err
	}
	if len(status.Problems) > 0 {
		return status, nil
	}
	sum, size, err := fileSHA256(path)
	if err != nil {
		return status, err
	}
	status.AtRest, status.SHA256, status.Size = true, sum, size
	return status, nil
}

func fileSHA256(path string) (string, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(hash.Sum(nil)), size, nil
}

func hotJournal(path string) (bool, int64, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, 0, nil
	}
	if err != nil {
		return false, 0, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return false, 0, err
	}
	if info.Size() == 0 {
		return false, 0, nil
	}
	first := make([]byte, 1)
	if _, err := file.ReadAt(first, 0); err != nil {
		return false, info.Size(), err
	}
	return first[0] != 0, info.Size(), nil
}
