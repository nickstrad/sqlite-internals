package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/nickstrad/walfleet/internal/recipe"
	"github.com/nickstrad/walfleet/internal/sqlitex"
	"github.com/nickstrad/walfleet/internal/walfile"

	_ "modernc.org/sqlite"
)

func main() {
	if walfile.ServeLiveCopyChild(os.Args) {
		return
	}
	ctx := context.Background()
	dir := filepath.Join(sqlitex.MustWorkDir("repair-ro"))
	path := filepath.Join(dir, "r.db")
	r := recipe.Default()
	if err := r.Build(ctx, path, 5); err != nil {
		panic(err)
	}
	options := r.SQLiteOptions()
	options.Conn.WALAutocheckpoint = 0
	writer, err := sqlitex.Open(ctx, path, options)
	if err != nil {
		panic(err)
	}
	for k := 6; k <= 7; k++ {
		if err := r.ApplyStep(ctx, writer, k); err != nil {
			panic(err)
		}
	}
	report := func(label string) {
		state, err := walfile.ReadLiveState(ctx, path, dir)
		if err != nil {
			panic(err)
		}
		fmt.Printf("%s: mxFrame=%d nBackfill=%d\n", label, state.Index.Header().MaxFrame, state.Index.Checkpoint.Backfill)
	}
	report("before")
	roOptions := r.SQLiteOptions()
	roOptions.ReadOnly = true
	roOptions.ImmediateTx = false
	roOptions.SingleConnection = true
	ro, err := sqlitex.Open(ctx, path, roOptions)
	if err != nil {
		panic(err)
	}
	result, err := sqlitex.Checkpoint(ctx, ro, "", sqlitex.CheckpointPassive)
	fmt.Printf("mode=ro PASSIVE with nothing backfilled: %s err=%v\n", result, err)
	report("after ro checkpoint")
	result, err = sqlitex.Checkpoint(ctx, writer, "", sqlitex.CheckpointPassive)
	fmt.Printf("writer PASSIVE: %s err=%v\n", result, err)
	report("after writer checkpoint")
	result, err = sqlitex.Checkpoint(ctx, ro, "", sqlitex.CheckpointPassive)
	fmt.Printf("mode=ro PASSIVE after full backfill: %s err=%v\n", result, err)
	ro.Close()
	writer.Close()
}
