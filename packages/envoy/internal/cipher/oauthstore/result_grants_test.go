package oauthstore_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/cipher/oauthstore"
)

// entryDigest stands for the results entry a grant is made under: 64 hex digits, as the column
// holds every digest.
const entryDigest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// resultGrant is a grant of one run, made under entryDigest and ending at expires.
func resultGrant(hash string, expires time.Time) oauthstore.ResultGrant {
	return oauthstore.ResultGrant{
		ID: uuid.NewString(), PlaceholderSHA256: hash, Subject: "workflow-results:o/r/.github/workflows/ci-results.yaml",
		Session: "ci-results:o/r/.github/workflows/ci-results.yaml", Principal: "workflow-results:o/r/.github/workflows/ci-results.yaml",
		Scopes: []string{"results"}, Extra: map[string]any{"ci": map[string]any{"check_run_id": "97"}, "person": "ada@example.com"},
		EntryDigest: entryDigest, ExpiresAt: expires.UTC().Truncate(time.Microsecond),
	}
}

// TestAnExpiredResultGrantIsSwept: a live grant reads back by its placeholder's hash and by its
// id, whole; a grant whose end has passed reads as not live by either while its row is still
// there, and so does an id no row has or one that is no uuid; Sweep deletes the ended row and
// keeps the live one.
func TestAnExpiredResultGrantIsSwept(t *testing.T) {
	ctx := context.Background()
	s, pool := newStore(t)
	live, ended := resultGrant("live-hash", time.Now().Add(time.Hour)), resultGrant("ended-hash", time.Now().Add(-time.Second))
	for _, g := range []oauthstore.ResultGrant{live, ended} {
		if err := s.CreateResultGrant(ctx, g); err != nil {
			t.Fatalf("CreateResultGrant(%s): %v", g.PlaceholderSHA256, err)
		}
	}
	for name, read := range map[string]func() (oauthstore.ResultGrant, error){
		"by its placeholder": func() (oauthstore.ResultGrant, error) { return s.ResultGrantByPlaceholder(ctx, live.PlaceholderSHA256) },
		"by its id":          func() (oauthstore.ResultGrant, error) { return s.ResultGrantByID(ctx, live.ID) },
	} {
		got, err := read()
		got.ExpiresAt = got.ExpiresAt.UTC()
		if err != nil || !reflect.DeepEqual(got, live) {
			t.Errorf("the live grant read %s = %+v, %v; want %+v", name, got, err, live)
		}
	}
	for name, read := range map[string]func() (oauthstore.ResultGrant, error){
		"the ended grant by its placeholder": func() (oauthstore.ResultGrant, error) {
			return s.ResultGrantByPlaceholder(ctx, ended.PlaceholderSHA256)
		},
		"the ended grant by its id": func() (oauthstore.ResultGrant, error) { return s.ResultGrantByID(ctx, ended.ID) },
		"an unknown placeholder":    func() (oauthstore.ResultGrant, error) { return s.ResultGrantByPlaceholder(ctx, "no-such-hash") },
		"an unknown id":             func() (oauthstore.ResultGrant, error) { return s.ResultGrantByID(ctx, uuid.NewString()) },
		"an id that is no uuid":     func() (oauthstore.ResultGrant, error) { return s.ResultGrantByID(ctx, "not-a-uuid") },
	} {
		if got, err := read(); !errors.Is(err, oauthstore.ErrResultGrantNotLive) {
			t.Errorf("%s = %+v, %v; want ErrResultGrantNotLive", name, got, err)
		}
	}
	var rows int
	if err := pool.QueryRow(ctx, `select count(*) from result_grants`).Scan(&rows); err != nil || rows != 2 {
		t.Fatalf("result_grants before the sweep holds %d rows (%v), want 2", rows, err)
	}
	if err := s.Sweep(ctx); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	var left []string
	got, err := pool.Query(ctx, `select placeholder_sha256 from result_grants`)
	if err != nil {
		t.Fatal(err)
	}
	defer got.Close()
	for got.Next() {
		var hash string
		if err := got.Scan(&hash); err != nil {
			t.Fatal(err)
		}
		left = append(left, hash)
	}
	if !reflect.DeepEqual(left, []string{live.PlaceholderSHA256}) {
		t.Errorf("result_grants after the sweep holds %v, want the live grant's alone", left)
	}
}
