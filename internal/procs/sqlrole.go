package procs

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/nickstrad/walfleet/internal/recipe"
	"github.com/nickstrad/walfleet/internal/sqlitex"
)

// Built-in child role names registered in Roles.
const (
	// RoleSQL identifies the role holding one SQLite connection across scripted steps.
	RoleSQL = "sql"
	// RoleCopier identifies the role that copies live files with plain OS reads, holding no SQLite connection.
	RoleCopier = "copier"
)

// Roles is the default role registry passed to Serve by multi-process experiments.
var Roles = map[string]Role{
	RoleSQL:    SQLRole,
	RoleCopier: CopierRole,
}

// SQLOptions configures a RoleSQL child process's SQLite connection and output paths.
type SQLOptions struct {
	Path              string
	JournalMode       string
	BusyTimeout       time.Duration
	WALAutocheckpoint int
	PagesOut          string
}

// Args encodes o into command-line flags accepted by SQLRole.
func (o SQLOptions) Args() []string {
	args := []string{"-db", o.Path, "-journal", o.JournalMode, "-timeout-ms", strconv.FormatInt(o.BusyTimeout.Milliseconds(), 10), "-autocheckpoint", strconv.Itoa(o.WALAutocheckpoint)}
	if o.PagesOut != "" {
		args = append(args, "-pages-out", o.PagesOut)
	}
	return args
}

