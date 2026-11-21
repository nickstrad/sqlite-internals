// Group 4 adversarial attacks for Chapter 7, Experiment 7.4 (read-only-snapshot-vfs) and internal/rangefs.
// Attacks:
//
//	SNAP-01: Write refusal on immutable=1 WAL snapshot and default-open rollback snapshot via rangefs.
//	SNAP-02: Overflow-page reads (documents table) and full PRAGMA integrity_check over rangefs.
//	SNAP-03: Repeat-query 0-request claim boundary under small PRAGMA cache_size=4 (cache eviction).
//	SNAP-04: Overwriting a snapshot object in place behind an open immutable=1 connection.
//	SNAP-05: Hostile HTTP servers (200 ignoring Range, truncated 206, mid-query 500, missing Content-Length).
//	SNAP-06: Hostile S3 server returning 200 OK ignoring Range header against rangefs.S3Source.
//	SNAP-07: internal/rangefs API edge cases (0-byte ReadAt, failed ReadRange BytesRead, Seek/Stat after Close, nil ctx).
//	SNAP-08: Non-4096 page sizes (512 and 16384) and truncated snapshot detection over rangefs.
//	SNAP-09: Live SeaweedFS S3 vs HTTP request-for-request parity across 7.4 queries + overflow + failure modes.
//	SNAP-10: 8 concurrent connections querying one shared rangefs.FS + vfs.New handle.
package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nickstrad/walfleet/internal/rangefs"
	"github.com/nickstrad/walfleet/internal/recipe"
	"github.com/nickstrad/walfleet/internal/sqlitex"
	_ "modernc.org/sqlite"
	"modernc.org/sqlite/vfs"
)

const (
	scratchRoot  = "_work/adv-ch07/snapshot-rangefs"
	pageSize     = 4096
	headerBytes  = 100
	snapshotName = "snapshot.db"
	s3Bucket     = "course"
	s3Prefix     = "ch07/adv-snap09"
)

func emit(id, tried, expect, got string, held bool) {
	verdict := "BROKEN"
	if held {
		verdict = "HELD"
	}
	fmt.Printf("ATTACK %s | %s | EXPECT %s | GOT %s | %s\n", id, tried, expect, got, verdict)
}

func fatal(msg string, err error) {
	fmt.Fprintf(os.Stderr, "FATAL %s: %v\n", msg, err)
	os.Exit(1)
}

func cleanDir(path string) string {
	if err := os.RemoveAll(path); err != nil {
		fatal("remove "+path, err)
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		fatal("mkdir "+path, err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		fatal("abs "+path, err)
	}
	return abs
}

func shortHash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:16]
}

func openSingle(dsn string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

func remoteDSN(vfsName, name, params string) string {
	dsn := "file:" + name + "?vfs=" + vfsName
	if params != "" {
		dsn += "&" + params
	}
	return dsn
}

func renderQuery(ctx context.Context, db *sql.DB, query string) (int, string, error) {
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return 0, "", err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return 0, "", err
	}
	var all strings.Builder
	n := 0
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return 0, "", err
		}
		parts := make([]string, len(vals))
		for i, v := range vals {
			switch x := v.(type) {
			case []byte:
				parts[i] = fmt.Sprintf("%q", x)
			default:
				parts[i] = fmt.Sprintf("%v", x)
			}
		}
		all.WriteString(strings.Join(parts, "|") + "\n")
		n++
	}
	if err := rows.Err(); err != nil {
		return 0, "", err
	}
	return n, shortHash([]byte(all.String())), nil
}

func localRender(ctx context.Context, path, query string) (int, string) {
	db, err := openSingle("file:" + path + "?mode=ro")
	if err != nil {
		fatal("local open", err)
	}
	defer db.Close()
	n, digest, err := renderQuery(ctx, db, query)
	if err != nil {
		fatal("local query", err)
	}
	return n, digest
}

func main() {
	ctx := context.Background()
	base := cleanDir(scratchRoot)

	// Build shared 30-step and 29-step recipe snapshots once for reuse across attacks.
	r := recipe.Default()
	snap30Path := filepath.Join(base, "snap30.db")
	snap29Path := filepath.Join(base, "snap29.db")
	if err := r.Build(ctx, snap30Path, r.Steps); err != nil {
		fatal("build snap30", err)
	}
	if err := r.Build(ctx, snap29Path, r.Steps-1); err != nil {
		fatal("build snap29", err)
	}
	snap30Bytes, err := os.ReadFile(snap30Path)
	if err != nil {
		fatal("read snap30", err)
	}
	snap29Bytes, err := os.ReadFile(snap29Path)
	if err != nil {
		fatal("read snap29", err)
	}

	attackSNAP01(ctx, base, snap30Bytes)
	attackSNAP02(ctx, base, snap30Path, snap30Bytes)
	attackSNAP03(ctx, base, snap30Path, snap30Bytes)
	attackSNAP04(ctx, base, snap29Bytes, snap30Bytes)
	attackSNAP05(ctx, snap30Bytes)
	attackSNAP06(ctx)
	attackSNAP07(ctx)
	attackSNAP08(ctx, base, snap30Bytes)
	attackSNAP09(ctx, base, snap30Path, snap30Bytes)
	attackSNAP10(ctx, base, snap30Path, snap30Bytes)
}

