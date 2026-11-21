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

type Env struct {
	Ctx     context.Context
	Work    string
	Store   *s3store.Store
	Catalog *catalog.Catalog
	Process node.ProcessConfig
	Hashes  []recipe.FileHashes
	Tracker Tracker
}

func Connect(ctx context.Context, process node.ProcessConfig) (*s3store.Store, error) {
	return s3store.Connect(ctx, rangefs.S3ConfigFromEnv(), process.Bucket, process.Prefix)
}

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

func (e *Env) ShownWork() string {
	if root, err := sqlitex.RepoRoot(); err == nil {
		if relative, err := filepath.Rel(root, e.Work); err == nil {
			return relative
		}
	}
	return e.Work
}

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

func (e *Env) Close() error {
	if e.Catalog == nil {
		return nil
	}
	err := e.Catalog.Close()
	e.Catalog = nil
	return err
}

func (e *Env) Config(name, fault string) node.ProcessConfig {
	config := e.Process
	config.Name, config.Fault = name, fault
	return config
}

func (e *Env) Start(config node.ProcessConfig) (*node.Process, error) {
	return node.StartProcess(e.Ctx, config.Name, config.Args()...)
}

func (e *Env) NodeDir(name string) string { return filepath.Join(e.Work, name) }

func (e *Env) DatabasePath(nodeName, id string) string {
	return filepath.Join(e.Work, nodeName, id+".db")
}

func (e *Env) Send(p *node.Process, command string) (node.Reply, error) {
	reply, err := p.Send(command)
	if err == nil {
		e.Tracker.Note(p.Name, reply)
	}
	return reply, err
}

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

func Ids(prefix string, count int) []string {
	ids := make([]string, count)
	for i := range ids {
		ids[i] = fmt.Sprintf("%s-%04d", prefix, i+1)
	}
	return ids
}

type Tracker struct {
	Replies    int
	Problems   []string
	OverBounds []string
}

func (t *Tracker) Note(name string, reply node.Reply) {
	t.Replies++
	for _, problem := range reply.Problems {
		t.Problems = append(t.Problems, name+": "+problem)
	}
	if !reply.WithinBounds() {
		t.OverBounds = append(t.OverBounds, name+": "+reply.Bounds)
	}
}

func (t *Tracker) NoteOutput(name, stdout string) []string {
	var replies []string
	lines := strings.Split(stdout, "\n")
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

type Recovered struct {
	UserVersion int
	Match       recipe.Match
	RecipeState bool
}

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

func (e *Env) MatchImage(image []byte) (Recovered, error) {
	return MatchImage(e.Hashes, image)
}

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
