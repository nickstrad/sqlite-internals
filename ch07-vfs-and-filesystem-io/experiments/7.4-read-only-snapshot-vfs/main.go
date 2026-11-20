package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/nickstrad/sqlite-internals/internal/rangefs"
	"github.com/nickstrad/sqlite-internals/internal/recipe"
	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
	"modernc.org/sqlite/vfs"
)

const (
	pageSize     = 4096
	headerBytes  = 100
	snapshotName = "snapshot.db"
	rollbackName = "snapshot-rollback.db"
	liveName     = "live.db"
	s3Bucket     = "course"
	s3Prefix     = "ch07/7.4"
)

type query struct {
	name string
	sql  string
}

var queries = []query{
	{"point lookup", "SELECT id, account_id, kind, amount, note FROM events WHERE id = 372"},
	{"index range scan", "SELECT id, amount FROM events WHERE account_id = 7 ORDER BY id"},
	{"full scan", "SELECT count(*), sum(amount), sum(length(note)) FROM events"},
}

var failures []string

func check(ok bool, claim string) {
	status := "ok"
	if !ok {
		status = "FAILED"
		failures = append(failures, claim)
	}
	fmt.Printf("check %s: %s\n", status, claim)
}

func fatal(what string, err error) {
	fmt.Printf("FATAL %s: %v\n", what, err)
	os.Exit(1)
}

type publisher interface {
	publish(ctx context.Context, name string, body []byte) error
	remove(ctx context.Context, name string) error
	source() rangefs.Source
	describe() string
}

type httpPublisher struct {
	dir    string
	server *httptest.Server
}

func newHTTPPublisher(dir string) (*httpPublisher, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &httpPublisher{dir: dir, server: httptest.NewServer(http.FileServer(http.Dir(dir)))}, nil
}

func (p *httpPublisher) publish(ctx context.Context, name string, body []byte) error {
	return os.WriteFile(filepath.Join(p.dir, name), body, 0o644)
}