func parseSQLOptions(args []string) (SQLOptions, error) {
	flags := flag.NewFlagSet(RoleSQL, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var options SQLOptions
	var timeoutMS int64
	flags.StringVar(&options.Path, "db", "", "")
	flags.StringVar(&options.JournalMode, "journal", sqlitex.JournalWAL, "")
	flags.Int64Var(&timeoutMS, "timeout-ms", 0, "")
	flags.IntVar(&options.WALAutocheckpoint, "autocheckpoint", 0, "")
	flags.StringVar(&options.PagesOut, "pages-out", "", "")
	if err := flags.Parse(args); err != nil {
		return options, err
	}
	if flags.NArg() != 0 {
		return options, fmt.Errorf("unexpected role arguments %q", flags.Args())
	}
	if options.Path == "" {
		return options, errors.New("-db is required")
	}
	options.BusyTimeout = time.Duration(timeoutMS) * time.Millisecond
	return options, nil
}

type sqlSession struct {
	options    SQLOptions
	recipe     recipe.Recipe
	sqlOptions sqlitex.Options
	db         *sqlitex.DB
	conn       *sql.Conn
}

// SQLRole opens the target database with explicit recipe settings and pins one
// *sql.Conn for the child's entire lifetime so explicit BEGIN, COMMIT,
// ROLLBACK, and sqlite_dbpage reads all execute on the same SQLite connection.
func SQLRole(ctx context.Context, args []string) (*Actor, error) {
	options, err := parseSQLOptions(args)
	if err != nil {
		return nil, err
	}
	r := recipe.Default().WithSteps(recipe.MaxSteps)
	if options.JournalMode != "" {
		r = r.WithJournalMode(options.JournalMode)
	}
	if err := r.Validate(); err != nil {
		return nil, err
	}
	sqlOptions := r.SQLiteOptions()
	sqlOptions.Conn.BusyTimeout = options.BusyTimeout
	sqlOptions.Conn.WALAutocheckpoint = options.WALAutocheckpoint
	// Leave _txlock deferred so the script controls exact BEGIN DEFERRED / IMMEDIATE / EXCLUSIVE SQL.
	sqlOptions.ImmediateTx = false
	db, err := sqlitex.Open(ctx, options.Path, sqlOptions)
	if err != nil {
		return nil, err
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		db.Close()
		return nil, err
	}
	s := &sqlSession{options: options, recipe: r, sqlOptions: sqlOptions, db: db, conn: conn}
	return &Actor{
		Actions: map[string]Handler{
			"exec":           s.exec,
			"query":          s.query,
			"begin":          s.begin,
			"commit":         s.simple("COMMIT"),
			"rollback":       s.simple("ROLLBACK"),
			"timeout":        s.timeout,
			"checkpoint":     s.checkpoint,
			"recipe-exec":    s.recipeExec,
			"recipe-step":    s.recipeStep,
			"recipe-session": s.recipeSession,
			"user-version":   s.userVersion,
			"stray-close":    s.strayClose,
			"dbpage-write":   s.dbpageWrite,
		},
		Close: s.close,
	}, nil
}

func (s *sqlSession) close() error {
	connErr := s.conn.Close()
	dbErr := s.db.Close()
	return errors.Join(connErr, dbErr)
}

func joinedSQL(args []string) (string, error) {
	statement := strings.TrimSpace(strings.Join(args, " "))
	if statement == "" {
		return "", errors.New("no SQL given")
	}
	return statement, nil
}

func (s *sqlSession) exec(ctx context.Context, args []string) (string, error) {
	statement, err := joinedSQL(args)
	if err != nil {
		return "", err
	}
	_, err = s.conn.ExecContext(ctx, statement)
	return "", err
}

func (s *sqlSession) query(ctx context.Context, args []string) (string, error) {
	statement, err := joinedSQL(args)
	if err != nil {
		return "", err
	}
	var value sql.NullString
	if err := s.conn.QueryRowContext(ctx, statement).Scan(&value); err != nil {
		return "", err
	}
	if !value.Valid {
		return "NULL", nil
	}
	return value.String, nil
}

func (s *sqlSession) begin(ctx context.Context, args []string) (string, error) {
	if len(args) != 1 {
		return "", errors.New("begin takes one mode: deferred, immediate or exclusive")
	}
	mode := strings.ToUpper(args[0])
	switch mode {
	case "DEFERRED", "IMMEDIATE", "EXCLUSIVE":
	default:
		return "", fmt.Errorf("unknown transaction mode %q", args[0])
	}
	_, err := s.conn.ExecContext(ctx, "BEGIN "+mode)
	return "", err
}

func (s *sqlSession) simple(statement string) Handler {
	return func(ctx context.Context, args []string) (string, error) {
		if len(args) != 0 {
			return "", fmt.Errorf("%s takes no arguments", statement)
		}
		_, err := s.conn.ExecContext(ctx, statement)
		return "", err
	}
}

func oneInt(args []string, what string) (int, error) {
	if len(args) != 1 {
		return 0, fmt.Errorf("want one %s", what)
	}
	value, err := strconv.Atoi(args[0])
	if err != nil {
		return 0, fmt.Errorf("%s %q: %w", what, args[0], err)
	}
	return value, nil
}

func (s *sqlSession) timeout(ctx context.Context, args []string) (string, error) {
	ms, err := oneInt(args, "busy timeout in milliseconds")
	if err != nil {
		return "", err
	}
	if ms < 0 {
		return "", fmt.Errorf("busy timeout %d is negative", ms)
	}
	_, err = s.conn.ExecContext(ctx, fmt.Sprintf("PRAGMA busy_timeout=%d", ms))
	return "", err
}

func (s *sqlSession) checkpoint(ctx context.Context, args []string) (string, error) {
	if len(args) != 1 {
		return "", errors.New("checkpoint takes one mode")
	}
	result, err := sqlitex.Checkpoint(ctx, s.conn, "", args[0])
	if err != nil {
		return "", err
	}
	return result.String(), nil
}

func (s *sqlSession) recipeExec(ctx context.Context, args []string) (string, error) {
	k, err := oneInt(args, "recipe step")
	if err != nil {
		return "", err
	}
	step, err := s.recipe.Step(k)
	if err != nil {
		return "", err
	}
	return "", step.ExecIn(ctx, s.conn)
}

func (s *sqlSession) recipeStep(ctx context.Context, args []string) (string, error) {
	k, err := oneInt(args, "recipe step")
	if err != nil {
		return "", err
	}
	step, err := s.recipe.Step(k)
	if err != nil {
		return "", err
	}
	if _, err := s.conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return "", err
	}
	if err := step.ExecIn(ctx, s.conn); err != nil {
		_, rollbackErr := s.conn.ExecContext(ctx, "ROLLBACK")
		return "", errors.Join(err, rollbackErr)
	}
	if _, err := s.conn.ExecContext(ctx, "COMMIT"); err != nil {
		_, rollbackErr := s.conn.ExecContext(ctx, "ROLLBACK")
		return "", errors.Join(err, rollbackErr)
	}
	return "", nil
}

