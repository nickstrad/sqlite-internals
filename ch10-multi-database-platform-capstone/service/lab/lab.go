// Package lab owns the shared experiment harness used by Experiments 10.2,
// 10.3, and 10.4: resetting scratch work directories and S3 prefixes, managing
// node child processes, tracking problem and standing-bounds lines across
// replies, matching recovered images against recipe step hashes, running the
// concurrent fleet checker subprocess, counting store operations, and
// quiescing closed databases with a TRUNCATE checkpoint.
package lab

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickstrad/sqlite-internals/ch10-multi-database-platform-capstone/service/catalog"
	"github.com/nickstrad/sqlite-internals/ch10-multi-database-platform-capstone/service/node"
	"github.com/nickstrad/sqlite-internals/ch10-multi-database-platform-capstone/service/s3store"
	"github.com/nickstrad/sqlite-internals/internal/connmgr"
	"github.com/nickstrad/sqlite-internals/internal/rangefs"
	"github.com/nickstrad/sqlite-internals/internal/recipe"
	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
)

// Env bundles one experiment's scratch work directory, S3 store, shared
// catalog, template node ProcessConfig, precomputed recipe step hashes, and
// reply Tracker.
type Env struct {
	Ctx     context.Context
	Work    string
	Store   *s3store.Store
	Catalog *catalog.Catalog
	Process node.ProcessConfig
	Hashes  []recipe.FileHashes
	Tracker Tracker
}

// Connect connects to the SeaweedFS S3 store configured in the environment
// using process.Bucket and process.Prefix.
func Connect(ctx context.Context, process node.ProcessConfig) (*s3store.Store, error) {
	return s3store.Connect(ctx, rangefs.S3ConfigFromEnv(), process.Bucket, process.Prefix)
}

// New prepares an Env for experiment name under _work/<name>/ and precomputes
// the recipe step hashes used to verify recovered and rebuilt databases.
func New(ctx context.Context, name string, store *s3store.Store, process node.ProcessConfig) (*Env, error) {
	work, err := sqlitex.WorkDir(name)
	if err != nil {
		return nil, err
	}
	hashes, err := recipe.Default().StepHashes(ctx)
	if err != nil {
		return nil, err
	}
	process.Work = work
	return &Env{Ctx: ctx, Work: work, Store: store, Process: process, Hashes: hashes}, nil
}

// ShownWork returns e.Work relative to the repository root when possible so
// INFO lines never print machine-specific absolute paths.
func (e *Env) ShownWork() string {
	if root, err := sqlitex.RepoRoot(); err == nil {
		if relative, err := filepath.Rel(root, e.Work); err == nil {
			return relative
		}
	}
	return e.Work
}

// Reset closes any open catalog, wipes e.Work and the experiment's S3 prefix,
// opens a fresh catalog, and registers ids with node.PlatformSettings().
func (e *Env) Reset(ids []string) error {
	if e.Catalog != nil {
		if err := e.Catalog.Close(); err != nil {
			return err
		}
		e.Catalog = nil
	}
	entries, err := os.ReadDir(e.Work)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := os.RemoveAll(filepath.Join(e.Work, entry.Name())); err != nil {
			return err
		}
	}
	if _, err := e.Store.WipeInBatches(e.Ctx); err != nil {
		return err
	}
	cat, err := catalog.Open(e.Ctx, filepath.Join(e.Work, "catalog.db"))
	if err != nil {
		return err
	}
	e.Catalog = cat
	return cat.RegisterAll(e.Ctx, ids, node.PlatformSettings())
}

// Close closes the open catalog handle, if any.
func (e *Env) Close() error {
	if e.Catalog == nil {
		return nil
	}
	err := e.Catalog.Close()
	e.Catalog = nil
	return err
}

// Config returns a copy of e.Process specialized for node name and optional
// faultstore rule string.
func (e *Env) Config(name, fault string) node.ProcessConfig {
	config := e.Process
	config.Name, config.Fault = name, fault
	return config
}

// Start launches a child node process with config and waits for its "ready" line.
func (e *Env) Start(config node.ProcessConfig) (*node.Process, error) {
	return node.StartProcess(e.Ctx, config.Name, config.Args()...)
}

// NodeDir returns the local directory owned by node name under e.Work.
func (e *Env) NodeDir(name string) string { return filepath.Join(e.Work, name) }

