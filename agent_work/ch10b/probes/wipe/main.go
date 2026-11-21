package main

import (
	"context"
	"fmt"
	"os"

	"github.com/nickstrad/walfleet/ch10-multi-database-platform-capstone/service/s3store"
	"github.com/nickstrad/walfleet/internal/rangefs"
)

func main() {
	ctx := context.Background()
	for _, prefix := range os.Args[1:] {
		store, err := s3store.Connect(ctx, rangefs.S3ConfigFromEnv(), "course-ch10b", prefix)
		if err != nil {
			panic(err)
		}
		n, err := store.WipeInBatches(ctx)
		fmt.Println(prefix, n, err)
	}
}
