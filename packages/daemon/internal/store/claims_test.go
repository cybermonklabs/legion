package store

import (
	"bytes"
	"context"
	"errors"
	"math"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// at is a time Postgres stores exactly: timestamptz keeps microseconds.
func at(minute int) time.Time {
	return time.Date(2026, 9, 22, 18, minute, 7, 123456000, time.UTC)
}

func tmuxClaim(token claim.Token) supervise.Claim {
	return supervise.Claim{
		Token:       token,
		Project:     "legion",
		Tree:        "LEGION-208",
		Issue:       "LEGION-209",
		Role:        claim.RoleImplementer,
		Generation:  4,
		Session:     "ses_implementer",
		SessionFile: "/state/sessions/implementer.jsonl",
		Locator: &runtime.Locator{
			Runtime:     runtime.RuntimeTmux,
			Claim:       token,
			Incarnation: "31001:900001",
			Tmux:        &runtime.TmuxLocator{Window: "@3", Pane: "%41"},
		},
		State:           supervise.StateIdle,
		Budgets:         supervise.Budgets{LaunchFailures: 1, Deaths: 2, PromptFailures: 2, PromptRetires: 1},
		BootTokenHash:   supervise.HashBootToken("boot-" + string(token)),
		CapabilityHash:  []byte{0xca, 0xfe, 0x01},
		UncertainStreak: 3,
		// The columns a restart reads to fence a stop, to attribute a completion and to refuse a
		// retried start's task: a daemon that rebuilt this claim without them would suspend a run
		// that had already replaced the one its stop names, refuse the completion of the worker it
		// is waiting on, and queue a task the claim was already given.
		LastStartRow:      7,
		ServingGeneration: 4,
		// The look of the production check this claim's worker last took, which says its next
		// turn's completion is a report on that look rather than a phase completion.
		ServingCheckLook: supervise.CheckLook{Look: 2, OpenedAt: at(3)},
		DeliveredRows:    []int64{5, 6},
	}
}

// sameClaim compares two claims field by field, with the delivery's times compared as instants:
// Postgres hands a timestamptz back in the session's zone, not the one it was written in.
func sameClaim(t *testing.T, got, want supervise.Claim) {
	t.Helper()
	gotPending, wantPending := got.Pending, want.Pending
	got.Pending, want.Pending = nil, nil
	// The served look's check instance is a timestamptz, handed back in the session's zone, so it
	// is compared as an instant and left out of the whole-struct comparison, as the moment the
	// claim failed and the delivery's times are below.
	gotOpened, wantOpened := got.ServingCheckLook.OpenedAt, want.ServingCheckLook.OpenedAt
	got.ServingCheckLook.OpenedAt, want.ServingCheckLook.OpenedAt = time.Time{}, time.Time{}
	gotFailed, wantFailed := got.FailedAt, want.FailedAt
	got.FailedAt, want.FailedAt = time.Time{}, time.Time{}
	// The handshake's two moments are timestamptz columns too, compared the same way.
	gotHandshake, wantHandshake := got.Handshake, want.Handshake
	got.Handshake.QueuedAt, got.Handshake.ConfirmedAt = time.Time{}, time.Time{}
	want.Handshake.QueuedAt, want.Handshake.ConfirmedAt = time.Time{}, time.Time{}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("claim read back differs:\n got %+v\nwant %+v", got, want)
	}
	if !gotOpened.Equal(wantOpened) {
		t.Errorf("the served look's check opened %v, want %v", gotOpened, wantOpened)
	}
	if !gotFailed.Equal(wantFailed) {
		t.Errorf("the claim failed at %v, want %v", gotFailed, wantFailed)
	}
	if !gotHandshake.QueuedAt.Equal(wantHandshake.QueuedAt) || !gotHandshake.ConfirmedAt.Equal(wantHandshake.ConfirmedAt) {
		t.Errorf("the claim's handshake was queued at %v and confirmed at %v, want %v and %v",
			gotHandshake.QueuedAt, gotHandshake.ConfirmedAt, wantHandshake.QueuedAt, wantHandshake.ConfirmedAt)
	}
	if (gotPending == nil) != (wantPending == nil) {
		t.Fatalf("pending delivery read back = %+v, want %+v", gotPending, wantPending)
	}
	if gotPending == nil {
		return
	}
	// Every field but the times is compared whole, so a field added to the delivery is read back
	// or this fails; the times are compared below as instants.
	untimed := func(d supervise.Delivery) supervise.Delivery {
		d.QueuedAt, d.DeliveredAt, d.ConfirmedAt, d.CheckLook.OpenedAt = time.Time{}, time.Time{}, time.Time{}, time.Time{}
		return d
	}
	if !reflect.DeepEqual(untimed(*gotPending), untimed(*wantPending)) {
		t.Errorf("pending delivery read back = %+v, want %+v", *gotPending, *wantPending)
	}
	for _, field := range []struct {
		name      string
		got, want time.Time
	}{
		{"QueuedAt", gotPending.QueuedAt, wantPending.QueuedAt},
		{"DeliveredAt", gotPending.DeliveredAt, wantPending.DeliveredAt},
		{"ConfirmedAt", gotPending.ConfirmedAt, wantPending.ConfirmedAt},
		{"CheckLook.OpenedAt", gotPending.CheckLook.OpenedAt, wantPending.CheckLook.OpenedAt},
	} {
		if !field.got.Equal(field.want) {
			t.Errorf("pending delivery %s = %v, want %v", field.name, field.got, field.want)
		}
	}
}

func onlyClaim(t *testing.T, store *Store) supervise.Claim {
	t.Helper()
	claims, err := store.Claims(context.Background())
	if err != nil {
		t.Fatalf("claims: %v", err)
	}
	if len(claims) != 1 {
		t.Fatalf("claims = %d rows, want 1: %+v", len(claims), claims)
	}
	return claims[0]
}

func TestAClaimRoundTripsWithItsLocatorAndDelivery(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := migratedStore(t)
	want := tmuxClaim("legion-LEGION-209-implementer")
	want.Pending = &supervise.Delivery{
		ID:          "delivery-1",
		Task:        "implement the plan",
		Phase:       phase.Implementing,
		Generation:  4,
		CheckLook:   supervise.CheckLook{Look: 2, OpenedAt: at(3)},
		QueuedAt:    at(1),
		DeliveredAt: at(2),
		Interrupted: true,
		// The prompt that set the mark, since re-queued under delivery-1: a restart judges a late
		// refusal against it.
		MarkedBy: "delivery-0",
	}

	if err := store.PutClaim(ctx, want); err != nil {
		t.Fatalf("put claim: %v", err)
	}
	if err := store.PutDelivery(ctx, want.Token, *want.Pending); err != nil {
		t.Fatalf("put delivery: %v", err)
	}

	sameClaim(t, onlyClaim(t, store), want)
}

// The retro run a claim serves (claims.serving_retro_run) and the retro run its pending task is
// of (pending_task_deliveries.retro_run) round-trip beside the production check look the claim
// serves and its task is of, as a restart reads them: a claim rebuilt without them would route
// the report of its retro after a close as a phase completion (INFRA-5602). The claim also serves
// a look and the task is of a run and an entry, so a column written under another's name is read
// back as the wrong field and fails by its field. The claim and the delivery are compared whole
// (sameClaim), the look's times as instants.
func TestAClaimRoundTripsTheRetroRunItServesAndTheRetroRunOfItsTask(t *testing.T) {
	ctx := context.Background()
	store := migratedStore(t)
	want := tmuxClaim("legion-LEGION-209-implementer")
	want.ServingRetroRun = 4
	want.Pending = &supervise.Delivery{
		ID:          "delivery-1",
		Task:        "Continue the issue. Issue: LEGION-209. Phase: retro.",
		Generation:  4,
		CheckLook:   supervise.CheckLook{Look: 2, OpenedAt: at(3)},
		RetroRun:    5,
		QueuedAt:    at(1),
		DeliveredAt: at(2),
	}

	if err := store.PutClaim(ctx, want); err != nil {
		t.Fatalf("put claim: %v", err)
	}
	if err := store.PutDelivery(ctx, want.Token, *want.Pending); err != nil {
		t.Fatalf("put delivery: %v", err)
	}

	got := onlyClaim(t, store)
	if got.ServingRetroRun != want.ServingRetroRun {
		t.Errorf("the claim read back serves retro run %d, want %d", got.ServingRetroRun, want.ServingRetroRun)
	}
	var gotRun supervise.RetroRun
	if got.Pending != nil {
		gotRun = got.Pending.RetroRun
	}
	if gotRun != want.Pending.RetroRun {
		t.Errorf("the pending delivery read back is of retro run %d, want %d", gotRun, want.Pending.RetroRun)
	}
	sameClaim(t, got, want)
}

// The record of this task's sends that never reached a turn round-trips with the delivery: how
// many failed, when the last of them failed and the error it failed with. A restart rebuilds
// every claim from this table, so a record lost here is a claim that has waited days on a send
// which never lands and says nothing of it after the next restart (INFRA-1480). The last
// failure's time is compared as an instant: Postgres hands a timestamptz back in the session's
// zone, not the one it was written in.
func TestADeliverysFailedSendsRoundTripAndItsConfirmationClearsThem(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := migratedStore(t)
	c := tmuxClaim("legion-LEGION-209-implementer")
	if err := store.PutClaim(ctx, c); err != nil {
		t.Fatalf("put claim: %v", err)
	}
	failing := supervise.Delivery{
		ID: "delivery-1", Task: "implement the plan", Phase: phase.Implementing, Generation: 4, QueuedAt: at(1),
		SendFailures: 4, LastSendFailureAt: at(5),
		LastSendError: "worker-stream: legion-LEGION-209-implementer refused adopt-working-copy: " +
			"the git lock /legion/workspaces/acme/widgets/legion-209/.git/index.lock was last written 31m ago",
	}
	if err := store.PutDelivery(ctx, c.Token, failing); err != nil {
		t.Fatalf("put the failing delivery: %v", err)
	}

	got := onlyClaim(t, store).Pending
	if got == nil {
		t.Fatal("the restarted daemon read no pending delivery")
	}
	if got.SendFailures != failing.SendFailures || !got.LastSendFailureAt.Equal(failing.LastSendFailureAt) ||
		got.LastSendError != failing.LastSendError {
		t.Fatalf("delivery read back = %+v, want %d failed sends, the last at %v, with its error",
			got, failing.SendFailures, failing.LastSendFailureAt)
	}

	// The send that reached a turn clears all three (supervise's confirmCarrying), through the
	// same upsert: a delivery read back still counting failures an earlier turn got past would name
	// a stall that is over.
	confirmed := failing
	confirmed.DeliveredAt, confirmed.MarkedBy, confirmed.ConfirmedAt = at(6), failing.ID, at(7)
	confirmed.SendFailures, confirmed.LastSendFailureAt, confirmed.LastSendError = 0, time.Time{}, ""
	if err := store.PutDelivery(ctx, c.Token, confirmed); err != nil {
		t.Fatalf("put the confirmed delivery: %v", err)
	}

	if got := onlyClaim(t, store).Pending; got == nil || got.SendFailures != 0 ||
		!got.LastSendFailureAt.IsZero() || got.LastSendError != "" {
		t.Fatalf("delivery read back = %+v, want its failure record cleared", got)
	}
}