// SNAP-01: Write refusal on immutable=1 WAL snapshot and default-open rollback-header snapshot.
func attackSNAP01(ctx context.Context, base string, snap30 []byte) {
	dir := cleanDir(filepath.Join(base, "snap01-ro"))
	if err := os.WriteFile(filepath.Join(dir, snapshotName), snap30, 0o644); err != nil {
		fatal("write snap", err)
	}
	rb := append([]byte(nil), snap30...)
	rb[18], rb[19] = 1, 1
	if err := os.WriteFile(filepath.Join(dir, "rollback.db"), rb, 0o644); err != nil {
		fatal("write rollback", err)
	}

	var nonReadMethods atomic.Int32
	fsHandler := http.FileServer(http.Dir(dir))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead && r.Method != http.MethodGet {
			nonReadMethods.Add(1)
		}
		fsHandler.ServeHTTP(w, r)
	}))
	defer srv.Close()

	remote := rangefs.New(ctx, rangefs.HTTPSource{BaseURL: srv.URL, Client: srv.Client()})
	vfsName, handle, err := vfs.New(remote)
	if err != nil {
		fatal("vfs.New", err)
	}
	defer handle.Close()

	writes := []string{
		"INSERT INTO events(id, account_id, kind, amount, note) VALUES(999999, 1, 'adv', 1, 'x')",
		"UPDATE events SET amount = 0 WHERE id = 372",
		"CREATE TABLE adv_write(id INTEGER PRIMARY KEY)",
		"PRAGMA user_version = 99",
		"VACUUM",
	}

	checkMode := func(file, params string) (int, []string) {
		db, err := openSingle(remoteDSN(vfsName, file, params))
		if err != nil {
			fatal("open "+file, err)
		}
		defer db.Close()
		refused := 0
		var codes []string
		for _, stmt := range writes {
			_, err := db.ExecContext(ctx, stmt)
			if err != nil {
				refused++
				c, _ := sqlitex.PrimaryErrorCode(err)
				codes = append(codes, strconv.Itoa(c))
			} else {
				codes = append(codes, "OK")
			}
		}
		return refused, codes
	}

	refImm, codesImm := checkMode(snapshotName, "immutable=1")
	refRB, codesRB := checkMode("rollback.db", "")

	afterSnap, _ := os.ReadFile(filepath.Join(dir, snapshotName))
	afterRB, _ := os.ReadFile(filepath.Join(dir, "rollback.db"))
	bytesIntact := shortHash(afterSnap) == shortHash(snap30) && shortHash(afterRB) == shortHash(rb)

	held := refImm == len(writes) && refRB == len(writes) && nonReadMethods.Load() == 0 && bytesIntact
	emit("SNAP-01",
		"attempt INSERT/UPDATE/CREATE/PRAGMA user_version/VACUUM on remote immutable=1 WAL and default-open rollback snapshots",
		"all 5 writes refused in both modes (SQLITE_READONLY=8), 0 non-HEAD/GET HTTP requests, remote bytes untouched",
		fmt.Sprintf("immRefused=%d/%d(codes=%v) rbRefused=%d/%d(codes=%v) nonReadHTTP=%d bytesIntact=%t",
			refImm, len(writes), codesImm, refRB, len(writes), codesRB, nonReadMethods.Load(), bytesIntact),
		held)
}

