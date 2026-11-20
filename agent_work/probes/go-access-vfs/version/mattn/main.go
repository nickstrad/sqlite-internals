// Prints the SQLite version mattn/go-sqlite3 runs. Build with and without -tags libsqlite3.
package main

import (
	"database/sql"
	"log"

	_ "github.com/mattn/go-sqlite3"
	"sqliteinternals/probes/goaccessvfs/internal/pin"
)

func main() {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		log.Fatal(err)
	}
	if err := pin.Report("mattn", db); err != nil {
		log.Fatal(err)
	}
}
