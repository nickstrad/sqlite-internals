// Package s3store implements walship.Store on an S3-compatible object store
// (SeaweedFS from compose.yaml) using aws-sdk-go-v2.
//
// It owns the thin translation between walship keys and S3 requests under one
// bucket and key prefix. It relies only on three S3 behaviors probed against
// SeaweedFS: atomic conditional put via If-None-Match: "*", read-after-write
// visibility, and list-after-write visibility. It does not rely on listing
// order (callers sort keys themselves) or on If-Match.
package s3store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"

	"github.com/nickstrad/walfleet/internal/rangefs"
	"github.com/nickstrad/walfleet/internal/walship"
)

// ErrUnreachable reports that the configured S3 endpoint did not answer the
// bucket check within Connect's deadline.
var ErrUnreachable = errors.New("the S3 store is unreachable")

// Store is a walship.Store backed by one S3 bucket and normalized key prefix.
type Store struct {
	Client   *s3.Client
	Bucket   string
	Prefix   string
	Endpoint string
}

// Connect builds a path-style S3 client for config, ensures bucket exists
// within a 10-second probe deadline, and returns a Store scoped to prefix.
func Connect(ctx context.Context, config rangefs.S3Config, bucket, prefix string) (*Store, error) {
	if err := walship.ValidateKey(strings.TrimSuffix(prefix, "/")); err != nil {
		return nil, err
	}
	client := rangefs.NewS3Client(config)
	probe, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := rangefs.EnsureBucket(probe, client, bucket); err != nil {
		return nil, fmt.Errorf("%w at %s (start it with: docker compose up -d seaweedfs): %v", ErrUnreachable, config.Endpoint, err)
	}
	return &Store{Client: client, Bucket: bucket, Prefix: strings.TrimSuffix(prefix, "/") + "/", Endpoint: config.Endpoint}, nil
}

func errorCode(err error) string {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		return apiErr.ErrorCode()
	}
	return ""
}

// PutIfAbsent creates key only if it does not already exist in the bucket by
// sending PutObject with If-None-Match: "*". A 412 PreconditionFailed response
// is mapped to walship.ErrExists.
//
// If the SDK retries a PUT after a transient error and the first attempt
// succeeded, the retry receives PreconditionFailed for the caller's own bytes;
// fence.Adapter handles that by reading the existing slot and comparing bytes.
func (s *Store) PutIfAbsent(ctx context.Context, key string, data []byte) error {
	if err := walship.ValidateKey(key); err != nil {
		return err
	}
	_, err := s.Client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(s.Bucket),
		Key:         aws.String(s.Prefix + key),
		Body:        bytes.NewReader(data),
		IfNoneMatch: aws.String("*"),
	})
	if errorCode(err) == "PreconditionFailed" {
		return fmt.Errorf("%w: %s", walship.ErrExists, key)
	}
	return err
}

// Get fetches key and returns its bytes, mapping NoSuchKey or NotFound to
// walship.ErrNotFound and rejecting a truncated body shorter than Content-Length.
func (s *Store) Get(ctx context.Context, key string) ([]byte, error) {
	if err := walship.ValidateKey(key); err != nil {
		return nil, err
	}
	out, err := s.Client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.Bucket), Key: aws.String(s.Prefix + key)})
	switch code := errorCode(err); {
	case code == "NoSuchKey" || code == "NotFound":
		return nil, fmt.Errorf("%w: %s", walship.ErrNotFound, key)
	case err != nil:
		return nil, err
	}
	defer out.Body.Close()
	data, err := io.ReadAll(out.Body)
	if err != nil {
		return nil, err
	}
	if out.ContentLength != nil && int64(len(data)) != *out.ContentLength {
		return nil, fmt.Errorf("get %s: read %d bytes, Content-Length %d", key, len(data), *out.ContentLength)
	}
	return data, nil
}

// List returns every key under prefix with the store's own prefix stripped.
// Keys are returned in whatever order the server lists them; walship and fence
// sort keys themselves.
func (s *Store) List(ctx context.Context, prefix string) ([]string, error) {
	if prefix != "" {
		if err := walship.ValidateKey(strings.TrimSuffix(prefix, "/")); err != nil {
			return nil, err
		}
	}
	pages := s3.NewListObjectsV2Paginator(s.Client, &s3.ListObjectsV2Input{Bucket: aws.String(s.Bucket), Prefix: aws.String(s.Prefix + prefix)})
	var keys []string
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, object := range page.Contents {
			key, ok := strings.CutPrefix(aws.ToString(object.Key), s.Prefix)
			if !ok {
				return nil, fmt.Errorf("list %s: key %q is outside the prefix %s", prefix, aws.ToString(object.Key), s.Prefix)
			}
			keys = append(keys, key)
		}
	}
	return keys, nil
}

// Delete removes key from the bucket. S3 DeleteObject is idempotent, so
// deleting a missing key returns nil.
func (s *Store) Delete(ctx context.Context, key string) error {
	if err := walship.ValidateKey(key); err != nil {
		return err
	}
	_, err := s.Client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(s.Bucket), Key: aws.String(s.Prefix + key)})
	return err
}

// Wipe deletes every object under s.Prefix and returns how many objects were
// deleted. Experiments call it at startup so each run begins from an empty prefix.
func (s *Store) Wipe(ctx context.Context) (int, error) {
	keys, err := s.List(ctx, "")
	if err != nil {
		return 0, err
	}
	for i, key := range keys {
		if err := s.Delete(ctx, key); err != nil {
			return i, err
		}
	}
	return len(keys), nil
}