// SNAP-02: Overflow-page reads (documents table) and full PRAGMA integrity_check over rangefs.
func attackSNAP02(ctx context.Context, base, snap30Path string, snap30 []byte) {
	dir := cleanDir(filepath.Join(base, "snap02-overflow"))
	if err := os.WriteFile(filepath.Join(dir, snapshotName), snap30, 0o644); err != nil {
		fatal("write snap", err)
	}
	srv := httptest.NewServer(http.FileServer(http.Dir(dir)))
	defer srv.Close()

	remote := rangefs.New(ctx, rangefs.HTTPSource{BaseURL: srv.URL, Client: srv.Client()})
	vfsName, handle, err := vfs.New(remote)
	if err != nil {
		fatal("vfs.New", err)
	}
	defer handle.Close()

	docSQL := "SELECT id, title, length(body), hex(substr(body, 1, 16)), hex(substr(body, -16)) FROM documents ORDER BY id"
	localRows, localDigest := localRender(ctx, snap30Path, docSQL)

	remote.TakeRequests()
	db, err := openSingle(remoteDSN(vfsName, snapshotName, "immutable=1"))
	if err != nil {
		fatal("open remote", err)
	}
	defer db.Close()

	var tables int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_schema").Scan(&tables); err != nil {
		fatal("schema", err)
	}
	remote.TakeRequests()

	remRows, remDigest, err := renderQuery(ctx, db, docSQL)
	if err != nil {
		fatal("remote docs", err)
	}
	docReqs := remote.TakeRequests()

	var ic string
	if err := db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&ic); err != nil {
		fatal("remote ic", err)
	}
	icReqs := remote.TakeRequests()

	oddReqs := 0
	for _, req := range append(append([]rangefs.Request(nil), docReqs...), icReqs...) {
		if req.Kind == rangefs.KindRange {
			if !(req.Offset%pageSize == 0 && req.Length == pageSize) {
				oddReqs++
			}
		}
	}
	docStats := rangefs.Summarize(docReqs)
	icStats := rangefs.Summarize(icReqs)
	held := remRows == localRows && remDigest == localDigest && ic == "ok" && oddReqs == 0 &&
		docStats.StatRequests == 0 && docStats.RangeRequests > 100 && icStats.StatRequests == 0
	emit("SNAP-02",
		"scan documents table (with multi-page overflow blobs) and run PRAGMA integrity_check over rangefs immutable=1",
		"remote matches local digest, integrity_check=ok, all range reads are whole 4096B pages, 0 stat requests, >100 pages read",
		fmt.Sprintf("rows=%d/%d digest=%s/%s ic=%q docReqs=(%s) icReqs=(%s) oddReqs=%d",
			remRows, localRows, remDigest, localDigest, ic, docStats, icStats, oddReqs),
		held)
}

// SNAP-03: Probe 7.4's "repeating the query on the same connection makes no request" under PRAGMA cache_size=4.
func attackSNAP03(ctx context.Context, base, snap30Path string, snap30 []byte) {
	dir := cleanDir(filepath.Join(base, "snap03-cache"))
	if err := os.WriteFile(filepath.Join(dir, snapshotName), snap30, 0o644); err != nil {
		fatal("write snap", err)
	}
	srv := httptest.NewServer(http.FileServer(http.Dir(dir)))
	defer srv.Close()

	remote := rangefs.New(ctx, rangefs.HTTPSource{BaseURL: srv.URL, Client: srv.Client()})
	vfsName, handle, err := vfs.New(remote)
	if err != nil {
		fatal("vfs.New", err)
	}
	defer handle.Close()

	fullScan := "SELECT count(*), sum(amount), sum(length(note)) FROM events"
	localRows, localDigest := localRender(ctx, snap30Path, fullScan)

	db, err := openSingle(remoteDSN(vfsName, snapshotName, "immutable=1"))
	if err != nil {
		fatal("open remote", err)
	}
	defer db.Close()

	// Set page cache to 4 pages (< 18 pages needed for events full scan).
	if _, err := db.ExecContext(ctx, "PRAGMA cache_size=4"); err != nil {
		fatal("cache_size", err)
	}
	var tables int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_schema").Scan(&tables); err != nil {
		fatal("schema", err)
	}
	remote.TakeRequests()

	r1, d1, err := renderQuery(ctx, db, fullScan)
	if err != nil {
		fatal("scan 1", err)
	}
	firstStats := rangefs.Summarize(remote.TakeRequests())

	r2, d2, err := renderQuery(ctx, db, fullScan)
	if err != nil {
		fatal("scan 2", err)
	}
	repeatStats := rangefs.Summarize(remote.TakeRequests())

	// Narrowed claim (and 7.4 smallCache check): immutable=1 keeps cached pages valid (no change-counter
	// invalidation), not cached. Under cache_size=4 (< 18 pages), repeating the scan re-reads evicted pages
	// (17 range requests, 0 stat) while returning the exact local digest.
	claimHeld := r1 == localRows && r2 == localRows && d1 == localDigest && d2 == localDigest && repeatStats.RangeRequests == 17 && repeatStats.StatRequests == 0
	emit("SNAP-03",
		"repeat 18-page events full scan on same immutable=1 connection with PRAGMA cache_size=4",
		"evicted pages are re-fetched on repeat (17 range requests, 0 stat) while digest matches local (immutable=1 keeps cached pages valid, not cached)",
		fmt.Sprintf("digestMatch=%t first=(%s) repeat=(%s)",
			d1 == localDigest && d2 == localDigest, firstStats, repeatStats),
		claimHeld)
}

