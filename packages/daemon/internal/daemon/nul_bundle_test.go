package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// A workspace bundle's refs are text the pod's shim answers, and PostgreSQL refuses a NUL in a
// text column (SQLSTATE 22021) and its \u0000 escape in jsonb (22P05). A save that stored them as
// they came would stop the release and keep the pod for good, since every later release takes
// the same refs again. The release saves the bundle instead, with U+FFFD where its head, a tip and
// a bookmark's name and commit held a NUL, and the bundle's own bytes, which are bytea, as they
// came, 0x00 included.
func TestAReleaseSavesABundleWhoseRefsHoldNULWithReplacementCharacters(t *testing.T) {
	rig := newIdleRig(t)
	ctx := context.Background()
	// A save the store refuses cancels the release, and the cancel's log line is the one place
	// that keeps the store's error: the hold it records names its kind alone.
	logs := &syncBuffer{}
	rig.runner.log = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	pack := []byte("PACK\x00\x01\x00the unpushed work")
	bookmark := "legion/" + rig.issue.Key + "\x00draft"
	rig.rt.bundle = runtime.WorkspaceBundle{Bundle: pack, Head: "f2f134541b30\x00dfa5160a5d1c",
		Tips: []string{"0a8f0aee63b1\x00682fefe975ba"}, Bookmarks: map[string]string{bookmark: "dffe88ae2f79\x003e13436cef39"},
		At: time.Now().UTC(), PodUID: releasedPodUID}
	cleanedBookmark := "legion/" + rig.issue.Key + "\uFFFDdraft"

	rig.w.releaseIdlePods(ctx)
	rig.drain(t)

	if !rig.bundled(t) {
		t.Fatalf("the release saved no bundle of %s; the outbox logged:\n%s", rig.issue.Key, logs.String())
	}
	if holds := rig.records(t).Holds; len(holds) != 0 {
		t.Fatalf("the release stopped with the holds %+v, want it to have gone on with the bundle saved", holds)
	}
	if got := rig.machine.Claim(); got.State != supervise.StateSuspended || got.SuspendedBy != supervise.SuspendedByReleased {
		t.Fatalf("the claim = %s by %s, want it suspended by the release that saved its work", got.State, got.SuspendedBy)
	}
	if _, suspended := rig.rt.reads(); suspended != 1 {
		t.Fatalf("Sandbox teardowns = %d, want the release's one", suspended)
	}

	var head string
	var tipsJSON, bookmarksJSON, bundle []byte
	if err := rig.pool.QueryRow(ctx, `select head, tips, bookmarks, bundle from workspace_bundles where issue = $1`,
		rig.issue.Key).Scan(&head, &tipsJSON, &bookmarksJSON, &bundle); err != nil {
		t.Fatalf("read the saved bundle of %s: %v", rig.issue.Key, err)
	}
	var tips []string
	if err := json.Unmarshal(tipsJSON, &tips); err != nil {
		t.Fatalf("the saved tips %s do not decode: %v", tipsJSON, err)
	}
	var bookmarks map[string]string
	if err := json.Unmarshal(bookmarksJSON, &bookmarks); err != nil {
		t.Fatalf("the saved bookmarks %s do not decode: %v", bookmarksJSON, err)
	}
	if want := "f2f134541b30\uFFFDdfa5160a5d1c"; head != want {
		t.Errorf("the saved head is %q, want %q", head, want)
	}
	if want := []string{"0a8f0aee63b1\uFFFD682fefe975ba"}; !slices.Equal(tips, want) {
		t.Errorf("the saved tips are %q, want %q", tips, want)
	}
	if want := map[string]string{cleanedBookmark: "dffe88ae2f79\uFFFD3e13436cef39"}; !maps.Equal(bookmarks, want) {
		t.Errorf("the saved bookmarks are %q, want %q", bookmarks, want)
	}
	if !bytes.Equal(bundle, pack) {
		t.Errorf("the saved bundle is %q, want the %q the shim answered, byte for byte", bundle, pack)
	}
}