// recipeSession opens a fresh SingleConnection handle with _txlock=immediate,
// applies recipe step k, and closes that handle while the role's own s.conn
// stays open. Opening a fresh connection per step reproduces the canonical
// recipe's page-1 change counter (enabling exact E1 comparison against
// recipe.StepHashes), while keeping s.conn open prevents sqlite3WalClose
// (src/wal.c:2507-2571) from checkpointing and deleting the WAL on close.
func (s *sqlSession) recipeSession(ctx context.Context, args []string) (string, error) {
	k, err := oneInt(args, "recipe step")
	if err != nil {
		return "", err
	}
	options := s.sqlOptions
	options.SingleConnection = true
	options.ImmediateTx = true
	db, err := sqlitex.Open(ctx, s.options.Path, options)
	if err != nil {
		return "", err
	}
	applyErr := s.recipe.ApplyStep(ctx, db, k)
	return "", errors.Join(applyErr, db.Close())
}

func (s *sqlSession) userVersion(ctx context.Context, args []string) (string, error) {
	if len(args) != 0 {
		return "", errors.New("user-version takes no arguments")
	}
	k, err := recipe.CurrentStep(ctx, s.conn)
	if err != nil {
		return "", err
	}
	return strconv.Itoa(k), nil
}

// strayClose intentionally triggers the POSIX advisory-lock hazard documented in
// src/os_unix.c:1189-1255: opening and closing any file descriptor to an inode
// drops every POSIX lock this process holds on that inode, even though SQLite's
// own connection and unixInodeInfo remain open and unaware.
func (s *sqlSession) strayClose(ctx context.Context, args []string) (string, error) {
	if len(args) != 1 {
		return "", errors.New("stray-close takes one file: db, -wal, -shm or -journal")
	}
	path, err := sideFile(s.options.Path, args[0])
	if err != nil {
		return "", err
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	return "", file.Close()
}

func sideFile(dbPath, which string) (string, error) {
	switch which {
	case "db":
		return dbPath, nil
	case "-wal", "-shm", "-journal":
		return dbPath + which, nil
	}
	return "", fmt.Errorf("unknown file %q: want db, -wal, -shm or -journal", which)
}

// dbpageWrite reads pages [first..last] through the sqlite_dbpage virtual table
// on s.conn (so they come from this connection's active read snapshot) and writes
// them at their page offsets into s.options.PagesOut.
func (s *sqlSession) dbpageWrite(ctx context.Context, args []string) (string, error) {
	if len(args) != 2 {
		return "", errors.New("dbpage-write takes a first and a last page number")
	}
	if s.options.PagesOut == "" {
		return "", errors.New("the role was started without -pages-out")
	}
	first, err := strconv.Atoi(args[0])
	if err != nil {
		return "", err
	}
	last, err := strconv.Atoi(args[1])
	if err != nil {
		return "", err
	}
	if first < 1 || last < first {
		return "", fmt.Errorf("page range %d-%d is empty or starts before page 1", first, last)
	}
	file, err := os.OpenFile(s.options.PagesOut, os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return "", err
	}
	writer := bufio.NewWriter(file)
	pageSize := 0
	fail := func(err error) (string, error) {
		file.Close()
		return "", err
	}
	for n := first; n <= last; n++ {
		var page []byte
		if err := s.conn.QueryRowContext(ctx, "SELECT data FROM sqlite_dbpage WHERE pgno = ?", n).Scan(&page); err != nil {
			return fail(fmt.Errorf("page %d: %w", n, err))
		}
		if pageSize == 0 {
			pageSize = len(page)
			if _, err := file.Seek(int64(first-1)*int64(pageSize), io.SeekStart); err != nil {
				return fail(err)
			}
		}
		if len(page) != pageSize || pageSize == 0 {
			return fail(fmt.Errorf("page %d has %d bytes, earlier pages %d", n, len(page), pageSize))
		}
		if _, err := writer.Write(page); err != nil {
			return fail(err)
		}
	}
	if err := writer.Flush(); err != nil {
		return fail(err)
	}
	if err := file.Sync(); err != nil {
		return fail(err)
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	return fmt.Sprintf("%d pages", last-first+1), nil
}

type copierSession struct {
	db  string
	out string
}

// CopierRole starts a child process with no SQLite connections that copies
// whole files ("copy") or byte ranges ("copy-range") of a live database and its
// sidecars into -out. Running in a separate process guarantees its os.Open and
// Close calls never disturb any SQLite process's POSIX locks.
func CopierRole(ctx context.Context, args []string) (*Actor, error) {
	flags := flag.NewFlagSet(RoleCopier, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var s copierSession
	flags.StringVar(&s.db, "db", "", "")
	flags.StringVar(&s.out, "out", "", "")
	if err := flags.Parse(args); err != nil {
		return nil, err
	}
	if s.db == "" || s.out == "" || flags.NArg() != 0 {
		return nil, errors.New("copier needs -db and -out and nothing else")
	}
	return &Actor{Actions: map[string]Handler{
		"copy":       s.copyWhole,
		"copy-range": s.copyRange,
	}}, nil
}

func (s copierSession) target(name string) (string, error) {
	if name == "" || strings.ContainsAny(name, `/\`) || name == "." || name == ".." {
		return "", fmt.Errorf("copy target %q must be a plain file name", name)
	}
	return filepath.Join(s.out, name), nil
}

func (s copierSession) copyWhole(ctx context.Context, args []string) (string, error) {
	if len(args) != 2 {
		return "", errors.New("copy takes a source (db, -wal, -shm, -journal) and a target name")
	}
	source, err := sideFile(s.db, args[0])
	if err != nil {
		return "", err
	}
	target, err := s.target(args[1])
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(source)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		return "missing", nil
	}
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(target, data, 0o644); err != nil {
		return "", err
	}
	return fmt.Sprintf("%d bytes", len(data)), nil
}

func (s copierSession) copyRange(ctx context.Context, args []string) (string, error) {
	if len(args) != 4 {
		return "", errors.New("copy-range takes a source, a target name, a start offset and an end offset or 'end'")
	}
	source, err := sideFile(s.db, args[0])
	if err != nil {
		return "", err
	}
	target, err := s.target(args[1])
	if err != nil {
		return "", err
	}
	start, err := strconv.ParseInt(args[2], 10, 64)
	if err != nil || start < 0 {
		return "", fmt.Errorf("bad start offset %q", args[2])
	}
	in, err := os.Open(source)
	if err != nil {
		return "", err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return "", err
	}
	end := info.Size()
	if args[3] != "end" {
		end, err = strconv.ParseInt(args[3], 10, 64)
		if err != nil || end < start {
			return "", fmt.Errorf("bad end offset %q", args[3])
		}
	}
	if end > info.Size() {
		end = info.Size()
	}
	if start > end {
		start = end
	}
	buffer, err := readRange(in, start, end)
	if err != nil {
		return "", err
	}
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return "", err
	}
	if _, err := out.WriteAt(buffer, start); err != nil {
		out.Close()
		return "", err
	}
	if err := out.Close(); err != nil {
		return "", err
	}
	return fmt.Sprintf("bytes %d-%d", start, start+int64(len(buffer))), nil
}

// readRange reads [start, end) from in and slices to the actual byte count so a
// live file that shrank after Stat is not zero-padded on EOF.
func readRange(in io.ReaderAt, start, end int64) ([]byte, error) {
	buffer := make([]byte, end-start)
	n, err := in.ReadAt(buffer, start)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return buffer[:n], nil
}
