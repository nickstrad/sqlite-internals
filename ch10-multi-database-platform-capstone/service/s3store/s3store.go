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

	"github.com/nickstrad/sqlite-internals/internal/rangefs"
	"github.com/nickstrad/sqlite-internals/internal/walship"
)

var ErrUnreachable = errors.New("the S3 store is unreachable")

type Store struct {
	Client   *s3.Client
	Bucket   string
	Prefix   string
	Endpoint string
}

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

func (s *Store) Delete(ctx context.Context, key string) error {
	if err := walship.ValidateKey(key); err != nil {
		return err
	}
	_, err := s.Client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(s.Bucket), Key: aws.String(s.Prefix + key)})
	return err
}

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