// SNAP-04: Overwrite the snapshot file behind an open immutable=1 connection (step 29 -> step 30 in place).
func attackSNAP04(ctx context.Context, base string, snap29, snap30 []byte) {
	dir := cleanDir(filepath.Join(base, "snap04-mutate"))
	servedPath := filepath.Join(dir, "mutable.db")
	if err := os.WriteFile(servedPath, snap29, 0o644); err != nil {
		fatal("write snap29", err)
	}
	srv := httptest.NewServer(http.FileServer(http.Dir(dir)))
	defer srv.Close()

	remote := rangefs.New(ctx, rangefs.HTTPSource{BaseURL: srv.URL, Client: srv.Client()})
	vfsName, handle, err := vfs.New(remote)
	if err != nil {
		fatal("vfs.New", err)
	}
	defer handle.Close()

	// Open step-29 snapshot with immutable=1 and read events (caches page 1 + events pages + pins size at open).
	db, err := openSingle(remoteDSN(vfsName, "mutable.db", "immutable=1"))
	if err != nil {
		fatal("open mutable", err)
	}
	defer db.Close()

	var uvBefore, evBefore int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&uvBefore); err != nil {
		fatal("uv before", err)
	}
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM events").Scan(&evBefore); err != nil {
		fatal("events before", err)
	}

	// Atomically overwrite mutable.db in place with step 30 bytes while the immutable=1 connection stays open.
	tmpPath := servedPath + ".tmp"
	if err := os.WriteFile(tmpPath, snap30, 0o644); err != nil {
		fatal("write tmp", err)
	}
	if err := os.Rename(tmpPath, servedPath); err != nil {
		fatal("rename", err)
	}

	// Query uncached documents pages and run integrity_check on the SAME open immutable=1 connection.
	var docCount int
	docErr := db.QueryRowContext(ctx, "SELECT count(*) FROM documents").Scan(&docCount)
	var ic string
	icErr := db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&ic)
	var uvAfter int
	_ = db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&uvAfter)

	// True step-29 counts for comparison:
	db29, _ := openSingle("file:" + filepath.Join(base, "snap29.db") + "?mode=ro")
	var trueDoc29 int
	_ = db29.QueryRowContext(ctx, "SELECT count(*) FROM documents").Scan(&trueDoc29)
	_ = db29.Close()

	db30, _ := openSingle("file:" + filepath.Join(base, "snap30.db") + "?mode=ro")
	var trueDoc30 int
	_ = db30.QueryRowContext(ctx, "SELECT count(*) FROM documents").Scan(&trueDoc30)
	_ = db30.Close()

	// Narrowed claim (sqlite.h.in:3922-3929): overwriting a served object in place behind an open
	// immutable=1 connection breaks it (stale cached page 1 mixed with newer pages -> SQLITE_CORRUPT (11)
	// and failed integrity_check); platforms must publish each snapshot under a new immutable key.
	docCode, _ := sqlitex.PrimaryErrorCode(docErr)
	corruptedAsExpected := uvBefore == 29 && uvAfter == 29 && docCode == 11 && icErr == nil && ic != "ok"
	icFlat := strings.ReplaceAll(ic, "\n", "; ")
	emit("SNAP-04",
		"overwrite served object in place (step 29 -> step 30) while an immutable=1 connection stays open",
		"open immutable=1 connection keeps stale cached page 1 and fails with SQLITE_CORRUPT (11) / non-ok integrity_check (proving snapshots need new immutable keys)",
		fmt.Sprintf("before(uv=%d ev=%d) after(uv=%d docs=%d docErr=%v ic=%q icErr=%v) truth(doc29=%d doc30=%d)",
			uvBefore, evBefore, uvAfter, docCount, docErr, icFlat, icErr, trueDoc29, trueDoc30),
		corruptedAsExpected)
}

