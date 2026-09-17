// Package objstore holds the object-storage abstraction used for backup
// artifacts, its Cloudflare R2 implementation, and the backup key scheme.
package objstore

import (
	"context"
	"io"
	"time"
)

// Object is one stored backup artifact.
type Object struct {
	Key          string
	Size         int64
	LastModified time.Time
}

// Store is the subset of object storage this service needs.
//
// List must never return a partially-enumerated result: callers use it to
// decide what to delete, so a truncated listing that looks complete would
// cause data loss. Implementations return (nil, err) if any page fails.
type Store interface {
	Upload(ctx context.Context, key string, r io.Reader, meta map[string]string) error
	List(ctx context.Context, prefix string) ([]Object, error)
	Download(ctx context.Context, key string, w io.Writer) error
	Head(ctx context.Context, key string) (Object, map[string]string, error)
	Delete(ctx context.Context, key string) error
}
