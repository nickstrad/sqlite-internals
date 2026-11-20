package walfile

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// LiveCopyChildMode is the first CLI argument used when CopyLiveFiles re-execs
// the current binary as a child process to copy open SQLite files safely.
const LiveCopyChildMode = "walfile-live-copy-child"

// CopyPair names one source path and destination path for CopyLiveFiles.
type CopyPair struct {
	From string
	To   string
}

// CopyResult reports whether a CopyPair's source existed and how many bytes
// were copied.
type CopyResult struct {
	From    string
	To      string
	Present bool
	Bytes   int64
}

// ServeLiveCopyChild handles the child-process file-copy mode and exits the
// process when args[1] == LiveCopyChildMode. Call it at the start of main or
// TestMain in any binary that invokes CopyLiveFiles or ReadLiveState.
//
// Copying in a child process avoids a POSIX lock hazard: closing any file
// descriptor to the database or -shm inside the process that holds an SQLite
// connection releases all POSIX fcntl locks that process holds on that inode
// (src/os_unix.c:1189-1278, 4703-4750).
func ServeLiveCopyChild(args []string) bool {
	if len(args) < 2 || args[1] != LiveCopyChildMode {
		return false
	}
	rest := args[2:]
	if len(rest)%2 != 0 {
		fmt.Fprintln(os.Stderr, "live copy child: arguments must be pairs of source and destination")
		os.Exit(2)
	}
	out := bufio.NewWriter(os.Stdout)
	for i := 0; i < len(rest); i += 2 {
		size, err := copyOne(rest[i], rest[i+1])
		switch {
		case isMissingSource(err):
			// Remove any stale copy at the destination so callers never read
			// leftover bytes from an earlier sample.
			if removeErr := os.Remove(rest[i+1]); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				fmt.Fprintf(os.Stderr, "live copy child: %v\n", removeErr)
				os.Exit(1)
			}
			fmt.Fprintf(out, "missing\n")
		case err != nil:
			fmt.Fprintf(os.Stderr, "live copy child: %v\n", err)
			os.Exit(1)
		default:
			fmt.Fprintf(out, "copied %d\n", size)
		}
	}
	if err := out.Flush(); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
	return true
}

func copyOne(from, to string) (int64, error) {
	source, err := os.Open(from)
	if err != nil {
		return 0, &sourceError{err}
	}
	defer source.Close()
	destination, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return 0, err
	}
	size, err := io.Copy(destination, source)
	if closeErr := destination.Close(); err == nil {
		err = closeErr
	}
	return size, err
}

// CopyLiveFiles spawns the current executable in LiveCopyChildMode to copy each
// source file to its destination without opening those files in the parent
// process that holds live SQLite connections.
func CopyLiveFiles(ctx context.Context, pairs []CopyPair) ([]CopyResult, error) {
	if len(pairs) == 0 {
		return nil, nil
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	args := []string{LiveCopyChildMode}
	for _, p := range pairs {
		args = append(args, p.From, p.To)
	}
	cmd := exec.CommandContext(ctx, executable, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("live copy child: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != len(pairs) {
		return nil, fmt.Errorf("live copy child: %d result lines for %d files", len(lines), len(pairs))
	}
	results := make([]CopyResult, len(pairs))
	for i, line := range lines {
		results[i] = CopyResult{From: pairs[i].From, To: pairs[i].To}
		if line == "missing" {
			continue
		}
		size, ok := strings.CutPrefix(line, "copied ")
		if !ok {
			return nil, fmt.Errorf("live copy child: unexpected line %q", line)
		}
		n, err := strconv.ParseInt(size, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("live copy child: %w", err)
		}
		results[i].Present = true
		results[i].Bytes = n
	}
	return results, nil
}

// DatabaseFileSet returns CopyPairs for a database file and its -wal and -shm
// sidecar files copied into directory under name.
func DatabaseFileSet(database, directory, name string) []CopyPair {
	var pairs []CopyPair
	for _, suffix := range []string{"", "-wal", "-shm"} {
		pairs = append(pairs, CopyPair{From: database + suffix, To: filepath.Join(directory, name+suffix)})
	}
	return pairs
}

type sourceError struct{ err error }

func (e *sourceError) Error() string { return "source: " + e.err.Error() }

func (e *sourceError) Unwrap() error { return e.err }

func isMissingSource(err error) bool {
	var source *sourceError
	return errors.As(err, &source) && errors.Is(err, os.ErrNotExist)
}

// LiveState holds a parsed snapshot of a live database's -wal and -shm files
// sampled through CopyLiveFiles.
type LiveState struct {
	WALPresent   bool
	WALSize      int64
	WAL          *File
	WALProblem   string
	IndexPresent bool
	Index        *Index
}

// ReadLiveState copies database's -wal and -shm into scratchDirectory via a
// child process, parses both, and removes the temporary copies before returning.
func ReadLiveState(ctx context.Context, database, scratchDirectory string) (LiveState, error) {
	walCopy := filepath.Join(scratchDirectory, "live-state-copy-wal")
	indexCopy := filepath.Join(scratchDirectory, "live-state-copy-shm")
	results, err := CopyLiveFiles(ctx, []CopyPair{{database + "-wal", walCopy}, {database + "-shm", indexCopy}})
	if err != nil {
		return LiveState{}, err
	}
	defer os.Remove(walCopy)
	defer os.Remove(indexCopy)
	var state LiveState
	if results[0].Present {
		state.WALPresent = true
		state.WALSize = results[0].Bytes
		wal, err := ParseFile(walCopy)
		if err != nil {
			state.WALProblem = err.Error()
		}
		state.WAL = wal
	}
	if results[1].Present {
		state.IndexPresent = true
		index, err := ParseIndexFile(indexCopy)
		if err != nil {
			return state, fmt.Errorf("wal-index copy: %w", err)
		}
		state.Index = index
	}
	for _, path := range []string{walCopy, indexCopy} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return state, err
		}
	}
	return state, nil
}
