package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/nickstrad/sqlite-internals/internal/recipe"
	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
	"github.com/nickstrad/sqlite-internals/internal/trace"
)

const (
	pageSize = 4096
	dbName   = "t.db"
)

type caseSpec struct {
	id           string
	desc         string
	mode         string
	synchronous  string
	secureDelete bool
	autockpt     int
	setupSQL     []string // executed when building the closed base DB
	preMarkerSQL []string // executed in the traced session before marker-1
	targetSQL    []string // executed between marker-1 and marker-2 (measured)
	cliSyncOver  string   // optional override for CLI synchronous (must-be-able-to-fail test)
}

func deterministicHexBlob(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte((i*31 + 17) & 0xff)
	}
	return "X'" + strings.ToUpper(hex.EncodeToString(b)) + "'"
}

func buildCases() []caseSpec {
	blob10k := deterministicHexBlob(10000)

	// Build a deterministic 100-row table setup reused by index, vacuum, analyze, and mmap cases.
	var bulkRows strings.Builder
	bulkRows.WriteString("CREATE TABLE t(id INTEGER PRIMARY KEY, k INTEGER NOT NULL, v TEXT NOT NULL);")
	for i := 1; i <= 100; i++ {
		fmt.Fprintf(&bulkRows, "INSERT INTO t(id, k, v) VALUES(%d, %d, 'val-%04d-%s');", i, i%17, i, strings.Repeat("a", 80))
	}

	var fragSetup []string
	fragSetup = append(fragSetup, bulkRows.String())
	fragSetup = append(fragSetup, "DELETE FROM t WHERE id % 2 = 0;")

	var indexedSetup []string
	indexedSetup = append(indexedSetup, bulkRows.String())
	indexedSetup = append(indexedSetup, "CREATE INDEX idx_t_k ON t(k, id);")

	return []caseSpec{
		{
			id:           "TWIN-01",
			desc:         "WAL steady-state 2nd commit (synchronous=FULL)",
			mode:         sqlitex.JournalWAL,
			synchronous:  sqlitex.SyncFull,
			secureDelete: true,
			autockpt:     1000,
			setupSQL:     []string{"CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT NOT NULL); INSERT INTO t(id, v) VALUES(1, 'one');"},
			preMarkerSQL: []string{"INSERT INTO t(id, v) VALUES(2, 'two');"},
			targetSQL:    []string{"INSERT INTO t(id, v) VALUES(3, 'three');"},
		},
		{
			id:           "TWIN-02",
			desc:         "WAL steady-state 2nd commit (synchronous=NORMAL)",
			mode:         sqlitex.JournalWAL,
			synchronous:  sqlitex.SyncNormal,
			secureDelete: true,
			autockpt:     1000,
			setupSQL:     []string{"CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT NOT NULL); INSERT INTO t(id, v) VALUES(1, 'one');"},
			preMarkerSQL: []string{"INSERT INTO t(id, v) VALUES(2, 'two');"},
			targetSQL:    []string{"INSERT INTO t(id, v) VALUES(3, 'three');"},
		},
		{
			id:           "TWIN-03",
			desc:         "rollback mode multi-page overflow INSERT (10KB BLOB)",
			mode:         sqlitex.JournalDelete,
			synchronous:  sqlitex.SyncFull,
			secureDelete: true,
			autockpt:     1000,
			setupSQL:     []string{"CREATE TABLE docs(id INTEGER PRIMARY KEY, body BLOB NOT NULL); INSERT INTO docs(id, body) VALUES(1, X'0102');"},
			targetSQL:    []string{"INSERT INTO docs(id, body) VALUES(2, " + blob10k + ");"},
		},
		{
			id:           "TWIN-04",
			desc:         "WAL mode multi-page overflow INSERT (10KB BLOB)",
			mode:         sqlitex.JournalWAL,
			synchronous:  sqlitex.SyncFull,
			secureDelete: true,
			autockpt:     1000,
			setupSQL:     []string{"CREATE TABLE docs(id INTEGER PRIMARY KEY, body BLOB NOT NULL); INSERT INTO docs(id, body) VALUES(1, X'0102');"},
			targetSQL:    []string{"INSERT INTO docs(id, body) VALUES(2, " + blob10k + ");"},
		},
		{
			id:           "TWIN-05",
			desc:         "rollback mode DELETE freeing 10KB overflow chain (secure_delete=ON)",
			mode:         sqlitex.JournalDelete,
			synchronous:  sqlitex.SyncFull,
			secureDelete: true,
			autockpt:     1000,
			setupSQL:     []string{"CREATE TABLE docs(id INTEGER PRIMARY KEY, body BLOB NOT NULL); INSERT INTO docs(id, body) VALUES(1, " + blob10k + "); INSERT INTO docs(id, body) VALUES(2, X'0102');"},
			targetSQL:    []string{"DELETE FROM docs WHERE id = 1;"},
		},
		{
			id:           "TWIN-06",
			desc:         "rollback mode explicit ROLLBACK after multi-page INSERT",
			mode:         sqlitex.JournalDelete,
			synchronous:  sqlitex.SyncFull,
			secureDelete: true,
			autockpt:     1000,
			setupSQL:     []string{"CREATE TABLE docs(id INTEGER PRIMARY KEY, body BLOB NOT NULL); INSERT INTO docs(id, body) VALUES(1, X'0102');"},
			targetSQL: []string{
				"BEGIN IMMEDIATE;",
				"INSERT INTO docs(id, body) VALUES(2, " + blob10k + ");",
				"ROLLBACK;",
			},
		},
		{
			id:           "TWIN-07",
			desc:         "WAL mode CREATE INDEX on 100-row table",
			mode:         sqlitex.JournalWAL,
			synchronous:  sqlitex.SyncFull,
			secureDelete: true,
			autockpt:     1000,
			setupSQL:     []string{bulkRows.String()},
			targetSQL:    []string{"CREATE INDEX idx_t_v ON t(v);"},
		},
		{
			id:           "TWIN-08",
			desc:         "WAL mode PRAGMA wal_checkpoint(TRUNCATE) after WAL write",
			mode:         sqlitex.JournalWAL,
			synchronous:  sqlitex.SyncFull,
			secureDelete: true,
			autockpt:     0,
			setupSQL:     []string{"CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT NOT NULL); INSERT INTO t(id, v) VALUES(1, 'one');"},
			preMarkerSQL: []string{"INSERT INTO t(id, v) VALUES(2, 'two');"},
			targetSQL:    []string{"PRAGMA wal_checkpoint(TRUNCATE);"},
		},
		{
			id:           "TWIN-09",
			desc:         "rollback mode VACUUM on fragmented 100-row table",
			mode:         sqlitex.JournalDelete,
			synchronous:  sqlitex.SyncFull,
			secureDelete: true,
			autockpt:     1000,
			setupSQL:     fragSetup,
			targetSQL:    []string{"VACUUM;"},
		},
		{
			id:           "TWIN-10",
			desc:         "rollback mode ANALYZE on indexed 100-row table (hunting ENABLE_STAT4 divergence)",
			mode:         sqlitex.JournalDelete,
			synchronous:  sqlitex.SyncFull,
			secureDelete: true,
			autockpt:     1000,
			setupSQL:     indexedSetup,
			targetSQL:    []string{"ANALYZE;"},
		},
		{
			id:           "TWIN-11",
			desc:         "rollback mode PRAGMA mmap_size=268435456 + SELECT + INSERT (hunting mmap divergence)",
			mode:         sqlitex.JournalDelete,
			synchronous:  sqlitex.SyncFull,
			secureDelete: true,
			autockpt:     1000,
			setupSQL:     []string{bulkRows.String()},
			targetSQL: []string{
				"PRAGMA mmap_size = 268435456;",
				"SELECT count(*), sum(length(v)) FROM t;",
				"INSERT INTO t(id, k, v) VALUES(201, 1, 'after-mmap');",
			},
		},
		{
			id:           "TWIN-12",
			desc:         "must-be-able-to-fail: Go synchronous=FULL vs CLI synchronous=NORMAL on rollback INSERT",
			mode:         sqlitex.JournalDelete,
			synchronous:  sqlitex.SyncFull,
			secureDelete: true,
			autockpt:     1000,
			setupSQL:     []string{"CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT NOT NULL); INSERT INTO t(id, v) VALUES(1, 'one');"},
			targetSQL:    []string{"INSERT INTO t(id, v) VALUES(2, 'two');"},
			cliSyncOver:  sqlitex.SyncNormal,
		},
	}
}

