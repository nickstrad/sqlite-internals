package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/nickstrad/sqlite-internals/internal/recipe"
)

func main() {
	ctx := context.Background()
	dir, _ := os.MkdirTemp("", "size")
	defer os.RemoveAll(dir)
	r := recipe.Default()
	for _, k := range []int{0, 4, 5, 10, 20} {
		p := filepath.Join(dir, fmt.Sprintf("s%d.db", k))
		start := time.Now()
		if err := r.Build(ctx, p, k); err != nil {
			panic(err)
		}
		st, _ := os.Stat(p)
		fmt.Println(k, st.Size(), time.Since(start))
	}
	fmt.Println(recipe.IndexSQL)
}