// DatabasePath returns the local path to database id inside nodeName's directory.
func (e *Env) DatabasePath(nodeName, id string) string {
	return filepath.Join(e.Work, nodeName, id+".db")
}

// Send dispatches command to node process p and records its reply in e.Tracker.
func (e *Env) Send(p *node.Process, command string) (node.Reply, error) {
	reply, err := p.Send(command)
	if err == nil {
		e.Tracker.Note(p.Name, reply)
	}
	return reply, err
}

// Stop cleanly closes node process p, records its closing reply in e.Tracker,
// and appends a problem if the close reply did not start with "ok ".
func (e *Env) Stop(p *node.Process) (node.Reply, error) {
	reply, err := p.Close()
	if err == nil {
		e.Tracker.Note(p.Name, reply)
		if !strings.HasPrefix(reply.Line, "ok ") {
			e.Tracker.Problems = append(e.Tracker.Problems, p.Name+": "+reply.Line)
		}
	}
	return reply, err
}

// Ids generates count zero-padded database ids ("<prefix>-0001" .. "<prefix>-NNNN").
func Ids(prefix string, count int) []string {
	ids := make([]string, count)
	for i := range ids {
		ids[i] = fmt.Sprintf("%s-%04d", prefix, i+1)
	}
	return ids
}

// Tracker aggregates bounds checks, background node problems, and out-of-bounds
// lines observed across all child processes in an experiment run.
type Tracker struct {
	Replies    int
	Problems   []string
	OverBounds []string
}

// Note records one interactive command reply from node name.
func (t *Tracker) Note(name string, reply node.Reply) {
	t.Replies++
	for _, problem := range reply.Problems {
		t.Problems = append(t.Problems, name+": "+problem)
	}
	if !reply.WithinBounds() {
		t.OverBounds = append(t.OverBounds, name+": "+reply.Bounds)
	}
}

// NoteOutput parses the captured stdout of a non-interactive (for example,
// strace-killed) node process, discarding any trailing incomplete line left by
// SIGKILL, recording bounds and problem lines, and returning all complete
// "ok …"/"refused …" replies.
func (t *Tracker) NoteOutput(name, stdout string) []string {
	var replies []string
	lines := strings.Split(stdout, "\n")
	// A killed process may die mid-line; ignore a trailing line with no newline
	// so partial stdout never turns into a false problem or acknowledgement.
	if len(lines) > 0 && !strings.HasSuffix(stdout, "\n") {
		lines = lines[:len(lines)-1]
	}
	for _, line := range lines {
		switch {
		case line == "" || line == "ready" || strings.HasPrefix(line, "standing assertion: "):
		case strings.HasPrefix(line, "bounds "):
			t.Replies++
			if !strings.HasSuffix(line, " ok") {
				t.OverBounds = append(t.OverBounds, name+": "+line)
			}
		case strings.HasPrefix(line, "ok ") || strings.HasPrefix(line, "refused "):
			replies = append(replies, line)
		default:
			t.Problems = append(t.Problems, name+": "+line)
		}
	}
	return replies
}

// Recovered summarizes matching a closed database image against the recipe's
// known step hashes.
type Recovered struct {
	UserVersion int
	Match       recipe.Match
	RecipeState bool
}

// MatchFile verifies that path is at rest (no -wal/-shm sidecars), reads its
// bytes, and matches them against e.Hashes.
func (e *Env) MatchFile(path string) (Recovered, error) {
	status, err := connmgr.CheckAtRest(path)
	if err != nil {
		return Recovered{}, err
	}
	if !status.AtRest {
		return Recovered{}, fmt.Errorf("lab: %s is not at rest: %s", path, status)
	}
	image, err := os.ReadFile(path)
	if err != nil {
		return Recovered{}, err
	}
	return e.MatchImage(image)
}

// MatchImage matches raw database bytes image against e.Hashes.
func (e *Env) MatchImage(image []byte) (Recovered, error) {
	return MatchImage(e.Hashes, image)
}

// AtRestSHA256 verifies that database id on nodeName is at rest and returns its
// SHA-256 hex digest.
func (e *Env) AtRestSHA256(nodeName, id string) (string, error) {
	status, err := connmgr.CheckAtRest(e.DatabasePath(nodeName, id))
	if err != nil {
		return "", err
	}
	if !status.AtRest {
		return "", fmt.Errorf("lab: %s on node %s is not at rest: %s", id, nodeName, status)
	}
	return status.SHA256, nil
}
