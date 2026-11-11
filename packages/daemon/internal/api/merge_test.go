package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/credential"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// mergeNow is the moment the merge-mark harness's daemon clock stands at. The routes read the
// clock the server was given (Options.Now), and a mark records the moment of the call, so a test
// that reads a mark back compares it against this and never against the wall clock.
var mergeNow = time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)

// newMergeHarness serves the three merge-mark routes over a real store, with the project's
// Dispatch key (the merge holds are kept under it), the repositories the project serves (a hold
// may name one of them and no other), and a clock the test holds still and may move. It records
// the tree the marks are about, so the issue checks have a recorded issue to find.
//
// The clock is answered through a pointer the test may write: the holds route reads "now" once
// per call, for the hold list's own end-time check and for the moment it expedites to, so a test
// that wants a later call moves the clock between the two.
func newMergeHarness(t *testing.T) (*harness, *time.Time) {
	t.Helper()
	h := newHarness(t)
	clock := mergeNow
	h.handler = NewServer("127.0.0.1", 8437, Options{
		Steps:           config.DesignGateRootIssues.Steps(),
		Supervisor:      h.supervisor,
		BootTokens:      h.tokens,
		Project:         testProject,
		DispatchProject: testDispatchProject,
		OperatorToken:   testOperatorToken,
		Controller:      h.store,
		Grants:          credential.New(nil),
		Pool:            h.store.Pool(),
		Record:          record.NewStore(),
		GitHubRepositories: func() []ghrepo.Repository {
			return []ghrepo.Repository{ghrepo.MustParse("acme/widgets"), ghrepo.MustParse("sjawhar/legion")}
		},
		Now: func() time.Time { return clock },
	}).Handler
	seedTree(t, h, "LEGION-208", "LEGION-209")
	return h, &clock
}

// controllerSessionOf registers a controller session with a freshly minted capability, as
// `legion controller start` and the controller's own registration do, and answers its session id:
// the session a mark this harness writes records as its author.
func controllerSessionOf(t *testing.T, h *harness) string {
	t.Helper()
	h.registeredController(h.mintedSecret(), "ses_controller")
	return "ses_controller"
}

// jumpOf is the jump mark the record holds for issue, nil when it holds none.
func jumpOf(t *testing.T, h *harness, issue string) *record.MergeJump {
	t.Helper()
	var mark *record.MergeJump
	if err := pgx.BeginFunc(context.Background(), h.store.Pool(), func(tx pgx.Tx) error {
		var err error
		mark, err = record.NewStore().MergeJumpOf(context.Background(), tx, issue)
		return err
	}); err != nil {
		t.Fatalf("read the jump mark of %s: %v", issue, err)
	}
	return mark
}

// holdsAt is the project's merge hold list as the daemon and the controller read it at at: the
// read that deletes the holds that have ended by then (record.Store.MergeHolds).
func holdsAt(t *testing.T, h *harness, at time.Time) []record.MergeHold {
	t.Helper()
	var holds []record.MergeHold
	if err := pgx.BeginFunc(context.Background(), h.store.Pool(), func(tx pgx.Tx) error {
		var err error
		holds, err = record.NewStore().MergeHolds(context.Background(), tx, testDispatchProject, at)
		return err
	}); err != nil {
		t.Fatalf("read the merge holds of %s: %v", testDispatchProject, err)
	}
	return holds
}

// enqueueReadyMerge queues one ready_merge row for issue, deferred to nextAt, as a submission the
// daemon has put off does, and answers its id.
func enqueueReadyMerge(t *testing.T, h *harness, issue string, nextAt time.Time) int64 {
	t.Helper()
	row, err := record.NewOutboxRow(issue, record.ReadyMerge{Number: 42, Head: "ready-head"}, nextAt)
	if err != nil {
		t.Fatalf("build the ready_merge row of %s: %v", issue, err)
	}
	if err := pgx.BeginFunc(context.Background(), h.store.Pool(), func(tx pgx.Tx) error {
		return record.NewStore().Enqueue(context.Background(), tx, row)
	}); err != nil {
		t.Fatalf("queue the ready_merge row of %s: %v", issue, err)
	}
	var id int64
	if err := h.store.Pool().QueryRow(context.Background(),
		"select id from outbox where kind = 'ready_merge' and issue = $1", issue).Scan(&id); err != nil {
		t.Fatalf("read the id of the ready_merge row of %s: %v", issue, err)
	}
	return id
}

