package daemon

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/redact"
	"github.com/sjawhar/legion/daemon/internal/store/storetext"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// A read the reconcile gives up on records Dispatch's error as the daemon keeps any far side's
// error text (storetext.FarSideError), for a pair's change set and for a review history alike. An
// error that holds a NUL, a byte that is not UTF-8, a URL's token and more than 2048 runes is
// recorded valid, with U+FFFD for the NUL and for the byte, with no token, and cut to its start.
// So the store keeps the reason as the reconcile wrote it, and the record and the news hold one
// text. The whole error is in the log beside the row (storetext.LogWhole), its last words
// included, and with no token either.
func TestAReadGivenUpOnRecordsDispatchsErrorWithItsNULAsAFarSidesErrorAndLogsItWhole(t *testing.T) {
	const planted = "planted-reconcile-token"
	failure := errors.New("dispatch: 502 BAD_GATEWAY from https://dispatch.example/api/v1/artifacts/" + reconcileDoc + "?token=" + planted +
		" after \x00 a reset \xff, " + strings.Repeat("the upstream answer goes on. ", 100) + "these are the last words")
	const cleaned = "dispatch: 502 BAD_GATEWAY from https://dispatch.example/api/v1/artifacts/" + reconcileDoc + " after \uFFFD a reset \uFFFD, "
	question := record.DesignHistoryQuestion{ArtifactID: reconcileDoc, To: 5}
	for _, tc := range []struct {
		name   string
		told   int
		what   string
		fail   func(*changeSetDispatch)
		reason func(*newsReconcileRig) (string, bool)
	}{
		{
			name: "a pair's change set", told: 4, what: "it",
			fail: func(d *changeSetDispatch) {
				d.answer = func(record.VersionPair) (dispatch.ArtifactChanges, error) { return dispatch.ArtifactChanges{}, failure }
			},
			reason: func(r *newsReconcileRig) (string, bool) {
				changes, held := r.recorded(reconcilePair)
				return changes.Reason, held && changes.State == record.DesignChangesUnavailable
			},
		},
		{
			name: "a review history", told: -1, what: "the document's review history",
			fail: func(d *changeSetDispatch) { d.before = func(string, int) (int, error) { return 0, failure } },
			reason: func(r *newsReconcileRig) (string, bool) {
				unread, held := r.historyUnread(question)
				return unread.Reason, held
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newNewsReconcileRig(t)
			logs := &syncBuffer{}
			r.runner.log = slog.New(slog.NewJSONHandler(logs, redact.HandlerOptions(nil)))
			r.member(reconcileRoot, "", phase.Reviewing)
			r.told(reconcileRoot, record.AudienceHolder, tc.told)
			r.holder(reconcileRoot, claim.RoleReviewer, supervise.StateIdle, "", 1)
			tc.fail(r.client)

			if err := r.reconcile(designChangesAttempts - 1); err != nil {
				t.Fatalf("the last attempt under a failing Dispatch = %v, want the read recorded and the row finished", err)
			}
			reason, held := tc.reason(r)
			if !held {
				t.Fatalf("the record holds no reason for the read given up on, want one")
			}
			prefix := fmt.Sprintf("Dispatch failed %d reads of %s, the last with: ", designChangesAttempts, tc.what)
			kept, found := strings.CutPrefix(reason, prefix)
			if !found || !strings.HasPrefix(kept, cleaned) {
				t.Fatalf("the recorded reason starts %q, want %q and then Dispatch's first words %q", reason[:min(len(reason), 200)], prefix, cleaned)
			}
			if !utf8.ValidString(reason) || strings.ContainsRune(reason, 0) || strings.Contains(reason, planted) {
				t.Fatalf("the recorded reason %q is not valid UTF-8 free of NUL and of the planted token", reason)
			}
			if runes := utf8.RuneCountInString(kept); runes != storetext.FarSideErrorRunes || !strings.HasSuffix(kept, "…") {
				t.Fatalf("the recorded reason keeps %d runes of Dispatch's error, ending %q, want %d ending with …",
					runes, kept[max(0, len(kept)-16):], storetext.FarSideErrorRunes)
			}

			if strings.Contains(logs.String(), planted) {
				t.Fatalf("the log holds the planted token")
			}
			var whole strings.Builder
			for _, entry := range loggedMessages(t, logs.String(), designReadErrorInFull) {
				if entry["row"] == float64(1) && entry["issue"] == reconcileRoot {
					part, _ := entry["error"].(string)
					whole.WriteString(part)
				}
			}
			if !strings.HasPrefix(whole.String(), cleaned) || !strings.HasSuffix(whole.String(), "these are the last words") {
				t.Fatalf("the log holds %q of the error in full, want all of it, from %q to its last words", whole.String(), cleaned)
			}
		})
	}
}
