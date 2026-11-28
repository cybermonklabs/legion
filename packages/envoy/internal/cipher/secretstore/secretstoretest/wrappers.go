package secretstoretest

import (
	"context"
	"sync/atomic"

	"github.com/sjawhar/envoy/internal/cipher/secretstore"
)

// Counting is Store with its Describe calls counted: the reads a reread or a reload makes.
type Counting struct {
	secretstore.Store
	Describes atomic.Int64
}

func (c *Counting) Describe(ctx context.Context, id string, opts secretstore.ReadOptions) (secretstore.Listing, error) {
	c.Describes.Add(1)
	return c.Store.Describe(ctx, id, opts)
}

// Held is Store with each Describe held until Release closes, the id sent on Entered before it
// waits, so a test controls exactly when a reread's metadata read returns. A Describe whose ctx
// ends first answers ctx's error.
type Held struct {
	secretstore.Store
	Entered chan<- string
	Release <-chan struct{}
}

func (h Held) Describe(ctx context.Context, id string, opts secretstore.ReadOptions) (secretstore.Listing, error) {
	h.Entered <- id
	select {
	case <-h.Release:
	case <-ctx.Done():
		return secretstore.Listing{}, ctx.Err()
	}
	return h.Store.Describe(ctx, id, opts)
}

// Failing is Store with each call whose error is set answering that error, and every other call
// answered by Store.
type Failing struct {
	secretstore.Store
	ListErr, DescribeErr, ReadErr error
}

func (f Failing) List(ctx context.Context, prefix string, opts secretstore.ReadOptions) ([]secretstore.Listing, error) {
	if f.ListErr != nil {
		return nil, f.ListErr
	}
	return f.Store.List(ctx, prefix, opts)
}

func (f Failing) Describe(ctx context.Context, id string, opts secretstore.ReadOptions) (secretstore.Listing, error) {
	if f.DescribeErr != nil {
		return secretstore.Listing{}, f.DescribeErr
	}
	return f.Store.Describe(ctx, id, opts)
}

func (f Failing) Read(ctx context.Context, source string) (string, error) {
	if f.ReadErr != nil {
		return "", f.ReadErr
	}
	return f.Store.Read(ctx, source)
}
