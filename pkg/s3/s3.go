// Package s3 keeps objects in an S3-compatible store (AWS S3, MinIO) through
// the minio-go client, a small dependency next to the AWS SDK.
package s3

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// Config locates the store. An empty Endpoint means no store.
type Config struct {
	Endpoint  string `env:"ENDPOINT"` // host:port
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

// New connects and creates the bucket if it does not exist.
func New(ctx context.Context, cfg Config) (*Client, error) {
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

// Get opens an object; the caller closes it.
func (c *Client) Get(ctx context.Context, key string) (io.ReadCloser, error) {
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

// DeletePrefix removes every object whose key starts with prefix. The error
// channel is read to the end: minio's sender blocks until it is.
func (c *Client) DeletePrefix(ctx context.Context, prefix string) error {
	objects := c.mc.ListObjects(ctx, c.bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true})
	var errs []error
	for e := range c.mc.RemoveObjects(ctx, c.bucket, objects, minio.RemoveObjectsOptions{}) {
		errs = append(errs, fmt.Errorf("s3 delete %s: %w", e.ObjectName, e.Err))
	}
	return errors.Join(errs...)
}
