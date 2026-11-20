package rangefs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

// Default connection settings matching the SeaweedFS S3 service in compose.yaml
// and docker/seaweedfs/s3.json.
const (
	DefaultS3Endpoint  = "http://127.0.0.1:18333"
	DefaultS3Region    = "us-east-1"
	DefaultS3AccessKey = "course"
	DefaultS3SecretKey = "course-secret"
)

// S3Config holds endpoint, region, and static credentials for an S3-compatible store.
type S3Config struct {
	Endpoint  string
	Region    string
	AccessKey string
	SecretKey string
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

// S3ConfigFromEnv reads COURSE_S3_ENDPOINT, COURSE_S3_REGION,
// COURSE_S3_ACCESS_KEY, and COURSE_S3_SECRET_KEY, falling back to the
// compose.yaml SeaweedFS defaults.
func S3ConfigFromEnv() S3Config {
	return S3Config{
		Endpoint:  envOr("COURSE_S3_ENDPOINT", DefaultS3Endpoint),
		Region:    envOr("COURSE_S3_REGION", DefaultS3Region),
		AccessKey: envOr("COURSE_S3_ACCESS_KEY", DefaultS3AccessKey),
		SecretKey: envOr("COURSE_S3_SECRET_KEY", DefaultS3SecretKey),
	}
}

// NewS3Client builds a path-style S3 client for cfg. Checksum calculation and
// response validation are limited to "when required" so plain range GETs against
// SeaweedFS work without extra checksum headers.
func NewS3Client(cfg S3Config) *s3.Client {
	return s3.New(s3.Options{
		Region:       cfg.Region,
		BaseEndpoint: aws.String(cfg.Endpoint),
		UsePathStyle: true,
		Credentials:  credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, ""),

		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
	})
}

func errorCode(err error) string {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		return apiErr.ErrorCode()
	}
	return ""
}

// EnsureBucket creates bucket if it does not already exist, ignoring
// BucketAlreadyOwnedByYou and BucketAlreadyExists.
func EnsureBucket(ctx context.Context, client *s3.Client, bucket string) error {
	_, err := client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)})
	if err == nil {
		return nil
	}
	switch errorCode(err) {
	case "BucketAlreadyOwnedByYou", "BucketAlreadyExists":
		return nil
	}
	return err
}

// PutObject uploads body to bucket/key.
func PutObject(ctx context.Context, client *s3.Client, bucket, key string, body []byte) error {
	_, err := client.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), Body: bytes.NewReader(body)})
	return err
}

// DeleteObject removes bucket/key from the store.
func DeleteObject(ctx context.Context, client *s3.Client, bucket, key string) error {
	_, err := client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	return err
}

// S3Source implements Source against an S3 bucket and optional key prefix using
// HeadObject and ranged GetObject calls.
type S3Source struct {
	Client *s3.Client
	Bucket string
	Prefix string
}

func (s S3Source) key(name string) string {
	if s.Prefix == "" {
		return name
	}
	return strings.TrimSuffix(s.Prefix, "/") + "/" + name
}

// Stat calls HeadObject for name under s.Prefix and maps NotFound/NoSuchKey to
// fs.ErrNotExist.
func (s S3Source) Stat(ctx context.Context, name string) (int64, error) {
	out, err := s.Client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(s.Bucket), Key: aws.String(s.key(name))})
	if err != nil {
		switch errorCode(err) {
		case "NotFound", "NoSuchKey":
			return 0, fs.ErrNotExist
		}
		return 0, err
	}
	if out.ContentLength == nil {
		return 0, fmt.Errorf("HeadObject %s: no content length", name)
	}
	return *out.ContentLength, nil
}

// ReadRange calls GetObject with an inclusive byte-range header for len(p)
// bytes starting at offset.
func (s S3Source) ReadRange(ctx context.Context, name string, offset int64, p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	out, err := s.Client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.Bucket),
		Key:    aws.String(s.key(name)),
		Range:  aws.String(fmt.Sprintf("bytes=%d-%d", offset, offset+int64(len(p))-1)),
	})
	if err != nil {
		return 0, err
	}
	defer out.Body.Close()
	return io.ReadFull(out.Body, p)
}
