package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

func env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func newClient() *minio.Client {
	endpoint := strings.TrimPrefix(env("COURSE_S3_ENDPOINT", "http://127.0.0.1:18333"), "http://")
	client, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(env("COURSE_S3_ACCESS_KEY", "course"), env("COURSE_S3_SECRET_KEY", "course-secret"), ""),
		Region: env("COURSE_S3_REGION", "us-east-1"),
		Secure: false,
	})
	if err != nil {
		panic(err)
	}
	return client
}

func code(err error) string {
	if err == nil {
		return "OK"
	}
	return minio.ToErrorResponse(err).Code
}

func main() {
	ctx := context.Background()
	client := newClient()
	bucket := env("PROBE_BUCKET", "course-probe-minio")
	err := client.MakeBucket(ctx, bucket, minio.MakeBucketOptions{})
	fmt.Println("create bucket:", code(err))
	put := func(c *minio.Client, key, body string, opts minio.PutObjectOptions) (minio.UploadInfo, error) {
		return c.PutObject(ctx, bucket, key, strings.NewReader(body), int64(len(body)), opts)
	}
	client.RemoveObject(ctx, bucket, "obj", minio.RemoveObjectOptions{})
	first, err := put(client, "obj", "v1", minio.PutObjectOptions{})
	fmt.Println("put:", code(err))
	var absent minio.PutObjectOptions
	absent.SetMatchETagExcept("*")
	_, err = put(client, "obj", "v2", absent)
	fmt.Println("SetMatchETagExcept(*) on existing key:", code(err))
	var stale minio.PutObjectOptions
	stale.SetMatchETag("00000000000000000000000000000000")
	_, err = put(client, "obj", "v2", stale)
	fmt.Println("SetMatchETag(wrong etag):", code(err))
	var current minio.PutObjectOptions
	current.SetMatchETag(first.ETag)
	_, err = put(client, "obj", "v2", current)
	fmt.Println("SetMatchETag(current etag):", code(err))
	wins := 0
	for round := range 30 {
		key := fmt.Sprintf("race-%03d", round)
		client.RemoveObject(ctx, bucket, key, minio.RemoveObjectOptions{})
		var wg sync.WaitGroup
		results := make([]error, 2)
		start := make(chan struct{})
		for w := range 2 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				c := newClient()
				var opts minio.PutObjectOptions
				opts.SetMatchETagExcept("*")
				<-start
				_, results[w] = put(c, key, fmt.Sprint(w), opts)
			}()
		}
		close(start)
		wg.Wait()
		if (results[0] == nil) != (results[1] == nil) {
			wins++
		}
		client.RemoveObject(ctx, bucket, key, minio.RemoveObjectOptions{})
	}
	fmt.Printf("concurrent SetMatchETagExcept(*) 2 writers x 30 rounds: exactly one winner in %d\n", wins)
	client.RemoveObject(ctx, bucket, "obj", minio.RemoveObjectOptions{})
}
