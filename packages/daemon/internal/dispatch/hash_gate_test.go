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
// digest_owed and a false one read apart. An approval whose digest Dispatch owes does not stand,
// though Dispatch shows it approved (artifact-owed.json). The fixtures hold no read of a Dispatch
// that sends no digest field, so version mode reads artifact.json with neither current_sha256 nor
// digest_owed, approved at its latest version, and the read the client's own tests serve
// (approvedAtLatestBody).
func TestHashGateApprovalReadsTheVerdict(t *testing.T) {
	// standingDigest is artifact.json's current_sha256, the latest version's digest.
	const standingDigest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	standing := hashGateFixture(t, "artifact.json")
	bodies := map[string][]byte{
		"standing": standing,
		"owed":     hashGateFixture(t, "artifact-owed.json"),
		"version-mode": withApproval(t, standing, func(approval map[string]any) {
			delete(approval, "current_sha256")
			delete(approval, "digest_owed")
			approval["version"] = approval["latest_version"]
		}),
		"client-version-mode": []byte(mustJSON(t, approvedAtLatestBody)),
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

	t.Run("version mode reads today's rule", func(t *testing.T) {
		for id, latest := range map[string]int{
			"version-mode":        2,
			"client-version-mode": 3,
		} {
			approval := read(t, id)
			if approval.DigestOwed != nil || approval.CurrentSHA256 != nil {
				t.Fatalf("%s: approval = %#v, want no digest_owed and no current_sha256 from a Dispatch that sends neither", id, approval)
			}
			if approval.State != "approved" || approval.Version == nil || *approval.Version != latest || approval.LatestVersion != latest {
				t.Fatalf("%s: approval = %#v, want the approval of version %d, its latest", id, approval, latest)
			}
			if approval.ApprovedLatest() != latest || !approval.Stands() {
				t.Fatalf("%s: ApprovedLatest = %d and Stands = %t, want %d and Stands as today's rule", id, approval.ApprovedLatest(), approval.Stands(), latest)
			}
		}
	})

	t.Run("an approval standing below the latest version stands", func(t *testing.T) {
		approval := read(t, "standing")
		if approval.State != "approved" || approval.Version == nil || *approval.Version != 1 || approval.LatestVersion != 2 {
			t.Fatalf("approval = %#v, want the approval of version 1 at latest version 2", approval)
		}
		if approval.DigestOwed == nil || *approval.DigestOwed || approval.CurrentSHA256 == nil || *approval.CurrentSHA256 != standingDigest {
			t.Fatalf("approval = %#v, want digest_owed false and the latest version's current_sha256", approval)
		}
		if !approval.Stands() {
			t.Fatal("Stands = false, want true: Dispatch owes no digest and shows the approval")
		}
		if approval.ApprovedLatest() != 0 {
			t.Fatalf("ApprovedLatest = %d, want 0: its meaning is unchanged, the approval is of version 1 below latest 2", approval.ApprovedLatest())
		}
	})

	t.Run("a digest owed does not stand", func(t *testing.T) {
		approval := read(t, "owed")
		if approval.State != "approved" || approval.Version == nil || *approval.Version != 1 || approval.LatestVersion != 2 {
			t.Fatalf("approval = %#v, want Dispatch's approval of version 1 at latest version 2", approval)
		}
		if approval.DigestOwed == nil || !*approval.DigestOwed || approval.CurrentSHA256 != nil {
			t.Fatalf("approval = %#v, want digest_owed true and no current_sha256", approval)
		}
		if approval.Stands() {
			t.Fatal("Stands = true, want false: Dispatch shows the approval, but owes the digest of the latest version")
		}
	})

	t.Run("digest_owed absent reads apart from false", func(t *testing.T) {
		approval := read(t, "no-digest-owed")
		if approval.DigestOwed != nil {
			t.Fatalf("DigestOwed = %v, want nil for a read that carries no digest_owed", *approval.DigestOwed)
		}
		if approval.Stands() {
			t.Fatal("Stands = true, want today's rule: with no digest_owed an approval of version 1 at latest 2 does not stand")
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
}