func (p *httpPublisher) remove(ctx context.Context, name string) error {
	err := os.Remove(filepath.Join(p.dir, name))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func (p *httpPublisher) source() rangefs.Source {
	return rangefs.HTTPSource{BaseURL: p.server.URL, Client: p.server.Client()}
}

func (p *httpPublisher) describe() string {
	return "http: an in-process HTTP server (net/http FileServer, which answers Range requests); HEAD for stat, GET with a Range header for each read"
}

type s3Publisher struct {
	client   *s3.Client
	endpoint string
}

func (p *s3Publisher) publish(ctx context.Context, name string, body []byte) error {
	return rangefs.PutObject(ctx, p.client, s3Bucket, s3Prefix+"/"+name, body)
}

func (p *s3Publisher) remove(ctx context.Context, name string) error {
	return rangefs.DeleteObject(ctx, p.client, s3Bucket, s3Prefix+"/"+name)
}

func (p *s3Publisher) source() rangefs.Source {
	return rangefs.S3Source{Client: p.client, Bucket: s3Bucket, Prefix: s3Prefix}
}

func (p *s3Publisher) describe() string {
	return fmt.Sprintf("s3: SeaweedFS from compose.yaml, bucket %q, keys %s/<name>; HeadObject for stat, GetObject with a Range header for each read", s3Bucket, s3Prefix)
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "child-writer" {
		childWriter(os.Args[2])
		return
	}
	useS3 := flag.Bool("s3", false, "read the snapshot from the SeaweedFS S3 service instead of the in-process HTTP server")
	flag.Parse()
	ctx := context.Background()
	sqlitex.MustPinCheckMemory(ctx)
	dir := sqlitex.MustWorkDir("7.4")
	fmt.Printf("INFO: work dir %s\n", sqlitex.DisplayPath(dir))

	var pub publisher
	if *useS3 {
		cfg := rangefs.S3ConfigFromEnv()
		client := rangefs.NewS3Client(cfg)
		timeout, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := rangefs.EnsureBucket(timeout, client, s3Bucket)
		cancel()
		if err != nil {
			fatal("S3 bucket", fmt.Errorf("%w (start the store with: docker compose up -d seaweedfs)", err))
		}
		fmt.Printf("INFO: S3 endpoint %s\n", cfg.Endpoint)
		pub = &s3Publisher{client: client, endpoint: cfg.Endpoint}
	} else {
		p, err := newHTTPPublisher(filepath.Join(dir, "published"))
		if err != nil {
			fatal("http", err)
		}
		defer p.server.Close()
		pub = p
	}
	fmt.Printf("route %s\n", pub.describe())
	for _, name := range []string{snapshotName, rollbackName, liveName, liveName + "-wal", liveName + "-journal", snapshotName + "-wal", snapshotName + "-journal"} {
		if err := pub.remove(ctx, name); err != nil {
			fatal("remove "+name, err)
		}
	}
	remote := rangefs.New(ctx, pub.source())
	vfsName, handle, err := vfs.New(remote)
	if err != nil {
		fatal("vfs.New", err)
	}
	defer handle.Close()

	r := recipe.Default()
	local := filepath.Join(dir, snapshotName)
	snapshot := buildSnapshot(ctx, r, local)
	if err := pub.publish(ctx, snapshotName, snapshot); err != nil {
		fatal("publish", err)
	}

	fmt.Println()
	fmt.Println("== queries: local file vs the published snapshot read through modernc.org/sqlite/vfs + rangefs (immutable=1) ==")
	for _, q := range queries {
		runQuery(ctx, q, local, vfsName, remote)
	}

	fmt.Println()
	fmt.Println("== why immutable=1: the same WAL-format snapshot opened without it ==")
	withoutImmutable(ctx, vfsName, remote, snapshotName, local)

	fmt.Println()
	fmt.Println("== the same bytes with header bytes 18-19 set to 1 (rollback format) open without immutable=1 ==")
	rollbackFormat(ctx, pub, snapshot, vfsName, remote, local)

	fmt.Println()
	fmt.Println("== why a fully checkpointed file: a live database whose last commit is still in its -wal ==")
	staleRead(ctx, r, dir, pub, vfsName, remote)

	if len(failures) > 0 {
		fmt.Printf("FAIL: %d claim(s) did not hold\n", len(failures))
		os.Exit(1)
	}
	fmt.Println("ALL CHECKS PASSED")
}

func buildSnapshot(ctx context.Context, r recipe.Recipe, path string) []byte {
	if err := r.Build(ctx, path, r.Steps); err != nil {
		fatal("build snapshot", err)
	}
	match, err := r.MatchStep(ctx, path)
	if err != nil {
		fatal("match", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		fatal("read snapshot", err)
	}
	_, walErr := os.Stat(path + "-wal")
	fmt.Printf("snapshot: recipe seed %d, %d steps, journal_mode=%s, closed by its last connection: %d bytes, %d pages, sha256 %s, equals %s\n", r.Seed, r.Steps, r.JournalMode, len(body), len(body)/pageSize, shortHash(body), match)
	fmt.Printf("snapshot header bytes 18-19 (write/read format version): %d %d (2 = WAL); -wal file present: %t\n", body[18], body[19], walErr == nil)
	check(body[18] == 2 && body[19] == 2 && os.IsNotExist(walErr) && match.Step == r.Steps && match.Exact, "the snapshot is a WAL-format database with every commit checkpointed into the main file and no -wal left")
	return body
}

func shortHash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:16]
}

func remoteDSN(vfsName, name, params string) string {
	dsn := "file:" + name + "?vfs=" + vfsName
	if params != "" {
		dsn += "&" + params
	}
	return dsn
}

func openSingle(dsn string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

func render(ctx context.Context, db *sql.DB, query string) (int, string, string, error) {
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return 0, "", "", err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return 0, "", "", err
	}
	var all strings.Builder
	first := ""
	n := 0
	for rows.Next() {
		values := make([]any, len(cols))
		pointers := make([]any, len(cols))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			return 0, "", "", err
		}
		parts := make([]string, len(values))
		for i, v := range values {
			switch x := v.(type) {
			case []byte:
				parts[i] = fmt.Sprintf("%q", x)
			default:
				parts[i] = fmt.Sprintf("%v", x)
			}
		}
		line := strings.Join(parts, "|")
		if n == 0 {
			first = line
		}
		all.WriteString(line + "\n")
		n++
	}
	if err := rows.Err(); err != nil {
		return 0, "", "", err
	}
	return n, shortHash([]byte(all.String())), first, nil
}

