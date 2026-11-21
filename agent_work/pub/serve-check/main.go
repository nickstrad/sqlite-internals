// Command serve-check serves a built static site directory over loopback HTTP
// under a configured base path and crawls every same-site href and src link to
// confirm every reachable URL returns HTTP 200.
package main

import (
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
)

var hrefPattern = regexp.MustCompile(`\s(?:href|src)="([^"#]*)(?:#[^"]*)?"`)

func main() {
	dir := flag.String("dir", "_site", "site directory")
	base := flag.String("base", "/sqlite-internals/", "base path")
	flag.Parse()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Println("FAIL:", err)
		os.Exit(1)
	}
	// Mount the site behind StripPrefix to match GitHub Pages subpath serving.
	mux := http.NewServeMux()
	mux.Handle(*base, http.StripPrefix(strings.TrimSuffix(*base, "/"), http.FileServer(http.Dir(*dir))))
	go http.Serve(listener, mux)
	origin := "http://" + listener.Addr().String()
	fmt.Println("INFO: serving", *dir, "at", origin+*base)
	start := flag.Args()
	if len(start) == 0 {
		start = []string{""}
	}
	queue := []string{}
	for _, p := range start {
		queue = append(queue, *base+p)
	}
	seen := map[string]int{}
	failed := false
	// Breadth-first crawl of same-site links; external URLs such as GitHub are skipped.
	for len(queue) > 0 {
		target := queue[0]
		queue = queue[1:]
		if _, done := seen[target]; done {
			continue
		}
		resp, err := http.Get(origin + target)
		if err != nil {
			fmt.Println("FAIL:", target, err)
			failed = true
			seen[target] = -1
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		seen[target] = resp.StatusCode
		if resp.StatusCode != http.StatusOK {
			failed = true
		}
		if !strings.Contains(resp.Header.Get("Content-Type"), "text/html") {
			continue
		}
		for _, m := range hrefPattern.FindAllSubmatch(body, -1) {
			link := string(m[1])
			if strings.HasPrefix(link, *base) {
				queue = append(queue, link)
			}
		}
	}
	var paths []string
	for p := range seen {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		fmt.Printf("%d %s\n", seen[p], p)
	}
	fmt.Printf("fetched %d URL(s)\n", len(paths))
	if failed {
		fmt.Println("FAIL: some URL did not return 200")
		os.Exit(1)
	}
	fmt.Println("PASS: every fetched URL returned 200")
}
