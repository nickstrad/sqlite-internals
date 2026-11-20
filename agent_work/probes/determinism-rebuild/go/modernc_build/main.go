// modernc_build: run seed.sql through the modernc.org/sqlite (transpiled 3.53.4) driver into a
// fresh db file, mirroring the CLI build "delete" variant of q1. Prints sqlite_version().
// Usage: go run ./modernc_build SEED.sql OUT.db
package main

import (
	"database/sql"
	"fmt"
	"os"

	_ "modernc.org/sqlite"
)

func main() {
	seed, err := os.ReadFile(os.Args[1])
	must(err)
	os.Remove(os.Args[2])
	db, err := sql.Open("sqlite", os.Args[2])
	must(err)
	db.SetMaxOpenConns(1)
	var v, src string
	must(db.QueryRow("select sqlite_version(), sqlite_source_id()").Scan(&v, &src))
	fmt.Println("driver sqlite", v, src)
	_, err = db.Exec("PRAGMA page_size=4096; PRAGMA journal_mode=DELETE;")
	must(err)
	_, err = db.Exec("BEGIN;" + string(seed) + "COMMIT;")
	must(err)
	must(db.Close())
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
