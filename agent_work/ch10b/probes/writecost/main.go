package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime/pprof"
	"time"

	"github.com/nickstrad/walfleet/ch10-multi-database-platform-capstone/service/catalog"
	"github.com/nickstrad/walfleet/ch10-multi-database-platform-capstone/service/node"
	"github.com/nickstrad/walfleet/ch10-multi-database-platform-capstone/service/s3store"
	"github.com/nickstrad/walfleet/internal/rangefs"
	"github.com/nickstrad/walfleet/internal/recipe"
	"github.com/nickstrad/walfleet/internal/sqlitex"
)

func main() {
	ctx := context.Background()
	work, _ := os.MkdirTemp("", "writecost")
	defer os.RemoveAll(work)
	store, err := s3store.Connect(ctx, rangefs.S3ConfigFromEnv(), "course-ch10b", "probe-writecost")
	if err != nil {
		panic(err)
	}
	store.WipeInBatches(ctx)
	cat, _ := catalog.Open(ctx, filepath.Join(work, "catalog.db"))
	workload := recipe.Default()
	n, err := node.Start(node.Config{Name: "a", Dir: filepath.Join(work, "a"), Catalog: cat, Store: store, Capacity: 32, File: workload.SQLiteOptions().File,
		Create: func(ctx context.Context, path string) error { return workload.Build(ctx, path, 0) }})
	if err != nil {
		panic(err)
	}
	f, _ := os.Create("/tmp/writecost.pprof")
	pprof.StartCPUProfile(f)
	var claim, write time.Duration
	for i := range 20 {
		id := fmt.Sprintf("db-%04d", i)
		cat.Register(ctx, id, node.PlatformSettings())
		t := time.Now()
		if _, err := n.Claim(ctx, id); err != nil {
			panic(err)
		}
		claim += time.Since(t)
		for range 4 {
			t = time.Now()
			if _, err := n.Write(ctx, id, node.StoreDurable, func(ctx context.Context, db *sqlitex.DB) error { _, err := workload.ApplyNext(ctx, db); return err }); err != nil {
				panic(err)
			}
			write += time.Since(t)
		}
	}
	pprof.StopCPUProfile()
	fmt.Println("claim avg", claim/20, "write avg", write/80)
	n.Close()
	store.WipeInBatches(ctx)
}
