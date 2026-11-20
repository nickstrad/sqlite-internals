// Fault-injection feasibility over the real os_unix.c (cgo/sqlitec/faultvfs.c shim):
//  1. fail the Nth xWrite / Nth xSync of a WAL commit and show the error SQLite returns;
//  2. for every xSync position k of a WAL commit, run a child that _exit(9)s at the start of
//     xSync #k, then reopen with the plain unix VFS and check integrity + committed rows.
// Usage: go run ./cgo/cmd/fault
package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"

	"sqliteinternals/probes/goaccessvfs/cgo/sqlitec"
)

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

func setup(path string) {
	db, err := sqlitec.Open(path, "")
	must(err)
	must(db.Exec("pragma journal_mode=wal; create table t(x); insert into t values('base');"))
	must(db.Close())
}

// commit opens through the fault VFS and runs a single-row commit; returns the error.
func commit(path string, failWrite, failSync, exitSync int) (error, int, int) {
	must(sqlitec.RegisterFaultVfs("fault", false))
	db, err := sqlitec.Open(path, "fault")
	must(err)
	sqlitec.SetFaults(failWrite, failSync, exitSync)
	err = db.Exec("pragma synchronous=full; insert into t values('new')")
	w, s := sqlitec.FaultCounts()
	db.Close()
	return err, w, s
}

func check(path string) string {
	db, err := sqlitec.Open(path, "")
	must(err)
	defer db.Close()
	ic, err := db.Query("pragma integrity_check")
	must(err)
	rows, err := db.Query("select group_concat(x) from t")
	must(err)
	return fmt.Sprintf("integrity=%v rows=%v", ic[0][0], rows[0][0])
}

func main() {
	if len(os.Args) == 3 && os.Args[1] == "child" {
		k, _ := strconv.Atoi(os.Args[2])
		commit(os.Getenv("DB"), 0, 0, k)
		os.Exit(0)
	}
	dir, _ := os.MkdirTemp("", "fault")
	defer os.RemoveAll(dir)
	fmt.Println("source_id:", sqlitec.SourceID())

	p := filepath.Join(dir, "count.db")
	setup(p)
	err, w, s := commit(p, 0, 0, 0)
	fmt.Printf("baseline WAL commit: err=%v xWrite=%d xSync=%d\n", err, w, s)

	for n := 1; n <= w; n++ {
		p := filepath.Join(dir, fmt.Sprintf("w%d.db", n))
		setup(p)
		err, _, _ := commit(p, n, 0, 0)
		fmt.Printf("fail xWrite #%d -> %v ; after: %s\n", n, err, check(p))
	}
	for n := 1; n <= s; n++ {
		p := filepath.Join(dir, fmt.Sprintf("s%d.db", n))
		setup(p)
		err, _, _ := commit(p, 0, n, 0)
		fmt.Printf("fail xSync #%d -> %v ; after: %s\n", n, err, check(p))
	}
	self, _ := os.Executable()
	for k := 1; k <= s; k++ {
		p := filepath.Join(dir, fmt.Sprintf("k%d.db", k))
		setup(p)
		cmd := exec.Command(self, "child", strconv.Itoa(k))
		cmd.Env = append(os.Environ(), "DB="+p)
		err := cmd.Run()
		fmt.Printf("child _exit at xSync #%d -> child %v ; reopen: %s\n", k, err, check(p))
	}
}