func localResult(ctx context.Context, path, query string) (int, string, string, []string) {
	db, err := openSingle("file:" + path + "?mode=ro")
	if err != nil {
		fatal("local open", err)
	}
	defer db.Close()
	n, digest, first, err := render(ctx, db, query)
	if err != nil {
		fatal("local query", err)
	}
	rows, err := db.QueryContext(ctx, "EXPLAIN QUERY PLAN "+query)
	if err != nil {
		fatal("plan", err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			fatal("plan", err)
		}
		plan = append(plan, detail)
	}
	return n, digest, first, plan
}

type pageRead struct {
	header bool
	page   int64
}

func classify(requests []rangefs.Request) (pages []pageRead, odd []string) {
	for _, req := range requests {
		if req.Kind != rangefs.KindRange {
			continue
		}
		switch {
		case req.Offset == 0 && req.Length == headerBytes:
			pages = append(pages, pageRead{header: true})
		case req.Offset%pageSize == 0 && req.Length == pageSize:
			pages = append(pages, pageRead{page: req.Offset/pageSize + 1})
		default:
			odd = append(odd, req.String())
		}
	}
	return pages, odd
}

func describePages(pages []pageRead) string {
	parts := make([]string, len(pages))
	for i, p := range pages {
		if p.header {
			parts[i] = "hdr"
		} else {
			parts[i] = fmt.Sprintf("%d", p.page)
		}
	}
	return strings.Join(parts, " ")
}

func runQuery(ctx context.Context, q query, local, vfsName string, remote *rangefs.FS) {
	localRows, localDigest, localFirst, plan := localResult(ctx, local, q.sql)
	fmt.Println()
	fmt.Printf("-- %s: %s\n", q.name, q.sql)
	fmt.Printf("plan: %s\n", strings.Join(plan, "; "))
	fmt.Printf("local file: %d row(s), digest %s, first row %s\n", localRows, localDigest, localFirst)

	remote.TakeRequests()
	db, err := openSingle(remoteDSN(vfsName, snapshotName, "immutable=1"))
	if err != nil {
		fatal("remote open", err)
	}
	var tables int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_schema").Scan(&tables); err != nil {
		fatal("remote schema", err)
	}
	openRequests := remote.TakeRequests()
	rows, digest, first, err := render(ctx, db, q.sql)
	if err != nil {
		fatal("remote query", err)
	}
	queryRequests := remote.TakeRequests()
	if _, _, _, err := render(ctx, db, q.sql); err != nil {
		fatal("remote repeat", err)
	}
	repeatRequests := remote.TakeRequests()
	if err := db.Close(); err != nil {
		fatal("remote close", err)
	}
	fmt.Printf("remote snapshot: %d row(s), digest %s, first row %s\n", rows, digest, first)
	openStats, queryStats, repeatStats := rangefs.Summarize(openRequests), rangefs.Summarize(queryRequests), rangefs.Summarize(repeatRequests)
	fmt.Printf("  open + schema load: %s\n", openStats)
	for _, req := range openRequests {
		fmt.Printf("    %s\n", req)
	}
	queryPages, odd := classify(queryRequests)
	fmt.Printf("  query (cold cache): %s; pages read in order: %s\n", queryStats, describePages(queryPages))
	fmt.Printf("  same query again on the same connection: %s\n", repeatStats)
	fmt.Printf("  first query on a new connection, total: %d requests, %d bytes\n", openStats.Requests()+queryStats.Requests(), openStats.BytesRead+queryStats.BytesRead)
	_, oddOpen := classify(openRequests)
	check(rows > 0 && rows == localRows && digest == localDigest, fmt.Sprintf("%s: the remote result equals the local file's (%d row(s), digest %s)", q.name, rows, digest))
	check(len(odd) == 0 && len(oddOpen) == 0 && queryStats.StatRequests == 0, fmt.Sprintf("%s: every read is one range request for one whole %d-byte page (or the %d-byte header): one request per page miss, no read-ahead", q.name, pageSize, headerBytes))
	check(repeatStats.Requests() == 0, fmt.Sprintf("%s: repeating the query on the same connection makes no request (immutable=1: no change-counter check, the page cache stays valid)", q.name))
}

