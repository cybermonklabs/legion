package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// storedStoreFailure is the two columns of the session store's failure on token's claim row,
// read from the row itself: the zero time and "" for a row that holds none.
func storedStoreFailure(t *testing.T, h *harness, token claim.Token) (time.Time, string) {
	t.Helper()
	var since *time.Time
	var storeError string
	if err := h.store.Pool().QueryRow(h.ctx, `select store_failed_since, store_failed_error from claims where token = $1`,
		string(token)).Scan(&since, &storeError); err != nil {
		t.Fatalf("read the store failure of %s: %v", token, err)
	}
	if since == nil {
		return time.Time{}, storeError
	}
	return *since, storeError
}

// Half a store failure is a malformed report: one with no moment the session read it from, or no
// store error, is refused 400 by the field it leaves out, as a report with no reading time is,
// and the claim keeps no failure. The control is the whole failure, which the route writes.
func TestClaimActivityRefusesAStoreFailureWithNoSinceOrNoError(t *testing.T) {
	h := newHarness(t)
	token, bootToken := h.launch("LEGION-208", "implementer")
	registered := h.registered(bootToken, "ses_implementer")
	read := time.Date(2026, 11, 24, 2, 17, 0, 0, time.UTC)
	since := read.Add(-time.Minute)

	for _, tc := range []struct {
		name    string
		failure *claim.StoreFailure
		field   string
	}{
		{name: "no since", failure: &claim.StoreFailure{Error: "Session file changed before rewrite"}, field: "activity.storeFailure.since"},
		{name: "no error", failure: &claim.StoreFailure{Since: since}, field: "activity.storeFailure.error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			refused := h.request(http.MethodPost, "/legion/v1/claims/activity", claim.ActivityRequest{
				ClaimToken: token, SessionID: "ses_implementer", Secret: registered.Secret,
				Activity: claim.SessionActivity{At: read, StoreFailure: tc.failure},
			}, nil)
			if refused.Code != http.StatusBadRequest || !strings.Contains(refused.Body.String(), tc.field) {
				t.Errorf("a store failure with %s = %d %s, want 400 naming %s", tc.name, refused.Code, refused.Body, tc.field)
			}
			if stored, storeError := storedStoreFailure(t, h, token); !stored.IsZero() || storeError != "" {
				t.Errorf("a refused report left the store failure {%s %q}", stored, storeError)
			}
		})
	}

	whole := h.request(http.MethodPost, "/legion/v1/claims/activity", claim.ActivityRequest{
		ClaimToken: token, SessionID: "ses_implementer", Secret: registered.Secret,
		Activity: claim.SessionActivity{At: read, StoreFailure: &claim.StoreFailure{Since: since, Error: "Session file changed before rewrite"}},
	}, nil)
	if whole.Code != http.StatusNoContent {
		t.Fatalf("a whole store failure = %d %s, want 204", whole.Code, whole.Body)
	}
	if stored, storeError := storedStoreFailure(t, h, token); !stored.Equal(since) || storeError != "Session file changed before rewrite" {
		t.Errorf("the whole store failure is stored as {%s %q}, want {%s %q}", stored, storeError, since, "Session file changed before rewrite")
	}
}

// storeFailureContract is the contract whose entry in version.go added the key, and this
// daemon's own contract is never below it: a caller at this contract is served the failure.
func TestTheStoreFailureContractIsTheContractThatAddedTheKey(t *testing.T) {
	const addedStoreFailure = 264
	if storeFailureContract != addedStoreFailure {
		t.Fatalf("storeFailureContract = %d, want %d, the contract whose entry in version.go adds the claim views' `storeFailure`",
			storeFailureContract, addedStoreFailure)
	}
	if storeFailureContract > DaemonAPIVersion {
		t.Fatalf("storeFailureContract = %d, above DaemonAPIVersion %d: no caller could be served the key",
			storeFailureContract, DaemonAPIVersion)
	}
}

// A plugin of the contract before storeFailureContract reads each claim view with a strict
// schema that does not know `storeFailure`, so the route serves a caller that states that
// contract every claim view without the key, and says so in its own log; a caller that states the
// contract is served the key, and the rest of the two bodies is the same. Every build from
// shapedAnswersContract on states its contract in the header, so the header is what places a
// caller below this band, as it places one below intakeGiveUpsContract's.
func TestAnOlderPluginContractGetsTheStateWithoutStoreFailure(t *testing.T) {
	older, narrowings := stateNarrowedStating(t, populatedState(t), "192.0.2.1:41000", nil, storeFailureContract-1)
	current := stateStating(t, stateServer(t, populatedState(t), nil), "192.0.2.1:41000", storeFailureContract)
	if bytes.Contains(older, []byte(`"storeFailure"`)) {
		t.Errorf("a caller at %d is served `storeFailure`, which its plugin's schema does not name: %s", storeFailureContract-1, older)
	}
	if !bytes.Contains(current, []byte(`"storeFailure"`)) {
		t.Fatalf("a caller at %d is served no `storeFailure`, though the state carries one: %s", storeFailureContract, current)
	}
	var dropped []string
	for _, narrowing := range narrowings {
		if narrowing.PluginContract != storeFailureContract-1 {
			t.Errorf("the daemon logged %+v for a caller at %d, a line naming another contract", narrowing, storeFailureContract-1)
		}
		dropped = append(dropped, narrowing.Dropped)
	}
	if want := []string{"the claim views' storeFailure"}; !slices.Equal(dropped, want) {
		t.Errorf("the daemon logged %+v for a caller at %d, want one line saying it dropped the claim views' storeFailure", narrowings, storeFailureContract-1)
	}
	var olderBody, currentBody map[string]any
	if err := json.Unmarshal(older, &olderBody); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(withoutStoreFailureKeys(t, current), &currentBody); err != nil {
		t.Fatal(err)
	}
	for _, body := range []map[string]any{olderBody, currentBody} {
		delete(body, "contract")
	}
	olderRest, _ := json.Marshal(olderBody)
	currentRest, _ := json.Marshal(currentBody)
	if !bytes.Equal(olderRest, currentRest) {
		t.Errorf("the two bodies differ beyond `storeFailure`:\n%s\n%s", olderRest, currentRest)
	}
}
