package dispatch

//ci:reads packages/contracts/fixtures/hash-gate/** hashGateFixture

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// hashGateFixture is one file of packages/contracts/fixtures/hash-gate, the wire contract of
// Dispatch's hash gate (R1 of dispatch://INFRA-6065), as its bytes.
func hashGateFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "contracts", "fixtures", "hash-gate", name))
	if err != nil {
		t.Fatalf("read the hash-gate fixture %s: %v", name, err)
	}
	return raw
}

// withApproval is the GET body body with its approval changed by change: a read the fixtures hold
// but for one field.
func withApproval(t *testing.T, body []byte, change func(approval map[string]any)) []byte {
	t.Helper()
	var read map[string]any
	if err := json.Unmarshal(body, &read); err != nil {
		t.Fatalf("decode the GET body: %v", err)
	}
	approval, ok := read["approval"].(map[string]any)
	if !ok {
		t.Fatalf("the GET body %s has no approval object", body)
	}
	change(approval)
	changed, err := json.Marshal(read)
	if err != nil {
		t.Fatalf("encode the GET body: %v", err)
	}
	return changed
}

// The artifact read takes Dispatch's verdict on an approval from the hash-gate fixtures through
// the real client: an approval stands (Stands) by digest_owed where Dispatch sends it, whatever
// the approved version, and by today's rule (ApprovedLatest) where it sends none, so an absent
// digest_owed and a false one read apart. LatestReviewer names the reviewer exactly where the
// approval stands, since no read here shows a changes request.
func TestHashGateApprovalReadsTheVerdict(t *testing.T) {
	const (
		capturedContent = "f5294a90ce06985da0b45ea2e8b4ec99eb106f5df0d03c343878119e5464542c"
		approvedContent = "fcb3d52cac0a068e46478d4d8d59fbdc95a3e46fdc05a121b0eede5bcd633981"
		latestDigest    = "3cd63d61a307828c936c8f57e230973fa538f32a7a23bf04e068b872673202f2"
	)
	standing := hashGateFixture(t, "approval-standing-below-latest.json")
	bodies := map[string][]byte{
		"version-mode": hashGateFixture(t, "approval-version-mode.json"),
		"standing":     standing,
		"owed":         hashGateFixture(t, "approval-digest-owed.json"),
		"no-digest-owed": withApproval(t, standing, func(approval map[string]any) {
			delete(approval, "digest_owed")
		}),
		"not-approved": withApproval(t, standing, func(approval map[string]any) {
			approval["state"] = "awaiting"
		}),
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertBearer(t, r)
		body, ok := bodies[strings.TrimPrefix(r.URL.Path, "/api/v1/artifacts/")]
		if r.Method != http.MethodGet || !ok {
			t.Errorf("request = %s %s, want GET of one fixture", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write(body); err != nil {
			t.Errorf("write the fixture: %v", err)
		}
	}))
	defer server.Close()
	client := New(server.URL, BearerTransport(dispatchToken, nil))
	read := func(t *testing.T, id string) Approval {
		t.Helper()
		approval, err := client.Approval(context.Background(), id)
		if err != nil {
			t.Fatalf("read the approval of %s: %v", id, err)
		}
		return approval
	}
	person := Actor{Kind: "user", ID: "person@example.com"}

	t.Run("version mode reads today's rule", func(t *testing.T) {
		approval := read(t, "version-mode")
		if approval.DigestOwed != nil || approval.CurrentSHA256 != nil {
			t.Fatalf("approval = %#v, want no digest_owed and no current_sha256 from a Dispatch that sends neither", approval)
		}
		if approval.IssueKey != "INFRA-1098" || approval.State != "approved" || approval.Version == nil || *approval.Version != 180 ||
			approval.LatestVersion != 180 || approval.ContentSHA256 != capturedContent || approval.By != person {
			t.Fatalf("approval = %#v, want the captured approval of version 180, its content digest and its approver", approval)
		}
		if approval.ApprovedLatest() != 180 || !approval.Stands() || approval.Stands() != (approval.ApprovedLatest() != 0) {
			t.Fatalf("ApprovedLatest = %d and Stands = %t, want 180 and Stands as today's rule", approval.ApprovedLatest(), approval.Stands())
		}
	})

	t.Run("an approval standing below the latest version stands", func(t *testing.T) {
		approval := read(t, "standing")
		if approval.State != "approved" || approval.Version == nil || *approval.Version != 7 || approval.LatestVersion != 8 {
			t.Fatalf("approval = %#v, want the approval of version 7 at latest version 8", approval)
		}
		if approval.DigestOwed == nil || *approval.DigestOwed || approval.CurrentSHA256 == nil || *approval.CurrentSHA256 != latestDigest ||
			approval.ContentSHA256 != approvedContent {
			t.Fatalf("approval = %#v, want digest_owed false, the latest version's current_sha256 and the approved version's content_sha256", approval)
		}
		if !approval.Stands() {
			t.Fatal("Stands = false, want true: Dispatch owes no digest and shows the approval")
		}
		if approval.ApprovedLatest() != 0 {
			t.Fatalf("ApprovedLatest = %d, want 0: its meaning is unchanged, the approval is of version 7 below latest 8", approval.ApprovedLatest())
		}
	})

	t.Run("a digest owed does not stand", func(t *testing.T) {
		approval := read(t, "owed")
		if approval.State != "stale" || approval.DigestOwed == nil || !*approval.DigestOwed || approval.CurrentSHA256 != nil {
			t.Fatalf("approval = %#v, want a stale approval, digest_owed true and no current_sha256", approval)
		}
		if approval.Stands() {
			t.Fatal("Stands = true, want false while Dispatch owes the digest of the latest version")
		}
	})

	t.Run("digest_owed absent reads apart from false", func(t *testing.T) {
		approval := read(t, "no-digest-owed")
		if approval.DigestOwed != nil {
			t.Fatalf("DigestOwed = %v, want nil for a read that carries no digest_owed", *approval.DigestOwed)
		}
		if approval.Stands() {
			t.Fatal("Stands = true, want today's rule: with no digest_owed an approval of version 7 at latest 8 does not stand")
		}
		if standing := read(t, "standing"); !standing.Stands() {
			t.Fatal("the same read with digest_owed false does not stand, want it standing")
		}
	})

	t.Run("a verdict with no approval does not stand", func(t *testing.T) {
		if approval := read(t, "not-approved"); approval.Stands() {
			t.Fatalf("approval = %#v stands, want an approval request with digest_owed false not standing", approval)
		}
	})

	t.Run("LatestReviewer follows Stands", func(t *testing.T) {
		for id, want := range map[string]struct {
			stands   bool
			reviewer Actor
		}{
			"version-mode":   {true, person},
			"standing":       {true, person},
			"owed":           {false, Actor{}},
			"no-digest-owed": {false, Actor{}},
			"not-approved":   {false, Actor{}},
		} {
			approval := read(t, id)
			if got := approval.LatestReviewer(); approval.Stands() != want.stands || got != want.reviewer {
				t.Errorf("%s: Stands = %t and LatestReviewer = %#v, want %t and %#v", id, approval.Stands(), got, want.stands, want.reviewer)
			}
		}
	})
}
