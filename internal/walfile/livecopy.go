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

const LiveCopyChildMode = "walfile-live-copy-child"

type CopyPair struct {
	From string
	To   string
}

type CopyResult struct {
	From    string
	To      string
	Present bool
	Bytes   int64
}

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

func CopyLiveFiles(ctx context.Context, pairs []CopyPair) ([]CopyResult, error) {
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

type LiveState struct {
	WALPresent   bool
	WALSize      int64
	WAL          *File
	WALProblem   string
	IndexPresent bool
	Index        *Index
}

func ReadLiveState(ctx context.Context, database, scratchDirectory string) (LiveState, error) {
	walCopy := filepath.Join(scratchDirectory, "live-state-copy-wal")
	indexCopy := filepath.Join(scratchDirectory, "live-state-copy-shm")
	results, err := CopyLiveFiles(ctx, []CopyPair{{database + "-wal", walCopy}, {database + "-shm", indexCopy}})
	if err != nil {
		return LiveState{}, err
	}
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
