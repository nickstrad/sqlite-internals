// Package recipe generates and applies a seeded, deterministic SQLite workload.
//
// Every step k is one transaction ending in PRAGMA user_version = k. Running
// the same recipe produces byte-identical database files at rest, giving every
// chapter a known ground-truth SHA-256 hash after each step (Rebuild Ladder
// rung R0).
package recipe

import (
	"fmt"
	"math/rand/v2"
	"strings"

	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
)

// Default recipe parameters and bounds.
const (
	// DefaultSeed is the PRNG seed used by Default.
	DefaultSeed uint64 = 1
	// DefaultSteps is the number of workload steps after schema step 0.
	DefaultSteps = 30
	// IndexStep is the step that creates the secondary index on events.
	IndexStep = 5
	// MaxSteps caps the step count accepted by Validate.
	MaxSteps = 10000
)

// FormatVersion versions the on-disk step-hash cache. Bump it whenever the
// generated SQL, parameters, or SQLite options change.
const FormatVersion = 1

// Recipe identifies a deterministic workload by PRNG seed, step count, and
// journal mode ("wal" or "delete").
type Recipe struct {
	Seed        uint64
	Steps       int
	JournalMode string
}

// Default returns the standard course recipe: seed 1, 30 workload steps, and
// WAL mode (the platform default).
func Default() Recipe {
	return Recipe{Seed: DefaultSeed, Steps: DefaultSteps, JournalMode: sqlitex.JournalWAL}
}

// WithSteps returns a copy of r with Steps replaced by steps.
func (r Recipe) WithSteps(steps int) Recipe {
	r.Steps = steps
	return r
}

// WithJournalMode returns a copy of r with JournalMode replaced by mode.
func (r Recipe) WithJournalMode(mode string) Recipe {
	r.JournalMode = mode
	return r
}

// Validate reports an error if r's step count is out of range or its journal
// mode is neither WAL nor DELETE.
func (r Recipe) Validate() error {
	if r.Steps < 0 || r.Steps > MaxSteps {
		return fmt.Errorf("recipe: steps %d out of range 0..%d", r.Steps, MaxSteps)
	}
	switch r.JournalMode {
	case sqlitex.JournalWAL, sqlitex.JournalDelete:
		return nil
	}
	return fmt.Errorf("recipe: journal mode %q not supported (want %q or %q)", r.JournalMode, sqlitex.JournalWAL, sqlitex.JournalDelete)
}

// String formats r as a compact description for logs and evidence output.
func (r Recipe) String() string {
	return fmt.Sprintf("seed=%d steps=%d journal_mode=%s", r.Seed, r.Steps, r.JournalMode)
}

// SQLiteOptions returns the explicit file and connection settings used when
// building or opening a recipe database.
//
// SecureDelete is always enabled so freed space inside B-tree pages and
// freelist leaves is zeroed rather than keeping stale bytes from earlier
// histories. SingleConnection pins the pool to one SQLite connection so step
// execution never interleaves across connections.
func (r Recipe) SQLiteOptions() sqlitex.Options {
	options := sqlitex.DefaultOptions()
	options.File.JournalMode = r.JournalMode
	options.Conn.SecureDelete = true
	options.SingleConnection = true
	return options
}

// Statement is one parameterized SQL statement and its bound arguments.
type Statement struct {
	SQL  string
	Args []any
}

// StepKind classifies a statement inside a Step for summary counts and tests.
type StepKind int

// Statement kinds recorded alongside each generated Statement.
const (
	KindSchema StepKind = iota
	KindInsert
	KindUpdate
	KindDelete
	KindIndex
	KindUserVersion
)

// Step is one numbered transaction in a recipe plan. Number 0 creates the
// schema; numbers 1..N run workload statements.
type Step struct {
	Number     int
	Statements []Statement
	Kinds      []StepKind
}

// StepCounts summarizes how many statements of each kind appear in a Step.
type StepCounts struct {
	Schema  int
	Inserts int
	Updates int
	Deletes int
	Indexes int
}

// Counts tallies the non-metadata statements in s by StepKind.
func (s Step) Counts() StepCounts {
	var counts StepCounts
	for _, kind := range s.Kinds {
		switch kind {
		case KindSchema:
			counts.Schema++
		case KindInsert:
			counts.Inserts++
		case KindUpdate:
			counts.Updates++
		case KindDelete:
			counts.Deletes++
		case KindIndex:
			counts.Indexes++
		}
	}
	return counts
}

// Summary formats s's statement counts as a single human-readable line.
func (s Step) Summary() string {
	counts := s.Counts()
	return fmt.Sprintf("step %d: schema=%d inserts=%d updates=%d deletes=%d indexes=%d", s.Number, counts.Schema, counts.Inserts, counts.Updates, counts.Deletes, counts.Indexes)
}

func (s *Step) add(kind StepKind, sql string, args ...any) {
	s.Statements = append(s.Statements, Statement{SQL: sql, Args: args})
	s.Kinds = append(s.Kinds, kind)
}

// SchemaSQL holds the three CREATE TABLE statements executed in step 0. Every
// table uses an explicit INTEGER PRIMARY KEY so rowids are supplied by Go
// rather than chosen by SQLite.
var SchemaSQL = []string{
	"CREATE TABLE accounts(id INTEGER PRIMARY KEY, name TEXT NOT NULL, balance INTEGER NOT NULL)",
	"CREATE TABLE documents(id INTEGER PRIMARY KEY, account_id INTEGER NOT NULL, title TEXT NOT NULL, body BLOB NOT NULL)",
	"CREATE TABLE events(id INTEGER PRIMARY KEY, account_id INTEGER NOT NULL, kind TEXT NOT NULL, amount INTEGER NOT NULL, note TEXT NOT NULL)",
}