func errorLine(err error) string {
	if err == nil {
		return "no error"
	}
	if code, ok := sqlitex.ErrorCode(err); ok {
		return fmt.Sprintf("SQLite error code %d: %s", code, err.Error())
	}
	return err.Error()
}

func tryOpen(ctx context.Context, vfsName string, remote *rangefs.FS, name, params string) (string, error, []rangefs.Request) {
	remote.TakeRequests()
	db, err := openSingle(remoteDSN(vfsName, name, params))
	if err != nil {
		return "", err, nil
	}
	defer db.Close()
	var count int
	var version int
	err = db.QueryRowContext(ctx, "SELECT count(*) FROM events").Scan(&count)
	if err == nil {
		err = db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version)
	}
	requests := remote.TakeRequests()
	if err != nil {
		return "", err, requests
	}
	return fmt.Sprintf("user_version=%d events=%d", version, count), nil, requests
}

func localState(ctx context.Context, path string) string {
	db, err := openSingle("file:" + path)
	if err != nil {
		fatal("local", err)
	}
	defer db.Close()
	var count, version int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM events").Scan(&count); err != nil {
		fatal("local count", err)
	}
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		fatal("local version", err)
	}
	return fmt.Sprintf("user_version=%d events=%d", version, count)
}

func printRequests(requests []rangefs.Request) {
	for _, req := range requests {
		fmt.Printf("    %s\n", req)
	}
}

func withoutImmutable(ctx context.Context, vfsName string, remote *rangefs.FS, name, local string) {
	for _, params := range []string{"", "mode=ro", "immutable=1"} {
		label := params
		if label == "" {
			label = "(no parameters)"
		}
		state, err, requests := tryOpen(ctx, vfsName, remote, name, params)
		if err != nil {
			fmt.Printf("%s: %s; %s\n", label, errorLine(err), rangefs.Summarize(requests))
		} else {
			fmt.Printf("%s: %s; %s\n", label, state, rangefs.Summarize(requests))
		}
		printRequests(requests)
		code, _ := sqlitex.ErrorCode(err)
		switch params {
		case "immutable=1":
			check(err == nil && state == localState(ctx, local), "with immutable=1 the WAL-format snapshot opens and reads the same state as the local file")
		default:
			check(code == 14, fmt.Sprintf("without immutable=1 (%s) the WAL-format snapshot fails to open with SQLITE_CANTOPEN (14)", label))
		}
	}
}

func rollbackFormat(ctx context.Context, pub publisher, snapshot []byte, vfsName string, remote *rangefs.FS, local string) {
	patched := append([]byte(nil), snapshot...)
	patched[18], patched[19] = 1, 1
	if err := pub.publish(ctx, rollbackName, patched); err != nil {
		fatal("publish", err)
	}
	state, err, requests := tryOpen(ctx, vfsName, remote, rollbackName, "")
	fmt.Printf("(no parameters): %s; %s\n", fallback(state, err), rangefs.Summarize(requests))
	printRequests(requests)
	check(err == nil && state == localState(ctx, local), "a checkpointed snapshot with header bytes 18-19 rewritten to 1 opens without immutable=1 and reads the same state (the WAL flag in the header, not the content, is what demands shared memory)")
	journalChecks := 0
	for _, req := range requests {
		if req.Kind == rangefs.KindStat && strings.HasSuffix(req.Name, "-journal") {
			journalChecks++
		}
	}
	check(journalChecks > 0, fmt.Sprintf("without immutable=1 SQLite checks for a hot journal (%d stat request(s) for %s-journal), which immutable=1 skips", journalChecks, rollbackName))
}

func fallback(state string, err error) string {
	if err != nil {
		return errorLine(err)
	}
	return state
}

func childWriter(path string) {
	ctx := context.Background()
	r := recipe.Default()
	db, err := r.Open(ctx, path)
	if err != nil {
		fatal("child open", err)
	}
	if err := r.ApplyStep(ctx, db, r.Steps); err != nil {
		fatal("child step", err)
	}
	fmt.Println("READY")
	io.Copy(io.Discard, os.Stdin)
	if err := db.Close(); err != nil {
		fatal("child close", err)
	}
}

