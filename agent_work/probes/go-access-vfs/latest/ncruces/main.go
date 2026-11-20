// Prints the SQLite version ncruces/go-sqlite3 runs.
package main

import (
	"database/sql"
	"log"

	_ "github.com/ncruces/go-sqlite3/driver"
	"sqliteinternals/probes/goaccessvfs/latest/internal/pin"
)

func main() {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		log.Fatal(err)
	}
	if err := pin.Report("ncruces", db); err != nil {
		log.Fatal(err)
	}
}
