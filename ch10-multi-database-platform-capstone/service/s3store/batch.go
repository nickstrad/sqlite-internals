package s3store

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

const deleteBatch = 1000

func (s *Store) WipeInBatches(ctx context.Context) (int, error) {
	keys, err := s.List(ctx, "")
	if err != nil {
		return 0, err
	}
	deleted := 0
	for start := 0; start < len(keys); start += deleteBatch {
		batch := keys[start:min(start+deleteBatch, len(keys))]
		objects := make([]types.ObjectIdentifier, len(batch))
		for i, key := range batch {
			objects[i] = types.ObjectIdentifier{Key: aws.String(s.Prefix + key)}
		}
		out, err := s.Client.DeleteObjects(ctx, &s3.DeleteObjectsInput{Bucket: aws.String(s.Bucket), Delete: &types.Delete{Objects: objects, Quiet: aws.Bool(true)}})
		if err != nil {
			return deleted, err
		}
		if len(out.Errors) > 0 {
			return deleted, fmt.Errorf("s3store: delete %s: %s", aws.ToString(out.Errors[0].Key), aws.ToString(out.Errors[0].Message))
		}
		deleted += len(batch)
	}
	remaining, err := s.List(ctx, "")
	if err != nil {
		return deleted, err
	}
	if len(remaining) != 0 {
		return deleted, fmt.Errorf("s3store: %d objects remain under %s after the wipe", len(remaining), s.Prefix)
	}
	return deleted, nil
}
