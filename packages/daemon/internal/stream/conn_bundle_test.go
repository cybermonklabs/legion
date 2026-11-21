package stream

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// The shim builds a workspace's bundle before it can read the bundle's size, and refuses one over
// the bound the frame sent it, naming the size the bundle came to. That refusal, and no other, is
// a workspace too large to carry: the daemon records an idle release it cancels for it under
// `bundle_too_large`, and logs a running copy it could not take under the same name, where any
// other refusal is a bundle the shim could not build. So a refusal naming a size over the bound is
// a runtime.WorkspaceBundleTooLargeError carrying that size and the bound, and one naming a size
// within the bound, or none, stays a RefusedError carrying the shim's words. An answer that
// carries a bundle over the bound all the same, against the rule, is the same
// WorkspaceBundleTooLargeError, carrying the size it arrived at, since the daemon's callers read
// no bundle's size against the bound themselves; one that carries a bundle at the bound is the
// bundle. Those two answers cross chunked, as a bundle that size does.
func TestABundleTheShimRefusesOverTheBoundItWasSentIsTooLargeAndNoOtherRefusalIs(t *testing.T) {
	for _, tc := range []struct {
		name string
		// ok is an answer carrying a bundle of size bytes, where false is a refusal naming size.
		ok       bool
		size     int
		tooLarge bool
	}{
		{name: "a size over the bound", size: runtime.MaxWorkspaceBundleBytes + 1, tooLarge: true},
		{name: "a size at the bound", size: runtime.MaxWorkspaceBundleBytes},
		{name: "no size", size: 0},
		{name: "an OK answer over the bound", ok: true, size: runtime.MaxWorkspaceBundleBytes + 1, tooLarge: true},
		{name: "an OK answer at the bound", ok: true, size: runtime.MaxWorkspaceBundleBytes},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := startListener(t, harnessOptions{})
			p := h.connect(testToken)
			var bundle runtime.WorkspaceBundle
			result := async(func() error {
				var err error
				bundle, err = h.conn().BundleWorkspace(context.Background(), "INFRA-945", time.Minute)
				return err
			})
			request := p.expect(shimwire.TypeBundleWorkspace).(shimwire.BundleWorkspace)
			if request.MaxBytes != runtime.MaxWorkspaceBundleBytes {
				t.Fatalf("the frame bounds the bundle at %d bytes, want %d", request.MaxBytes, runtime.MaxWorkspaceBundleBytes)
			}
			reason := "worker-shim: the workspace could not be bundled"
			if tc.ok {
				answer, err := json.Marshal(shimwire.BundleWorkspaceResult{ID: request.ID, OK: true, SizeBytes: tc.size,
					Bundle: make([]byte, tc.size), Head: "c0ffee", Tips: []string{"c0ffee"}})
				if err != nil {
					t.Fatal(err)
				}
				p.sendChunked("bundle-workspace-result-1", answer)
			} else {
				p.send(shimwire.BundleWorkspaceResult{ID: request.ID, OK: false, Error: reason, SizeBytes: tc.size})
			}
			err := awaitResult(t, result)
			var tooLarge *runtime.WorkspaceBundleTooLargeError
			var refused *RefusedError
			switch {
			case tc.tooLarge:
				if !errors.As(err, &tooLarge) || tooLarge.Bytes != tc.size || tooLarge.Max != request.MaxBytes {
					t.Fatalf("err = %v, want a WorkspaceBundleTooLargeError of %d bytes over %d", err, tc.size, request.MaxBytes)
				}
			case tc.ok:
				if err != nil || bundle.Size() != tc.size {
					t.Fatalf("the answer carrying %d bytes at the bound read as %d bytes (%v), want the whole bundle", tc.size, bundle.Size(), err)
				}
			case errors.As(err, &tooLarge):
				t.Fatalf("err = %v, a refusal naming %d bytes within the bound read as too large", err, tc.size)
			case !errors.As(err, &refused) || refused.Command != shimwire.TypeBundleWorkspace || refused.Reason != reason:
				t.Fatalf("err = %v, want a RefusedError of %s carrying the shim's words", err, shimwire.TypeBundleWorkspace)
			}
		})
	}
}
