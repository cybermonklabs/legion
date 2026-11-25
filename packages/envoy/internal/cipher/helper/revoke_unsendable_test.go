// packages/envoy/internal/cipher/helper/revoke_unsendable_test.go
//go:build linux

package helper

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sjawhar/envoy/cipheraddr"
	"github.com/sjawhar/envoy/internal/cipher/proof"
	"github.com/sjawhar/envoy/internal/cipherclient"
)

// TestARevokeOfAnIDNoURLCanHoldIsRefusedUnsent: the id a revoke ends its route in is what
// `unenroll --enrollment` was typed, and an id holding a '#' (net/http would cut the rest off as a
// fragment and revoke the enrollment the id begins with), a bad escape or a control byte is no
// path a URL can hold. The revoke is refused before anything is sent, in the fixed words of
// errPathUnsendable, and its error names none of the id. Control: a plain id reaches the stand-in
// (plain).
func TestARevokeOfAnIDNoURLCanHoldIsRefusedUnsent(t *testing.T) {
	var sent atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sent.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	key, err := proof.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	authority := &cipherclient.Authority{Key: key, CredentialID: "lcred-1"}
	for _, c := range []struct{ name, id string }{
		{"fragment", "enr-1#hunter2pw-typed-7c1e5a"},
		{"bad-escape", "enr-1%zzhunter2pw-typed-7c1e5a"},
		{"control-byte", "enr-1\x7fhunter2pw-typed-7c1e5a"},
	} {
		t.Run(c.name, func(t *testing.T) {
			b := &Broker{URL: cipheraddr.New(srv.URL), HTTP: srv.Client()}
			t.Cleanup(b.Close)
			before := sent.Load()
			err := b.revokeUnder(context.Background(), authority, nil, c.id)
			if err == nil || !strings.Contains(err.Error(), "is not one a URL can hold") {
				t.Errorf("the revoke of an id %q = %v; want it refused as a path no URL can hold", c.id, err)
			}
			if err != nil && strings.Contains(err.Error(), "hunter2pw") {
				t.Errorf("the revoke's error %q names the id", err)
			}
			if n := sent.Load() - before; n != 0 {
				t.Errorf("the revoke of an id %q sent %d requests; want none", c.id, n)
			}
		})
	}
	t.Run("plain", func(t *testing.T) {
		b := &Broker{URL: cipheraddr.New(srv.URL), HTTP: srv.Client()}
		t.Cleanup(b.Close)
		before := sent.Load()
		if err := b.revokeUnder(context.Background(), authority, nil, "enr-1"); err != nil {
			t.Errorf("the revoke of a plain id: %v", err)
		}
		if n := sent.Load() - before; n != 1 {
			t.Errorf("the revoke of a plain id sent %d requests; want one", n)
		}
	})
}
