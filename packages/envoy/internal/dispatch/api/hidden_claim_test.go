package api

import (
	"net/http"
	"slices"
	"strings"
	"testing"
)

// TestAClaimOverAHolderItsCallerMayNotSeeAsksTheListenerNothing: an issue alice sees may be claimed
// by a session she may not see, one that has acted in PRIV. Her claim of such an issue answers the
// same whether that holder is running or gone, and never asks the listener about it, so the answer
// says nothing of whether the session is live: PUB's first issue is claimed by s-priv-gone, which
// the walk's listener holds no record of, and its second by privateSession, which it holds live.
// Both of alice's claims are refused 409 ISSUE_CLAIMED naming no holder, byte for byte, and neither
// makes a registry read. carol, who sees PRIV, still has the gone holder judged by the listener: her
// claim of the first issue reads s-priv-gone's record and takes the claim.
//
// Control: judgeHolder asking the listener about every holder. alice's claim of the first issue
// reads s-priv-gone's registry record, finds it gone and takes the claim (200), while her claim of
// the second is refused, and the row fails naming both.
func TestAClaimOverAHolderItsCallerMayNotSeeAsksTheListenerNothing(t *testing.T) {
	t.Parallel()
	walk := seedHiddenWrites(t)
	const goneSession = "s-priv-gone"
	// s-priv-gone acts in PRIV under the credential that proves it, which gives it a
	// session_projects pair of PRIV: a session hidden from alice, an open viewer who does not see
	// PRIV. The walk's listener holds no record of it.
	walk.asSession(t, goneSession, seedPrivate, http.MethodPost, "/api/v1/issues/"+walk.issue[seedPrivate]+"/comments",
		map[string]any{"body": goneSession + " works on PRIV."}, http.StatusCreated)
	second := decodeBody[struct {
		Key string `json:"key"`
	}](t, walk.mustRequest(t, callerCarol, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": seedPublic, "title": "The second issue of PUB",
	}, http.StatusCreated)).Key
	for issue, holder := range map[string]string{walk.issue[seedPublic]: goneSession, second: privateSession} {
		walk.mustRequest(t, callerCarolPrivToken, http.MethodPost, "/api/v1/issues/"+issue+"/claim",
			map[string]any{"actor": map[string]string{"kind": "session", "id": holder}}, http.StatusOK)
	}
	answers := map[string]string{}
	for issue, holder := range map[string]string{walk.issue[seedPublic]: goneSession, second: privateSession} {
		claimed, _, calls := walk.recorded(t, callerAlice, http.MethodPost, "/api/v1/issues/"+issue+"/claim", map[string]any{})
		answers[holder] = strings.TrimSpace(claimed.Body.String())
		if claimed.Code != http.StatusConflict || !strings.Contains(answers[holder], `"code":"ISSUE_CLAIMED"`) ||
			strings.Contains(answers[holder], holder) {
			t.Errorf("alice claims %s, held by %s, which she may not see: %d %s, want 409 ISSUE_CLAIMED naming no holder",
				issue, holder, claimed.Code, answers[holder])
		}
		if asked := slices.IndexFunc(calls, func(call string) bool { return strings.Contains(call, "/v1/registry/") }); asked >= 0 {
			t.Errorf("alice's claim of %s asked the listener %s about a holder she may not see", issue, calls[asked])
		}
	}
	if answers[goneSession] != answers[privateSession] {
		t.Errorf("alice's claim over a gone hidden holder answers %s, and over a live one %s", answers[goneSession], answers[privateSession])
	}
	taken, _, calls := walk.recorded(t, callerCarol, http.MethodPost, "/api/v1/issues/"+walk.issue[seedPublic]+"/claim", map[string]any{})
	if taken.Code != http.StatusOK || !slices.ContainsFunc(calls, func(call string) bool { return strings.Contains(call, "/v1/registry/"+goneSession) }) {
		t.Errorf("carol, who sees %s, claims %s over it: %d %s after listener calls %v, want 200 after reading its record",
			goneSession, walk.issue[seedPublic], taken.Code, strings.TrimSpace(taken.Body.String()), calls)
	}
}
