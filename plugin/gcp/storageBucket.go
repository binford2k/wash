package gcp

import (
	"context"
	"fmt"
	"strings"
	"time"

	"cloud.google.com/go/storage"
	"github.com/golang/protobuf/ptypes/timestamp"
	"github.com/puppetlabs/wash/activity"
	"github.com/puppetlabs/wash/plugin"
	"google.golang.org/api/iterator"
	"google.golang.org/genproto/googleapis/monitoring/v3"
)

type storageBucket struct {
	plugin.EntryBase
	storageProjectClient
}

func newStorageBucket(client storageProjectClient, bucket *storage.BucketAttrs) *storageBucket {
	stor := &storageBucket{EntryBase: plugin.NewEntry(bucket.Name), storageProjectClient: client}
	stor.Attributes().
		SetCrtime(bucket.Created).
		SetCtime(bucket.Created).
		SetMtime(bucket.Created).
		SetMeta(bucket)
	return stor
}

type fullMeta struct {
	*storage.BucketAttrs
	Size float64
}

func (s *storageBucket) Metadata(ctx context.Context) (plugin.JSONObject, error) {
	bucket, err := s.Bucket(s.Name()).Attrs(ctx)
	if err != nil {
		return nil, err
	}

	var size float64
	if s.metrics != nil {
		today := time.Now()
		before := today.AddDate(0, 0, -1)
		interval := &monitoring.TimeInterval{
			StartTime: &timestamp.Timestamp{Seconds: before.Unix()},
			EndTime:   &timestamp.Timestamp{Seconds: today.Unix()},
		}
		req := &monitoring.ListTimeSeriesRequest{
			Name:     "projects/" + s.projectID,
			Filter:   `metric.type = "storage.googleapis.com/storage/total_bytes" AND resource.label.bucket_name = "` + s.Name() + `"`,
			Interval: interval,
			PageSize: 1,
		}
		point, err := s.metrics.ListTimeSeries(ctx, req).Next()
		if err != nil {
			activity.Record(ctx, "Unable to get bucket size for %v from Stackdriver: %v", s.Name(), err)
		} else if len(point.Points) <= 0 {
			activity.Record(ctx, "Stackdriver returned no data points for storage.googleapis.com/storage/total_bytes metric of bucket %v", s.Name())
		} else {
			size = point.Points[0].Value.GetDoubleValue()
		}
	}

	return plugin.ToJSONObject(fullMeta{bucket, size}), nil
}

// List all storage objects as dirs and files.
func (s *storageBucket) List(ctx context.Context) ([]plugin.Entry, error) {
	bucket := s.Bucket(s.Name())
	return listBucket(ctx, bucket, "")
}

func (s *storageBucket) Delete(ctx context.Context) (bool, error) {
	// GCP only deletes empty buckets, so we'll need to delete all of its
	// objects before deleting the bucket.
	err := deleteObjects(ctx, s.Bucket(s.Name()), "")
	if err != nil {
		return false, err
	}
	err = s.Bucket(s.Name()).Delete(ctx)
	return true, err
}

func (s *storageBucket) Schema() *plugin.EntrySchema {
	return plugin.NewEntrySchema(s, "bucket").
		SetMetaAttributeSchema(storage.BucketAttrs{}).
		SetMetadataSchema(fullMeta{}).
		SetDescription(storageBucketDescription)
}

func (s *storageBucket) ChildSchemas() []*plugin.EntrySchema {
	return bucketSchemas()
}

const delimiter = "/"

func listBucket(ctx context.Context, bucket *storage.BucketHandle, prefix string) ([]plugin.Entry, error) {
	var entries []plugin.Entry
	// Get objects directly under this prefix.
	it := bucket.Objects(ctx, &storage.Query{Delimiter: delimiter, Prefix: prefix})
	for {
		objAttrs, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, err
		}

		// https://godoc.org/cloud.google.com/go/storage#Query notes that providing a delimiter returns
		// results in a directory-like fashion. Results will contain objects whose names, aside from
		// the prefix, do not contain delimiter. Objects whose names, aside from the prefix, contain
		// delimiter will have their name, truncated after the delimiter, returned in prefixes.
		// Duplicate prefixes are omitted, and if Prefix is filled in then no other attributes are
		// included.
		if objAttrs.Prefix != "" {
			name := strings.TrimPrefix(strings.TrimSuffix(objAttrs.Prefix, delimiter), prefix)
			preAttrs, err := bucket.Object(objAttrs.Prefix).Attrs(ctx)
			if err != nil {
				// Don't treat this as an error. Not all prefixes have attributes.
				activity.Record(ctx, "Could not get attributes of %v: %v", objAttrs.Prefix, err)
			}
			entries = append(entries, newStorageObjectPrefix(bucket, name, objAttrs.Prefix, preAttrs))
		} else if objAttrs.Name != prefix {
			name := strings.TrimPrefix(objAttrs.Name, prefix)
			entries = append(entries, newStorageObject(name, bucket.Object(objAttrs.Name), objAttrs))
		}
	}
	return entries, nil
}

func deleteObjects(ctx context.Context, bucket *storage.BucketHandle, prefix string) error {
	// Unfortunately, GCP doesn't have a BatchDelete endpoint so we will have to
	// delete each object one at a time.
	//
	// TODO: Parallelize this
	it := bucket.Objects(ctx, &storage.Query{Prefix: prefix})
	for {
		objAttrs, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return err
		}
		if err := bucket.Object(objAttrs.Name).Delete(ctx); err != nil {
			return fmt.Errorf("failed to delete the %v object: %v", objAttrs.Name, err)
		}
	}
	return nil
}

func bucketSchemas() []*plugin.EntrySchema {
	return []*plugin.EntrySchema{(&storageObjectPrefix{}).Schema(), (&storageObject{}).Schema()}
}

const storageBucketDescription = `
This is a Storage bucket. For convenience, we impose some hierarchical structure
on its objects by grouping keys with common prefixes into a specific directory.
For example, the objects 'foo/bar' and 'foo/baz' are represented as files with
path 'foo/bar' and path 'foo/baz', where 'foo' is represented as a 'directory'.
Thus, if you ls this bucket, then everything you'll see is either a Storage
object prefix ('directory') or a Storage object ('file').
`
