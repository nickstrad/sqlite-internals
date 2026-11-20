package main

import (
	"context"
	"fmt"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

func main() {
	client, err := minio.New("127.0.0.1:18333", &minio.Options{Creds: credentials.NewStaticV4("course", "course-secret", ""), Secure: false})
	if err != nil {
		panic(err)
	}
	buckets, err := client.ListBuckets(context.Background())
	fmt.Println(len(buckets), err)
}