func report(id, tried, expect, got string, held bool) {
	verdict := "BROKEN"
	if held {
		verdict = "HELD"
	}
	fmt.Printf("ATTACK %s | %s | EXPECT %s | GOT %s | %s\n", id, tried, expect, got, verdict)
}

func fatal(what string, err error) {
	fmt.Fprintf(os.Stderr, "FATAL %s: %v\n", what, err)
	os.Exit(1)
}

func caseOptions(spec caseSpec) sqlitex.Options {
	o := sqlitex.DefaultOptions()
	o.File.JournalMode = spec.mode
	o.Conn.Synchronous = spec.synchronous
	o.Conn.SecureDelete = spec.secureDelete
	o.Conn.WALAutocheckpoint = spec.autockpt
	o.SingleConnection = true
	return o
}

func cliPrelude(o sqlitex.Options) string {
	onOff := func(b bool) string {
		if b {
			return "ON"
		}
		return "OFF"
	}
	c := o.Conn
	return fmt.Sprintf(
		"PRAGMA journal_mode=%s;\nPRAGMA synchronous=%s;\nPRAGMA foreign_keys=%s;\nPRAGMA secure_delete=%s;\nPRAGMA cache_size=%d;\nPRAGMA wal_autocheckpoint=%d;\nPRAGMA busy_timeout=%d;\n",
		o.File.JournalMode, c.Synchronous, onOff(c.ForeignKeys), onOff(c.SecureDelete),
		c.CacheSize, c.WALAutocheckpoint, c.BusyTimeout.Milliseconds(),
	)
}

