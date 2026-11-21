package main

import (
	"fmt"
	"os"

	"github.com/nickstrad/walfleet/internal/dbfile"
)

func main() {
	f, err := dbfile.Open(os.Args[1])
	if err != nil {
		fmt.Println("open:", err)
		os.Exit(1)
	}
	defer f.Close()
	for _, field := range f.Header.Fields() {
		fmt.Printf("%3d %2d %-34s %s\n", field.Offset, field.Size, field.Name, field.Value)
	}
	fmt.Println("page count", f.PageCount(), "from", f.PageCountSource())
	c, err := f.Census()
	if err != nil {
		fmt.Println("census:", err)
		os.Exit(1)
	}
	for _, t := range c.SortedTrees() {
		fmt.Printf("tree %-20s %-6s root=%d pages=%d overflow=%d cells=%d\n", t.Name, t.Type, t.RootPage, t.Pages, t.Overflow, t.Cells)
	}
	fmt.Println("roles", c.RoleCounts(), "freelist", c.FreelistTrunks, c.FreelistLeaves, "unaccounted", c.Unaccounted)
	if len(os.Args) > 2 {
		for _, p := range c.Pages {
			fmt.Printf("%d %s %s ncell=%d payload=%d unused=%d mx=%d\n", p.Number, p.Role, p.Tree, p.CellCount, p.Payload, p.Unused, p.MaxPayload)
		}
	}
}