// SNAP-05: Hostile HTTP servers against rangefs.HTTPSource.
func attackSNAP05(ctx context.Context, snap30 []byte) {
	// (a) 200 OK ignoring Range header
	srv200 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(snap30)))
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			_, _ = w.Write(snap30)
		}
	}))
	defer srv200.Close()
	src200 := rangefs.HTTPSource{BaseURL: srv200.URL, Client: srv200.Client()}
	buf := make([]byte, pageSize)
	_, err200 := src200.ReadRange(ctx, snapshotName, pageSize, buf)
	ok200 := err200 != nil && strings.Contains(err200.Error(), "206")

	// (b) Truncated 206 Partial Content (only 100 bytes returned for 4096 requested)
	srvTrunc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", strconv.Itoa(len(snap30)))
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", pageSize, pageSize+pageSize-1, len(snap30)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(snap30[pageSize : pageSize+100])
	}))
	defer srvTrunc.Close()
	srcTrunc := rangefs.HTTPSource{BaseURL: srvTrunc.URL, Client: srvTrunc.Client()}
	_, errTrunc := srcTrunc.ReadRange(ctx, snapshotName, pageSize, buf)
	okTrunc := errors.Is(errTrunc, io.ErrUnexpectedEOF)

	// (c) Mid-query HTTP 500: allow HEAD, header 100B, and page 1, then fail page reads with 500.
	var getCount atomic.Int32
	srv500 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", strconv.Itoa(len(snap30)))
			w.WriteHeader(http.StatusOK)
			return
		}
		if getCount.Add(1) > 2 {
			http.Error(w, "injected 500", http.StatusInternalServerError)
			return
		}
		http.ServeContent(w, r, snapshotName, time.Time{}, strings.NewReader(string(snap30)))
	}))
	defer srv500.Close()
	rem500 := rangefs.New(ctx, rangefs.HTTPSource{BaseURL: srv500.URL, Client: srv500.Client()})
	vfs500, h500, err := vfs.New(rem500)
	if err != nil {
		fatal("vfs 500", err)
	}
	defer h500.Close()
	db500, err := openSingle(remoteDSN(vfs500, snapshotName, "immutable=1"))
	if err != nil {
		fatal("open 500", err)
	}
	var cnt500 int
	err500 := db500.QueryRowContext(ctx, "SELECT count(*) FROM events").Scan(&cnt500)
	_ = db500.Close()
	code500, _ := sqlitex.PrimaryErrorCode(err500)
	ok500 := err500 != nil && code500 == 10

	// (d) HEAD 200 OK with missing Content-Length (Transfer-Encoding: chunked / Content-Length=-1)
	srvNoCL := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		conn, rw, err := hj.Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = rw.WriteString("HTTP/1.1 200 OK\r\nConnection: close\r\n\r\n")
		_ = rw.Flush()
	}))
	defer srvNoCL.Close()
	srcNoCL := rangefs.HTTPSource{BaseURL: srvNoCL.URL, Client: srvNoCL.Client()}
	_, errNoCL := srcNoCL.Stat(ctx, snapshotName)
	okNoCL := errNoCL != nil && strings.Contains(errNoCL.Error(), "Content-Length")

	held := ok200 && okTrunc && ok500 && okNoCL
	emit("SNAP-05",
		"hostile HTTP servers against HTTPSource: 200-ignoring-Range, truncated 206, mid-query 500, HEAD without Content-Length",
		"all 4 hostile responses rejected (200->error, trunc->UnexpectedEOF, mid-query 500->SQLITE_IOERR=10, no-CL->error)",
		fmt.Sprintf("200ok=%t(%v) truncOK=%t(%v) mid500OK=%t(code=%d) noCLOK=%t(%v)",
			ok200, err200 != nil, okTrunc, errTrunc, ok500, code500, okNoCL, errNoCL != nil),
		held)
}

// SNAP-06: Hostile S3 endpoint returning 200 OK (ignoring Range header) against rangefs.S3Source.
func attackSNAP06(ctx context.Context) {
	// Construct an 8192-byte object: first 4096 bytes = 0xAA, second 4096 bytes = 0xBB.
	obj := make([]byte, 2*pageSize)
	for i := range pageSize {
		obj[i] = 0xAA
		obj[pageSize+i] = 0xBB
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Deliberately ignore Range header and respond 200 OK with the whole object (no Content-Range).
		w.Header().Set("Content-Length", strconv.Itoa(len(obj)))
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			_, _ = w.Write(obj)
		}
	}))
	defer srv.Close()

	cfg := rangefs.S3Config{
		Endpoint:  srv.URL,
		Region:    "us-east-1",
		AccessKey: "test",
		SecretKey: "test-secret",
	}
	src := rangefs.S3Source{
		Client: rangefs.NewS3Client(cfg),
		Bucket: "bkt",
		Prefix: "pfx",
	}
	buf := make([]byte, pageSize)
	n, err := src.ReadRange(ctx, "snap.db", pageSize, buf)
	// Expect S3Source to reject a 200 OK non-partial response (matching HTTPSource's safeguard)
	// rather than silently serving bytes from offset 0 (0xAA) into a request for offset 4096 (0xBB).
	rejected := err != nil
	gotByte := byte(0)
	if n > 0 {
		gotByte = buf[0]
	}
	emit("SNAP-06",
		"hostile S3 endpoint returns 200 OK with full object (ignoring Range: bytes=4096-8191) to rangefs.S3Source.ReadRange",
		"S3Source rejects non-206 / missing Content-Range response instead of silently returning offset 0 bytes",
		fmt.Sprintf("err=%v n=%d firstByte=0x%02x (want error or 0xbb, offset 0 is 0xaa)", err, n, gotByte),
		rejected)
}

type stubSource struct {
	size     int64
	readErr  error
	readData []byte
	readCnt  int
}

