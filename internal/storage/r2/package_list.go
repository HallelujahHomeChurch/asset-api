package r2

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// ListPackageObjects returns only upload-size evidence from this package's
// staging prefix. It never reads final media or establishes package readiness.
func (s *Store) ListPackageObjects(ctx context.Context, id string, maxObjects int) (map[string]int64, error) {
	prefix := "recordings/packages/" + id + "/staging/"
	if !packageStagingKey.MatchString(prefix+"master.m3u8") || maxObjects < 1 || maxObjects > 10_000 {
		return nil, errors.New("invalid recording package listing")
	}
	input := &s3.ListObjectsV2Input{Bucket: aws.String(s.bucket), Prefix: aws.String(prefix), MaxKeys: aws.Int32(1000)}
	objects := make(map[string]int64)
	tokens := make(map[string]bool)
	for {
		page, err := s.client.ListObjectsV2(ctx, input)
		if err != nil {
			return nil, fmt.Errorf("list R2 package objects: %w", err)
		}
		if page.IsTruncated == nil {
			return nil, errors.New("incomplete R2 package listing")
		}
		for _, object := range page.Contents {
			key := aws.ToString(object.Key)
			path := strings.TrimPrefix(key, prefix)
			_, duplicate := objects[path]
			if !strings.HasPrefix(key, prefix) || !packageStagingKey.MatchString(key) || object.Size == nil || *object.Size < 0 || duplicate || len(objects) >= maxObjects {
				return nil, errors.New("invalid R2 package listing")
			}
			objects[path] = *object.Size
		}
		if !aws.ToBool(page.IsTruncated) {
			return objects, nil
		}
		token := aws.ToString(page.NextContinuationToken)
		if token == "" || tokens[token] || len(page.Contents) == 0 {
			return nil, errors.New("invalid R2 package listing continuation")
		}
		tokens[token] = true
		input.ContinuationToken = aws.String(token)
	}
}
