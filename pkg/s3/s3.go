// Package s3 keeps objects in an S3-compatible store (AWS S3, MinIO) through
// the minio-go client, a small dependency next to the AWS SDK.
package s3

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// Config locates the store.
type Config struct {
	Endpoint  string `env:"ENDPOINT, required"` // host:port
	AccessKey string `env:"ACCESS_KEY"`
	SecretKey string `env:"SECRET_KEY"`
	Bucket    string `env:"BUCKET, default=agentlite"`
	Region    string `env:"REGION"`
	UseSSL    bool   `env:"USE_SSL"`
}

// ErrNotFound is returned for a missing object.
var ErrNotFound = errors.New("object not found")

// Client stores objects in one bucket.
type Client struct {
	mc     *minio.Client
	bucket string
}

// bucketTimeout bounds the bucket check at start.
const bucketTimeout = 30 * time.Second

// New connects and creates the bucket if it does not exist.
func New(ctx context.Context, cfg Config) (*Client, error) {
	ctx, cancel := context.WithTimeout(ctx, bucketTimeout)
	defer cancel()
	mc, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds: credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""), Secure: cfg.UseSSL, Region: cfg.Region,
	})
	if err != nil {
		return nil, fmt.Errorf("s3: %w", err)
	}
	exists, err := mc.BucketExists(ctx, cfg.Bucket)
	if err != nil {
		return nil, fmt.Errorf("s3 bucket %s: %w", cfg.Bucket, err)
	}
	if !exists {
		if err := mc.MakeBucket(ctx, cfg.Bucket, minio.MakeBucketOptions{Region: cfg.Region}); err != nil {
			return nil, fmt.Errorf("s3 create bucket %s: %w", cfg.Bucket, err)
		}
	}
	return &Client{mc: mc, bucket: cfg.Bucket}, nil
}

// Put stores an object, replacing any with the same key.
func (c *Client) Put(ctx context.Context, key, contentType string, body []byte) error {
	_, err := c.mc.PutObject(ctx, c.bucket, key, bytes.NewReader(body), int64(len(body)), minio.PutObjectOptions{ContentType: contentType})
	if err != nil {
		return fmt.Errorf("s3 put %s: %w", key, err)
	}
	return nil
}

// Get opens an object, which can seek; the caller closes it.
func (c *Client) Get(ctx context.Context, key string) (io.ReadSeekCloser, error) {
	obj, err := c.mc.GetObject(ctx, c.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("s3 get %s: %w", key, err)
	}
	if _, err := obj.Stat(); err != nil { // GetObject is lazy; Stat reports a missing key
		obj.Close()
		if minio.ToErrorResponse(err).Code == minio.NoSuchKey {
			return nil, fmt.Errorf("s3 get %s: %w", key, ErrNotFound)
		}
		return nil, fmt.Errorf("s3 get %s: %w", key, err)
	}
	return obj, nil
}

// DeletePrefix removes every object whose key starts with prefix; a failed
// listing is an error too.
func (c *Client) DeletePrefix(ctx context.Context, prefix string) error {
	var errs []error
	listed := func(yield func(minio.ObjectInfo) bool) {
		for o := range c.mc.ListObjectsIter(ctx, c.bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true}) {
			if o.Err != nil {
				errs = append(errs, fmt.Errorf("s3 list %s: %w", prefix, o.Err))
				return
			}
			if !yield(o) {
				return
			}
		}
	}
	removed, err := c.mc.RemoveObjectsWithIter(ctx, c.bucket, listed, minio.RemoveObjectsOptions{})
	if err != nil {
		return fmt.Errorf("s3 delete %s: %w", prefix, err)
	}
	for r := range removed {
		if r.Err != nil {
			errs = append(errs, fmt.Errorf("s3 delete %s: %w", r.ObjectName, r.Err))
		}
	}
	return errors.Join(errs...)
}

// Delete removes objects by key; a missing one is no error.
func (c *Client) Delete(ctx context.Context, keys ...string) error {
	var errs []error
	for _, k := range keys {
		if err := c.mc.RemoveObject(ctx, c.bucket, k, minio.RemoveObjectOptions{}); err != nil {
			errs = append(errs, fmt.Errorf("s3 delete %s: %w", k, err))
		}
	}
	return errors.Join(errs...)
}
