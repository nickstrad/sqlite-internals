package equal

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"strings"

	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
)

// ErrNoDBHash is returned by LocateDBHash when the pinned dbhash binary is not
// available on the system.
var ErrNoDBHash = errors.New("dbhash not found")

// LocateDBHash finds the dbhash binary built from the pinned SQLite tree.
func LocateDBHash() (string, error) {
	path, err := sqlitex.LocateTool("dbhash")
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrNoDBHash, err)
	}
	return path, nil
}

// RunDBHash executes the pinned dbhash tool against dbPath and returns its
// 40-character hexadecimal SHA-1 digest.
func RunDBHash(ctx context.Context, tool, dbPath string) (string, error) {
	target := dbPath
	busy, err := HasLiveSideFile(dbPath)
	if err != nil {
		return "", err
	}
	// dbhash opens files read-write by default (tool/dbhash.c:433-438) with
	// SQLITE_OPEN_URI enabled. When a live -wal or -journal exists, pass a
	// mode=ro URI so closing dbhash does not checkpoint and delete the WAL.
	if busy {
		target, err = sqlitex.FileURI(dbPath, url.Values{"mode": {"ro"}})
		if err != nil {
			return "", err
		}
	}
	cmd := exec.CommandContext(ctx, tool, target)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("dbhash %s: %w: %s", dbPath, err, strings.TrimSpace(stderr.String()))
	}
	// dbhash exits 0 even when a database cannot be opened, logging the error
	// to stderr and continuing (tool/dbhash.c:440), so treat any stderr as failure.
	if stderr.Len() > 0 {
		return "", fmt.Errorf("dbhash %s: %s", dbPath, strings.TrimSpace(stderr.String()))
	}
	fields := strings.Fields(stdout.String())
	if len(fields) < 2 || len(fields[0]) != 40 {
		return "", fmt.Errorf("dbhash %s: unexpected output %q", dbPath, stdout.String())
	}
	return fields[0], nil
}