// outboxNextAt is when outbox row id runs next.
func outboxNextAt(t *testing.T, h *harness, id int64) time.Time {
	t.Helper()
	var next time.Time
	if err := h.store.Pool().QueryRow(context.Background(), "select next_at from outbox where id = $1", id).Scan(&next); err != nil {
		t.Fatalf("read the next_at of outbox row %d: %v", id, err)
	}
	return next.UTC()
}

// liveHold is one hold of a request list that passes every check: a path, a reason, and an end
// time after the call.
func liveHold(endsAt time.Time) MergeHoldRequest {
	return MergeHoldRequest{Paths: []string{"packages/daemon/"}, Reason: "the release is going out", EndsAt: endsAt}
}

// Each of the three merge-mark routes answers the empty object, and each has a fixture of its
// own, as every other route of this package does: the plugin parses these exact answers, so a
// route whose wire shape changed would have to change its fixture here and say so.
func TestMergeJumpAndMergeHoldRouteGoldens(t *testing.T) {
	golden(t, "merge-jump.json", EmptyResponse{})
	golden(t, "merge-jump-clear.json", EmptyResponse{})
	golden(t, "merge-holds.json", EmptyResponse{})
}

// A jump mark records what the controller decided, who decided it and when: the issue, the
// reason, the controller session the grant was minted for, and the daemon's own clock at the
// call. The account of a pull request that jumped the queue is written from those, so a mark that
// recorded neither author nor moment would leave that account anonymous.
func TestMergeJumpRecordsTheMarkWithItsSessionAndTheTimeOfTheCall(t *testing.T) {
	h, _ := newMergeHarness(t)
	session := controllerSessionOf(t, h)
	grant := controllerGrant(t, h)

	recorder := h.request(http.MethodPost, "/legion/v1/merge/jump", MergeJumpRequest{
		GrantID: grant.GrantID, Issue: "LEGION-208", Reason: "the release waits on this merge",
	}, nil)

	if recorder.Code != http.StatusOK {
		t.Fatalf("merge jump = %d: %s", recorder.Code, recorder.Body)
	}
	var response EmptyResponse
	decodeInto(t, recorder, &response)
	mark := jumpOf(t, h, "LEGION-208")
	if mark == nil {
		t.Fatal("the jump mark of LEGION-208 reads as none, want the mark the route wrote")
	}
	if mark.Reason != "the release waits on this merge" || mark.Session != session || !mark.SetAt.Equal(mergeNow) {
		t.Fatalf("the jump mark = %+v, want the reason of the call, the session %s and %s", *mark, session, mergeNow)
	}
}

// A second mark of the same issue is the controller's newer word and replaces the first: an issue
// holds one mark, and the merge about to be submitted is the one it governs.
func TestMergeJumpReplacesTheMarkTheIssueHolds(t *testing.T) {
	h, clock := newMergeHarness(t)
	controllerSessionOf(t, h)
	mark := func(reason string) {
		t.Helper()
		grant := controllerGrant(t, h)
		recorder := h.request(http.MethodPost, "/legion/v1/merge/jump", MergeJumpRequest{
			GrantID: grant.GrantID, Issue: "LEGION-208", Reason: reason,
		}, nil)
		if recorder.Code != http.StatusOK {
			t.Fatalf("merge jump = %d: %s", recorder.Code, recorder.Body)
		}
	}

	mark("the release waits on this merge")
	*clock = mergeNow.Add(time.Hour)
	mark("the incident fix goes first")

	held := jumpOf(t, h, "LEGION-208")
	if held == nil || held.Reason != "the incident fix goes first" || !held.SetAt.Equal(mergeNow.Add(time.Hour)) {
		t.Fatalf("the jump mark after a second set = %+v, want the second mark alone", held)
	}
}