// The text of a failed send is the far side's own: the wedged command's stderr, relayed by the
// shim, or Oh My Pi's `error` decoded from JSON, where a `\u0000` becomes a NUL byte. This column
// holds only what its server encoding can read, and the store's text rule is what makes such a
// text fit: the NUL is stored as U+FFFD, and every other character as it was written.
//
// Pinned here against the real server. A write of this text once failed, and that failure
// returned before the refused adoption was charged, so the send was re-sent with no bound
// (INFRA-1480). The raw text is taken now and reads back whole, with U+FFFD where the NUL was, so
// the diagnosis an operator reads survives a restart.
func TestADeliverysSendErrorWithANULRoundTripsWithAReplacementCharacter(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := migratedStore(t)
	c := tmuxClaim("legion-LEGION-209-implementer")
	if err := store.PutClaim(ctx, c); err != nil {
		t.Fatalf("put claim: %v", err)
	}
	const raw = "worker-stream: legion-LEGION-209-implementer refused adopt-working-copy: " +
		"Command failed (exit 1): jj metaedit: the git lock " +
		"/legion/workspaces/acme/widgets/legion-209/.git/index.lock\x00 was last written 31m ago"
	stored := strings.ReplaceAll(raw, "\x00", "\uFFFD")
	d := supervise.Delivery{
		ID: "delivery-1", Task: "implement the plan", Phase: phase.Implementing, Generation: 4, QueuedAt: at(1),
		SendFailures: 1, LastSendFailureAt: at(5), LastSendError: raw,
	}

	if err := store.PutDelivery(ctx, c.Token, d); err != nil {
		t.Fatalf("put the delivery whose send error holds a NUL: %v", err)
	}
	got := onlyClaim(t, store).Pending
	if got == nil || got.LastSendError != stored {
		t.Fatalf("delivery read back = %+v, want the send error whole with U+FFFD in place of the NUL", got)
	}
}

// A task is queued uninterrupted, and a death marks it interrupted — and, when its pod ran out of
// memory, marks that too — by writing the same delivery again: both marks only ever reach the
// store through the upsert's update. They have to survive that write, or a restart re-sends the
// task without the sentence saying its turn was interrupted and without the note saying the pod
// ran out of memory.
func TestARePutDeliveryCarriesItsInterruption(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := migratedStore(t)
	c := tmuxClaim("legion-LEGION-209-implementer")
	if err := store.PutClaim(ctx, c); err != nil {
		t.Fatalf("put claim: %v", err)
	}
	queued := supervise.Delivery{ID: "delivery-1", Task: "implement the plan", Phase: phase.Implementing, Generation: 4, QueuedAt: at(1)}
	if err := store.PutDelivery(ctx, c.Token, queued); err != nil {
		t.Fatalf("queue the delivery: %v", err)
	}
	interrupted := queued
	interrupted.ID, interrupted.Interrupted, interrupted.OutOfMemory = "delivery-2", true, true
	if err := store.PutDelivery(ctx, c.Token, interrupted); err != nil {
		t.Fatalf("mark it interrupted: %v", err)
	}

	if got := onlyClaim(t, store).Pending; got == nil || got.ID != "delivery-2" || !got.Interrupted || !got.OutOfMemory {
		t.Fatalf("delivery read back = %+v, want the re-put delivery, interrupted and out of memory", got)
	}
}

