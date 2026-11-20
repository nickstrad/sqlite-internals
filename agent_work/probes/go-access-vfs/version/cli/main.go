// Drives the pinned sqlite3 CLI from Go via os/exec: version check with -json, the CLI's own
// -vfstrace (same ext/misc/vfstrace.c, over os_unix.c), .filectrl, and stats.
// Usage: go run ./version/cli
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"sqliteinternals/probes/goaccessvfs/internal/pin"
)

func run(args ...string) string {
	b, err := exec.Command("sqlite3", args...).CombinedOutput()
	if err != nil {
		log.Fatalf("%v: %s", err, b)
	}
	return string(b)
}

func main() {
	var rows []map[string]any
	if err := json.Unmarshal([]byte(run("-json", ":memory:", "select sqlite_version() v, sqlite_source_id() id")), &rows); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("route=cli\nsqlite_version=%v\nsqlite_source_id=%v\nmatches_pin=%v\n", rows[0]["v"], rows[0]["id"], rows[0]["id"] == pin.SourceID)
	opts := run("-batch", ":memory:", "select group_concat(compile_options,' ') from pragma_compile_options where compile_options like 'ENABLE_SNAPSHOT' or compile_options like 'ENABLE_DB%' or compile_options like 'ENABLE_SESSION' or compile_options like 'ENABLE_PREUPDATE%'")
	fmt.Println("compile_options(selected)=", strings.TrimSpace(opts))

	dir, _ := os.MkdirTemp("", "cli")
	defer os.RemoveAll(dir)
	db := filepath.Join(dir, "t.db")
	run(db, "pragma journal_mode=wal; create table t(x);")
	// -vfstrace writes to stderr; one invocation = one process = open..close.
	tr := run("-vfstrace", db, "insert into t values(randomblob(100));")
	var commitLines []string
	for _, l := range strings.Split(tr, "\n") {
		if strings.Contains(l, "xWrite") || strings.Contains(l, "xSync") || strings.Contains(l, "WRITE)") {
			commitLines = append(commitLines, strings.ReplaceAll(l, dir+"/", ""))
		}
	}
	fmt.Printf("-vfstrace: %d lines total; write/sync/WAL-write-lock lines:\n  %s\n", strings.Count(tr, "\n"), strings.Join(commitLines, "\n  "))
	fmt.Print(".filectrl data_version / persist_wal:\n", run(db, ".filectrl data_version", ".filectrl persist_wal"))
	// Two statements in one process, with a checkpoint result row (busy, log, checkpointed):
	fmt.Print("pragma wal_checkpoint via CLI (busy|log|checkpointed): ", run(db, "pragma wal_autocheckpoint=0; insert into t values(1); pragma wal_checkpoint(passive);"))
	fmt.Print(".stats excerpt:\n", firstLines(run("-cmd", ".stats on", db, "select count(*) from t;"), 6))
}

func firstLines(s string, n int) string {
	l := strings.SplitAfter(s, "\n")
	if len(l) > n {
		l = l[:n]
	}
	return strings.Join(l, "")
}
