package r2

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// NewPackageCleanup is a serial reconciler's invocation-local snapshot. A fresh
// invocation lists again so a late write cannot be hidden by an earlier sweep.
func (s *Store) NewPackageCleanup() func(context.Context, []string) error {
	snapshots := map[string]map[string]bool{}
	return func(ctx context.Context, keys []string) error {
		scope, _, err := packageDeleteBatch(keys)
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		// Share one final listing across an owner's many purged attempt receipts.
		prefix := strings.Join(strings.Split(scope, "/")[:4], "/") + "/"
		existing, ok := snapshots[prefix]
		if !ok {
			started := time.Now()
			existing = map[string]bool{}
			pager := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{Bucket: aws.String(s.bucket), Prefix: aws.String(prefix), MaxKeys: aws.Int32(1000)})
			pages := 0
			for pager.HasMorePages() {
				pages++
				if pages > 100 {
					return errors.New("recording cleanup listing exceeds page limit")
				}
				page, err := pager.NextPage(ctx)
				if err != nil {
					return errors.Join(errors.New("recording cleanup listing failed"), ctx.Err())
				}
				if len(page.Contents) > 1000 {
					return errors.New("recording cleanup listing exceeds page size")
				}
				for _, object := range page.Contents {
					key := aws.ToString(object.Key)
					if !strings.HasPrefix(key, prefix) {
						return errors.New("recording cleanup listing escaped scope")
					}
					existing[key] = true
				}
			}
			snapshots[prefix] = existing
			slog.Info("recording_cleanup_inventory", "objects", len(existing), "pages", pages, "elapsed_ms", time.Since(started).Milliseconds())
		}
		var present []string
		for _, key := range keys {
			if existing[key] {
				present = append(present, key)
			}
		}
		if len(present) == 0 {
			return nil
		}
		// The caller's declared scope still owns deletion; listing never grants it.
		if err := s.DeletePackageObjects(ctx, present); err != nil {
			return err
		}
		for _, key := range present {
			delete(existing, key)
		}
		return nil
	}
}