func markerPath(dir, name string) string {
	return filepath.Join(dir, "marker-"+name)
}

func createMarkers(dir string) {
	for _, name := range []string{"1", "2"} {
		if err := os.WriteFile(markerPath(dir, name), nil, 0o644); err != nil {
			fatal("marker", err)
		}
	}
}

func touchMarker(dir, name string) {
	if _, err := os.ReadFile(markerPath(dir, name)); err != nil {
		fatal("touch marker", err)
	}
}

func createSetup(ctx context.Context, path string, spec caseSpec) {
	if err := recipe.RemoveDatabase(path); err != nil {
		fatal("remove", err)
	}
	db, err := sqlitex.Open(ctx, path, caseOptions(spec))
	if err != nil {
		fatal("setup open", err)
	}
	for _, stmt := range spec.setupSQL {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			db.Close()
			fatal("setup exec", err)
		}
	}
	if err := db.Close(); err != nil {
		fatal("setup close", err)
	}
	if err := recipe.CheckAtRest(path); err != nil {
		fatal("setup at rest", err)
	}
}

func pageHashes(ctx context.Context, path string, spec caseSpec) []string {
	db, err := sqlitex.Open(ctx, path, caseOptions(spec))
	if err != nil {
		fatal("open sqlite_dbpage", err)
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, "SELECT pgno, data FROM sqlite_dbpage ORDER BY pgno")
	if err != nil {
		fatal("sqlite_dbpage", err)
	}
	defer rows.Close()
	var hashes []string
	for rows.Next() {
		var pgno int
		var data []byte
		if err := rows.Scan(&pgno, &data); err != nil {
			fatal("scan sqlite_dbpage", err)
		}
		sum := sha256.Sum256(data)
		hashes = append(hashes, hex.EncodeToString(sum[:]))
	}
	if err := rows.Err(); err != nil {
		fatal("rows sqlite_dbpage", err)
	}
	return hashes
}