// Taking a mark off leaves the issue carrying none, and taking off a mark that is not there is
// the same answer: the controller asked for an issue with no mark, and that is what it has.
func TestMergeJumpClearTakesTheMarkOff(t *testing.T) {
	h, _ := newMergeHarness(t)
	controllerSessionOf(t, h)
	set := controllerGrant(t, h)
	if recorder := h.request(http.MethodPost, "/legion/v1/merge/jump", MergeJumpRequest{
		GrantID: set.GrantID, Issue: "LEGION-208", Reason: "the release waits on this merge",
	}, nil); recorder.Code != http.StatusOK {
		t.Fatalf("merge jump = %d: %s", recorder.Code, recorder.Body)
	}

	for _, round := range []string{"the mark the controller set", "an issue that carries none"} {
		grant := controllerGrant(t, h)
		recorder := h.request(http.MethodPost, "/legion/v1/merge/jump/clear", MergeJumpClearRequest{
			GrantID: grant.GrantID, Issue: "LEGION-208",
		}, nil)
		if recorder.Code != http.StatusOK {
			t.Fatalf("merge jump clear of %s = %d: %s", round, recorder.Code, recorder.Body)
		}
		var response EmptyResponse
		decodeInto(t, recorder, &response)
		if mark := jumpOf(t, h, "LEGION-208"); mark != nil {
			t.Fatalf("the jump mark after clearing %s = %+v, want none", round, *mark)
		}
	}
}

// What the two jump routes refuse, each by a code of its own and with a sentence naming what it
// refused, and each leaving the record exactly as it was: a mark already standing is still the
// mark the issue holds after every refusal below.
func TestMergeJumpRefusesWhatItCannotMark(t *testing.T) {
	for _, tc := range []struct {
		name   string
		route  string
		body   func(grant string) any
		status int
		code   string
		// names is what the refusal's sentence must name, so the answer says what was refused.
		names string
	}{
		{
			name:  "a mark with no grant",
			route: "/legion/v1/merge/jump",
			body: func(string) any {
				return MergeJumpRequest{Issue: "LEGION-208", Reason: "the release waits on this merge"}
			},
			status: http.StatusBadRequest, code: "MISSING_FIELD", names: "grantId",
		},
		{
			name:  "a mark naming no issue",
			route: "/legion/v1/merge/jump",
			body: func(grant string) any {
				return MergeJumpRequest{GrantID: grant, Reason: "the release waits on this merge"}
			},
			status: http.StatusBadRequest, code: "MISSING_FIELD", names: "issue",
		},
		{
			name:   "a mark with no reason",
			route:  "/legion/v1/merge/jump",
			body:   func(grant string) any { return MergeJumpRequest{GrantID: grant, Issue: "LEGION-208"} },
			status: http.StatusBadRequest, code: "MISSING_FIELD", names: "reason",
		},
		{
			name: "a mark of an issue the daemon does not record",
			body: func(grant string) any {
				return MergeJumpRequest{GrantID: grant, Issue: "LEGION-404", Reason: "the release waits on this merge"}
			},
			route: "/legion/v1/merge/jump", status: http.StatusNotFound, code: "ISSUE_NOT_RECORDED", names: "LEGION-404",
		},
		{
			name:  "taking a mark off an issue the daemon does not record",
			route: "/legion/v1/merge/jump/clear",
			body: func(grant string) any {
				return MergeJumpClearRequest{GrantID: grant, Issue: "LEGION-404"}
			},
			status: http.StatusNotFound, code: "ISSUE_NOT_RECORDED", names: "LEGION-404",
		},
		{
			name:   "taking a mark off no issue",
			route:  "/legion/v1/merge/jump/clear",
			body:   func(grant string) any { return MergeJumpClearRequest{GrantID: grant} },
			status: http.StatusBadRequest, code: "MISSING_FIELD", names: "issue",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := newMergeHarness(t)
			controllerSessionOf(t, h)
			standing := controllerGrant(t, h)
			if recorder := h.request(http.MethodPost, "/legion/v1/merge/jump", MergeJumpRequest{
				GrantID: standing.GrantID, Issue: "LEGION-209", Reason: "the mark that was already there",
			}, nil); recorder.Code != http.StatusOK {
				t.Fatalf("the standing mark = %d: %s", recorder.Code, recorder.Body)
			}
			grant := controllerGrant(t, h)

			recorder := h.request(http.MethodPost, tc.route, tc.body(grant.GrantID), nil)

			refusal := recorder.Body.String()
			assertFailure(t, recorder, tc.status, tc.code)
			assertNames(t, refusal, tc.names)
			if mark := jumpOf(t, h, "LEGION-209"); mark == nil || mark.Reason != "the mark that was already there" {
				t.Fatalf("the standing mark after the refusal = %+v, want it where it was", mark)
			}
			if mark := jumpOf(t, h, "LEGION-208"); mark != nil {
				t.Fatalf("a refused request wrote the mark %+v of LEGION-208, want nothing written", *mark)
			}
		})
	}
}

