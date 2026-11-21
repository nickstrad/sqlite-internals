package equal

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
)

var ErrNoDBHash = errors.New("dbhash not found")

func LocateDBHash() (string, error) {
	path, err := sqlitex.LocateTool("dbhash")
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrNoDBHash, err)
	}
	return path, nil
}

func RunDBHash(ctx context.Context, tool, dbPath string) (string, error) {
	cmd := exec.CommandContext(ctx, tool, dbPath)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("dbhash %s: %w: %s", dbPath, err, strings.TrimSpace(stderr.String()))
	}
	if stderr.Len() > 0 {
		return "", fmt.Errorf("dbhash %s: %s", dbPath, strings.TrimSpace(stderr.String()))
	}
	fields := strings.Fields(stdout.String())
	if len(fields) < 2 || len(fields[0]) != 40 {
		return "", fmt.Errorf("dbhash %s: unexpected output %q", dbPath, stdout.String())
	}
	return fields[0], nil
}
