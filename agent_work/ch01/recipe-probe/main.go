package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/nickstrad/sqlite-internals/internal/recipe"
	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
)

func oneConnection(ctx context.Context, r recipe.Recipe, path string, change func(*sqlitex.Options)) recipe.FileHashes {
	recipe.RemoveDatabase(path)
	o := r.SQLiteOptions()
	change(&o)
	db, err := sqlitex.Open(ctx, path, o)
	if err != nil {
		panic(err)
	}
	plan, _ := r.Plan()
	for _, s := range plan {
		if err := recipe.ApplyStep(ctx, db, s); err != nil {
			panic(err)
		}
	}
	db.Close()
	h, _ := recipe.HashFile(path)
	return h
}

func perSession(ctx context.Context, r recipe.Recipe, path string, change func(*sqlitex.Options)) recipe.FileHashes {
	recipe.RemoveDatabase(path)
	o := r.SQLiteOptions()
	change(&o)
	plan, _ := r.Plan()
	for _, s := range plan {
		db, err := sqlitex.Open(ctx, path, o)
		if err != nil {
			panic(err)
		}
		if err := recipe.ApplyStep(ctx, db, s); err != nil {
			panic(err)
		}
		db.Close()
	}
	h, _ := recipe.HashFile(path)
	return h
}

func main() {
	ctx := context.Background()
	dir := sqlitex.MustWorkDir("ch01-recipe-probe")
	for _, mode := range []string{"wal", "delete"} {
		r := recipe.Default().WithJournalMode(mode)
		start := time.Now()
		path := filepath.Join(dir, mode+".db")
		if err := r.Build(ctx, path, r.Steps); err != nil {
			panic(err)
		}
		info, _ := os.Stat(path)
		h, _ := recipe.HashFile(path)
		fmt.Printf("%s Build (session per step): %v size=%d pages=%d hash=%s\n", mode, time.Since(start).Round(time.Millisecond), info.Size(), info.Size()/4096, h.Exact[:16])
		for _, variant := range []struct {
			name string
			f    func(*sqlitex.Options)
		}{
			{"recipe-options", func(o *sqlitex.Options) {}},
			{"autockpt0", func(o *sqlitex.Options) { o.Conn.WALAutocheckpoint = 0 }},
			{"autockpt1", func(o *sqlitex.Options) { o.Conn.WALAutocheckpoint = 1 }},
			{"cache5", func(o *sqlitex.Options) { o.Conn.CacheSize = 5 }},
			{"syncnormal", func(o *sqlitex.Options) { o.Conn.Synchronous = "NORMAL" }},
			{"nosecuredelete", func(o *sqlitex.Options) { o.Conn.SecureDelete = false }},
		} {
			a := oneConnection(ctx, r, filepath.Join(dir, mode+variant.name+"-one.db"), variant.f)
			b := perSession(ctx, r, filepath.Join(dir, mode+variant.name+"-per.db"), variant.f)
			fmt.Printf("  %-15s one-conn exact=%v masked=%v | per-session exact=%v masked=%v\n", variant.name, a.Exact == h.Exact, a.Masked == h.Masked, b.Exact == h.Exact, b.Masked == h.Masked)
		}
	}
}
