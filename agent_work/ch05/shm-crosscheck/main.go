package main

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"github.com/nickstrad/walfleet/internal/sqlitex"
	"github.com/nickstrad/walfleet/internal/walfile"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: go run ./agent_work/ch05/shm-crosscheck FILE-shm")
		os.Exit(2)
	}
	index, err := walfile.ParseIndexFile(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	tool, err := sqlitex.LocateTool("showshm")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	out, err := exec.Command(tool, os.Args[1]).Output()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	values := map[string][]int64{}
	pattern := regexp.MustCompile(`^\s*[0-9a-f]{3}: (?:[0-9a-f]{2} )+\s+(-?\d+)  (.+)$`)
	for _, line := range strings.Split(string(out), "\n") {
		m := pattern.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		n, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil {
			continue
		}
		values[m[2]] = append(values[m[2]], n)
	}
	h := index.Header()
	marks := index.Checkpoint.ReadMarks
	signed := func(v uint32) int64 { return int64(int32(v)) }
	want := map[string][]int64{
		"Wal-index version":         {int64(h.Version), int64(index.Headers[1].Version)},
		"transaction counter":       {int64(h.Change), int64(index.Headers[1].Change)},
		"1 when initialized":        {int64(h.IsInit), int64(index.Headers[1].IsInit)},
		"database page size":        {int64(h.PageSize()), int64(index.Headers[1].PageSize())},
		"mxFrame":                   {int64(h.MaxFrame), int64(index.Headers[1].MaxFrame)},
		"Size of database in pages": {int64(h.Pages), int64(index.Headers[1].Pages)},
		"nBackfill":                 {int64(index.Checkpoint.Backfill)},
		"aReadMark[0]":              {signed(marks[0])},
		"aReadMark[1]":              {signed(marks[1])},
		"aReadMark[2]":              {signed(marks[2])},
		"aReadMark[3]":              {signed(marks[3])},
		"aReadMark[4]":              {signed(marks[4])},
		"nBackfillAttempted":        {int64(index.Checkpoint.BackfillAttempted)},
	}
	problems := 0
	for label, w := range want {
		g := values[label]
		if fmt.Sprint(g) != fmt.Sprint(w) {
			fmt.Printf("DIFF %s: showshm %v walfile %v\n", label, g, w)
			problems++
		} else {
			fmt.Printf("same %s: %v\n", label, w)
		}
	}
	fmt.Printf("header checksum ok (walfile): %v; header copies agree: %v\n", h.ChecksumOK, index.HeadersAgree())
	if problems > 0 {
		os.Exit(1)
	}
	fmt.Println("SHM CROSSCHECK OK")
}
