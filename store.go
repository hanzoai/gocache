package main

import (
	"context"
	"errors"
	"io"
)

// ErrMiss reports that a key is absent. It is distinct from a transport or
// authorization failure: an absent key means the store is healthy and simply
// does not have the object, so it must not count against the failure budget
// that trips the breaker. A cold cache is not a broken cache.
var ErrMiss = errors.New("miss")

// Store is a shared blob store addressed by opaque string keys.
type Store interface {
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	Put(ctx context.Context, key string, size int64, body io.Reader, hash string) error
}