func staleRead(ctx context.Context, r recipe.Recipe, dir string, pub publisher, vfsName string, remote *rangefs.FS) {
	live := filepath.Join(dir, liveName)
	if err := r.Build(ctx, live, r.Steps-1); err != nil {
		fatal("build live", err)
	}
	command, err := os.Executable()
	if err != nil {
		fatal("executable", err)
	}
	child := exec.CommandContext(ctx, command, "child-writer", live)
	stdin, err := child.StdinPipe()
	if err != nil {
		fatal("pipe", err)
	}
	stdout, err := child.StdoutPipe()
	if err != nil {
		fatal("pipe", err)
	}
	child.Stderr = os.Stderr
	if err := child.Start(); err != nil {
		fatal("child", err)
	}
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "READY" {
		fatal("child", fmt.Errorf("child said %q: %v", line, err))
	}
	truth := localState(ctx, live)
	dbBytes, err := os.ReadFile(live)
	if err != nil {
		fatal("read live", err)
	}
	walBytes, err := os.ReadFile(live + "-wal")
	if err != nil {
		fatal("read live wal", err)
	}
	fmt.Printf("a child process built %s to step %d, then committed step %d and keeps its connection open (no checkpoint yet)\n", liveName, r.Steps-1, r.Steps)
	fmt.Printf("local read through SQLite (sees the -wal): %s\n", truth)
	fmt.Printf("published %s (%d bytes) and %s-wal (%d bytes, %d frames) as they are on disk\n", liveName, len(dbBytes), liveName, len(walBytes), (len(walBytes)-32)/(24+pageSize))
	if err := pub.publish(ctx, liveName, dbBytes); err != nil {
		fatal("publish", err)
	}
	if err := pub.publish(ctx, liveName+"-wal", walBytes); err != nil {
		fatal("publish", err)
	}
	stale, err, requests := tryOpen(ctx, vfsName, remote, liveName, "immutable=1")
	fmt.Printf("remote, immutable=1: %s; %s\n", fallback(stale, err), rangefs.Summarize(requests))
	walRequests := 0
	for _, req := range requests {
		if strings.HasSuffix(req.Name, "-wal") {
			walRequests++
		}
	}
	_, plainErr, _ := tryOpen(ctx, vfsName, remote, liveName, "")
	fmt.Printf("remote, no parameters: %s\n", errorLine(plainErr))
	wantStale := fmt.Sprintf("user_version=%d", r.Steps-1)
	check(err == nil && strings.HasPrefix(stale, wantStale+" ") && strings.HasPrefix(truth, fmt.Sprintf("user_version=%d ", r.Steps)) && stale != truth,
		fmt.Sprintf("immutable=1 ignores the -wal: the remote read returns step %d's state while the writer has committed step %d (a stale read, no error)", r.Steps-1, r.Steps))
	check(walRequests == 0, "with immutable=1 SQLite never asks for the -wal file (0 requests name it)")
	code, _ := sqlitex.ErrorCode(plainErr)
	check(code == 14, "without immutable=1 the live WAL database cannot be opened through the read-only VFS either (SQLITE_CANTOPEN)")

	stdin.Close()
	if err := child.Wait(); err != nil {
		fatal("child exit", err)
	}
	_, walErr := os.Stat(live + "-wal")
	dbBytes, err = os.ReadFile(live)
	if err != nil {
		fatal("read live", err)
	}
	if err := pub.publish(ctx, liveName, dbBytes); err != nil {
		fatal("publish", err)
	}
	if err := pub.remove(ctx, liveName+"-wal"); err != nil {
		fatal("remove", err)
	}
	fresh, err, _ := tryOpen(ctx, vfsName, remote, liveName, "immutable=1")
	fmt.Printf("after the writer closed (last connection: checkpoint, -wal deleted: %t) and the main file was republished: remote, immutable=1: %s\n", os.IsNotExist(walErr), fallback(fresh, err))
	check(err == nil && fresh == truth, fmt.Sprintf("once the commit is checkpointed into the main file, the republished snapshot shows step %d", r.Steps))
}