// What the hold list refuses, each by a code of its own and with a sentence naming the hold it
// refused, and each leaving the list that stands exactly as it was: the list is replaced whole,
// so one bad hold in a list must leave the whole of the old list in force rather than half of
// the new one. Every one is a 400: each is the caller's own request being wrong about a hold,
// and holdList writes no other status.
func TestMergeHoldsRefusesAListItCannotKeep(t *testing.T) {
	ended := mergeNow.Add(-time.Minute)
	for _, tc := range []struct {
		name  string
		holds []MergeHoldRequest
		code  string
		names string
	}{
		{
			name:  "a hold with no path",
			holds: []MergeHoldRequest{{Reason: "the release is going out", EndsAt: mergeNow.Add(time.Hour)}},
			code:  "HOLD_WITHOUT_PATH", names: "hold 1 of the list",
		},
		{
			name: "a hold whose path is empty",
			holds: []MergeHoldRequest{liveHold(mergeNow.Add(time.Hour)),
				{Paths: []string{""}, Reason: "the release is going out", EndsAt: mergeNow.Add(time.Hour)}},
			code: "HOLD_WITHOUT_PATH", names: "hold 2 of the list",
		},
		{
			name:  "a hold with no reason",
			holds: []MergeHoldRequest{{Paths: []string{"packages/daemon/"}, EndsAt: mergeNow.Add(time.Hour)}},
			code:  "HOLD_WITHOUT_REASON", names: "hold 1 of the list",
		},
		{
			name:  "a hold that has ended already",
			holds: []MergeHoldRequest{liveHold(ended)},
			code:  "HOLD_ALREADY_ENDED", names: ended.Format(time.RFC3339),
		},
		{
			name:  "a hold that ends at the moment of the call",
			holds: []MergeHoldRequest{liveHold(mergeNow)},
			code:  "HOLD_ALREADY_ENDED", names: "is not after",
		},
		{
			name: "a hold whose path is no regular expression",
			holds: []MergeHoldRequest{{Paths: []string{"packages/daemon/", "charts/[a-"},
				Reason: "the release is going out", EndsAt: mergeNow.Add(time.Hour)}},
			code: "INVALID_HOLD_PATH", names: "charts/[a-",
		},
		{
			name: "a hold of a repository the project does not serve",
			holds: []MergeHoldRequest{{Paths: []string{"packages/daemon/"}, Repository: "acme/gadgets",
				Reason: "the release is going out", EndsAt: mergeNow.Add(time.Hour)}},
			code: "REPOSITORY_NOT_SERVED", names: "acme/gadgets",
		},
		{
			name: "a hold whose repository is not owner/name",
			holds: []MergeHoldRequest{{Paths: []string{"packages/daemon/"}, Repository: "widgets",
				Reason: "the release is going out", EndsAt: mergeNow.Add(time.Hour)}},
			code: "REPOSITORY_NOT_SERVED", names: "widgets",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := newMergeHarness(t)
			controllerSessionOf(t, h)
			standing := controllerGrant(t, h)
			if recorder := h.request(http.MethodPost, "/legion/v1/merge/holds", MergeHoldsRequest{
				GrantID: standing.GrantID,
				Holds:   []MergeHoldRequest{{Paths: []string{"infra/"}, Reason: "a migration lands by hand", EndsAt: mergeNow.Add(3 * time.Hour)}},
			}, nil); recorder.Code != http.StatusOK {
				t.Fatalf("the standing list = %d: %s", recorder.Code, recorder.Body)
			}
			grant := controllerGrant(t, h)

			recorder := h.request(http.MethodPost, "/legion/v1/merge/holds", MergeHoldsRequest{GrantID: grant.GrantID, Holds: tc.holds}, nil)

			refusal := recorder.Body.String()
			assertFailure(t, recorder, http.StatusBadRequest, tc.code)
			assertNames(t, refusal, tc.names)
			holds := holdsAt(t, h, mergeNow)
			if len(holds) != 1 || holds[0].Reason != "a migration lands by hand" {
				t.Fatalf("the hold list after the refusal = %+v, want the one hold that stood before it", holds)
			}
		})
	}
}

