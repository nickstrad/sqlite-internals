package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/nickstrad/walfleet/internal/recipe"
	"github.com/nickstrad/walfleet/internal/sqlitex"
	"github.com/nickstrad/walfleet/internal/trace"
)

func main() {
	ctx := context.Background()
	r := recipe.Default().WithJournalMode(sqlitex.JournalDelete)
	if len(os.Args) == 4 && os.Args[1] == "child" {
		runtime.LockOSThread()
		k, _ := strconv.Atoi(os.Args[3])
		db, err := r.Open(ctx, os.Args[2])
		if err != nil {
			panic(err)
		}
		if err := r.ApplyStep(ctx, db, k); err != nil {
			panic(err)
		}
		db.Close()
		return
	}
	dir := sqlitex.MustWorkDir("ch04-step-probe")
	for k := 1; k <= 30; k++ {
		path := filepath.Join(dir, fmt.Sprintf("s%d.db", k))
		if err := r.Build(ctx, path, k-1); err != nil {
			panic(err)
		}
		cmd, _ := trace.SelfCommand("child", path, strconv.Itoa(k))
		res, err := trace.Run(ctx, trace.Options{Command: cmd})
		if err != nil {
			panic(err)
		}
		counts := map[string]int{}
		for _, c := range res.Log.Calls {
			if !strings.HasPrefix(c.Path, dir) {
				continue
			}
			key := c.Name + " " + filepath.Base(c.Path)
			counts[key]++
		}
		fmt.Printf("step %d: jw=%d dbw=%d fsync=%d unlink=%d\n", k, counts["pwrite64 s"+strconv.Itoa(k)+".db-journal"], counts["pwrite64 s"+strconv.Itoa(k)+".db"], counts["fsync s"+strconv.Itoa(k)+".db-journal"]+counts["fsync s"+strconv.Itoa(k)+".db"]+counts["fsync ch04-step-probe"], counts["unlink s"+strconv.Itoa(k)+".db-journal"])
	}
}