// The fact that a reviewer's task ends with the daemon's two sentences of its review round
// (supervise.Delivery.RoundNamed) reaches the store with the delivery and comes back from it, in
// both directions, through a re-put that replaces the delivery: a restarted daemon sends what it
// reads back, and a fact lost on the way would send a task of this release with the daemon's
// sentences naming nothing after its own, or keep one the release before queued from them. A
// delivery the release before wrote, which names no such column, reads back without the fact.
func TestADeliveryCarriesTheFactThatItNamesItsReviewRound(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := migratedStore(t)
	c := tmuxClaim("legion-LEGION-212-reviewer")
	if err := store.PutClaim(ctx, c); err != nil {
		t.Fatalf("put claim: %v", err)
	}
	named := supervise.Delivery{ID: "outbox:7", Task: "Continue. Phase: reviewing. Last reviewed commit: none. Review entry: 1.", Phase: phase.Reviewing, QueuedAt: at(1), RoundNamed: true}
	for _, put := range []supervise.Delivery{named, {ID: "outbox:8", Task: "Continue. Phase: reviewing.", Phase: phase.Reviewing, QueuedAt: at(2)}, named} {
		if err := store.PutDelivery(ctx, c.Token, put); err != nil {
			t.Fatalf("put the delivery %s: %v", put.ID, err)
		}
		if got := onlyClaim(t, store).Pending; got == nil || got.ID != put.ID || got.RoundNamed != put.RoundNamed {
			t.Fatalf("the delivery read back = %+v, want %s with the fact %v", got, put.ID, put.RoundNamed)
		}
	}
	// The release before writes a delivery naming no round_named, so a delivery it writes on a
	// claim that holds none takes the column's default.
	if _, err := store.pool.Exec(ctx, `delete from pending_task_deliveries where claim_token = $1`, string(c.Token)); err != nil {
		t.Fatalf("retire the delivery: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `insert into pending_task_deliveries (claim_token, delivery_id, task, phase, generation, queued_at)
		values ($1, 'outbox:9', 'Continue. Phase: reviewing.', 'reviewing', 1, now())`, string(c.Token)); err != nil {
		t.Fatalf("write the delivery as the release before writes it: %v", err)
	}
	if got := onlyClaim(t, store).Pending; got == nil || got.ID != "outbox:9" || got.RoundNamed {
		t.Fatalf("the delivery read back = %+v, want outbox:9 with no fact: the release before named no round", got)
	}
}

// The phase a task was queued for is what says the task is still the work to do, and it has to
// survive the path that rewrites the delivery: a prompt refused or unanswered is re-put under a
// new id, and a restart then reads the delivery back from this table. A phase lost on either side
// of that round trip brings back the drop this column exists to make: the daemon would send a
// finished phase's task to a worker that had moved on, silently, after every restart.
func TestARePutDeliveryKeepsThePhaseItWasQueuedFor(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := migratedStore(t)
	c := tmuxClaim("legion-LEGION-211-implementer")
	queued := supervise.Delivery{ID: "outbox:42", Task: "Continue. Phase: implementing.", Phase: phase.Implementing, QueuedAt: at(1)}
	if err := store.PutClaim(ctx, c); err != nil {
		t.Fatalf("put claim: %v", err)
	}
	if err := store.PutDelivery(ctx, c.Token, queued); err != nil {
		t.Fatalf("put delivery: %v", err)
	}

	// What promptFailed does: the same task under a new id, unconfirmed.
	rotated := queued
	rotated.ID = "S4YXQ2F7TDNX"
	if err := store.PutDelivery(ctx, c.Token, rotated); err != nil {
		t.Fatalf("re-put the delivery under its new id: %v", err)
	}

	read := onlyClaim(t, store)
	if read.Pending == nil {
		t.Fatal("the restarted daemon read no pending delivery")
	}
	if read.Pending.ID != rotated.ID || read.Pending.Phase != phase.Implementing {
		t.Fatalf("delivery read back = %+v, want %s queued for %s", read.Pending, rotated.ID, phase.Implementing)
	}
}

// The ladder a provider error puts a task on outlives the daemon that armed it: the count says
// which step the next send waits, and the sentence is what that send puts in front of the task
// (dispatch://INFRA-956 R2). The claim's own run of such turns is beside them, because that is
// what the controller's notice is counted from, and neither a restart nor a re-put of the
// delivery under a new id may lose either.
func TestAProviderErrorsLadderAndItsRunRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := migratedStore(t)
	c := tmuxClaim("legion-LEGION-212-implementer")
	c.ProviderErrorRun = 4
	if err := store.PutClaim(ctx, c); err != nil {
		t.Fatalf("put claim: %v", err)
	}
	note := "Your previous turn on this task stopped because of a provider error (HTTP 529)."
	queued := supervise.Delivery{ID: "delivery-1", Task: "implement the plan", Phase: phase.Implementing, QueuedAt: at(1),
		ProviderErrorRetries: 3, ProviderErrorNote: note}
	if err := store.PutDelivery(ctx, c.Token, queued); err != nil {
		t.Fatalf("put delivery: %v", err)
	}

	read := onlyClaim(t, store)
	if read.ProviderErrorRun != 4 {
		t.Errorf("the claim's run of provider errors read back as %d, want 4", read.ProviderErrorRun)
	}
	if p := read.Pending; p == nil || p.ProviderErrorRetries != 3 || p.ProviderErrorNote != note {
		t.Fatalf("delivery read back = %+v, want three sends counted and the sentence %q", p, note)
	}

	// A task no provider error has taken back carries neither.
	fresh := supervise.Delivery{ID: "delivery-2", Task: "test the change", Phase: phase.Testing, QueuedAt: at(2)}
	if err := store.PutDelivery(ctx, c.Token, fresh); err != nil {
		t.Fatalf("put the newer delivery: %v", err)
	}
	if p := onlyClaim(t, store).Pending; p == nil || p.ProviderErrorRetries != 0 || p.ProviderErrorNote != "" {
		t.Fatalf("delivery read back = %+v, want a task no provider error has taken back", p)
	}
}

func TestAClaimWithNothingOptionalRoundTripsAsNothing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := migratedStore(t)
	want := supervise.Claim{
		Token:   "legion-LEGION-210-architect",
		Project: "legion",
		Tree:    "LEGION-210",
		Issue:   "LEGION-210",
		Role:    claim.RoleArchitect,
		State:   supervise.StateQueued,
	}

	if err := store.PutClaim(ctx, want); err != nil {
		t.Fatalf("put claim: %v", err)
	}

	sameClaim(t, onlyClaim(t, store), want)
}

func TestPutClaimOverwritesTheClaimItNames(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := migratedStore(t)
	first := tmuxClaim("legion-LEGION-209-tester")
	if err := store.PutClaim(ctx, first); err != nil {
		t.Fatalf("put claim: %v", err)
	}

	second := first
	second.Generation = 5
	second.State = supervise.StateSuspended
	second.Locator = nil
	second.Budgets = supervise.Budgets{}
	second.UncertainStreak = 0
	if err := store.PutClaim(ctx, second); err != nil {
		t.Fatalf("put claim again: %v", err)
	}

	sameClaim(t, onlyClaim(t, store), second)
}

// A claim's plugin contract and the daemon contract its last stale relaunch was made for are
// their own columns, round-tripping independently of the locator and budgets LEGION-164 adds no
// relation to.
func TestAClaimsPluginContractRoundTrips(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := migratedStore(t)
	want := tmuxClaim("legion-LEGION-209-pluginholder")
	want.PluginContract = 23
	want.PluginRelaunchFor = 24
	if err := store.PutClaim(ctx, want); err != nil {
		t.Fatalf("put claim: %v", err)
	}
	sameClaim(t, onlyClaim(t, store), want)

	want.PluginContract = 24
	want.PluginRelaunchFor = 0
	if err := store.PutClaim(ctx, want); err != nil {
		t.Fatalf("put claim again: %v", err)
	}
	sameClaim(t, onlyClaim(t, store), want)
}

// The mark of a launch that had Oh My Pi resume the claim's session by its id itself
// (AgentResumeGeneration) is a column of its own: a daemon restarted between that launch and its
// agent's registration reads the mark back, and with it whether the launch's end is charged and
// whether the claim's next try forgets the session (dispatch://INFRA-4108 R3). The claim marked
// is one launched at generation 7 that records its session and no file; once its agent registered
// with the file, the mark is 0, no such launch outstanding, and reads back as 0.
func TestAClaimsAgentResumeGenerationRoundTrips(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := migratedStore(t)
	want := tmuxClaim("legion-LEGION-209-implementer")
	want.Generation, want.SessionFile, want.AgentResumeGeneration = 7, "", 7
	if err := store.PutClaim(ctx, want); err != nil {
		t.Fatalf("put claim: %v", err)
	}
	sameClaim(t, onlyClaim(t, store), want)

	want.SessionFile, want.AgentResumeGeneration = "/state/sessions/implementer.jsonl", 0
	if err := store.PutClaim(ctx, want); err != nil {
		t.Fatalf("put claim again: %v", err)
	}
	sameClaim(t, onlyClaim(t, store), want)
}

// The turn a claim last started and the one it last ended are nullable columns of their own: a
// claim that has taken no turn reads back with neither time rather than with a zero one, and a
// claim that has worked reads both back to the microsecond, which is what a restarted daemon
// hands every reader of the claim.
func TestAClaimsTurnTimesRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := migratedStore(t)
	c := tmuxClaim("legion-LEGION-209-implementer")
	if err := store.PutClaim(ctx, c); err != nil {
		t.Fatalf("put claim: %v", err)
	}
	if got := onlyClaim(t, store); !got.LastTurnStart.IsZero() || !got.LastTurnEnd.IsZero() {
		t.Fatalf("a claim that has taken no turn read back %v and %v, want no turn times", got.LastTurnStart, got.LastTurnEnd)
	}

	c.LastTurnStart, c.LastTurnEnd = at(3), at(9)
	if err := store.PutClaim(ctx, c); err != nil {
		t.Fatalf("put the worked claim: %v", err)
	}
	got := onlyClaim(t, store)
	if !got.LastTurnStart.Equal(at(3)) || !got.LastTurnEnd.Equal(at(9)) {
		t.Fatalf("turn times read back = %v and %v, want %v and %v", got.LastTurnStart, got.LastTurnEnd, at(3), at(9))
	}
}

// A claim's two causes and the time the daemon recorded each are columns of their own, so a
// restarted daemon reads each cause back beside its time, to the microsecond
// (#5349). A claim that holds no cause reads back with no time; a cause recorded
// again with the same text reads back with its new time; and a cause the daemon dropped, as it
// drops a start failure at its agent's ready, reads back with no time. So does a time the row
// holds beside an empty cause, which a daemon of the release before the columns leaves when it
// drops the start failure and not the column it never names.
func TestClaimCauseTimesSurviveAReload(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := migratedStore(t)
	c := tmuxClaim("legion-LEGION-209-implementer")
	put := func(what string) supervise.Claim {
		t.Helper()
		if err := store.PutClaim(ctx, c); err != nil {
			t.Fatalf("put the claim %s: %v", what, err)
		}
		return onlyClaim(t, store)
	}
	want := func(got supervise.Claim, what string, death, startFailure time.Time) {
		t.Helper()
		if got.LastDeathCause != c.LastDeathCause || !got.LastDeathAt.Equal(death) {
			t.Errorf("the claim %s read back the death %q at %v, want %q at %v", what, got.LastDeathCause, got.LastDeathAt, c.LastDeathCause, death)
		}
		if got.LastStartFailure != c.LastStartFailure || !got.LastStartFailureAt.Equal(startFailure) {
			t.Errorf("the claim %s read back the start failure %q at %v, want %q at %v",
				what, got.LastStartFailure, got.LastStartFailureAt, c.LastStartFailure, startFailure)
		}
	}

	want(put("that holds no cause"), "that holds no cause", time.Time{}, time.Time{})

	c.LastDeathCause, c.LastDeathAt = "gone: pod legion-legion-209 (uid 8fdd52cc) ended: OOMKilled", at(3)
	c.LastStartFailure, c.LastStartFailureAt = "init container workspace-init failed: exit 1", at(9)
	want(put("with both causes"), "with both causes", at(3), at(9))

	c.LastDeathAt = at(21)
	want(put("whose death was recorded again with the same text"), "whose death was recorded again with the same text", at(21), at(9))

	c.LastStartFailure, c.LastStartFailureAt = "", time.Time{}
	want(put("whose start failure was dropped"), "whose start failure was dropped", at(21), time.Time{})

	if _, err := store.Pool().Exec(ctx, `update claims set last_start_failure_at = $1 where token = $2`, at(30), string(c.Token)); err != nil {
		t.Fatalf("leave a start failure's time beside no start failure: %v", err)
	}
	want(onlyClaim(t, store), "holding a time beside no start failure", at(21), time.Time{})
}

// After a rollback and a roll-forward, a row can hold a start failure beside an older failure's
// time: the release before 20261124073512 names neither last_start_failure_at nor its digest, so a
// start failure it drops at its agent's ready leaves that failure's time and digest standing, and
// a failure of the same text it records later is written beside them, where the digest matches.
// This release drops the start failure, with its time, at every ready (supervise's ready), so a
// start failure's time before last_ready is never the time of the failure the row holds: the claim
// reads that start failure back with no time, and its death's time, which no ready clears, as it
// stands (#5349).
func TestAStartFailureTimeBeforeTheReadyIsReadAsNone(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := migratedStore(t)
	c := tmuxClaim("legion-LEGION-209-implementer")
	c.State = supervise.StateLaunching
	c.LastDeathCause, c.LastDeathAt = "gone: pod legion-legion-209 ended: OOMKilled", at(2)
	c.LastStartFailure, c.LastStartFailureAt = "the launcher refused the start: the pod was not ready", at(5)
	c.LastReady = at(10)
	if err := store.PutClaim(ctx, c); err != nil {
		t.Fatalf("put the rolled-back claim: %v", err)
	}

	got := onlyClaim(t, store)
	if got.LastStartFailure != c.LastStartFailure || !got.LastStartFailureAt.IsZero() {
		t.Errorf("the rolled-back claim read back its start failure %q at %v, want %q with no time: a time before its ready %v dates an older failure",
			got.LastStartFailure, got.LastStartFailureAt, c.LastStartFailure, c.LastReady)
	}
	if !got.LastDeathAt.Equal(at(2)) {
		t.Errorf("the rolled-back claim read back its death at %v, want %v: a death before the ready is one the agent survived", got.LastDeathAt, at(2))
	}

	c.LastStartFailureAt = at(12)
	if got := putAndRead(t, store, c); !got.LastStartFailureAt.Equal(at(12)) {
		t.Errorf("a start failure recorded after the ready read back at %v, want %v", got.LastStartFailureAt, at(12))
	}
}

// A daemon of the release before 20261124073512 rewrites a claim's two causes and names none of
// the columns that date them (that release's upsertClaim), so after a rollback and a roll-forward
// a row can hold a cause beside the time this release gave the cause before it. A cause that
// release wrote with text other than the cause the time dates reads back with no time. One it
// wrote with the same text, a death or a start failure with no ready between, reads back with the
// older time, which no read can tell from its own (#5349).
func TestACauseAnEarlierReleaseWroteIsReadWithNoTime(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := migratedStore(t)
	c := tmuxClaim("legion-LEGION-209-implementer")
	c.State = supervise.StateLaunching
	c.LastDeathCause, c.LastDeathAt = "gone: pod legion-legion-209 (uid 8fdd52cc) ended: OOMKilled", at(3)
	c.LastStartFailure, c.LastStartFailureAt = "the launcher refused the start: the pod was not ready", at(5)
	if got := putAndRead(t, store, c); !got.LastDeathAt.Equal(at(3)) || !got.LastStartFailureAt.Equal(at(5)) {
		t.Fatalf("the claim read back its death at %v and its start failure at %v, want %v and %v",
			got.LastDeathAt, got.LastStartFailureAt, at(3), at(5))
	}
	// earlierWrite is that release's write of the two causes: their text, and neither column
	// this release dates them with.
	earlierWrite := func(death, startFailure string) supervise.Claim {
		t.Helper()
		if _, err := store.Pool().Exec(ctx, `update claims set last_death_cause = $1, last_start_failure = $2 where token = $3`,
			death, startFailure, string(c.Token)); err != nil {
			t.Fatalf("write the two causes as the earlier release does: %v", err)
		}
		return onlyClaim(t, store)
	}

	got := earlierWrite(c.LastDeathCause, c.LastStartFailure)
	if got.LastDeathCause != c.LastDeathCause || !got.LastDeathAt.Equal(at(3)) {
		t.Errorf("the death the earlier release wrote again with the same text read back as %q at %v, want %q at the older time %v",
			got.LastDeathCause, got.LastDeathAt, c.LastDeathCause, at(3))
	}
	if got.LastStartFailure != c.LastStartFailure || !got.LastStartFailureAt.Equal(at(5)) {
		t.Errorf("the start failure the earlier release wrote again with the same text read back as %q at %v, want %q at the older time %v",
			got.LastStartFailure, got.LastStartFailureAt, c.LastStartFailure, at(5))
	}

	const death, startFailure = "gone: pod legion-legion-209 (uid 0b1e44a7) ended: Evicted", "init container workspace-init failed: exit 1"
	got = earlierWrite(death, startFailure)
	if got.LastDeathCause != death || !got.LastDeathAt.IsZero() {
		t.Errorf("the death the earlier release wrote with other text read back as %q at %v, want %q with no time: %v dates the death before it",
			got.LastDeathCause, got.LastDeathAt, death, at(3))
	}
	if got.LastStartFailure != startFailure || !got.LastStartFailureAt.IsZero() {
		t.Errorf("the start failure the earlier release wrote with other text read back as %q at %v, want %q with no time: %v dates the failure before it",
			got.LastStartFailure, got.LastStartFailureAt, startFailure, at(5))
	}
}

// putAndRead writes c and reads it back as the only claim.
func putAndRead(t *testing.T, store *Store, c supervise.Claim) supervise.Claim {
	t.Helper()
	if err := store.PutClaim(context.Background(), c); err != nil {
		t.Fatalf("put claim %s: %v", c.Token, err)
	}
	return onlyClaim(t, store)
}

// The digest the store writes beside a cause's time is the 64-bit FNV-1a of the cause's UTF-8
// bytes, in 8 big-endian bytes, and none where there is no time (causeDigest). The read serves a
// time only while those 8 bytes match the cause the row holds, so a digest that changed between
// releases would drop every stored time at the next read. The answers are written out, not
// computed by causeDigest: the first three are the FNV reference's published 64-bit FNV-1a
// values, and the last, a cause of two lines with text outside ASCII, was computed by a separate
// implementation of the algorithm.
func TestTheCauseDigestIsTheFNV1a64OfTheCausesBytes(t *testing.T) {
	t.Parallel()
	const twoLines = "gone: pod legion-legion-209 (uid 8fdd52cc) ended: OOMKilled\n" +
		"init container workspace-init: «HEAD.lock» — prise, réessai ✓\n"
	if len(twoLines) != 129 {
		t.Fatalf("the two-line cause is %d bytes, want 129: the text the answer below was computed for changed", len(twoLines))
	}
	for _, tc := range []struct {
		cause string
		want  []byte
	}{
		{"", []byte{0xcb, 0xf2, 0x9c, 0xe4, 0x84, 0x22, 0x23, 0x25}},
		{"a", []byte{0xaf, 0x63, 0xdc, 0x4c, 0x86, 0x01, 0xec, 0x8c}},
		{"foobar", []byte{0x85, 0x94, 0x41, 0x71, 0xf7, 0x39, 0x67, 0xe8}},
		{twoLines, []byte{0x43, 0x5e, 0x4c, 0xc1, 0xfb, 0x38, 0x8a, 0x5d}},
	} {
		if got := causeDigest(tc.cause, at(1)); !bytes.Equal(got, tc.want) {
			t.Errorf("causeDigest(%q) = %x, want %x, the 64-bit FNV-1a of its bytes", tc.cause, got, tc.want)
		}
	}
	if got := causeDigest(twoLines, time.Time{}); got != nil {
		t.Errorf("causeDigest of a cause with no time = %x, want none: a row with no time holds no digest", got)
	}
}

// What the daemon charged for the death of the process a claim records round-trips: a restart
// builds every claim from this table and has charged nothing of its own, so a claim restored
// without this is charged a second death for the one dead process it stands over, which costs
// that claim what supervise.Claim.DeathCharged states (INFRA-956). It round-trips with the
// deaths it charged, which the same write carries.
func TestWhatAClaimsDeathWasChargedRoundTrips(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := migratedStore(t)
	want := tmuxClaim("legion-LEGION-209-deathcharged")
	want.State = supervise.StateWorking
	want.DeathCharged = true
	if err := store.PutClaim(ctx, want); err != nil {
		t.Fatalf("put claim: %v", err)
	}
	sameClaim(t, onlyClaim(t, store), want)

	// The launch that replaces that process clears it with the process it lets go (supervise's
	// letGo), and the claim it writes records a process charged nothing.
	want.DeathCharged = false
	if err := store.PutClaim(ctx, want); err != nil {
		t.Fatalf("put claim again: %v", err)
	}
	sameClaim(t, onlyClaim(t, store), want)
}

// The round a claim left is a column of its own, and it is read back exactly as it was written:
// the daemon decides from it what the pod of a round took with it when it went, and a restart
// has nothing else to read that from (INFRA-484). It round-trips independently of the round the
// claim records, which a claim that leaves keeps.
func TestTheRoundAClaimLeftRoundTrips(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := migratedStore(t)
	want := tmuxClaim("legion-LEGION-209-leftaround")
	want.UpdateRound = supervise.PodRound("9d3f0c7a-2b41-4a39-9f6b-6c1d0a7e5b22")
	want.UpdateLeftRound = want.UpdateRound
	if err := store.PutClaim(ctx, want); err != nil {
		t.Fatalf("put claim: %v", err)
	}
	sameClaim(t, onlyClaim(t, store), want)

	// The round ends, which is the one thing that clears it.
	want.UpdateRound = ""
	want.UpdateLeftRound = ""
	if err := store.PutClaim(ctx, want); err != nil {
		t.Fatalf("put claim again: %v", err)
	}
	sameClaim(t, onlyClaim(t, store), want)
}

// A locator is checked where it is read back: a row nothing could act on is refused by name rather
// than loaded as a process the daemon believes in.
func TestClaimsRefusesAStoredLocatorThatDoesNotValidate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := migratedStore(t)
	stored := tmuxClaim("legion-LEGION-209-reviewer")
	if err := store.PutClaim(ctx, stored); err != nil {
		t.Fatalf("put claim: %v", err)
	}
	if _, err := store.pool.Exec(ctx,
		`update claims set locator = '{"runtime":"tmux","claim":"legion-LEGION-209-reviewer","incarnation":"1:2"}' where token = $1`,
		string(stored.Token),
	); err != nil {
		t.Fatalf("write a locator with no tmux member: %v", err)
	}

	_, err := store.Claims(ctx)
	if err == nil {
		t.Fatal("claims loaded a locator with no tmux member")
	}
	for _, want := range []string{"legion-LEGION-209-reviewer", "no tmux member"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not name %q: %v", want, err)
		}
	}
}

func TestClaimByBootTokenHashFindsTheClaimCarryingIt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := migratedStore(t)
	implementer := tmuxClaim("legion-LEGION-209-implementer")
	tester := tmuxClaim("legion-LEGION-209-tester")
	for _, c := range []supervise.Claim{implementer, tester} {
		if err := store.PutClaim(ctx, c); err != nil {
			t.Fatalf("put claim %s: %v", c.Token, err)
		}
	}

	got, ok, err := store.ClaimByBootTokenHash(ctx, supervise.HashBootToken("boot-legion-LEGION-209-tester"))
	if err != nil {
		t.Fatalf("claim by boot token hash: %v", err)
	}
	if !ok {
		t.Fatal("no claim found for the tester's boot token hash")
	}
	sameClaim(t, got, tester)

	_, ok, err = store.ClaimByBootTokenHash(ctx, supervise.HashBootToken("a token no launch minted"))
	if err != nil {
		t.Fatalf("claim by an unknown boot token hash: %v", err)
	}
	if ok {
		t.Error("an unknown boot token hash found a claim")
	}
}

func TestADeliveryIsPutConfirmedAndRetired(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := migratedStore(t)
	c := tmuxClaim("legion-LEGION-209-merger")
	if err := store.PutClaim(ctx, c); err != nil {
		t.Fatalf("put claim: %v", err)
	}
	delivery := supervise.Delivery{ID: "delivery-7", Task: "merge-ready check", QueuedAt: at(3)}
	if err := store.PutDelivery(ctx, c.Token, delivery); err != nil {
		t.Fatalf("put delivery: %v", err)
	}
	c.Pending = &delivery
	sameClaim(t, onlyClaim(t, store), c)

	delivery.DeliveredAt = at(4)
	delivery.ConfirmedAt = at(5)
	if err := store.PutDelivery(ctx, c.Token, delivery); err != nil {
		t.Fatalf("put the confirmed delivery: %v", err)
	}
	c.Pending = &delivery
	sameClaim(t, onlyClaim(t, store), c)

	c.Pending = nil
	if err := store.RetireDelivery(ctx, c, delivery.ID); err != nil {
		t.Fatalf("retire delivery: %v", err)
	}
	sameClaim(t, onlyClaim(t, store), c)
}

// Retiring is fenced on the delivery id: a caller retiring a delivery the claim no longer holds
// is told so, and the delivery the claim does hold stays.
func TestRetireDeliveryRefusesADeliveryTheClaimDoesNotHold(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := migratedStore(t)
	c := tmuxClaim("legion-LEGION-209-planner")
	if err := store.PutClaim(ctx, c); err != nil {
		t.Fatalf("put claim: %v", err)
	}
	delivery := supervise.Delivery{ID: "delivery-new", Task: "plan it", QueuedAt: at(6)}
	if err := store.PutDelivery(ctx, c.Token, delivery); err != nil {
		t.Fatalf("put delivery: %v", err)
	}

	// The claim the refused retire carries is rolled back with it: the delivery and the run the
	// claim serves move together or not at all.
	stale := c
	stale.ServingGeneration = 99
	err := store.RetireDelivery(ctx, stale, "delivery-old")
	if err == nil {
		t.Fatal("retired a delivery the claim does not hold")
	}
	if !strings.Contains(err.Error(), "delivery-old") {
		t.Errorf("refusal does not name the delivery it refused: %v", err)
	}
	c.Pending = &delivery
	sameClaim(t, onlyClaim(t, store), c)
}

// A delete that removes no row says one of two things, because pending_task_deliveries is keyed
// on the claim rather than on the delivery, and the daemon answers them in opposite ways. The
// row holds no delivery at all: the retirement this caller already committed, whose answer a
// broken connection lost, which supervise reads as its own write made good. Or the row holds a
// delivery under another id: a write that rotated the id committed after its answer was cut
// off, which is no retirement of anything and fails the decision. Only the row can tell them
// apart, so the store does, in the transaction the delete found nothing in, and the second
// refusal names both ids — the one asked for and the one the row holds — since an operator
// reading it has no other way to learn which delivery is stranded.
func TestRetireDeliveryTellsARetirementAlreadyMadeFromARetirementUnderAnotherId(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := migratedStore(t)
	c := tmuxClaim("legion-LEGION-209-reviewer")
	if err := store.PutClaim(ctx, c); err != nil {
		t.Fatalf("put claim: %v", err)
	}

	err := store.RetireDelivery(ctx, c, "delivery-retired")
	if !errors.Is(err, supervise.ErrNoSuchDelivery) {
		t.Fatalf("retiring against a row with no delivery = %v, want supervise.ErrNoSuchDelivery", err)
	}
	if errors.Is(err, supervise.ErrOtherDelivery) {
		t.Fatalf("the refusal of a row holding nothing also reads as one holding another delivery: %v", err)
	}

	// The rotation committed after its answer was cut off: the row holds the new delivery while
	// the caller is retiring the old one.
	rotated := supervise.Delivery{ID: "delivery-rotated", Task: "review it again", QueuedAt: at(8)}
	if err := store.PutDelivery(ctx, c.Token, rotated); err != nil {
		t.Fatalf("put the rotated delivery: %v", err)
	}

	err = store.RetireDelivery(ctx, c, "delivery-old")
	if !errors.Is(err, supervise.ErrOtherDelivery) {
		t.Fatalf("retiring against a row holding another delivery = %v, want supervise.ErrOtherDelivery", err)
	}
	if errors.Is(err, supervise.ErrNoSuchDelivery) {
		t.Fatalf("the refusal reads as a retirement already made, which would drop the delivery its caller holds: %v", err)
	}
	for _, want := range []string{"delivery-old", rotated.ID} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q: %v", want, err)
		}
	}

	// Neither refusal wrote anything: the claim it carried went back with the delete, and the
	// delivery the row holds is where it was.
	c.Pending = &rotated
	sameClaim(t, onlyClaim(t, store), c)
}

func TestPutDeliveryRefusesAClaimTheStoreDoesNotHold(t *testing.T) {
	t.Parallel()
	store := migratedStore(t)
	err := store.PutDelivery(context.Background(), "legion-LEGION-404-tester",
		supervise.Delivery{ID: "delivery-1", Task: "t", QueuedAt: at(7)})
	if err == nil {
		t.Fatal("put a delivery for a claim the store does not hold")
	}
}

// A confirmation writes the claim and its delivery together. The delivery write failing must take
// the claim's with it: a stored claim recorded as working whose delivery never landed is a task
// the shim has already answered and nothing sends again.
func TestPutClaimAndDeliveryWritesBothOrNeither(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := migratedStore(t)
	c := tmuxClaim("legion-LEGION-212-tester")
	c.State = supervise.StateIdle
	if err := store.PutClaim(ctx, c); err != nil {
		t.Fatalf("put claim: %v", err)
	}

	working := c
	working.State = supervise.StateWorking
	// A generation no bigint holds: the delivery's write is refused, inside the transaction that
	// carries the claim's.
	confirmed := supervise.Delivery{ID: "delivery-1", Task: "test it", QueuedAt: at(1),
		DeliveredAt: at(2), ConfirmedAt: at(3), Generation: math.MaxUint64}
	if err := store.PutClaimAndDelivery(ctx, working, confirmed); err == nil {
		t.Fatal("wrote a delivery whose generation no bigint holds")
	}

	stored := onlyClaim(t, store)
	if stored.State != supervise.StateIdle || stored.Pending != nil {
		t.Fatalf("claim read back = %s with pending %+v, want the idle row it was, unchanged",
			stored.State, stored.Pending)
	}
}

func TestAClaimsEnrollmentRoundTrips(t *testing.T) {
	t.Parallel()
	s := migratedStore(t)
	ctx := context.Background()
	c := supervise.Claim{
		Token: "legion-LEGION-209-implementer", Project: "legion", Tree: "LEGION-208", Issue: "LEGION-209",
		Role: claim.RoleImplementer, Generation: 3, State: supervise.StateRegistered,
		Locator: &runtime.Locator{Runtime: runtime.RuntimeSandbox, Claim: "legion-LEGION-209-implementer", Incarnation: "pod-uid-3/3",
			Sandbox: &runtime.SandboxLocator{Namespace: "legion", Name: "legion-legion-209", PodUID: "pod-uid-3", Container: "roles", Generation: 3}},
		Enrollment: &supervise.Enrollment{ID: "enr-1", Incarnation: "pod-uid-3/3"},
	}
	if err := s.PutClaim(ctx, c); err != nil {
		t.Fatal(err)
	}
	claims, err := s.Claims(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 1 || claims[0].Enrollment == nil || *claims[0].Enrollment != *c.Enrollment {
		t.Fatalf("read back %+v", claims)
	}
	c.Enrollment = nil
	if err := s.PutClaim(ctx, c); err != nil {
		t.Fatal(err)
	}
	claims, _ = s.Claims(ctx)
	if claims[0].Enrollment != nil {
		t.Fatalf("a cleared enrollment read back as %+v", claims[0].Enrollment)
	}
}

func TestAnEnrollmentWithoutAnIncarnationIsRefusedByTheSchema(t *testing.T) {
	t.Parallel()
	s := migratedStore(t)
	c := supervise.Claim{Token: "legion-LEGION-209-tester", Project: "legion", Tree: "LEGION-208", Issue: "LEGION-209",
		Role: claim.RoleTester, State: supervise.StateQueued, Enrollment: &supervise.Enrollment{ID: "enr-1"}}
	if err := s.PutClaim(context.Background(), c); err == nil {
		t.Fatal("an enrollment naming no incarnation was stored")
	}
}

// Who suspended a claim round-trips with it, and the schema refuses a source it does not know:
// the daemon's resume by rule reads the column after a restart, and a value it cannot read would
// be a claim nobody can say whether to resume.
func TestASuspendedClaimsSourceRoundTripsAndTheSchemaRefusesAnUnknownOne(t *testing.T) {
	t.Parallel()
	s := migratedStore(t)
	ctx := context.Background()
	for _, by := range []supervise.SuspendSource{supervise.SuspendedByOperator, supervise.SuspendedByDaemon, supervise.SuspendedByExit,
		supervise.SuspendedByReleased, ""} {
		c := supervise.Claim{Token: "legion-LEGION-209-tester", Project: "legion", Tree: "LEGION-208", Issue: "LEGION-209",
			Role: claim.RoleTester, Generation: 2, State: supervise.StateSuspended, Session: "ses_tester", SuspendedBy: by}
		if err := s.PutClaim(ctx, c); err != nil {
			t.Fatalf("put a claim suspended by %q: %v", by, err)
		}
		claims, err := s.Claims(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(claims) != 1 || claims[0].SuspendedBy != by {
			t.Fatalf("a claim suspended by %q read back as %+v", by, claims)
		}
	}
	c := supervise.Claim{Token: "legion-LEGION-209-tester", Project: "legion", Tree: "LEGION-208", Issue: "LEGION-209",
		Role: claim.RoleTester, State: supervise.StateSuspended, SuspendedBy: "someone"}
	if err := s.PutClaim(ctx, c); err == nil {
		t.Fatal("a claim suspended by an unknown source was stored")
	}
}

// The project controller's claim, which the daemon launches under `controller: daemon`, is a row
// like any claim's: on the role `controller`, with no issue and no tree, so its boot token resolves
// a shim's hello and a registration as any launch's does.
func TestTheControllersClaimRoundTripsWithNoIssueAndNoTree(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := migratedStore(t)
	want := supervise.Claim{
		Token:         claim.ControllerToken("legion"),
		Project:       "legion",
		Role:          claim.RoleController,
		Generation:    2,
		State:         supervise.StateLaunching,
		BootTokenHash: []byte("boot-token-hash"),
	}
	if err := store.PutClaim(ctx, want); err != nil {
		t.Fatalf("put the controller's claim: %v", err)
	}
	sameClaim(t, onlyClaim(t, store), want)
	found, ok, err := store.ClaimByBootTokenHash(ctx, []byte("boot-token-hash"))
	if err != nil || !ok || found.Token != want.Token {
		t.Fatalf("ClaimByBootTokenHash = %+v, %t, %v; want the controller's claim", found, ok, err)
	}
}

// The schema holds the controller's shape both ways: the controller role on no issue and no tree,
// and no other role there, so neither a controller claim on an issue nor a worker claim with no
// issue is ever stored.
func TestTheSchemaRefusesAControllerClaimOnAnIssueAndAWorkflowClaimOnNone(t *testing.T) {
	t.Parallel()
	s := migratedStore(t)
	for _, c := range []supervise.Claim{
		{Token: "legion-legion-controller", Project: "legion", Tree: "LEGION-208", Issue: "LEGION-208", Role: claim.RoleController, State: supervise.StateQueued},
		{Token: "legion-legion-architect", Project: "legion", Role: claim.RoleArchitect, State: supervise.StateQueued},
	} {
		if err := s.PutClaim(context.Background(), c); err == nil {
			t.Errorf("stored %s on role %s with issue %q and tree %q", c.Token, c.Role, c.Issue, c.Tree)
		}
	}
}

// IssueRunsElsewhere names a claim of the issue in another tree that may hold a process: one in a
// state supervise's RunsNothing does not hold, or one that still records a locator. Every claim
// state is walked against RunsNothing, so the query and the machine read one set; a launch whose
// outcome is uncertain and a suspended claim that still records a locator are named. A claim of the
// asking tree or of another issue is not.
func TestIssueRunsElsewhereNamesOnlyAClaimOfAnotherTreeThatHoldsAProcess(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := migratedStore(t)
	elsewhere := func(tree string) string {
		t.Helper()
		got, err := store.IssueRunsElsewhere(ctx, "legion", "LEGION-209", tree)
		if err != nil {
			t.Fatalf("IssueRunsElsewhere(LEGION-209, %s): %v", tree, err)
		}
		return got
	}
	implementer := tmuxClaim("legion-LEGION-209-implementer")
	locator := implementer.Locator
	put := func(state supervise.ClaimState, withLocator bool) {
		t.Helper()
		implementer.State, implementer.Locator = state, nil
		if withLocator {
			implementer.Locator = locator
		}
		if err := store.PutClaim(ctx, implementer); err != nil {
			t.Fatalf("put %s %s: %v", implementer.Token, state, err)
		}
	}
	for _, state := range supervise.States() {
		put(state, false)
		named := elsewhere("LEGION-209") == "legion-LEGION-209-implementer:"+string(state)
		if runs := !(supervise.Claim{State: state}).RunsNothing(); named != runs {
			t.Errorf("a %s claim of tree LEGION-208 without a locator: named %t, want %t, as RunsNothing says", state, named, runs)
		}
	}
	for _, tc := range []struct {
		name        string
		state       supervise.ClaimState
		withLocator bool
	}{
		{"a launch whose outcome is uncertain", supervise.StateLaunchUncertain, false},
		{"a suspended claim that still records a locator", supervise.StateSuspended, true},
		{"an idle claim with its locator", supervise.StateIdle, true},
	} {
		put(tc.state, tc.withLocator)
		if got, want := elsewhere("LEGION-209"), "legion-LEGION-209-implementer:"+string(tc.state); got != want {
			t.Errorf("%s of tree LEGION-208: IssueRunsElsewhere = %q, want %q", tc.name, got, want)
		}
		if got := elsewhere("LEGION-208"); got != "" {
			t.Errorf("%s, asked by the claim's own tree: IssueRunsElsewhere = %q, want none", tc.name, got)
		}
	}
	other := tmuxClaim("legion-LEGION-210-implementer")
	other.Issue = "LEGION-210"
	put(supervise.StateSuspended, false)
	if err := store.PutClaim(ctx, other); err != nil {
		t.Fatalf("put %s: %v", other.Token, err)
	}
	if got := elsewhere("LEGION-209"); got != "" {
		t.Fatalf("only another issue's claim runs: IssueRunsElsewhere = %q, want none", got)
	}
}

// ClaimUnretired is true for a stored claim in every state but retired, the set the orphan sweep
// is told of (the daemon's knownClaims), and false for a retired claim and for a token no claim
// holds.
func TestClaimUnretiredIsEveryStoredStateButRetired(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := migratedStore(t)
	controller := tmuxClaim(claim.ControllerToken("legion"))
	controller.Tree, controller.Issue, controller.Role, controller.Locator = "", "", claim.RoleController, nil
	unretired := func() bool {
		t.Helper()
		got, err := store.ClaimUnretired(ctx, controller.Token)
		if err != nil {
			t.Fatalf("ClaimUnretired(%s): %v", controller.Token, err)
		}
		return got
	}
	if unretired() {
		t.Fatalf("ClaimUnretired(%s) with no claim stored = true, want false", controller.Token)
	}
	for _, state := range supervise.States() {
		controller.State = state
		if err := store.PutClaim(ctx, controller); err != nil {
			t.Fatalf("put %s %s: %v", controller.Token, state, err)
		}
		if got, want := unretired(), state != supervise.StateRetired; got != want {
			t.Errorf("ClaimUnretired of a %s claim = %t, want %t", state, got, want)
		}
	}
}

// storedActivity is the four activity columns of token's claim row, which no claim read carries:
// the report lives on the row alone, and only the idle view reads it back.
func storedActivity(t *testing.T, store *Store, token claim.Token) (at time.Time, background bool, kinds string, reminder time.Time) {
	t.Helper()
	var read, due *time.Time
	if err := store.Pool().QueryRow(context.Background(),
		`select activity_at, activity_background, activity_kinds, activity_next_reminder_at from claims where token = $1`,
		string(token)).Scan(&read, &background, &kinds, &due); err != nil {
		t.Fatalf("read the activity of %s: %v", token, err)
	}
	return orZero(read), background, kinds, orZero(due)
}

// A session fires its reports without an in-flight guard — pi-legion's five-second timer and its
// session_stop report race — so an earlier reading can reach the daemon after a later one. The
// earlier one must not win: a reading taken mid-turn before a background job started would then
// stand as the newest, and the idle rule would release a pod whose job is still running.
func TestAnActivityReportNeverOverwritesANewerOne(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := migratedStore(t)
	c := tmuxClaim("legion-LEGION-209-implementer")
	if err := store.PutClaim(ctx, c); err != nil {
		t.Fatalf("put claim: %v", err)
	}

	// The end-of-turn report: a background job of the session's own is running.
	due := at(20)
	newest := claim.SessionActivity{
		At:             at(10),
		BackgroundWork: claim.BackgroundWork{Running: true, Kinds: []string{"async-job"}},
		Phase:          claim.PhaseActivity{NextReminderAt: &due},
	}
	if known, err := store.PutClaimActivity(ctx, c.Token, c.Generation, newest); err != nil || !known {
		t.Fatalf("PutClaimActivity of the newest report = %t, %v", known, err)
	}

	// The reading taken before the job started, which the five-second timer sent first and which
	// arrives second.
	stale := claim.SessionActivity{At: at(9)}
	if known, err := store.PutClaimActivity(ctx, c.Token, c.Generation, stale); err != nil || !known {
		t.Fatalf("PutClaimActivity of the stale report = %t, %v", known, err)
	}
	gotAt, background, kinds, reminder := storedActivity(t, store, c.Token)
	if !gotAt.Equal(newest.At) || !background || kinds != "async-job" || !reminder.Equal(due) {
		t.Fatalf("after the stale report the row reads %s, background %t, kinds %q, reminder %s; want the newest report kept",
			gotAt, background, kinds, reminder)
	}

	// A report of the same instant is the same reading sent twice, and is written: a retry must
	// not be refused.
	same := claim.SessionActivity{At: at(10)}
	if known, err := store.PutClaimActivity(ctx, c.Token, c.Generation, same); err != nil || !known {
		t.Fatalf("PutClaimActivity of the repeated report = %t, %v", known, err)
	}
	if _, background, kinds, reminder = storedActivity(t, store, c.Token); background || kinds != "" || !reminder.IsZero() {
		t.Fatalf("the repeated report left background %t, kinds %q, reminder %s; want the quiet reading written",
			background, kinds, reminder)
	}

	// A newer one is written whole, the armed reminder cleared with the rest.
	newer := claim.SessionActivity{At: at(11)}
	if known, err := store.PutClaimActivity(ctx, c.Token, c.Generation, newer); err != nil || !known {
		t.Fatalf("PutClaimActivity of the newer report = %t, %v", known, err)
	}
	if gotAt, _, _, _ = storedActivity(t, store, c.Token); !gotAt.Equal(newer.At) {
		t.Fatalf("activity_at = %s after the newer report, want %s", gotAt, newer.At)
	}
}

// A session chooses the time it reports. One dated ahead of the daemon's clock — a host whose
// clock stepped forward and was corrected back afterwards — would, under the ordering guard
// above, drop every honest report after it while the reading it left stood as current,
// background work and all. It is bounded to the daemon's receipt time, as the listener's report
// is (intake.decodeListenerWake).
func TestAnActivityReportDatedAheadOfTheDaemonIsBoundedToItsReceipt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := migratedStore(t)
	c := tmuxClaim("legion-LEGION-210-implementer")
	if err := store.PutClaim(ctx, c); err != nil {
		t.Fatalf("put claim: %v", err)
	}

	before := time.Now().UTC()
	ahead := claim.SessionActivity{At: before.Add(2 * time.Hour),
		BackgroundWork: claim.BackgroundWork{Running: true, Kinds: []string{"async-job"}}}
	if known, err := store.PutClaimActivity(ctx, c.Token, c.Generation, ahead); err != nil || !known {
		t.Fatalf("PutClaimActivity of the report dated ahead = %t, %v", known, err)
	}
	after := time.Now().UTC()
	stored, _, _, _ := storedActivity(t, store, c.Token)
	// The receipt the row kept is a timestamptz, which holds microseconds, so the stored
	// instant is the Go instant of the receipt with its nanoseconds dropped: it can sit up to
	// a microsecond below the window's own lower bound, which is what the window is truncated
	// for. The report's time, two hours ahead, is outside the window either way, so this still
	// fails on a report that was stored unbounded.
	if stored.Before(before.Truncate(time.Microsecond)) || stored.After(after) {
		t.Fatalf("a report dated %s was stored as %s, want the daemon's receipt time between %s and %s",
			ahead.At, stored, before, after)
	}

	// The session's next honest report is written, which is what the unbounded time cost.
	honest := claim.SessionActivity{At: time.Now().UTC()}
	if known, err := store.PutClaimActivity(ctx, c.Token, c.Generation, honest); err != nil || !known {
		t.Fatalf("PutClaimActivity of the honest report = %t, %v", known, err)
	}
	if _, background, kinds, _ := storedActivity(t, store, c.Token); background || kinds != "" {
		t.Fatalf("after the honest report the row reads background %t, kinds %q; want the quiet reading written",
			background, kinds)
	}
}

// A report for a claim the daemon has forgotten is nothing to record, and says so: the route
// answers it as it answers a wrong secret. A report the ordering guard drops is not that.
func TestAnActivityReportOfAnUnknownClaimIsNotRecorded(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := migratedStore(t)
	known, err := store.PutClaimActivity(ctx, "legion-LEGION-999-implementer", 1, claim.SessionActivity{At: at(1)})
	if err != nil {
		t.Fatalf("PutClaimActivity: %v", err)
	}
	if known {
		t.Fatal("a report for a claim the store does not hold was recorded")
	}
}

// activityReport is one reader's answer about the four activity columns of a claim. The
// role-claim read carries no kinds — a rule over a run asks whether such work is running and
// never what it is (record.RoleClaim) — so a reader that does not carry them leaves them nil.
type activityReport struct {
	at         time.Time
	background bool
	kinds      []string
	reminder   time.Time
}

// A session's activity report counts for the process that made it and for no other. The four
// columns survive every write but a launch's: a launch moves the claim's generation on
// (supervise's launchNow), and putClaim clears the report in that same write, leaving the zero
// values that already say this session has reported nothing. All three readers of those columns
// are held to that here, in one test, because none of them checks anything of its own: the
// daemon's sweep reads the claim (claimSelect), the rules over a run read the record (record's
// readRoleClaims), and the idle release reads its own view (readIdleView). A write that keeps
// the generation leaves the report whole, and the replacement's first report is written and read
// whole even when the dead process's was dated later, since the clear also lifts the ordering
// guard that report would otherwise hold over it.
func TestAnActivityReportOfAnEarlierProcessIsNoReportToAnyReader(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := migratedStore(t)
	openRunningTree(t, st)
	token := claim.Token("legion-legion-legion-208-implementer")
	worker := supervise.Claim{Token: token, Project: "legion", Tree: rootIssue, Issue: rootIssue,
		Role: claim.RoleImplementer, TreeEpoch: 1, Generation: 4, ServingGeneration: 1,
		State: supervise.StateReady, Session: "ses_implementer", SessionFile: "/sessions/implementer.jsonl"}
	if err := st.PutClaim(ctx, worker); err != nil {
		t.Fatalf("record the worker's claim: %v", err)
	}
	// The report the dying process made at the end of its turn: a background task of its own is
	// running, with the phase reminder armed in that same session.
	dead := activityReport{at: time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond), background: true,
		kinds: []string{"claude-background-task"}}
	dead.reminder = dead.at.Add(10 * time.Minute)
	if known, err := st.PutClaimActivity(ctx, token, worker.Generation, claim.SessionActivity{At: dead.at,
		BackgroundWork: claim.BackgroundWork{Running: true, Kinds: dead.kinds},
		Phase:          claim.PhaseActivity{NextReminderAt: &dead.reminder},
	}); err != nil || !known {
		t.Fatalf("report the worker's background work = %t, %v", known, err)
	}

	readers := []struct {
		name  string
		kinds bool
		read  func(*testing.T) activityReport
	}{
		{
			name: "the sweep's claim read", kinds: true,
			read: func(t *testing.T) activityReport {
				t.Helper()
				held, err := st.Claims(ctx)
				if err != nil {
					t.Fatalf("read the claims: %v", err)
				}
				if len(held) != 1 || held[0].Token != token {
					t.Fatalf("the claims = %+v, want the one worker's", held)
				}
				return activityReport{at: held[0].ActivityAt, background: held[0].ActivityBackground,
					kinds: held[0].ActivityKinds, reminder: held[0].ActivityNextReminderAt}
			},
		},
		{
			name: "the rules' role-claim read",
			read: func(t *testing.T) activityReport {
				t.Helper()
				var held *record.RoleClaim
				if err := Transact(ctx, st.pool, func(ctx context.Context, tx pgx.Tx) error {
					claims, err := record.NewStore().RoleClaims(ctx, tx, []claim.Token{token})
					held = claims[token]
					return err
				}); err != nil {
					t.Fatalf("read the role claim of %s: %v", token, err)
				}
				if held == nil {
					t.Fatalf("the record holds no claim of %s", token)
				}
				return activityReport{at: held.ActivityAt, background: held.ActivityBackground,
					reminder: held.ActivityNextReminderAt}
			},
		},
		{
			name: "the idle release's claim read", kinds: true,
			read: func(t *testing.T) activityReport {
				t.Helper()
				view := idleViewOf(t, st, rootIssue)
				if len(view.Claims) != 1 {
					t.Fatalf("the idle view's claims = %+v, want the one worker's", view.Claims)
				}
				got := view.Claims[0]
				return activityReport{at: got.ActivityAt, background: got.ActivityBackground,
					kinds: got.ActivityKinds, reminder: got.ActivityNextReminderAt}
			},
		},
	}
	whole := func(t *testing.T, want activityReport, when string) {
		t.Helper()
		for _, reader := range readers {
			got := reader.read(t)
			if !got.at.Equal(want.at) || got.background != want.background || !got.reminder.Equal(want.reminder) {
				t.Errorf("%s reads %+v %s, want the whole report made at %s, background %t, with its reminder due at %s",
					reader.name, got, when, want.at, want.background, want.reminder)
			}
			if reader.kinds && !slices.Equal(got.kinds, want.kinds) {
				t.Errorf("%s reads the kinds %q %s, want the kinds %q the report named", reader.name, got.kinds, when, want.kinds)
			}
		}
	}
	none := func(t *testing.T, when string) {
		t.Helper()
		for _, reader := range readers {
			got := reader.read(t)
			if !got.at.IsZero() || got.background || len(got.kinds) != 0 || !got.reminder.IsZero() {
				t.Errorf("%s reads %+v %s, want no report at all: the four are one report and are cleared together",
					reader.name, got, when)
			}
		}
	}

	whole(t, dead, "while the process that made it is the one running")

	// Another state write of the same process keeps the generation, and the report with it.
	worker.State = supervise.StateIdle
	if err := st.PutClaim(ctx, worker); err != nil {
		t.Fatalf("record the worker gone idle: %v", err)
	}
	whole(t, dead, "after a write that kept the generation")

	// The pod is replaced: the launch of the worker in the pod that replaced it moves the
	// claim's generation on and persists it (supervise's launchNow), and the new process has
	// reported nothing yet.
	worker.Generation++
	worker.State = supervise.StateLaunching
	if err := st.PutClaim(ctx, worker); err != nil {
		t.Fatalf("record the launch that replaced the worker's process: %v", err)
	}
	none(t, "after the launch that replaced the process that made it")

	// The replacement's first report is dated before the dead process's — a host clock behind
	// the old pod's — and is written whole: the dead report is gone, and with it the ordering
	// guard it would have held over every report dated before it.
	first := activityReport{at: dead.at.Add(-time.Minute), background: true, kinds: []string{"async-job", "subagent"}}
	first.reminder = first.at.Add(5 * time.Minute)
	if known, err := st.PutClaimActivity(ctx, token, worker.Generation, claim.SessionActivity{At: first.at,
		BackgroundWork: claim.BackgroundWork{Running: true, Kinds: first.kinds},
		Phase:          claim.PhaseActivity{NextReminderAt: &first.reminder},
	}); err != nil || !known {
		t.Fatalf("report the replacement's background work = %t, %v", known, err)
	}
	whole(t, first, "after the replacement's first report")
}

// A report belongs to the launch whose secret it was sent with, and a dead process's report can
// reach the store after the relaunch that replaced it: the route authenticated it just before the
// launch and wrote it just after the launch's clear. It is refused, so the new process's empty
// report stands — a Claude Code worker reports only at its Stop hook, and a dead process's
// background report written over that emptiness would hold its silence repair off for good —
// and answered as a claim the store does not hold at that launch, which the route answers with
// claim.InvalidSecret.
func TestAnActivityReportOfALaunchThatIsGoneIsRefused(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := migratedStore(t)
	c := tmuxClaim("legion-LEGION-212-implementer")
	if err := store.PutClaim(ctx, c); err != nil {
		t.Fatalf("put claim: %v", err)
	}
	old := c.Generation
	c.Generation++
	c.State = supervise.StateLaunching
	if err := store.PutClaim(ctx, c); err != nil {
		t.Fatalf("record the launch that replaced the process: %v", err)
	}

	dead := claim.SessionActivity{At: time.Now().UTC().Add(-time.Minute),
		BackgroundWork: claim.BackgroundWork{Running: true, Kinds: []string{"claude-background-task"}}}
	known, err := store.PutClaimActivity(ctx, c.Token, old, dead)
	if err != nil {
		t.Fatalf("PutClaimActivity of the old launch's report: %v", err)
	}
	if known {
		t.Error("the report of a launch that is gone was answered as one the store holds")
	}
	if at, background, kinds, reminder := storedActivity(t, store, c.Token); !at.IsZero() || background || kinds != "" || !reminder.IsZero() {
		t.Fatalf("after the old launch's report the row reads %s, background %t, kinds %q, reminder %s; want the launch's clear standing",
			at, background, kinds, reminder)
	}

	// The new launch's own report is written.
	current := claim.SessionActivity{At: time.Now().UTC().Add(-time.Second)}
	if known, err := store.PutClaimActivity(ctx, c.Token, c.Generation, current); err != nil || !known {
		t.Fatalf("PutClaimActivity of the current launch's report = %t, %v", known, err)
	}
	if at, _, _, _ := storedActivity(t, store, c.Token); at.IsZero() {
		t.Fatal("the current launch's report was not written")
	}
}

// The kinds a session names reach the architect's worker-background-stalled notice and the
// daemon's log, and the session chooses every word it sends, so the write keeps only the three
// known kinds, in the session's order, and drops every other string. Whether work is running is
// the session's own flag, and a report whose kinds were all unknown keeps it.
func TestAnActivityReportKeepsOnlyTheKnownKinds(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := migratedStore(t)
	c := tmuxClaim("legion-LEGION-211-implementer")
	if err := store.PutClaim(ctx, c); err != nil {
		t.Fatalf("put claim: %v", err)
	}

	named := claim.SessionActivity{At: at(10), BackgroundWork: claim.BackgroundWork{Running: true,
		Kinds: []string{"subagent", "<b>urgent</b>", "async-job", "", "claude-background-task", "async-job,evil"}}}
	if known, err := store.PutClaimActivity(ctx, c.Token, c.Generation, named); err != nil || !known {
		t.Fatalf("PutClaimActivity of the report naming unknown kinds = %t, %v", known, err)
	}
	if _, background, kinds, _ := storedActivity(t, store, c.Token); !background || kinds != "subagent,async-job,claude-background-task" {
		t.Fatalf("the row reads background %t, kinds %q; want the work running and the three known kinds alone, in order",
			background, kinds)
	}

	unknown := claim.SessionActivity{At: at(11), BackgroundWork: claim.BackgroundWork{Running: true,
		Kinds: []string{"shell"}}}
	if known, err := store.PutClaimActivity(ctx, c.Token, c.Generation, unknown); err != nil || !known {
		t.Fatalf("PutClaimActivity of the report naming only an unknown kind = %t, %v", known, err)
	}
	if _, background, kinds, _ := storedActivity(t, store, c.Token); !background || kinds != "" {
		t.Fatalf("the row reads background %t, kinds %q; want the work running and no kind", background, kinds)
	}
}

// storedStep is the eight step columns of token's claim row, read from the row itself: a claim
// read carries the same record back, and the tests below compare the two.
func storedStep(t *testing.T, store *Store, token claim.Token) supervise.StepRecord {
	t.Helper()
	var step supervise.StepRecord
	var stepAt, turn, started, ended *time.Time
	if err := store.Pool().QueryRow(context.Background(),
		`select step_at, turn_started_at, step_tool, step_call_id, step_args_bytes, step_args_hash,
			step_started_at, step_ended_at from claims where token = $1`, string(token)).
		Scan(&stepAt, &turn, &step.Tool, &step.CallID, &step.ArgsBytes, &step.ArgsHash, &started, &ended); err != nil {
		t.Fatalf("read the step record of %s: %v", token, err)
	}
	step.At, step.TurnStartedAt = orZero(stepAt), orZero(turn)
	step.StartedAt, step.EndedAt = orZero(started), orZero(ended)
	return step
}

// sameStep compares two records, the times as instants: Postgres hands a timestamptz back in the
// session's zone, not the one it was written in.
func sameStep(t *testing.T, got, want supervise.StepRecord, what string) {
	t.Helper()
	if got.Tool != want.Tool || got.CallID != want.CallID ||
		got.ArgsBytes != want.ArgsBytes || got.ArgsHash != want.ArgsHash {
		t.Errorf("%s names the call %q/%q of %d bytes, hash %q; want %q/%q of %d bytes, hash %q",
			what, got.Tool, got.CallID, got.ArgsBytes, got.ArgsHash,
			want.Tool, want.CallID, want.ArgsBytes, want.ArgsHash)
	}
	for _, field := range []struct {
		name      string
		got, want time.Time
	}{
		{"At", got.At, want.At},
		{"TurnStartedAt", got.TurnStartedAt, want.TurnStartedAt},
		{"StartedAt", got.StartedAt, want.StartedAt},
		{"EndedAt", got.EndedAt, want.EndedAt},
	} {
		if !field.got.Equal(field.want) {
			t.Errorf("%s %s = %v, want %v", what, field.name, field.got, field.want)
		}
	}
}

// A bash call of a worker, as the stream's two frames leave it on the claim: the turn's start
// first, then the call, with the length and the hash of its arguments and no text of them.
func aBashStep() supervise.StepRecord {
	return supervise.StepRecord{
		At:        at(3),
		Tool:      "bash",
		CallID:    "toolu_turn1_0",
		ArgsBytes: 142,
		ArgsHash:  "9f2b7c1d4e5a6b8c",
		StartedAt: at(2),
		EndedAt:   at(3),
	}
}

// The backstop that ends a hung turn reads this record and nothing else, and it runs in whatever
// daemon is up at the time: a daemon that restarted mid-turn has only what the row holds, so the
// whole record has to survive the restart. A reopened store is that restart (reopenStore).
func TestAStepRecordRoundTripsThroughAReopenedStore(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := migratedStore(t)
	c := tmuxClaim("legion-LEGION-209-implementer")
	if err := store.PutClaim(ctx, c); err != nil {
		t.Fatalf("put claim: %v", err)
	}

	if known, err := store.PutClaimStep(ctx, c.Token, supervise.StepRecord{TurnStartedAt: at(1)}); err != nil || !known {
		t.Fatalf("PutClaimStep of the turn's start = %t, %v", known, err)
	}
	step := aBashStep()
	if known, err := store.PutClaimStep(ctx, c.Token, step); err != nil || !known {
		t.Fatalf("PutClaimStep of the call = %t, %v", known, err)
	}

	want := step
	want.TurnStartedAt = at(1)
	sameStep(t, storedStep(t, store, c.Token), want, "the row's step record")

	reopened := reopenStore(t, store)
	claims, err := reopened.Claims(ctx)
	if err != nil {
		t.Fatalf("claims of the reopened store: %v", err)
	}
	if len(claims) != 1 {
		t.Fatalf("claims = %d rows, want 1: %+v", len(claims), claims)
	}
	sameStep(t, claims[0].Step, want, "the step record a reopened store reads back")
}

// The record arrives between the machine's writes and the machine's in-memory claim does not
// carry it, so a machine that persists its next state must leave it standing: a claim written
// whole would erase every step the stream reported since that machine read its claim, and the
// backstop would read a worker mid-call as one that has never stepped.
func TestPutClaimLeavesTheStepRecordAlone(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := migratedStore(t)
	c := tmuxClaim("legion-LEGION-210-implementer")
	if err := store.PutClaim(ctx, c); err != nil {
		t.Fatalf("put claim: %v", err)
	}
	step := aBashStep()
	step.TurnStartedAt = at(1)
	if known, err := store.PutClaimStep(ctx, c.Token, step); err != nil || !known {
		t.Fatalf("PutClaimStep = %t, %v", known, err)
	}

	// The machine, which never read the record, persists its next state.
	c.State = supervise.StateWorking
	if err := store.PutClaim(ctx, c); err != nil {
		t.Fatalf("put the claim again: %v", err)
	}

	sameStep(t, storedStep(t, store, c.Token), step, "the step record after the machine's write")
	sameStep(t, onlyClaim(t, store).Step, step, "the step record a claim read carries")
}

// A connection that dies with frames unflushed hands them to the next one, so an earlier step can
// reach the daemon after a later one. The earlier one must not win: the backstop would read a
// worker that has been silent for an hour as one that stepped a moment ago, which is the stall
// this record exists to find.
func TestAStepOlderThanTheRecordedStepDoesNotMoveItBack(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := migratedStore(t)
	c := tmuxClaim("legion-LEGION-211-implementer")
	if err := store.PutClaim(ctx, c); err != nil {
		t.Fatalf("put claim: %v", err)
	}
	newest := aBashStep()
	newest.At = at(10)
	if known, err := store.PutClaimStep(ctx, c.Token, newest); err != nil || !known {
		t.Fatalf("PutClaimStep of the newest step = %t, %v", known, err)
	}

	older := supervise.StepRecord{At: at(9), Tool: "eval", CallID: "toolu_turn0_3",
		ArgsBytes: 31, ArgsHash: "0011223344556677", StartedAt: at(8), EndedAt: at(9)}
	if known, err := store.PutClaimStep(ctx, c.Token, older); err != nil || !known {
		t.Fatalf("PutClaimStep of the older step = %t, %v", known, err)
	}
	sameStep(t, storedStep(t, store, c.Token), newest, "the step record after the older step")

	// A step of the same instant is the same frame sent twice, and is written: a retry must not
	// be refused.
	same := supervise.StepRecord{At: at(10), Tool: "read", CallID: "toolu_turn1_1",
		ArgsBytes: 64, ArgsHash: "aabbccddeeff0011", StartedAt: at(10)}
	if known, err := store.PutClaimStep(ctx, c.Token, same); err != nil || !known {
		t.Fatalf("PutClaimStep of the repeated step = %t, %v", known, err)
	}
	sameStep(t, storedStep(t, store, c.Token), same, "the step record after the repeated step")

	// A newer one is written whole, the call it names replacing the one before it.
	newer := supervise.StepRecord{At: at(11), Tool: "write", CallID: "toolu_turn1_2",
		ArgsBytes: 900, ArgsHash: "2233445566778899", StartedAt: at(11)}
	if known, err := store.PutClaimStep(ctx, c.Token, newer); err != nil || !known {
		t.Fatalf("PutClaimStep of the newer step = %t, %v", known, err)
	}
	sameStep(t, storedStep(t, store, c.Token), newer, "the step record after the newer step")
}

// The fence a frame of a replaced process needs. Such a frame — one that arrived on a connection
// the claim no longer has, or on one older than the connection it has now (supervise's
// staleStep) — is stamped by the daemon on receipt like any other, so a burst of them would give
// a hung worker a fresh clock. Such a step carries no time of its own: it records the call it
// names, which is evidence of what the agent was doing, and leaves the claim's last-step time
// and its turn's start where they are.
func TestAStepWithNoTimeOfItsOwnRecordsItsCallAndLeavesTheLastStep(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := migratedStore(t)
	c := tmuxClaim("legion-LEGION-212-implementer")
	if err := store.PutClaim(ctx, c); err != nil {
		t.Fatalf("put claim: %v", err)
	}
	live := aBashStep()
	live.At, live.TurnStartedAt = at(10), at(1)
	if known, err := store.PutClaimStep(ctx, c.Token, live); err != nil || !known {
		t.Fatalf("PutClaimStep of the live step = %t, %v", known, err)
	}

	stale := supervise.StepRecord{Tool: "eval", CallID: "toolu_turn0_7",
		ArgsBytes: 2048, ArgsHash: "cafebabecafebabe", StartedAt: at(4)}
	if known, err := store.PutClaimStep(ctx, c.Token, stale); err != nil || !known {
		t.Fatalf("PutClaimStep of the stale step = %t, %v", known, err)
	}

	want := stale
	want.At, want.TurnStartedAt = at(10), at(1)
	sameStep(t, storedStep(t, store, c.Token), want, "the step record after the stale frame")
}

// A turn's start is written through the same narrow call and carries no call of its own. It
// moves no last-step time, and it leaves the call the record names standing: the recovery this
// record exists for ends the hung process, relaunches the claim on the same session and starts a
// turn on the replacement, and a turn's start that cleared the call would erase the one fact
// anybody is looking for — which call was open when the agent stopped.
func TestATurnsStartMovesTheTurnAndKeepsTheCallTheRecordNames(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := migratedStore(t)
	c := tmuxClaim("legion-LEGION-213-implementer")
	if err := store.PutClaim(ctx, c); err != nil {
		t.Fatalf("put claim: %v", err)
	}
	hung := aBashStep()
	hung.At, hung.TurnStartedAt = at(10), at(1)
	if known, err := store.PutClaimStep(ctx, c.Token, hung); err != nil || !known {
		t.Fatalf("PutClaimStep of the call that hung = %t, %v", known, err)
	}

	// The daemon ended the process and relaunched the claim; the replacement starts a turn.
	if known, err := store.PutClaimStep(ctx, c.Token, supervise.StepRecord{TurnStartedAt: at(20)}); err != nil || !known {
		t.Fatalf("PutClaimStep of the recovered turn's start = %t, %v", known, err)
	}

	want := hung
	want.TurnStartedAt = at(20)
	sameStep(t, storedStep(t, store, c.Token), want, "the step record after the recovered turn's start")
	sameStep(t, onlyClaim(t, store).Step, want, "the step record a claim read carries after the recovery")
}

// The next call replaces the last one whole, every field of it: no field of one call is ever
// read beside a field of another, which is what a record built field by field would allow.
func TestALaterCallOverwritesEveryFieldOfTheStepRecord(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := migratedStore(t)
	c := tmuxClaim("legion-LEGION-214-implementer")
	if err := store.PutClaim(ctx, c); err != nil {
		t.Fatalf("put claim: %v", err)
	}
	first := aBashStep()
	first.At, first.TurnStartedAt = at(10), at(1)
	if known, err := store.PutClaimStep(ctx, c.Token, first); err != nil || !known {
		t.Fatalf("PutClaimStep of the first call = %t, %v", known, err)
	}

	// Every field differs from the first call's, the still-open end among them.
	next := supervise.StepRecord{At: at(12), Tool: "eval", CallID: "toolu_turn2_5",
		ArgsBytes: 7, ArgsHash: "1314151617181920", StartedAt: at(12)}
	if known, err := store.PutClaimStep(ctx, c.Token, next); err != nil || !known {
		t.Fatalf("PutClaimStep of the next call = %t, %v", known, err)
	}

	want := next
	want.TurnStartedAt = at(1)
	sameStep(t, storedStep(t, store, c.Token), want, "the step record after the next call")
}

// A step of a claim the daemon has forgotten is nothing to record, and says so, as a session's
// activity report of one does. A step the ordering guard drops is not that: the claim is there.
func TestAStepOfAnUnknownClaimIsNotRecorded(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := migratedStore(t)
	known, err := store.PutClaimStep(ctx, "legion-LEGION-999-implementer", aBashStep())
	if err != nil {
		t.Fatalf("PutClaimStep: %v", err)
	}
	if known {
		t.Fatal("a step of a claim the store does not hold was recorded")
	}
}

// A claim's message mark is written apart from the claim (MarkClaimMessage), so a machine's write
// of the claim it holds in memory undoes no mark. Of the marks the role has taken no turn since,
// the earliest is the one kept and the one every mark answers; a turn at or after the mark ends
// it, and the next mark is a new one. A retired claim takes no mark, and a claim loses the one it
// holds as it retires.
func TestAMessageMarkOutlivesMachineWritesButStandsOnlyUntilTheNextTurn(t *testing.T) {
	ctx := t.Context()
	st := migratedStore(t)
	c := tmuxClaim("legion-LEGION-209-implementer")
	if err := st.PutClaim(ctx, c); err != nil {
		t.Fatal(err)
	}
	mark := func(at, stands time.Time) {
		t.Helper()
		got, err := MarkClaimMessage(ctx, st.Pool(), c.Token, at)
		if err != nil {
			t.Fatal(err)
		}
		if !got.Equal(stands) {
			t.Fatalf("a mark at %s answers %s, want the standing mark %s", at, got, stands)
		}
	}
	read := func(want time.Time, standing bool) {
		t.Helper()
		claims, err := st.Claims(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if got := claims[0]; !got.MessageMarkAt.Equal(want) || got.MessageMarked() != standing {
			t.Fatalf("mark = %s, stands %t; want %s, %t", got.MessageMarkAt, got.MessageMarked(), want, standing)
		}
	}
	at := time.Date(2026, 11, 25, 9, 0, 0, 0, time.UTC)
	mark(at.Add(time.Minute), at.Add(time.Minute))
	mark(at, at)
	mark(at.Add(time.Hour), at)
	if err := st.PutClaim(ctx, c); err != nil {
		t.Fatal(err)
	}
	read(at, true)
	c.LastTurnStart = at
	if err := st.PutClaim(ctx, c); err != nil {
		t.Fatal(err)
	}
	read(at, false)
	next := at.Add(time.Minute)
	mark(next, next)
	mark(at.Add(-time.Minute), next)
	read(next, true)

	c.State = supervise.StateRetired
	if err := st.PutClaim(ctx, c); err != nil {
		t.Fatal(err)
	}
	read(time.Time{}, false)
	mark(next.Add(time.Minute), time.Time{})
	read(time.Time{}, false)
}