func changedPages(before, after []string) []int {
	var changed []int
	for i := range max(len(before), len(after)) {
		if i >= len(before) || i >= len(after) || before[i] != after[i] {
			changed = append(changed, i+1)
		}
	}
	return changed
}

func runChild(caseID, dir string) {
	runtime.LockOSThread()
	ctx := context.Background()
	var spec caseSpec
	found := false
	for _, c := range buildCases() {
		if c.id == caseID {
			spec = c
			found = true
			break
		}
	}
	if !found {
		fatal("child unknown case", fmt.Errorf("%s", caseID))
	}
	path := filepath.Join(dir, dbName)
	db, err := sqlitex.Open(ctx, path, caseOptions(spec))
	if err != nil {
		fatal("child open", err)
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		fatal("child conn", err)
	}
	for _, stmt := range spec.preMarkerSQL {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			fatal("child pre-marker", err)
		}
	}
	touchMarker(dir, "1")
	for _, stmt := range spec.targetSQL {
		if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(stmt)), "SELECT") ||
			strings.HasPrefix(strings.ToUpper(strings.TrimSpace(stmt)), "PRAGMA WAL_CHECKPOINT") {
			rows, err := conn.QueryContext(ctx, stmt)
			if err != nil {
				fatal("child target query", err)
			}
			for rows.Next() {
			}
			if err := rows.Err(); err != nil {
				fatal("child target rows", err)
			}
			rows.Close()
		} else {
			if _, err := conn.ExecContext(ctx, stmt); err != nil {
				fatal("child target exec", err)
			}
		}
	}
	touchMarker(dir, "2")
	conn.Close()
	if err := db.Close(); err != nil {
		fatal("child close", err)
	}
}

var fileSyscalls = []string{
	"open", "openat", "close", "pread64", "pwrite64", "read", "write",
	"fsync", "fdatasync", "ftruncate", "unlink", "unlinkat", "fcntl", "mmap",
}

func isMarkerOpen(call trace.Syscall, dir string) (string, bool) {
	if call.Name != "open" && call.Name != "openat" {
		return "", false
	}
	prefix := markerPath(dir, "")
	if !strings.HasPrefix(call.Path, prefix) {
		return "", false
	}
	return strings.TrimPrefix(call.Path, prefix), true
}

func isMarkerFile(call trace.Syscall, dir string) bool {
	return strings.HasPrefix(call.Path, markerPath(dir, ""))
}

func isDatabaseIO(call trace.Syscall, dir string) bool {
	if !slices.Contains(fileSyscalls, call.Name) || isMarkerFile(call, dir) {
		return false
	}
	return call.Path == dir || strings.HasPrefix(call.Path, dir+string(filepath.Separator))
}

func segmentCalls(calls []trace.Syscall, dir string) ([]trace.Syscall, error) {
	start, end := -1, -1
	for i, call := range calls {
		name, ok := isMarkerOpen(call, dir)
		if !ok {
			continue
		}
		if name == "1" && start < 0 {
			start = i
		}
		if name == "2" && start >= 0 && end < 0 {
			end = i
		}
	}
	if start < 0 || end < 0 {
		return nil, fmt.Errorf("markers 1..2 not found")
	}
	var out []trace.Syscall
	for _, call := range calls[start+1 : end] {
		if isDatabaseIO(call, dir) {
			out = append(out, call)
		}
	}
	return trace.RelativeTo(dir, out), nil
}

