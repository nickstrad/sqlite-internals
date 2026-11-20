// mattn/go-sqlite3 + the vfstrace shim, through database/sql. Prints the source id and the
// commit's xWrite/xSync lines. Build with and without -tags libsqlite3.
package main

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	_ "github.com/mattn/go-sqlite3"
	"sqliteinternals/probes/goaccessvfs/vfs/mattnshim"
)

func main() {
	var b strings.Builder
	mattnshim.Line = func(s string) { b.WriteString(s) }
	// Register must run after SQLite is initialised; sql.Open+Ping does that via sqlite3_open_v2.
	pre, _ := sql.Open("sqlite3", ":memory:")
	var id string
	if err := pre.QueryRow("select sqlite_source_id()").Scan(&id); err != nil {
		log.Fatal(err)
	}
	if err := mattnshim.Register(); err != nil {
		log.Fatal(err)
	}
	dir, _ := os.MkdirTemp("", "mattnshim")
	defer os.RemoveAll(dir)
	db, err := sql.Open("sqlite3", "file:"+filepath.Join(dir, "t.db")+"?vfs=trace&_journal_mode=WAL")
	if err != nil {
		log.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	for _, q := range []string{"create table t(x)", "insert into t values(1)"} {
		b.Reset()
		if _, err := db.Exec(q); err != nil {
			log.Fatal(err)
		}
	}
	fmt.Println("source_id:", id)
	for _, l := range strings.Split(b.String(), "\n") {
		if strings.Contains(l, "xWrite") || strings.Contains(l, "xSync") {
			fmt.Println(" ", strings.ReplaceAll(l, dir+"/", ""))
		}
	}
}