func (s *stubSource) Stat(ctx context.Context, name string) (int64, error) {
	if name == "a.db" {
		return s.size, nil
	}
	return 0, fs.ErrNotExist
}

func (s *stubSource) ReadRange(ctx context.Context, name string, offset int64, p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	s.readCnt++
	if s.readErr != nil {
		return 0, s.readErr
	}
	n := copy(p, s.readData[offset:])
	return n, nil
}

// SNAP-07: internal/rangefs API and request-accounting edge cases.
func attackSNAP07(ctx context.Context) {
	// (a) Zero-length ReadAt should not log a bogus range request ("bytes 0--1 (0 bytes)").
	srcA := &stubSource{size: 4096, readData: make([]byte, 4096)}
	fsA := rangefs.New(ctx, srcA)
	fA, err := fsA.Open("a.db")
	if err != nil {
		fatal("open a.db", err)
	}
	fsA.TakeRequests()
	raA := fA.(io.ReaderAt)
	n0, err0 := raA.ReadAt([]byte{}, 0)
	reqs0 := fsA.TakeRequests()
	zeroReadClean := err0 == nil && n0 == 0 && srcA.readCnt == 0 && len(reqs0) == 0
	var bogusStr string
	if len(reqs0) > 0 {
		bogusStr = reqs0[0].String()
	}

	// (b) Failed ReadRange returning (0, err) should not add want bytes to Stats.BytesRead.
	srcB := &stubSource{size: 4096, readErr: errors.New("boom")}
	fsB := rangefs.New(ctx, srcB)
	fB, err := fsB.Open("a.db")
	if err != nil {
		fatal("open b", err)
	}
	fsB.TakeRequests()
	_, errFail := fB.(io.ReaderAt).ReadAt(make([]byte, 4096), 0)
	statsFail := rangefs.Summarize(fsB.TakeRequests())
	failBytesClean := errFail != nil && statsFail.BytesRead == 0

	// (c) Seek and Stat after Close should return fs.ErrClosed.
	_ = fA.Close()
	_, seekErr := fA.(io.Seeker).Seek(0, io.SeekStart)
	_, statErr := fA.Stat()
	closedGuarded := errors.Is(seekErr, fs.ErrClosed) && errors.Is(statErr, fs.ErrClosed)

	// (d) New(nil, HTTPSource) should default nil ctx to context.Background() instead of panicking.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "4096")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	var nilCtxPanicked bool
	func() {
		defer func() {
			if recover() != nil {
				nilCtxPanicked = true
			}
		}()
		//nolint:staticcheck // intentionally testing nil context handling
		var nilCtx context.Context
		fsNil := rangefs.New(nilCtx, rangefs.HTTPSource{BaseURL: srv.URL, Client: srv.Client()})
		_, _ = fsNil.Stat("a.db")
	}()

	held := zeroReadClean && failBytesClean && closedGuarded && !nilCtxPanicked
	emit("SNAP-07",
		"rangefs edge cases: 0-byte ReadAt, failed ReadRange BytesRead accounting, Seek/Stat after Close, New(nil, HTTPSource)",
		"0-byte ReadAt logs 0 requests, failed ReadRange adds 0 BytesRead, Seek/Stat after Close return ErrClosed, nil ctx does not panic",
		fmt.Sprintf("zeroReadClean=%t(logged=%q) failBytes=%d(want 0) seekAfterClose=%v statAfterClose=%v nilCtxPanicked=%t",
			zeroReadClean, bogusStr, statsFail.BytesRead, seekErr, statErr, nilCtxPanicked),
		held)
}

