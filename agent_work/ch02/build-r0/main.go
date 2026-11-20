package main

import (
	"context"
	"fmt"
	"os"
	"strconv"

	"github.com/nickstrad/sqlite-internals/internal/recipe"
)

func main() {
	steps, _ := strconv.Atoi(os.Args[2])
	r := recipe.Default().WithJournalMode(os.Args[3])
	if err := r.Build(context.Background(), os.Args[1], steps); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}
