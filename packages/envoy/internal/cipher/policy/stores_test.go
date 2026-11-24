package policy_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/secretstore"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/memory"
)

// listLag is the lag a lagging store of these tests documents (staleStore.ListLag): AWS Secrets
// Manager's five minutes, the window the reload rows here were written against.
const listLag = 5 * time.Minute

// staleStore is store whose List answers listed, a listing taken earlier, and first runs fetched
// when it is set, while Describe and Read stay live: a store's documented list lag, reproduced.
// Its ListLag is lag.
type staleStore struct {
	secretstore.Store
	listed  []secretstore.Listing
	lag     time.Duration
	fetched func()
}

// stale is store as staleStore, its listing taken now under policytest.Prefix and its lag listLag.
func stale(t *testing.T, store secretstore.Store) *staleStore {
	t.Helper()
	listed, err := store.List(context.Background(), policytest.Prefix, secretstore.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return &staleStore{Store: store, listed: listed, lag: listLag}
}

func (s *staleStore) List(context.Context, string, secretstore.ReadOptions) ([]secretstore.Listing, error) {
	if s.fetched != nil {
		s.fetched()
	}
	return s.listed, nil
}

func (s *staleStore) ListLag() time.Duration { return s.lag }

// localARN is the ARN prefix the AWS store's fake gave every secret when the version-digest
// goldens of these tests were taken. The version digests each secret's source, so a golden holds
// for the sources it was taken over and no others.
const localARN = "arn:aws:secretsmanager:local:000000000000:secret:"

// arnSourced is store with each source the one the goldens were taken over (localARN and the
// secret's name) in place of the memory store's own, and Read taking it back: the digest
// goldens then hold the bytes they always held, over the memory store.
type arnSourced struct{ *memory.Store }

func (a arnSourced) List(ctx context.Context, prefix string, opts secretstore.ReadOptions) ([]secretstore.Listing, error) {
	listings, err := a.Store.List(ctx, prefix, opts)
	for i := range listings {
		listings[i].Source = localARN + listings[i].ID
	}
	return listings, err
}

func (a arnSourced) Describe(ctx context.Context, id string, opts secretstore.ReadOptions) (secretstore.Listing, error) {
	ls, err := a.Store.Describe(ctx, id, opts)
	if err == nil {
		ls.Source = localARN + ls.ID
	}
	return ls, err
}

func (a arnSourced) Read(ctx context.Context, source string) (string, error) {
	id, ok := strings.CutPrefix(source, localARN)
	if !ok {
		return "", secretstore.ErrNotFound
	}
	return a.Store.Read(ctx, memory.Source(id))
}
