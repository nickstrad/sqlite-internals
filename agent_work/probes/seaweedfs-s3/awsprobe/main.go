package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

func env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func newClient() *s3.Client {
	return s3.New(s3.Options{
		Region:       env("COURSE_S3_REGION", "us-east-1"),
		BaseEndpoint: aws.String(env("COURSE_S3_ENDPOINT", "http://127.0.0.1:18333")),
		UsePathStyle: true,
		Credentials:  credentials.NewStaticCredentialsProvider(env("COURSE_S3_ACCESS_KEY", "course"), env("COURSE_S3_SECRET_KEY", "course-secret"), ""),
	})
}

func errorCode(err error) string {
	if err == nil {
		return "OK"
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		return apiErr.ErrorCode()
	}
	return err.Error()
}

func must(label string, err error) {
	if err != nil {
		fmt.Printf("FATAL %s: %v\n", label, err)
		os.Exit(1)
	}
}

type probe struct {
	ctx    context.Context
	client *s3.Client
	bucket string
}

func (p probe) put(key, body string, mutate func(*s3.PutObjectInput)) (string, error) {
	input := &s3.PutObjectInput{Bucket: &p.bucket, Key: &key, Body: strings.NewReader(body)}
	if mutate != nil {
		mutate(input)
	}
	out, err := p.client.PutObject(p.ctx, input)
	if err != nil {
		return "", err
	}
	return aws.ToString(out.ETag), nil
}

func (p probe) get(key string) (string, string, error) {
	out, err := p.client.GetObject(p.ctx, &s3.GetObjectInput{Bucket: &p.bucket, Key: &key})
	if err != nil {
		return "", "", err
	}
	defer out.Body.Close()
	data, err := io.ReadAll(out.Body)
	return string(data), aws.ToString(out.ETag), err
}

func (p probe) list(prefix string, pageSize int32) ([]string, int, error) {
	var keys []string
	pages := 0
	var token *string
	for {
		out, err := p.client.ListObjectsV2(p.ctx, &s3.ListObjectsV2Input{Bucket: &p.bucket, Prefix: &prefix, MaxKeys: aws.Int32(pageSize), ContinuationToken: token})
		if err != nil {
			return nil, pages, err
		}
		pages++
		for _, object := range out.Contents {
			keys = append(keys, aws.ToString(object.Key))
		}
		if !aws.ToBool(out.IsTruncated) {
			return keys, pages, nil
		}
		token = out.NextContinuationToken
	}
}

func (p probe) emptyBucket() {
	keys, _, err := p.list("", 1000)
	if err != nil {
		return
	}
	for _, key := range keys {
		p.client.DeleteObject(p.ctx, &s3.DeleteObjectInput{Bucket: &p.bucket, Key: aws.String(key)})
	}
	uploads, err := p.client.ListMultipartUploads(p.ctx, &s3.ListMultipartUploadsInput{Bucket: &p.bucket})
	if err == nil {
		for _, upload := range uploads.Uploads {
			p.client.AbortMultipartUpload(p.ctx, &s3.AbortMultipartUploadInput{Bucket: &p.bucket, Key: upload.Key, UploadId: upload.UploadId})
		}
	}
}

func waitReady(ctx context.Context, client *s3.Client) error {
	deadline := time.Now().Add(60 * time.Second)
	for {
		_, err := client.ListBuckets(ctx, &s3.ListBucketsInput{})
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return err
		}
		time.Sleep(time.Second)
	}
}