// SNAP-08: Non-4096 page sizes (512 and 16384) and truncated snapshot detection over rangefs.
func attackSNAP08(ctx context.Context, base string, snap30 []byte) {
	dir := cleanDir(filepath.Join(base, "snap08-pagesizes"))
	srv := httptest.NewServer(http.FileServer(http.Dir(dir)))
	defer srv.Close()

	remote := rangefs.New(ctx, rangefs.HTTPSource{BaseURL: srv.URL, Client: srv.Client()})
	vfsName, handle, err := vfs.New(remote)
	if err != nil {
		fatal("vfs.New", err)
	}
	defer handle.Close()

	q := "SELECT count(*), sum(amount) FROM events"
	psOK := true
	var psNotes []string

	for _, ps := range []int{512, 16384} {
		name := fmt.Sprintf("ps-%d.db", ps)
		localPath := filepath.Join(dir, name)
		opts := sqlitex.DefaultOptions()
		opts.File.PageSize = ps
		opts.File.JournalMode = sqlitex.JournalWAL
		opts.Conn.SecureDelete = true
		opts.SingleConnection = true
		r := recipe.Default().WithSteps(10)
		// Build a 10-step recipe DB with the non-default page size.
		db, err := sqlitex.Open(ctx, localPath, opts)
		if err != nil {
			fatal("open ps", err)
		}
		for s := 0; s <= 10; s++ {
			if err := r.ApplyStep(ctx, db, s); err != nil {
				fatal("apply ps", err)
			}
		}
		if err := db.Close(); err != nil {
			fatal("close ps", err)
		}

		locRows, locDigest := localRender(ctx, localPath, q)
		remote.TakeRequests()
		rdb, err := openSingle(remoteDSN(vfsName, name, "immutable=1"))
		if err != nil {
			fatal("remote ps open", err)
		}
		remRows, remDigest, err := renderQuery(ctx, rdb, q)
		if err != nil {
			fatal("remote ps query", err)
		}
		reqs := remote.TakeRequests()
		_ = rdb.Close()

		aligned := true
		rangeCnt := 0
		for _, req := range reqs {
			if req.Kind != rangefs.KindRange {
				continue
			}
			rangeCnt++
			if req.Offset == 0 && req.Length == headerBytes {
				continue
			}
			if req.Offset%int64(ps) != 0 || req.Length != int64(ps) {
				aligned = false
			}
		}
		ok := remRows == locRows && remDigest == locDigest && aligned && rangeCnt > 2
		if !ok {
			psOK = false
		}
		psNotes = append(psNotes, fmt.Sprintf("ps=%d(ok=%t ranges=%d aligned=%t)", ps, ok, rangeCnt, aligned))
	}

	// Truncated snapshot: remove the last 4096-byte page from snap30.
	truncPath := filepath.Join(dir, "truncated.db")
	if err := os.WriteFile(truncPath, snap30[:len(snap30)-pageSize], 0o644); err != nil {
		fatal("write trunc", err)
	}
	tdb, err := openSingle(remoteDSN(vfsName, "truncated.db", "immutable=1"))
	if err != nil {
		fatal("open trunc", err)
	}
	var ic string
	icErr := tdb.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&ic)
	_ = tdb.Close()
	truncDetected := icErr != nil || ic != "ok"

	emit("SNAP-08",
		"non-4096 page sizes (512 and 16384) and truncated snapshot (missing last page) over rangefs immutable=1",
		"512B and 16KB snapshots match local with page-aligned ranges; truncated snapshot fails integrity_check",
		fmt.Sprintf("%s; truncated(err=%v ic=%q)", strings.Join(psNotes, " "), icErr, ic),
		psOK && truncDetected)
}

// SNAP-09: Live SeaweedFS S3 vs HTTP request-for-request parity across 7.4 queries + overflow + error modes.
func attackSNAP09(ctx context.Context, base, snap30Path string, snap30 []byte) {
	cfg := rangefs.S3ConfigFromEnv()
	client := rangefs.NewS3Client(cfg)
	timeout, cancel := context.WithTimeout(ctx, 5*time.Second)
	err := rangefs.EnsureBucket(timeout, client, s3Bucket)
	cancel()
	if err != nil {
		fatal("SNAP-09 SeaweedFS unreachable (start with: docker compose up -d seaweedfs)", err)
	}

	// Publish snapshot to both HTTP and S3.
	dir := cleanDir(filepath.Join(base, "snap09-s3-parity"))
	if err := os.WriteFile(filepath.Join(dir, snapshotName), snap30, 0o644); err != nil {
		fatal("write snap", err)
	}
	srv := httptest.NewServer(http.FileServer(http.Dir(dir)))
	defer srv.Close()

	key := s3Prefix + "/" + snapshotName
	if err := rangefs.PutObject(ctx, client, s3Bucket, key, snap30); err != nil {
		fatal("s3 put", err)
	}
	defer func() {
		_ = rangefs.DeleteObject(ctx, client, s3Bucket, key)
	}()

	httpFS := rangefs.New(ctx, rangefs.HTTPSource{BaseURL: srv.URL, Client: srv.Client()})
	s3FS := rangefs.New(ctx, rangefs.S3Source{Client: client, Bucket: s3Bucket, Prefix: s3Prefix})
	httpVFS, hHTTP, err := vfs.New(httpFS)
	if err != nil {
		fatal("vfs http", err)
	}
	defer hHTTP.Close()
	s3VFS, hS3, err := vfs.New(s3FS)
	if err != nil {
		fatal("vfs s3", err)
	}
	defer hS3.Close()

	testQueries := []struct {
		name string
		sql  string
	}{
		{"point", "SELECT id, account_id, kind, amount, note FROM events WHERE id = 372"},
		{"range", "SELECT id, amount FROM events WHERE account_id = 7 ORDER BY id"},
		{"scan", "SELECT count(*), sum(amount), sum(length(note)) FROM events"},
		{"docs-overflow", "SELECT id, length(body) FROM documents ORDER BY id"},
	}

	reqStrings := func(reqs []rangefs.Request) []string {
		out := make([]string, len(reqs))
		for i, r := range reqs {
			out[i] = r.String()
		}
		return out
	}

	runOne := func(vfsName string, rfs *rangefs.FS, q string) (string, []string) {
		rfs.TakeRequests()
		db, err := openSingle(remoteDSN(vfsName, snapshotName, "immutable=1"))
		if err != nil {
			fatal("open", err)
		}
		defer db.Close()
		_, d, err := renderQuery(ctx, db, q)
		if err != nil {
			fatal("query", err)
		}
		return d, reqStrings(rfs.TakeRequests())
	}

	allEqual := true
	var notes []string
	for _, tq := range testQueries {
		_, locDigest := localRender(ctx, snap30Path, tq.sql)
		dH, rH := runOne(httpVFS, httpFS, tq.sql)
		dS, rS := runOne(s3VFS, s3FS, tq.sql)
		eq := dH == locDigest && dS == locDigest && slices.Equal(rH, rS)
		if !eq {
			allEqual = false
		}
		notes = append(notes, fmt.Sprintf("%s(eq=%t reqs=%d/%d)", tq.name, eq, len(rH), len(rS)))
	}

	// Also verify without-immutable error 14 matches on live S3.
	s3FS.TakeRequests()
	dbNoImm, _ := openSingle(remoteDSN(s3VFS, snapshotName, ""))
	var dummy int
	noImmErr := dbNoImm.QueryRowContext(ctx, "SELECT count(*) FROM events").Scan(&dummy)
	_ = dbNoImm.Close()
	noImmCode, _ := sqlitex.ErrorCode(noImmErr)
	noImmOK := noImmCode == 14
	if !noImmOK {
		allEqual = false
	}

	emit("SNAP-09",
		"live SeaweedFS S3 vs HTTP request-for-request parity on 7.4 queries + overflow documents + no-immutable error 14",
		"identical digests and identical ordered stat/range request lists between HTTP and SeaweedFS S3; no-immutable returns 14",
		fmt.Sprintf("%s; s3NoImmutableCode=%d", strings.Join(notes, " "), noImmCode),
		allEqual)
}