// Every merge mark is the controller's: a phase worker's grant redeems on all three routes and
// is refused by each of them, after every check on its request has passed, and writes nothing.
func TestMergeJumpAndMergeHoldRoutesRefuseAPhaseWorkersGrant(t *testing.T) {
	h, _ := newMergeHarness(t)
	controllerSessionOf(t, h)
	worker := newLiveClaim(t, h, "LEGION-208", claim.RoleImplementer)
	for _, tc := range []struct {
		route string
		body  func(grant string) any
	}{
		{route: "/legion/v1/merge/jump", body: func(grant string) any {
			return MergeJumpRequest{GrantID: grant, Issue: "LEGION-208", Reason: "the release waits on this merge"}
		}},
		{route: "/legion/v1/merge/jump/clear", body: func(grant string) any {
			return MergeJumpClearRequest{GrantID: grant, Issue: "LEGION-208"}
		}},
		{route: "/legion/v1/merge/holds", body: func(grant string) any {
			return MergeHoldsRequest{GrantID: grant, Holds: []MergeHoldRequest{liveHold(mergeNow.Add(time.Hour))}}
		}},
	} {
		t.Run(tc.route, func(t *testing.T) {
			recorder := h.request(http.MethodPost, tc.route, tc.body(worker.grant(t)), nil)

			assertFailure(t, recorder, http.StatusForbidden, "CONTROLLER_REQUIRED")
			if mark := jumpOf(t, h, "LEGION-208"); mark != nil {
				t.Fatalf("the phase worker's request wrote the mark %+v, want nothing written", *mark)
			}
			if holds := holdsAt(t, h, mergeNow); len(holds) != 0 {
				t.Fatalf("the phase worker's request wrote the holds %+v, want nothing written", holds)
			}
		})
	}
}

// The list is replaced whole: the second call is the list, so a hold the first call carried and
// the second does not is gone, and an empty list clears every hold. Each hold comes back as the
// controller sent it, with the session that set it and the time of the call.
func TestMergeHoldsReplacesTheWholeListAndAnEmptyListClearsIt(t *testing.T) {
	h, clock := newMergeHarness(t)
	session := controllerSessionOf(t, h)
	replace := func(holds []MergeHoldRequest) {
		t.Helper()
		grant := controllerGrant(t, h)
		recorder := h.request(http.MethodPost, "/legion/v1/merge/holds", MergeHoldsRequest{GrantID: grant.GrantID, Holds: holds}, nil)
		if recorder.Code != http.StatusOK {
			t.Fatalf("merge holds = %d: %s", recorder.Code, recorder.Body)
		}
		var response EmptyResponse
		decodeInto(t, recorder, &response)
	}

	replace([]MergeHoldRequest{
		{Paths: []string{"packages/daemon/", "charts/"}, Repository: "ACME/Widgets", Reason: "the release is going out", EndsAt: mergeNow.Add(time.Hour)},
		{Paths: []string{"docs/"}, Reason: "the handbook is being rewritten", EndsAt: mergeNow.Add(2 * time.Hour)},
	})
	if holds := holdsAt(t, h, mergeNow); len(holds) != 2 {
		t.Fatalf("the hold list = %+v, want the two holds the first call carried", holds)
	}

	*clock = mergeNow.Add(time.Minute)
	replace([]MergeHoldRequest{{Paths: []string{"infra/"}, Reason: "a migration lands by hand", EndsAt: mergeNow.Add(3 * time.Hour)}})

	holds := holdsAt(t, h, mergeNow)
	if len(holds) != 1 {
		t.Fatalf("the hold list after the second call = %+v, want the second list alone", holds)
	}
	if holds[0].Reason != "a migration lands by hand" || holds[0].Session != session || !holds[0].SetAt.Equal(mergeNow.Add(time.Minute)) {
		t.Fatalf("the hold = %+v, want it with the session %s and the time of its own call", holds[0], session)
	}
	if holds[0].Repository != "" {
		t.Fatalf("the hold's repository = %q, want every repository the project serves", holds[0].Repository)
	}

	replace(nil)

	if holds := holdsAt(t, h, mergeNow); len(holds) != 0 {
		t.Fatalf("the hold list after an empty list = %+v, want none", holds)
	}
}

