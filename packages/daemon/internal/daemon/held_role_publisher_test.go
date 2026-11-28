package daemon

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/wait"
)

// A claim the boot could not suspend keeps its process, and that process keeps the Envoy role its
// session claimed, so the outbox publishes nothing to the role of a claim that records a session
// it does not hold (outbox.publish, reviews rounds 13 to 15 of #4220), whatever the claim's state:
// a role message to a ready claim and to a retired one finishes undelivered with the publisher
// never called. A message to a claim holding its own session is published as before (R9), while a
// role message to a token with no supervising machine ends without publishing (roleWaitGate,
// dispatch://INFRA-4131/spec#b-r-wait-ready).
//
// The controls (the pull request records the runs): publish handing every topic to the publisher
// as it came publishes to both claims that do not hold their sessions.
func TestTheOutboxPublishesNothingToTheRoleOfAClaimThatDoesNotHoldItsSession(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, publisher, runner := heldTopicsRig(t, heldTopicsOptions{launched: true}, []supervise.Claim{
		{Token: mustClaimToken(t, "LEGION-941", claim.RoleImplementer), Tree: "LEGION-941", Issue: "LEGION-941", Role: claim.RoleImplementer, Session: "ses_941", State: supervise.StateReady},
		{Token: mustClaimToken(t, "LEGION-942", claim.RoleImplementer), Tree: "LEGION-942", Issue: "LEGION-942", Role: claim.RoleImplementer, Session: "ses_942", State: supervise.StateReady},
		{Token: mustClaimToken(t, "LEGION-944", claim.RoleTester), Tree: "LEGION-944", Issue: "LEGION-944", Role: claim.RoleTester, Session: "ses_944", State: supervise.StateRetired},
	}, "ses_942", "ses_944")
	held, unheld, retired := mustClaimToken(t, "LEGION-941", claim.RoleImplementer), mustClaimToken(t, "LEGION-942", claim.RoleImplementer), mustClaimToken(t, "LEGION-944", claim.RoleTester)

	const noClaim = "a-topic-of-no-claim"
	for i, role := range []string{string(unheld), string(held), string(retired), noClaim} {
		row := mustOutboxRow(t, "LEGION-941", record.RoleMessage{Role: role, Body: "CI settled at head"}, time.Now())
		row.ID = int64(90 + i)
		if err := runner.execute(ctx, row); err != nil {
			t.Fatalf("execute the role message to %s: %v", role, err)
		}
	}
	want := []string{notify.RoleTopicPrefix + string(held)}
	if got := publisher.topics(); !slices.Equal(got, want) {
		t.Fatalf("published to %v, want %v: %s and the retired %s record sessions another claim holds and %s has no claim, so nothing goes to their roles", got, want, unheld, retired, noClaim)
	}
}