// SNAP-10: 8 concurrent connections querying one shared rangefs.FS + vfs.New handle.
func attackSNAP10(ctx context.Context, base, snap30Path string, snap30 []byte) {
	dir := cleanDir(filepath.Join(base, "snap10-conc"))
	if err := os.WriteFile(filepath.Join(dir, snapshotName), snap30, 0o644); err != nil {
		fatal("write snap", err)
	}
	srv := httptest.NewServer(http.FileServer(http.Dir(dir)))
	defer srv.Close()

	remote := rangefs.New(ctx, rangefs.HTTPSource{BaseURL: srv.URL, Client: srv.Client()})
	vfsName, handle, err := vfs.New(remote)
	if err != nil {
		fatal("vfs.New", err)
	}
	defer handle.Close()

	qs := []string{
		"SELECT id, account_id, kind, amount, note FROM events WHERE id = 372",
		"SELECT id, amount FROM events WHERE account_id = 7 ORDER BY id",
		"SELECT count(*), sum(amount), sum(length(note)) FROM events",
		"SELECT count(*), sum(length(body)) FROM documents",
	}
	wantDigests := make([]string, len(qs))
	for i, q := range qs {
		_, wantDigests[i] = localRender(ctx, snap30Path, q)
	}

	const goroutines = 8
	const iters = 3
	remote.TakeRequests()
	var wg sync.WaitGroup
	errs := make([]error, goroutines)
	for g := range goroutines {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			db, err := openSingle(remoteDSN(vfsName, snapshotName, "immutable=1"))
			if err != nil {
				errs[idx] = err
				return
			}
			defer db.Close()
			for range iters {
				for qi, q := range qs {
					_, d, err := renderQuery(ctx, db, q)
					if err != nil {
						errs[idx] = err
						return
					}
					if d != wantDigests[qi] {
						errs[idx] = fmt.Errorf("digest mismatch on q%d: got %s want %s", qi, d, wantDigests[qi])
						return
					}
				}
			}
		}(g)
	}
	wg.Wait()

	ok := true
	var firstErr error
	for _, e := range errs {
		if e != nil {
			ok = false
			if firstErr == nil {
				firstErr = e
			}
		}
	}
	stats := rangefs.Summarize(remote.TakeRequests())
	emit("SNAP-10",
		"8 concurrent connections running 3 rounds of 4 queries each through one shared rangefs.FS + vfs.New",
		"all 96 queries succeed with exact local digests and clean request accounting",
		fmt.Sprintf("ok=%t firstErr=%v stats=(%s)", ok, firstErr, stats),
		ok && stats.StatRequests == goroutines && stats.RangeRequests > 0)
}
