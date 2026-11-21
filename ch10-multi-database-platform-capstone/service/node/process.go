package node

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/nickstrad/sqlite-internals/ch10-multi-database-platform-capstone/service/bounds"
	"github.com/nickstrad/sqlite-internals/ch10-multi-database-platform-capstone/service/catalog"
	"github.com/nickstrad/sqlite-internals/ch10-multi-database-platform-capstone/service/faultstore"
	"github.com/nickstrad/sqlite-internals/ch10-multi-database-platform-capstone/service/s3store"
	"github.com/nickstrad/sqlite-internals/internal/rangefs"
	"github.com/nickstrad/sqlite-internals/internal/recipe"
	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
	"github.com/nickstrad/sqlite-internals/internal/walship"
)

// Standing-bound constants used by RunProcess for each node child process.
const (
	// ConnectionsPerDatabase is 3: the connmgr writer connection plus walship's
	// control and pin connections.
	ConnectionsPerDatabase = 3
	// ReserveDescriptors covers non-database descriptors (pipes, catalog, S3
	// HTTP sockets, and runtime overhead).
	ReserveDescriptors = 32
)

// Process drives a node child process over its stdin/stdout line protocol.
type Process struct {
	Name  string
	cmd   *exec.Cmd
	stdin io.WriteCloser
	lines *bufio.Scanner
}

// Reply holds the lines returned by a node Process for one command.
type Reply struct {
	Line     string
	Problems []string
	Bounds   string
	Summary  string
}

// WithinBounds reports whether the node's bounds check after the command passed.
func (r Reply) WithinBounds() bool { return strings.HasSuffix(r.Bounds, " ok") }

// StartProcess re-executes the current binary with args, waits for its "ready"
// line, and returns a handle for sending commands.
func StartProcess(ctx context.Context, name string, args ...string) (*Process, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, self, args...)
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p := &Process{Name: name, cmd: cmd, stdin: stdin, lines: bufio.NewScanner(stdout)}
	if !p.lines.Scan() || p.lines.Text() != "ready" {
		return nil, errors.Join(fmt.Errorf("node %s: no ready line, got %q", name, p.lines.Text()), p.lines.Err(), cmd.Process.Kill(), cmd.Wait())
	}
	return p, nil
}

// Send writes one command line to p and reads lines until the trailing "bounds"
// line completes the reply.
func (p *Process) Send(command string) (Reply, error) {
	if _, err := fmt.Fprintln(p.stdin, command); err != nil {
		return Reply{}, err
	}
	var reply Reply
	for p.lines.Scan() {
		line := p.lines.Text()
		switch {
		case strings.HasPrefix(line, "bounds "):
			reply.Bounds = line
			return reply, nil
		case strings.HasPrefix(line, "ok ") || strings.HasPrefix(line, "refused "):
			reply.Line = line
		default:
			reply.Problems = append(reply.Problems, line)
		}
	}
	return reply, fmt.Errorf("node %s: output ended after %q: %v", p.Name, command, p.lines.Err())
}

// Kill terminates the child process with SIGKILL and reaps it.
func (p *Process) Kill() error {
	if err := p.cmd.Process.Kill(); err != nil {
		return err
	}
	if err := p.cmd.Wait(); err == nil {
		return fmt.Errorf("node %s: exited cleanly although it was killed", p.Name)
	}
	return nil
}

// Close sends "close", captures the trailing standing-assertion summary line,
// closes stdin, and waits for the child process to exit cleanly.
func (p *Process) Close() (Reply, error) {
	reply, err := p.Send("close")
	if err != nil {
		return reply, err
	}
	if p.lines.Scan() {
		reply.Summary = p.lines.Text()
	}
	return reply, errors.Join(p.stdin.Close(), p.cmd.Wait())
}

// ProcessConfig carries the command-line parameters for a node child process.
type ProcessConfig struct {
	Name     string
	Work     string
	Fault    string
	Bucket   string
	Prefix   string
	Capacity int
}

// Args renders c into the flag slice expected by an experiment's "-role node"
// entry point.
func (c ProcessConfig) Args() []string {
	return []string{"-role", "node", "-name", c.Name, "-work", c.Work, "-fault", c.Fault, "-bucket", c.Bucket, "-prefix", c.Prefix, "-capacity", fmt.Sprint(c.Capacity)}
}

// RunProcess is the entry point for a node child process: it connects to
// SeaweedFS S3 (optionally wrapped in faultstore), opens the shared catalog,
// starts a Node with the course recipe workload and standing bounds assertion,
// and serves commands on stdin/stdout.
func RunProcess(ctx context.Context, c ProcessConfig) error {
	s3, err := s3store.Connect(ctx, rangefs.S3ConfigFromEnv(), c.Bucket, c.Prefix)
	if err != nil {
		return err
	}
	var store walship.Store = s3
	if c.Fault != "" {
		rule, err := faultstore.ParseRule(c.Fault)
		if err != nil {
			return err
		}
		store = faultstore.New(s3, rule)
	}
	cat, err := catalog.Open(ctx, filepath.Join(c.Work, "catalog.db"))
	if err != nil {
		return err
	}
	defer cat.Close()
	workload := recipe.Default()
	n, err := Start(Config{
		Name: c.Name, Dir: filepath.Join(c.Work, c.Name), Catalog: cat, Store: store, Capacity: c.Capacity,
		File:   workload.SQLiteOptions().File,
		Create: func(ctx context.Context, path string) error { return workload.Build(ctx, path, 0) },
	})
	if err != nil {
		return err
	}
	assertion, err := bounds.Start(bounds.Bounds{
		OpenDatabases: c.Capacity, ConnectionsPerDatabase: ConnectionsPerDatabase,
		CacheBytes: bounds.CacheBytes(PlatformSettings().CacheSize, workload.SQLiteOptions().File.PageSize), ReserveDescriptors: ReserveDescriptors,
	})
	if err != nil {
		return err
	}
	apply := func(ctx context.Context, db *sqlitex.DB) error {
		_, err := workload.ApplyNext(ctx, db)
		return err
	}
	return Serve(ctx, n, assertion, apply, os.Stdin, os.Stdout)
}