// A hook's prompt publishes with keep (publishKeptToRole), and a kept copy waits on the role's kept
// lane for the role's next live holder, which is the unheld session's process while it lives. So
// the gate holds a kept publish as it holds every other (outbox.publishKept, the merge of
// cybermonklabs/legion#4088 into #4220): to a claim that records a session it does not hold, a hook
// prompt publishes nothing, kept or not, and its row waits for a session to hold the role as it
// waits on the listener's own no-holder answer (publishKeptToRole). To a claim holding its own
// session, it is published with keep as before (R9). One hook prompt to each claim stands in for
// the kept rows, since a hook prompt and a phase-ended message are the two kinds publishKeptToRole
// publishes, and one stands in for both.
//
// The control (the pull request records the run): publishKeptToRole calling the publisher's
// PublishKept directly, as #4088 wrote it, publishes the row to the claim whose session another
// claim holds as well.
func TestTheOutboxKeepsNothingForTheRoleOfAClaimThatDoesNotHoldItsSession(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, publisher, runner := heldTopicsRig(t, heldTopicsOptions{launched: true}, []supervise.Claim{
		{Token: mustClaimToken(t, "LEGION-941", claim.RoleTester), Tree: "LEGION-941", Issue: "LEGION-941", Role: claim.RoleTester, Session: "ses_941", State: supervise.StateReady},
		{Token: mustClaimToken(t, "LEGION-942", claim.RoleTester), Tree: "LEGION-942", Issue: "LEGION-942", Role: claim.RoleTester, Session: "ses_942", State: supervise.StateReady},
	}, "ses_942")
	held, unheld := mustClaimToken(t, "LEGION-941", claim.RoleTester), mustClaimToken(t, "LEGION-942", claim.RoleTester)

	hook := record.HookPrompt{Hook: "back-to-implementing", Role: claim.RoleTester, From: phase.Testing, To: phase.Implementing,
		Count: 1, Counts: map[string]int{"testing-implementing": 1}, Prompt: "Read the tester's thread before you write anything."}
	for i, issue := range []string{"LEGION-942", "LEGION-941"} {
		row := mustOutboxRow(t, issue, hook, time.Now())
		row.ID, row.CreatedAt = int64(80+i), time.Now()
		err := runner.execute(ctx, row)
		if issue == "LEGION-942" {
			if !errors.Is(err, wait.ErrWaiting) || !errors.Is(err, notify.ErrNoHolder) {
				t.Fatalf("execute %s's hook prompt row = %v, want a wait on the gate's no-holder answer", issue, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("execute %s's hook prompt row: %v", issue, err)
		}
	}
	publisher.mu.Lock()
	published := slices.Clone(publisher.publish)
	publisher.mu.Unlock()
	want := notify.RoleTopicPrefix + string(held)
	if len(published) != 1 {
		t.Fatalf("published %d messages, want 1, to %s: %s records a session another claim holds, so nothing goes to its role, kept or not: %+v",
			len(published), want, unheld, published)
	}
	for _, p := range published {
		if p.topic != want || !p.keep {
			t.Fatalf("published to %s (keep %v), want %s with keep: %s records a session another claim holds", p.topic, p.keep, want, unheld)
		}
	}
}

// A claim's forget of a session another claim holds opens the claim's role topic, since a claim
// recording no session holds what it records, and the Envoy listener still names that session as
// the role's holder while the other claim's process lives under it (as after a rollback). So the
// forget waits until the listener no longer does (supervise's mayForget, review round 16 of
// #4220): while it names the session the forget is refused and the claim's role messages are not
// published, and the gate writes its line once, at Info, while two rows of the claim take turns
// (the rows' own waits write the one line above Info, dispatch://INFRA-4131/spec#b-r-logged-once);
// once the role has no holder the forget goes on, the message is published to the claim's own
// role, and the gate's record of the line for the claim is gone.
//
// The controls (the pull request records the runs): today's forget, mayForget admitting every
// clear, is admitted while the listener names ses_942 and the role message is published to the
// role whose holder is ses_942, the process of the claim that holds that session; the gate keyed
// by row writes its line twice.
func TestAForgetWaitsUntilTheListenerNoLongerNamesTheSharedSession(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	contained := mustClaimToken(t, "LEGION-942", claim.RoleImplementer)
	const shared = "ses_942"
	listener := &fakeRoleListener{holders: map[string]string{string(contained): shared}}
	var logs syncBuffer
	sup, publisher, runner := heldTopicsRig(t, heldTopicsOptions{launched: true, roleHolder: listener.RoleHolder, log: slog.New(slog.NewJSONHandler(&logs, nil))},
		[]supervise.Claim{{Token: contained, Tree: "LEGION-942", Issue: "LEGION-942", Role: claim.RoleImplementer, Session: shared, State: supervise.StateSuspended}},
		shared)
	m, ok := sup.Machine(contained)
	if !ok {
		t.Fatalf("no machine for %s", contained)
	}
	message := func(id int64) {
		t.Helper()
		row := mustOutboxRow(t, "LEGION-942", record.RoleMessage{Role: string(contained), Body: "CI settled at head"}, time.Now())
		row.ID = id
		if err := runner.execute(ctx, row); err != nil {
			t.Fatalf("execute role message %d: %v", id, err)
		}
	}

	forgot := m.Handle(ctx, supervise.RequestForgetSession{Claim: contained, Session: shared, By: supervise.SuspendedByOperator})
	for _, id := range []int64{90, 91, 90, 91} {
		message(id)
	}
	if got := publisher.topics(); forgot == nil || len(got) != 0 {
		t.Fatalf("while the listener names %s as the holder of %s's role, the forget = %v and the outbox published to %v: a forget admitted then sends the role's messages to the process under %s, which another claim holds",
			shared, contained, forgot, got, shared)
	}
	if !strings.Contains(forgot.Error(), "legion claims suspend") {
		t.Fatalf("the refused forget = %v, want it to name the suspend that stops the process under %s", forgot, shared)
	}
	const logged = "outbox: not publishing to a claim that records a session it does not hold"
	if lines := loggedLines(t, logs.String(), logged); len(lines) != 1 || lines[0]["level"] != slog.LevelInfo.String() {
		t.Fatalf("the gate wrote %v while two rows of %s took turns, want one line, at Info", lines, contained)
	}

	listener.free(string(contained))
	if err := m.Handle(ctx, supervise.RequestForgetSession{Claim: contained, Session: shared, By: supervise.SuspendedByOperator}); err != nil {
		t.Fatalf("the forget once the listener names no holder of the role = %v, want it admitted", err)
	}
	message(92)
	if got, want := publisher.topics(), []string{notify.RoleTopicPrefix + string(contained)}; !slices.Equal(got, want) {
		t.Fatalf("published to %v once the forget went on, want %v", got, want)
	}
	if _, still := runner.unheldLogged.Load(contained); still {
		t.Fatalf("the gate still records a line for %s after its hold passed", contained)
	}
}

// R2 (f) of dispatch://INFRA-1585: the controller's role topic follows the deployment's controller
// mode (heldTopic, review round 15 of #4220). Under `controller: operator` the role is the
// operator's own controller's, so a launched controller claim an earlier boot retired
// (stopLaunchedController), whose session another claim holds, holds no controller notice back:
// it is published once the operator's registered session holds the role live. Under `controller:
// daemon` the launched claim is the project's controller, so a notice waits while that claim
// records a session it does not hold, logged as a wait rather than as a failure, and is published
// while it holds its own.
//
// The controls (the pull request records the runs): the code before this change, which publishes
// every one of them to the controller topic, notifications.legion.legion.controller; heldTopic
// holding the controller's role topic under either mode, which holds the notice under `controller:
// operator` too; and controllerNotice answering the gate's refusal as a plain failure, which fails
// the wait assertion.
func TestTheControllerRoleTopicIsHeldOnlyToTheDaemonsOwnController(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	controller := claim.ControllerToken("legion")
	notice := func(t *testing.T) record.OutboxRow {
		row := mustOutboxRow(t, "LEGION-941", record.ControllerNotice{Kind: "held", Role: claim.RolePlanner, Phase: phase.Planning}, time.Now())
		row.ID = 99
		return row
	}
	t.Run("controller: operator, a retired launched claim not holding its session", func(t *testing.T) {
		t.Parallel()
		_, publisher, runner := heldTopicsRig(t, heldTopicsOptions{controllerSession: "ses_operator"}, []supervise.Claim{
			{Token: controller, Role: claim.RoleController, Session: "ses_controller", State: supervise.StateRetired},
		}, "ses_controller")
		if err := runner.execute(ctx, notice(t)); err != nil {
			t.Fatalf("the controller notice under controller: operator = %v, want it published to the operator's controller", err)
		}
		if got, want := publisher.topics(), []string{controllerRoleTopic}; !slices.Equal(got, want) {
			t.Fatalf("published to %v, want %v: the operator's controller holds the role", got, want)
		}
	})
	t.Run("controller: daemon, its claim not holding its session", func(t *testing.T) {
		t.Parallel()
		_, publisher, runner := heldTopicsRig(t, heldTopicsOptions{launched: true, controllerSession: "ses_controller"}, []supervise.Claim{
			{Token: controller, Role: claim.RoleController, Session: "ses_controller", State: supervise.StateReady},
		}, "ses_controller")
		err := runner.execute(ctx, notice(t))
		if !errors.Is(err, wait.ErrWaiting) || !errors.Is(err, notify.ErrNoHolder) {
			t.Fatalf("the controller notice = %v, want a wait on the gate's refusal", err)
		}
		if got := publisher.topics(); len(got) != 0 {
			t.Fatalf("published to %v while the daemon's controller claim records a session it does not hold", got)
		}
	})
	t.Run("controller: daemon, its claim holding its session", func(t *testing.T) {
		t.Parallel()
		_, publisher, runner := heldTopicsRig(t, heldTopicsOptions{launched: true, controllerSession: "ses_controller"}, []supervise.Claim{
			{Token: controller, Role: claim.RoleController, Session: "ses_controller", State: supervise.StateReady},
		})
		if err := runner.execute(ctx, notice(t)); err != nil {
			t.Fatalf("the controller notice while the daemon's controller claim holds its session = %v, want it published", err)
		}
		if got, want := publisher.topics(), []string{controllerRoleTopic}; !slices.Equal(got, want) {
			t.Fatalf("published to %v, want %v: the controller claim holds its own session", got, want)
		}
	})
}

// fakeRoleListener is the Envoy listener's role lookup (GET /v1/roles/<role>), as a forget asks it
// (supervise.Deps.RoleHolder) and as the outbox asks it before a controller notice
// (outboxNotices.RoleHolder): the session each role a test named (holds) is held by, live, and no
// holder for any other role. A test publisher embeds it for its role read.
type fakeRoleListener struct {
	mu      sync.Mutex
	holders map[string]string
}

// RoleHolder is the listener's answer for role, as a forget and the outbox's publisher ask it.
func (l *fakeRoleListener) RoleHolder(_ context.Context, role string) (string, string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if session, ok := l.holders[role]; ok {
		return session, notify.StateLive, nil
	}
	return "", "", nil
}

// holds makes session the live holder of role, as its claim on the listener does.
func (l *fakeRoleListener) holds(role, session string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.holders == nil {
		l.holders = map[string]string{}
	}
	l.holders[role] = session
}

// free is the role claim reaper taking role's claim: the listener names no holder of it.
func (l *fakeRoleListener) free(role string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.holders, role)
}

// heldTopicsOptions is how heldTopicsRig builds its daemon: launched is `controller: daemon`,
// roleHolder the supervisor's listener lookup (nil asks none), log the outbox's (nil is quiet),
// and controllerSession the project's registered controller session, which the rig records in a
// store of its own and names the controller role's live holder at its publisher ("" records none,
// and the outbox has no store).
type heldTopicsOptions struct {
	launched          bool
	roleHolder        func(ctx context.Context, role string) (string, string, error)
	log               *slog.Logger
	controllerSession string
}

// heldTopicsRig is a supervisor with a machine for each of claims, in project legion at generation
// 1, and an outbox built as newOutbox builds it, as opts says. Each of taken is a session another
// claim, legion-943's implementer, recorded first.
func heldTopicsRig(t *testing.T, opts heldTopicsOptions, claims []supervise.Claim, taken ...string) (*supervisor, *outboxPublisher, *outbox) {
	t.Helper()
	ctx := context.Background()
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	sup.deps.RoleHolder = opts.roleHolder
	claimStore := sup.deps.Store.(*outboxClaimStore)
	holder := mustClaimToken(t, "LEGION-943", claim.RoleImplementer)
	for _, s := range taken {
		if _, err := claimStore.HoldSessions(ctx, "legion", holder, supervise.RecordedHolds(s, "")); err != nil {
			t.Fatalf("hold %s for %s, the claim that recorded it first: %v", s, holder, err)
		}
	}
	for _, c := range claims {
		c.Project, c.Generation, c.SessionFile = "legion", 1, "/sessions/"+c.Session+".jsonl"
		if _, _, err := sup.Create(ctx, c, ""); err != nil {
			t.Fatalf("create %s: %v", c.Token, err)
		}
	}
	publisher := &outboxPublisher{}
	var pool *store.Pool
	var records record.Store
	if opts.controllerSession != "" {
		pool, records = isolatedOutboxPool(t), record.NewStore()
		registerProjectControllerAs(t, pool.Config().ConnString(), "LEGION", opts.controllerSession)
		publisher.holds(legionController, opts.controllerSession)
	}
	runner := newOutbox(pool, records, &outboxDispatch{}, publisher, sup, nil, nil, nil, nil, "legion", "LEGION", t.TempDir(), "", nil, nil, opts.launched, cmp.Or(opts.log, quietLogger()))
	return sup, publisher, runner
}
