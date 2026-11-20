// Logs every VFS call SQLite makes (through the tree's own ext/misc/vfstrace.c, which wraps the
// real os_unix.c "unix" VFS) for: open, journal_mode=WAL, create, insert-commit, checkpoint, close.
// Usage: vfstrace [dir]   (default: a fresh temp dir). Lines go to stdout, prefixed by phase.
package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"sqliteinternals/probes/goaccessvfs/cgo/sqlitec"
)

func main() {
	dir, _ := os.MkdirTemp("", "vfstrace")
	defer os.RemoveAll(dir)
	if len(os.Args) > 1 {
		dir = os.Args[1]
	}
	phase := "setup"
	sqlitec.TraceLine = func(s string) {
		// Strip the absolute temp dir so output is stable between runs.
		fmt.Printf("%-10s %s\n", phase, strings.ReplaceAll(s, dir+"/", ""))
	}
	if err := sqlitec.RegisterVfsTrace("trace", false); err != nil {
		log.Fatal(err)
	}
	db, err := sqlitec.Open(filepath.Join(dir, "t.db"), "trace")
	if err != nil {
		log.Fatal(err)
	}
	step := func(p, sql string) {
		phase = p
		if err := db.Exec(sql); err != nil {
			log.Fatalf("%s: %v", p, err)
		}
	}
	step("wal", "pragma journal_mode=wal")
	step("create", "create table t(x)")
	step("insert", "insert into t values(randomblob(100))")
	phase = "checkpoint"
	nLog, nCkpt, err := db.CheckpointV2("main", 0)
	fmt.Printf("checkpoint nLog=%d nCkpt=%d err=%v\n", nLog, nCkpt, err)
	phase = "close"
	db.Close()
	fmt.Println("source_id:", sqlitec.SourceID())
}