func main() {
	ctx := context.Background()
	client := newClient()
	must("wait for S3", waitReady(ctx, client))
	p := probe{ctx: ctx, client: client, bucket: env("PROBE_BUCKET", "course-probe")}
	_, err := client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: &p.bucket})
	fmt.Println("create bucket:", errorCode(err))
	_, err = client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: &p.bucket})
	fmt.Println("create bucket again:", errorCode(err))
	p.emptyBucket()

	etag1, err := p.put("obj/a", "version-1", nil)
	must("put", err)
	body, getETag, err := p.get("obj/a")
	must("get", err)
	fmt.Printf("put+get: body=%q put_etag=%s get_etag=%s same=%v\n", body, etag1, getETag, etag1 == getETag)

	stale := 0
	for i := range 100 {
		want := fmt.Sprintf("raw-%03d", i)
		_, err := p.put("obj/raw", want, nil)
		must("raw put", err)
		got, _, err := p.get("obj/raw")
		must("raw get", err)
		if got != want {
			stale++
		}
	}
	fmt.Printf("read-after-overwrite: 100 put-then-get rounds, stale reads=%d\n", stale)

	listMissing := 0
	for i := range 50 {
		key := fmt.Sprintf("raw-list/%03d", i)
		_, err := p.put(key, "x", nil)
		must("list put", err)
		keys, _, err := p.list("raw-list/", 1000)
		must("list", err)
		if len(keys) != i+1 || keys[len(keys)-1] != key {
			listMissing++
		}
	}
	fmt.Printf("list-after-write: 50 put-then-list rounds, list missing the new key=%d\n", listMissing)

	order := []string{"k/b", "k/10", "k/2", "k/A", "k/1", "k/a", "k/a/x", "k/a-", "k/é", "k/_"}
	shuffled := append([]string(nil), order...)
	rand.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
	for _, key := range shuffled {
		_, err := p.put(key, key, nil)
		must("order put", err)
	}
	listed, pages, err := p.list("k/", 3)
	must("order list", err)
	sorted := append([]string(nil), order...)
	sort.Strings(sorted)
	fmt.Printf("list order (page size 3, %d pages): %q\n", pages, listed)
	fmt.Printf("list order equals byte-wise sort: %v\n", strings.Join(listed, ",") == strings.Join(sorted, ","))

	etag2, err := p.put("obj/a", "version-2", nil)
	must("overwrite", err)
	body, _, _ = p.get("obj/a")
	fmt.Printf("overwrite: body=%q etag_changed=%v\n", body, etag1 != etag2)

	_, err = p.put("obj/a", "should-not-land", func(in *s3.PutObjectInput) { in.IfNoneMatch = aws.String("*") })
	body, _, _ = p.get("obj/a")
	fmt.Printf("If-None-Match:* on existing key: %s; body after=%q\n", errorCode(err), body)
	_, err = p.put("obj/new", "fresh", func(in *s3.PutObjectInput) { in.IfNoneMatch = aws.String("*") })
	fmt.Printf("If-None-Match:* on absent key: %s\n", errorCode(err))

	_, err = p.put("obj/a", "stale-writer", func(in *s3.PutObjectInput) { in.IfMatch = aws.String(etag1) })
	body, _, _ = p.get("obj/a")
	fmt.Printf("If-Match with stale etag: %s; body after=%q\n", errorCode(err), body)
	etag3, err := p.put("obj/a", "version-3", func(in *s3.PutObjectInput) { in.IfMatch = aws.String(etag2) })
	body, _, _ = p.get("obj/a")
	fmt.Printf("If-Match with current etag: %s; body after=%q etag_changed=%v\n", errorCode(err), body, etag3 != etag2)
	_, err = p.put("obj/absent", "x", func(in *s3.PutObjectInput) { in.IfMatch = aws.String(etag3) })
	fmt.Printf("If-Match on absent key: %s\n", errorCode(err))

	createRaces(p, 50, 2)
	createRaces(p, 20, 8)
	casRaces(p, 50, 2)
	casRaces(p, 20, 8)

	multipart(p)

	_, err = client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &p.bucket, Key: aws.String("obj/a")})
	_, _, getErr := p.get("obj/a")
	fmt.Printf("delete: %s; get after delete: %s\n", errorCode(err), errorCode(getErr))
	_, err = p.put("obj/a", "recreated", func(in *s3.PutObjectInput) { in.IfNoneMatch = aws.String("*") })
	fmt.Printf("If-None-Match:* after delete: %s\n", errorCode(err))
	p.emptyBucket()
}

func createRaces(p probe, rounds, writers int) {
	exactlyOne := 0
	winnerVisible := 0
	var outcomes []string
	for round := range rounds {
		key := fmt.Sprintf("race/create-%d-%03d", writers, round)
		var wg sync.WaitGroup
		results := make([]error, writers)
		start := make(chan struct{})
		for w := range writers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				client := newClient()
				q := probe{ctx: p.ctx, client: client, bucket: p.bucket}
				<-start
				_, results[w] = q.put(key, fmt.Sprintf("writer-%d", w), func(in *s3.PutObjectInput) { in.IfNoneMatch = aws.String("*") })
			}()
		}
		close(start)
		wg.Wait()
		wins := 0
		winner := -1
		codes := map[string]int{}
		for w, err := range results {
			codes[errorCode(err)]++
			if err == nil {
				wins++
				winner = w
			}
		}
		if wins == 1 {
			exactlyOne++
			body, _, _ := p.get(key)
			if body == fmt.Sprintf("writer-%d", winner) {
				winnerVisible++
			}
		}
		if round == 0 || wins != 1 {
			outcomes = append(outcomes, fmt.Sprintf("round %d: %v", round, codes))
		}
	}
	fmt.Printf("concurrent If-None-Match:* (%d writers x %d rounds): exactly one winner in %d rounds; winner's body stored in %d\n", writers, rounds, exactlyOne, winnerVisible)
	for _, line := range outcomes[:min(len(outcomes), 5)] {
		fmt.Println("  ", line)
	}
}