func isSync(c trace.Syscall) bool { return c.Name == "fsync" || c.Name == "fdatasync" }
func isWrite(c trace.Syscall) bool {
	return c.Name == "pwrite64" || c.Name == "write" || c.Name == "ftruncate"
}
func isCloexec(c trace.Syscall) bool {
	return c.Name == "fcntl" && c.Lock == nil && strings.HasPrefix(c.Flags, "F_SETFD")
}

func normalized(call trace.Syscall) string {
	switch {
	case call.Name == "open" || call.Name == "openat":
		if call.Errno != "" {
			return "open " + call.Path + " = " + call.Errno
		}
		return "open " + call.Path
	case isSync(call):
		return "sync " + call.Path
	case call.Name == "unlink" || call.Name == "unlinkat":
		return "unlink " + call.Path
	case call.Name == "pwrite64" || call.Name == "pread64":
		return fmt.Sprintf("%s %s off=%d len=%d", call.Name, call.Path, call.Offset, call.Length)
	case call.Name == "ftruncate":
		return fmt.Sprintf("ftruncate %s len=%d", call.Path, call.Length)
	case call.Name == "mmap":
		return fmt.Sprintf("mmap %s off=%d len=%d", call.Path, call.Offset, call.Length)
	case call.Name == "fcntl" && call.Lock != nil:
		return fmt.Sprintf("fcntl %s %s %s start=%d len=%d", call.Path, call.Lock.Command, call.Lock.Type, call.Lock.Start, call.Lock.Length)
	case call.Name == "fcntl":
		return fmt.Sprintf("fcntl %s %s", call.Path, call.Flags)
	}
	return call.Name + " " + call.Path
}

func normFiltered(calls []trace.Syscall, keep func(trace.Syscall) bool) []string {
	var out []string
	for _, c := range calls {
		if keep == nil || keep(c) {
			out = append(out, normalized(c))
		}
	}
	return out
}

func firstDiff(a, b []string) string {
	for i := range max(len(a), len(b)) {
		var x, y string
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			return fmt.Sprintf("#%d Go=%q CLI=%q", i+1, x, y)
		}
	}
	return "none"
}

func buildCLIScript(dir string, spec caseSpec) string {
	o := caseOptions(spec)
	if spec.cliSyncOver != "" {
		o.Conn.Synchronous = spec.cliSyncOver
	}
	var b strings.Builder
	b.WriteString(cliPrelude(o))
	for _, s := range spec.preMarkerSQL {
		b.WriteString(s + "\n")
	}
	b.WriteString(".read '" + markerPath(dir, "1") + "'\n")
	for _, s := range spec.targetSQL {
		b.WriteString(s + "\n")
	}
	b.WriteString(".read '" + markerPath(dir, "2") + "'\n")
	return b.String()
}

