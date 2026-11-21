package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/nickstrad/sqlite-internals/internal/procs"
	"github.com/nickstrad/sqlite-internals/internal/recipe"
	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
	"github.com/nickstrad/sqlite-internals/internal/walfile"
)

func shm(lab *procs.Lab, db, label string) {
	index, err := walfile.ParseIndexFile(db + "-shm")
	if err != nil {
		lab.Fatal("shm", err)
	}
	h := index.Header()
	fmt.Printf("%-34s mxFrame=%d salt1=0x%08x %s\n", label, h.MaxFrame, h.Salt1, index.Checkpoint.String())
}

func main() {
	if procs.Serve(procs.Roles) {
		return
	}
	ctx := context.Background()
	dir := sqlitex.MustWorkDir("adv-ch06-probe-snap04")
	db := filepath.Join(dir, "r.db")
	if err := recipe.Default().Build(ctx, db, 5); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	lab := procs.NewLab(os.Stdout)
	writer := lab.SpawnSQL("writer", procs.SQLOptions{Path: db, JournalMode: sqlitex.JournalWAL, WALAutocheckpoint: 20})
	capture := lab.SpawnSQL("capture", procs.SQLOptions{Path: db, JournalMode: sqlitex.JournalWAL})
	lab.Must(writer, "recipe-step", "6")
	shm(lab, db, "after step 6 (autocheckpoint 20)")
	lab.Must(capture, "begin", "deferred")
	lab.Must(capture, "user-version")
	locks := lab.Locks("-shm", db+"-shm", procs.ShmLockRanges())
	lab.Check(procs.ReadLocked(locks.Ranges, "WAL_READ_LOCK(0)"), "the capture holds WAL_READ_LOCK(0)")
	shm(lab, db, "capture pinned")
	lab.Must(writer, "recipe-step", "7")
	shm(lab, db, "after step 7")
	lab.Must(capture, "commit")
	lab.Must(writer, "checkpoint", "PASSIVE")
	lab.Must(writer, "recipe-step", "8")
	shm(lab, db, "after capture ended, ckpt, step 8")
	lab.StopChildren()
	lab.Report("SNAP04 PROBE")
}