// IndexSQL is the secondary index created during step IndexStep.
const IndexSQL = "CREATE INDEX events_by_account ON events(account_id, id)"

var eventKinds = []string{"deposit", "withdraw", "transfer", "fee"}

// Bounds on generated document body sizes in bytes. On a 4096-byte page a
// table leaf row spills to overflow pages above 4061 bytes of payload, so this
// range exercises both inline rows and multi-page overflow chains.
const (
	MinBodyBytes = 2000
	MaxBodyBytes = 16000
)

type generator struct {
	seed          uint64
	nextAccountID int64
	nextDocID     int64
	nextEventID   int64
	accounts      []int64
	documents     []int64
	events        []int64
}

func newGenerator(seed uint64) *generator {
	return &generator{seed: seed, nextAccountID: 1, nextDocID: 1, nextEventID: 1}
}

// Plan generates steps 0 through r.Steps in order. Each step depends only on
// r.Seed and its step index, never on r.Steps.
func (r Recipe) Plan() ([]Step, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	g := newGenerator(r.Seed)
	steps := make([]Step, 0, r.Steps+1)
	for k := 0; k <= r.Steps; k++ {
		steps = append(steps, g.step(k))
	}
	return steps, nil
}

// Step generates and returns step k alone by walking the live-id state from
// step 0 through step k.
func (r Recipe) Step(k int) (Step, error) {
	if k < 0 || k > r.Steps {
		return Step{}, fmt.Errorf("recipe: step %d out of range 0..%d", k, r.Steps)
	}
	plan, err := r.WithSteps(k).Plan()
	if err != nil {
		return Step{}, err
	}
	return plan[k], nil
}

func userVersionSQL(k int) string {
	return fmt.Sprintf("PRAGMA user_version = %d", k)
}

func (g *generator) step(k int) Step {
	s := Step{Number: k}
	if k == 0 {
		for _, sql := range SchemaSQL {
			s.add(KindSchema, sql)
		}
		s.add(KindUserVersion, userVersionSQL(0))
		return s
	}
	// Seed each step from (seed, k) so every value comes from Go's deterministic
	// PRNG rather than SQL random() or wall-clock functions.
	rng := rand.New(rand.NewPCG(g.seed, uint64(k)))

	for range 4 + rng.IntN(5) {
		id := g.nextAccountID
		g.nextAccountID++
		g.accounts = append(g.accounts, id)
		s.add(KindInsert, "INSERT INTO accounts(id, name, balance) VALUES(?, ?, ?)", id, randomWord(rng, 6, 18), rng.Int64N(100000))
	}
	for range 2 + rng.IntN(4) {
		id := g.nextDocID
		g.nextDocID++
		g.documents = append(g.documents, id)
		s.add(KindInsert, "INSERT INTO documents(id, account_id, title, body) VALUES(?, ?, ?, ?)", id, pick(rng, g.accounts), randomWord(rng, 8, 40), randomBody(rng))
	}
	for range 30 + rng.IntN(31) {
		id := g.nextEventID
		g.nextEventID++
		g.events = append(g.events, id)
		s.add(KindInsert, "INSERT INTO events(id, account_id, kind, amount, note) VALUES(?, ?, ?, ?, ?)", id, pick(rng, g.accounts), eventKinds[rng.IntN(len(eventKinds))], rng.Int64N(20000)-10000, randomWord(rng, 10, 60))
	}

	for range 2 + rng.IntN(4) {
		s.add(KindUpdate, "UPDATE accounts SET balance = ? WHERE id = ?", rng.Int64N(100000), pick(rng, g.accounts))
	}
	if rng.IntN(2) == 0 {
		s.add(KindUpdate, "UPDATE documents SET body = ? WHERE id = ?", randomBody(rng), pick(rng, g.documents))
	}

	// Periodic deletes exercise freeblocks and freelist pages (and overflow
	// chain release when a document is removed).
	if k%3 == 0 {
		for range 4 + rng.IntN(8) {
			var id int64
			g.events, id = removeRandom(rng, g.events)
			s.add(KindDelete, "DELETE FROM events WHERE id = ?", id)
		}
	}
	if k%4 == 0 && len(g.documents) > 2 {
		var id int64
		g.documents, id = removeRandom(rng, g.documents)
		s.add(KindDelete, "DELETE FROM documents WHERE id = ?", id)
	}

	if k == IndexStep {
		s.add(KindIndex, IndexSQL)
	}
	// Record the step number in header bytes 60..63 inside the same transaction
	// so CurrentStep always agrees with the committed rows.
	s.add(KindUserVersion, userVersionSQL(k))
	return s
}

func pick(rng *rand.Rand, ids []int64) int64 {
	return ids[rng.IntN(len(ids))]
}

func removeRandom(rng *rand.Rand, ids []int64) ([]int64, int64) {
	i := rng.IntN(len(ids))
	id := ids[i]
	return append(ids[:i:i], ids[i+1:]...), id
}

const letters = "abcdefghijklmnopqrstuvwxyz"

func randomWord(rng *rand.Rand, minLength, maxLength int) string {
	n := minLength + rng.IntN(maxLength-minLength+1)
	var b strings.Builder
	b.Grow(n)
	for range n {
		b.WriteByte(letters[rng.IntN(len(letters))])
	}
	return b.String()
}

func randomBody(rng *rand.Rand) []byte {
	n := MinBodyBytes + rng.IntN(MaxBodyBytes-MinBodyBytes+1)
	body := make([]byte, n)
	for i := range body {
		body[i] = byte(rng.Uint32())
	}
	return body
}