func main() {
	if len(os.Args) == 4 && os.Args[1] == "child" {
		runChild(os.Args[2], os.Args[3])
		return
	}
	if !trace.Supported {
		fmt.Println("ATTACK TWIN-00 | strace twin-io suite | EXPECT Linux strace | GOT non-Linux platform | BROKEN")
		return
	}
	ctx := context.Background()
	repoRoot, err := sqlitex.RepoRoot()
	if err != nil {
		fatal("repo root", err)
	}
	cli, err := sqlitex.LocateCLI(ctx)
	if err != nil {
		fatal("locate CLI", err)
	}
	scratch := filepath.Join(repoRoot, "_work", "adv-ch01", "twin-io")
	if err := os.RemoveAll(scratch); err != nil {
		fatal("clean scratch", err)
	}

	for _, spec := range buildCases() {
		dir := filepath.Join(scratch, spec.id)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fatal("mkdir", err)
		}
		createMarkers(dir)
		dbPath := filepath.Join(dir, dbName)

		createSetup(ctx, dbPath, spec)
		beforePages := pageHashes(ctx, dbPath, spec)

		// Run Go child under strace.
		cmd, err := trace.SelfCommand("child", spec.id, dir)
		if err != nil {
			fatal("self command", err)
		}
		goRun, err := trace.Run(ctx, trace.Options{Command: cmd})
		if err != nil {
			fatal("strace Go "+spec.id, err)
		}
		if goRun.ExitCode != 0 {
			fatal("Go child "+spec.id, fmt.Errorf("exit %d: %s", goRun.ExitCode, goRun.Stderr))
		}
		goCalls, err := segmentCalls(goRun.Log.Calls, dir)
		if err != nil {
			fatal("Go segment "+spec.id, err)
		}
		afterPages := pageHashes(ctx, dbPath, spec)
		changed := changedPages(beforePages, afterPages)
		goHash, err := recipe.FileSHA256(dbPath)
		if err != nil {
			fatal("Go hash "+spec.id, err)
		}

		// Rebuild base and run C CLI under strace.
		createSetup(ctx, dbPath, spec)
		cliCmd := cli.Command(ctx, sqlitex.CLIRun{}).Args
		cliCmd[0] = cli.Path
		cliRun, err := trace.Run(ctx, trace.Options{
			Command: cliCmd,
			Dir:     dir,
			Stdin:   strings.NewReader(sqlitex.OpenScript(dbPath, buildCLIScript(dir, spec))),
		})
		if err != nil {
			fatal("strace CLI "+spec.id, err)
		}
		if cliRun.ExitCode != 0 {
			fatal("CLI "+spec.id, fmt.Errorf("exit %d: %s", cliRun.ExitCode, cliRun.Stderr))
		}
		cliCalls, err := segmentCalls(cliRun.Log.Calls, dir)
		if err != nil {
			fatal("CLI segment "+spec.id, err)
		}
		cliHash, err := recipe.FileSHA256(dbPath)
		if err != nil {
			fatal("CLI hash "+spec.id, err)
		}

		goWrites := normFiltered(goCalls, isWrite)
		cliWrites := normFiltered(cliCalls, isWrite)
		goSyncs := normFiltered(goCalls, isSync)
		cliSyncs := normFiltered(cliCalls, isSync)
		notClo := func(c trace.Syscall) bool { return !isCloexec(c) }
		goOther := normFiltered(goCalls, notClo)
		cliOther := normFiltered(cliCalls, notClo)

		writesMatch := slices.Equal(goWrites, cliWrites)
		syncsMatch := slices.Equal(goSyncs, cliSyncs)
		otherMatch := slices.Equal(goOther, cliOther)
		hashMatch := goHash == cliHash

		dbWrites := 0
		dbWriteOffsetsOK := true
		walWrites := 0
		for _, c := range goCalls {
			if c.Name == "pwrite64" && c.Path == dbName {
				dbWrites++
				pg := int(c.Offset/pageSize) + 1
				if c.Offset%pageSize != 0 || c.Length != pageSize || !slices.Contains(changed, pg) {
					dbWriteOffsetsOK = false
				}
			}
			if c.Name == "pwrite64" && c.Path == dbName+"-wal" {
				walWrites++
			}
		}

		switch spec.id {
		case "TWIN-01":
			// Steady-state 2nd WAL commit at FULL: 0 db writes, 2 WAL writes (at 4152+24, 4176+4096), 1 sync, full twin parity.
			wantWAL := []string{"pwrite64 t.db-wal off=4152 len=24", "pwrite64 t.db-wal off=4176 len=4096"}
			walOK := slices.Equal(goWrites, wantWAL)
			held := dbWrites == 0 && walOK && len(goSyncs) == 1 && writesMatch && syncsMatch && otherMatch && hashMatch
			report(
				spec.id,
				spec.desc,
				"0 db writes, WAL writes at 4152+24 & 4176+4096, 1 sync, Go==CLI syscalls & sha256",
				fmt.Sprintf("dbWrites=%d goWrites=%v syncs=%d writesEq=%v syncsEq=%v allEq=%v hashEq=%v",
					dbWrites, goWrites, len(goSyncs), writesMatch, syncsMatch, otherMatch, hashMatch),
				held,
			)
		case "TWIN-02":
			// Steady-state 2nd WAL commit at NORMAL: 0 db writes, 0 syncs, Go==CLI.
			held := dbWrites == 0 && walWrites == 2 && len(goSyncs) == 0 && writesMatch && syncsMatch && otherMatch && hashMatch
			report(
				spec.id,
				spec.desc,
				"0 db writes, 2 WAL writes, 0 syncs on commit, Go==CLI syscalls & sha256",
				fmt.Sprintf("dbWrites=%d walWrites=%d syncs=%d writesEq=%v syncsEq=%v allEq=%v hashEq=%v",
					dbWrites, walWrites, len(goSyncs), writesMatch, syncsMatch, otherMatch, hashMatch),
				held,
			)
		case "TWIN-03":
			// Multi-page overflow INSERT in rollback mode: all db writes page-aligned for changed pages, Go==CLI.
			held := dbWrites == len(changed) && dbWriteOffsetsOK && writesMatch && syncsMatch && otherMatch && hashMatch
			report(
				spec.id,
				spec.desc,
				"db pwrite64 offsets match (page-1)*4096 for every changed page, Go==CLI syscalls & sha256",
				fmt.Sprintf("changed=%v dbWrites=%d offsetsOK=%v writesEq=%v syncsEq=%v allEq=%v hashEq=%v",
					changed, dbWrites, dbWriteOffsetsOK, writesMatch, syncsMatch, otherMatch, hashMatch),
				held,
			)
		case "TWIN-04":
			// Multi-page overflow INSERT in WAL mode: 0 db writes, frames to WAL, Go==CLI.
			held := dbWrites == 0 && walWrites > 2 && writesMatch && syncsMatch && otherMatch && hashMatch
			report(
				spec.id,
				spec.desc,
				"0 db writes, multi-frame WAL writes, Go==CLI syscalls & sha256",
				fmt.Sprintf("changed=%v dbWrites=%d walWrites=%d writesEq=%v syncsEq=%v allEq=%v hashEq=%v",
					changed, dbWrites, walWrites, writesMatch, syncsMatch, otherMatch, hashMatch),
				held,
			)
		case "TWIN-12":
			// Must-be-able-to-fail: Go FULL vs CLI NORMAL must differ in syncs.
			detected := !syncsMatch
			report(
				spec.id,
				spec.desc,
				"twin check detects sync mismatch between Go(FULL) and CLI(NORMAL)",
				fmt.Sprintf("goSyncs=%d cliSyncs=%d mismatchDetected=%v", len(goSyncs), len(cliSyncs), detected),
				detected,
			)
		default:
			// General twin-check cases (TWIN-05..TWIN-11): compare writes, syncs, non-F_SETFD syscalls, and file bytes.
			// For TWIN-09 (VACUUM), temp files live outside dir; check db-dir writes, syncs, and exact final bytes.
			held := writesMatch && syncsMatch && otherMatch && hashMatch
			extra := ""
			if !writesMatch {
				extra += " writeDiff(" + firstDiff(goWrites, cliWrites) + ")"
			}
			if !syncsMatch {
				extra += " syncDiff(" + firstDiff(goSyncs, cliSyncs) + ")"
			}
			if writesMatch && syncsMatch && !otherMatch {
				extra += " syscallDiff(" + firstDiff(goOther, cliOther) + ")"
			}
			report(
				spec.id,
				spec.desc,
				"Go driver and C CLI issue identical writes, syncs, non-F_SETFD syscalls and leave byte-identical DB",
				fmt.Sprintf("goWrites=%d cliWrites=%d goSyncs=%d cliSyncs=%d writesEq=%v syncsEq=%v allEq=%v hashEq=%v%s",
					len(goWrites), len(cliWrites), len(goSyncs), len(cliSyncs),
					writesMatch, syncsMatch, otherMatch, hashMatch, extra),
				held,
			)
		}
	}
}
