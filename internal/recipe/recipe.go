package recipe

import (
	"fmt"
	"math/rand/v2"
	"strings"

	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
)

const (
	DefaultSeed  uint64 = 1
	DefaultSteps        = 30
	IndexStep           = 5
	MaxSteps            = 10000
)

const FormatVersion = 1

type Recipe struct {
	Seed        uint64
	Steps       int
	JournalMode string
}

func Default() Recipe {
	return Recipe{Seed: DefaultSeed, Steps: DefaultSteps, JournalMode: sqlitex.JournalWAL}
}

func (r Recipe) WithSteps(steps int) Recipe {
	r.Steps = steps
	return r
}

func (r Recipe) WithJournalMode(mode string) Recipe {
	r.JournalMode = mode
	return r
}

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

func (r Recipe) String() string {
	return fmt.Sprintf("seed=%d steps=%d journal_mode=%s", r.Seed, r.Steps, r.JournalMode)
}

func (r Recipe) SQLiteOptions() sqlitex.Options {
	options := sqlitex.DefaultOptions()
	options.File.JournalMode = r.JournalMode
	options.Conn.SecureDelete = true
	options.SingleConnection = true
	return options
}

type Statement struct {
	SQL  string
	Args []any
}

type StepKind int

const (
	KindSchema StepKind = iota
	KindInsert
	KindUpdate
	KindDelete
	KindIndex
	KindUserVersion
)

type Step struct {
	Number     int
	Statements []Statement
	Kinds      []StepKind
}

type StepCounts struct {
	Schema  int
	Inserts int
	Updates int
	Deletes int
	Indexes int
}

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

func (s Step) Summary() string {
	counts := s.Counts()
	return fmt.Sprintf("step %d: schema=%d inserts=%d updates=%d deletes=%d indexes=%d", s.Number, counts.Schema, counts.Inserts, counts.Updates, counts.Deletes, counts.Indexes)
}

func (s *Step) add(kind StepKind, sql string, args ...any) {
	s.Statements = append(s.Statements, Statement{SQL: sql, Args: args})
	s.Kinds = append(s.Kinds, kind)
}

var SchemaSQL = []string{
	"CREATE TABLE accounts(id INTEGER PRIMARY KEY, name TEXT NOT NULL, balance INTEGER NOT NULL)",
	"CREATE TABLE documents(id INTEGER PRIMARY KEY, account_id INTEGER NOT NULL, title TEXT NOT NULL, body BLOB NOT NULL)",
	"CREATE TABLE events(id INTEGER PRIMARY KEY, account_id INTEGER NOT NULL, kind TEXT NOT NULL, amount INTEGER NOT NULL, note TEXT NOT NULL)",
}

const IndexSQL = "CREATE INDEX events_by_account ON events(account_id, id)"

var eventKinds = []string{"deposit", "withdraw", "transfer", "fee"}

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