// A hold whose end time has passed is no longer in the list the daemon and the controller read:
// the read deletes it (record.Store.MergeHolds), so a controller reading the list before it
// replaces it never copies a dead hold forward, and the live hold beside it stays.
func TestMergeHoldsAreAbsentFromTheListOnceTheyHaveEnded(t *testing.T) {
	h, _ := newMergeHarness(t)
	controllerSessionOf(t, h)
	grant := controllerGrant(t, h)
	if recorder := h.request(http.MethodPost, "/legion/v1/merge/holds", MergeHoldsRequest{
		GrantID: grant.GrantID,
		Holds: []MergeHoldRequest{
			{Paths: []string{"packages/daemon/"}, Reason: "the release is going out", EndsAt: mergeNow.Add(time.Hour)},
			{Paths: []string{"docs/"}, Reason: "the handbook is being rewritten", EndsAt: mergeNow.Add(3 * time.Hour)},
		},
	}, nil); recorder.Code != http.StatusOK {
		t.Fatalf("merge holds = %d: %s", recorder.Code, recorder.Body)
	}

	holds := holdsAt(t, h, mergeNow.Add(2*time.Hour))

	if len(holds) != 1 || holds[0].Reason != "the handbook is being rewritten" {
		t.Fatalf("the hold list two hours on = %+v, want the hold that had not ended", holds)
	}
}

// A replacement releases the merges the project is holding back: every deferred ready_merge row
// of the project is made due at the moment of the call, in the same transaction that writes the
// list. Without it a merge deferred to the end of a hold the controller has just taken off would
// wait until a moment the record no longer holds. A row of another project is left where it is,
// and a row already due is not moved back.
func TestMergeHoldsExpeditesTheProjectsDeferredMerges(t *testing.T) {
	h, _ := newMergeHarness(t)
	controllerSessionOf(t, h)
	seedTree(t, h, "OTHER-7")
	deferred := enqueueReadyMerge(t, h, "LEGION-208", mergeNow.Add(30*time.Minute))
	due := enqueueReadyMerge(t, h, "LEGION-209", mergeNow.Add(-time.Minute))
	other := enqueueReadyMerge(t, h, "OTHER-7", mergeNow.Add(30*time.Minute))
	grant := controllerGrant(t, h)

	if recorder := h.request(http.MethodPost, "/legion/v1/merge/holds", MergeHoldsRequest{GrantID: grant.GrantID}, nil); recorder.Code != http.StatusOK {
		t.Fatalf("merge holds = %d: %s", recorder.Code, recorder.Body)
	}

	if next := outboxNextAt(t, h, deferred); !next.Equal(mergeNow) {
		t.Fatalf("the deferred merge runs at %s, want the moment of the call %s", next, mergeNow)
	}
	if next := outboxNextAt(t, h, due); !next.Equal(mergeNow.Add(-time.Minute)) {
		t.Fatalf("the merge already due runs at %s, want it where it was", next)
	}
	if next := outboxNextAt(t, h, other); !next.Equal(mergeNow.Add(30 * time.Minute)) {
		t.Fatalf("another project's merge runs at %s, want it where it was", next)
	}
}

// assertNames holds a refusal's sentence to naming what it refused: the field, the issue, the
// hold, the path or the repository the caller sent, so the answer tells a controller what to
// change without reading the daemon's log. It takes the body as text, read before the refusal
// was decoded: the recorder's body is a reader, and decoding it leaves nothing to read.
func assertNames(t *testing.T, refusal, named string) {
	t.Helper()
	if !strings.Contains(refusal, named) {
		t.Fatalf("the refusal %s does not name %q", refusal, named)
	}
}
