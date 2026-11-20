package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/nickstrad/sqlite-internals/internal/recipe"
	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
)

func buildWithCache(ctx context.Context, r recipe.Recipe, path string, cacheSize int) ([]recipe.FileHashes, error) {
	if err := recipe.RemoveDatabase(path); err != nil {
		return nil, err
	}
	plan, err := r.Plan()
	if err != nil {
		return nil, err
	}
	o := r.SQLiteOptions()
	o.Conn.CacheSize = cacheSize
	var hashes []recipe.FileHashes
	for _, s := range plan {
		db, err := sqlitex.Open(ctx, path, o)
		if err != nil {
			return nil, err
		}
		if err := recipe.ApplyStep(ctx, db, s); err != nil {
			return nil, err
		}
		if err := db.Close(); err != nil {
			return nil, err
		}
		h, err := recipe.HashFile(path)
		if err != nil {
			return nil, err
		}
		hashes = append(hashes, h)
	}
	return hashes, nil
}

func main() {
	ctx := context.Background()
	dir, err := sqlitex.WorkDir("ch01-repair-probe")
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	r := recipe.Default()
	want, err := r.StepHashes(ctx)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	for _, cs := range []int{0, 1, 10, -2000} {
		for run := range 3 {
			got, err := buildWithCache(ctx, r, filepath.Join(dir, fmt.Sprintf("cs%d.db", cs)), cs)
			if err != nil {
				fmt.Println(err)
				os.Exit(1)
			}
			first := -1
			for k := range got {
				if got[k] != want[k] {
					first = k
					break
				}
			}
			maskedOK := true
			for k := range got {
				if got[k].Masked != want[k].Masked {
					maskedOK = false
				}
			}
			fmt.Printf("cache_size=%d run %d: first exact difference at step %d, masked all equal %v\n", cs, run+1, first, maskedOK)
		}
	}
}

func init() {
	if len(os.Args) == 2 && os.Args[1] == "counters" {
		ctx := context.Background()
		dir, _ := sqlitex.WorkDir("ch01-repair-probe-counters")
		r := recipe.Default().WithSteps(2)
		for _, cs := range []int{-2000, 0} {
			path := filepath.Join(dir, fmt.Sprintf("cs%d.db", cs))
			o := r.SQLiteOptions()
			o.Conn.CacheSize = cs
			recipe.RemoveDatabase(path)
			db, err := sqlitex.Open(ctx, path, o)
			if err != nil {
				fmt.Println(err)
				os.Exit(1)
			}
			db.Close()
			image, _ := os.ReadFile(path)
			fmt.Printf("cache_size=%d after Open+Close of a new file: change counter %v version-valid-for %v size %d\n", cs, image[24:28], image[92:96], len(image))
			recipe.RemoveDatabase(path)
			plan, _ := r.Plan()
			for _, s := range plan {
				db, err := sqlitex.Open(ctx, path, o)
				if err != nil {
					fmt.Println(err)
					os.Exit(1)
				}
				if err := recipe.ApplyStep(ctx, db, s); err != nil {
					fmt.Println(err)
					os.Exit(1)
				}
				db.Close()
				image, _ := os.ReadFile(path)
				fmt.Printf("cache_size=%d after step %d: change counter %v\n", cs, s.Number, image[24:28])
			}
		}
		os.Exit(0)
	}
}

func init() {
	if len(os.Args) == 2 && os.Args[1] == "cli-order" {
		ctx := context.Background()
		dir, _ := sqlitex.WorkDir("ch01-repair-probe-cli")
		r := recipe.Default()
		want, err := r.StepHashes(ctx)
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		cli, err := sqlitex.LocateCLI(ctx)
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		o := r.SQLiteOptions()
		layout := fmt.Sprintf("PRAGMA page_size=%d;\nPRAGMA auto_vacuum=%s;\nPRAGMA encoding='%s';\n", o.File.PageSize, o.File.AutoVacuum, o.File.Encoding)
		identity := fmt.Sprintf("PRAGMA application_id=%d;\nPRAGMA user_version=%d;\n", o.File.ApplicationID, o.File.UserVersion)
		journal := "PRAGMA journal_mode=wal;\n"
		conn := fmt.Sprintf("PRAGMA synchronous=%s;\nPRAGMA foreign_keys=ON;\nPRAGMA secure_delete=ON;\nPRAGMA cache_size=%d;\nPRAGMA wal_autocheckpoint=%d;\n", o.Conn.Synchronous, o.Conn.CacheSize, o.Conn.WALAutocheckpoint)
		step0 := "BEGIN IMMEDIATE;\n"
		for _, sql := range recipe.SchemaSQL {
			step0 += sql + ";\n"
		}
		step0 += "PRAGMA user_version = 0;\nCOMMIT;\n"
		for name, prelude := range map[string]string{"old order (identity, then journal_mode)": layout + identity + journal, "new order (journal_mode, then identity)": layout + journal + identity} {
			path := filepath.Join(dir, "step0.db")
			recipe.RemoveDatabase(path)
			if _, err := cli.Exec(ctx, path, "PRAGMA busy_timeout=5000;\n"+prelude+conn+step0); err != nil {
				fmt.Println(err)
				os.Exit(1)
			}
			h, _ := recipe.HashFile(path)
			fmt.Printf("CLI step 0 with %s: equals the Go step-0 hash: %v\n", name, h == want[0])
		}
		os.Exit(0)
	}
}