func casRaces(p probe, rounds, writers int) {
	exactlyOne := 0
	winnerVisible := 0
	var outcomes []string
	for round := range rounds {
		key := fmt.Sprintf("race/cas-%d-%03d", writers, round)
		base, err := p.put(key, "base", nil)
		must("cas base", err)
		var wg sync.WaitGroup
		results := make([]error, writers)
		start := make(chan struct{})
		for w := range writers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				q := probe{ctx: p.ctx, client: newClient(), bucket: p.bucket}
				<-start
				_, results[w] = q.put(key, fmt.Sprintf("writer-%d", w), func(in *s3.PutObjectInput) { in.IfMatch = aws.String(base) })
			}()
		}
		close(start)
		wg.Wait()
		wins := 0
		winner := -1
		codes := map[string]int{}
		for w, err := range results {
			codes[errorCode(err)]++
			if err == nil {
				wins++
				winner = w
			}
		}
		if wins == 1 {
			exactlyOne++
			body, _, _ := p.get(key)
			if body == fmt.Sprintf("writer-%d", winner) {
				winnerVisible++
			}
		}
		if round == 0 || wins != 1 {
			outcomes = append(outcomes, fmt.Sprintf("round %d: %v", round, codes))
		}
	}
	fmt.Printf("concurrent If-Match on one etag (%d writers x %d rounds): exactly one winner in %d rounds; winner's body stored in %d\n", writers, rounds, exactlyOne, winnerVisible)
	for _, line := range outcomes[:min(len(outcomes), 5)] {
		fmt.Println("  ", line)
	}
}

func multipart(p probe) {
	key := "mp/object"
	created, err := p.client.CreateMultipartUpload(p.ctx, &s3.CreateMultipartUploadInput{Bucket: &p.bucket, Key: &key})
	must("create multipart", err)
	partSize := 5 << 20
	part1 := bytes.Repeat([]byte("a"), partSize)
	part2 := []byte("tail")
	var parts []types.CompletedPart
	for i, data := range [][]byte{part1, part2} {
		out, err := p.client.UploadPart(p.ctx, &s3.UploadPartInput{Bucket: &p.bucket, Key: &key, UploadId: created.UploadId, PartNumber: aws.Int32(int32(i + 1)), Body: bytes.NewReader(data)})
		must("upload part", err)
		parts = append(parts, types.CompletedPart{ETag: out.ETag, PartNumber: aws.Int32(int32(i + 1))})
	}
	_, _, getErr := p.get(key)
	keys, _, _ := p.list("mp/", 1000)
	uploads, _ := p.client.ListMultipartUploads(p.ctx, &s3.ListMultipartUploadsInput{Bucket: &p.bucket})
	fmt.Printf("multipart before complete: get=%s listed_keys=%q in_progress_uploads=%d\n", errorCode(getErr), keys, len(uploads.Uploads))
	_, err = p.client.CompleteMultipartUpload(p.ctx, &s3.CompleteMultipartUploadInput{Bucket: &p.bucket, Key: &key, UploadId: created.UploadId, MultipartUpload: &types.CompletedMultipartUpload{Parts: parts}})
	body, _, getErr := p.get(key)
	keys, _, _ = p.list("mp/", 1000)
	fmt.Printf("multipart complete: %s; get=%s size=%d listed_keys=%q\n", errorCode(err), errorCode(getErr), len(body), keys)

	aborted := "mp/aborted"
	created, err = p.client.CreateMultipartUpload(p.ctx, &s3.CreateMultipartUploadInput{Bucket: &p.bucket, Key: &aborted})
	must("create multipart 2", err)
	_, err = p.client.UploadPart(p.ctx, &s3.UploadPartInput{Bucket: &p.bucket, Key: &aborted, UploadId: created.UploadId, PartNumber: aws.Int32(1), Body: bytes.NewReader(part2)})
	must("upload part 2", err)
	_, err = p.client.AbortMultipartUpload(p.ctx, &s3.AbortMultipartUploadInput{Bucket: &p.bucket, Key: &aborted, UploadId: created.UploadId})
	_, _, getErr = p.get(aborted)
	fmt.Printf("multipart abort: %s; get after abort=%s\n", errorCode(err), errorCode(getErr))

	guarded := "mp/object"
	created, err = p.client.CreateMultipartUpload(p.ctx, &s3.CreateMultipartUploadInput{Bucket: &p.bucket, Key: &guarded})
	must("create multipart 3", err)
	out, err := p.client.UploadPart(p.ctx, &s3.UploadPartInput{Bucket: &p.bucket, Key: &guarded, UploadId: created.UploadId, PartNumber: aws.Int32(1), Body: bytes.NewReader(part2)})
	must("upload part 3", err)
	_, err = p.client.CompleteMultipartUpload(p.ctx, &s3.CompleteMultipartUploadInput{Bucket: &p.bucket, Key: &guarded, UploadId: created.UploadId, IfNoneMatch: aws.String("*"),
		MultipartUpload: &types.CompletedMultipartUpload{Parts: []types.CompletedPart{{ETag: out.ETag, PartNumber: aws.Int32(1)}}}})
	body, _, _ = p.get(guarded)
	fmt.Printf("multipart complete with If-None-Match:* over an existing key: %s; size after=%d\n", errorCode(err), len(body))
}
