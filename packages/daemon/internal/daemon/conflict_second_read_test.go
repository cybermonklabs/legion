package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/classify"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// The log lines of the second read's row (conflictConfirm) and of the workflow's answer to it that
// these tests read. secondReadLine is the start of every line the row writes for an attempt that
// read GitHub, one per attempt, and of no other line: its line for a row it ends without reading
// is secondReadSkipped.
const (
	secondReadLine     = "outbox conflict confirm:"
	secondReadAnswered = "outbox conflict confirm: GitHub answered; the workflow decides on the answer"
	secondReadSkipped  = "outbox conflict confirm finished without a read"
	secondReadReleased = "workflow: a second read did not confirm the conflict"
	rulesReadLine      = "github: read the base's rules for a conflicting head"
)

// rulesAnswer is how the rules stand-in answers one path: status (200 when zero), body, and the
// path of the next page, which it names in a Link header, "" for none.
type rulesAnswer struct {
	status int
	body   string
	next   string
}

// rulesStandIn is GitHub's REST API answering each escaped path routes names as it says, and 404
// for any other, recording every escaped path it was asked for.
func rulesStandIn(t *testing.T, routes map[string]rulesAnswer) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.EscapedPath()
		mu.Lock()
		seen = append(seen, path)
		mu.Unlock()
		answer, ok := routes[path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if answer.next != "" {
			w.Header().Set("Link", "<"+server.URL+answer.next+`>; rel="next"`)
		}
		w.Header().Set("Content-Type", "application/json")
		if answer.status != 0 {
			w.WriteHeader(answer.status)
		}
		_, _ = io.WriteString(w, answer.body)
	}))
	t.Cleanup(server.Close)
	return server, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}

// appliedRead applies GitHub's read of CAPTURE-1's pull request 86 at head code on base, its
// `mergeable` as given, through applyMergeability with the implement App's client of acme/widgets
// at api, as every read of the daemon applies it, and answers the one fact the read applied as the
// workflow's handler receives it, with the log the read wrote.
func appliedRead(t *testing.T, pool *store.Pool, api, base string, mergeable bool, eventID string) (intake.PullRequestMergeability, string) {
	t.Helper()
	logs := &syncBuffer{}
	w := readRuntime(pool, api, slog.New(slog.NewJSONHandler(logs, nil)))
	facts := &outboxFactHandler{}
	w.handlers = []intake.Handler{facts}
	pull := githubPullRequest{Number: 86, State: "open", Mergeable: &mergeable, MergeableState: "dirty"}
	pull.Base.Ref, pull.Head.Ref, pull.Head.SHA = base, "legion/CAPTURE-1", "code"
	github := githubrest.Client{Token: "outbox-token", API: githubrest.RepositoryAPI(api, ghrepo.MustParse("acme/widgets"))}
	pr := record.PullRequest{State: record.PullRequestOpen, Issue: "CAPTURE-1", Repo: "acme/widgets", Number: 86, Base: base, HeadSHA: "code"}
	if err := w.applyMergeability(context.Background(), github, pr, pull, time.Now().UTC(), eventID); err != nil {
		t.Fatalf("apply the read: %v", err)
	}
	applied := facts.facts()
	if len(applied) != 1 {
		t.Fatalf("the read applied %d facts %+v, want its one mergeability", len(applied), applied)
	}
	fact, ok := applied[0].(intake.PullRequestMergeability)
	if !ok {
		t.Fatalf("the read applied %T, want an intake.PullRequestMergeability", applied[0])
	}
	return fact, logs.String()
}

// A head GitHub reports conflicting is decided on the rules its base holds for its required
// checks (dispatch://INFRA-4374 R0), so the read that finds it conflicting reads them beside the
// pull request and its fact carries them: every `required_status_checks` rule of the base's
// rulesets, over every page, and the contexts of a classic protection that is enforced, each once
// and sorted, with Strict from a ruleset alone (a classic rule's answer carries none). GitHub's 403
// that refuses rulesets to a private repository on a plan without them, in its "GitHub Pro" and its
// "GitHub Team" wording with the closing period GitHub sends, is no failure: such a repository
// requires no check. Any other refusal of either read is rules the daemon could not read, and says
// why. A base whose name holds `#` and `/` reads its own rules, each segment escaped. A read that
// finds the head mergeable reads no rules at all.
func TestAConflictSecondReadCarriesTheBaseRules(t *testing.T) {
	pool := isolatedOutboxPool(t)
	const (
		rules    = "/repos/acme/widgets/rules/branches/main"
		branch   = "/repos/acme/widgets/branches/main"
		docs     = `"documentation_url":"https://docs.github.com/rest/repos/rules#get-rules-for-a-branch","status":"403"`
		open     = `{"name":"main","protected":false}`
		proPlan  = `{"message":"Upgrade to GitHub Pro or make this repository public to enable this feature.",` + docs + `}`
		teamPlan = `{"message":"Upgrade to GitHub Team or make this repository public to enable this feature.",` + docs + `}`
	)
	for i, tc := range []struct {
		name   string
		base   string
		routes map[string]rulesAnswer
		want   record.BaseRules
		// unread is what Unread must say, each part of it; none when the read answered.
		unread []string
		// asked is the escaped paths the stand-in must have been asked for, none when unchecked.
		asked []string
	}{
		{name: "a ruleset over two pages", base: "main", routes: map[string]rulesAnswer{
			rules: {body: `[{"type":"pull_request","parameters":{"required_approving_review_count":1}},` +
				`{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"lint"}],"strict_required_status_checks_policy":false}}]`,
				next: "/repositories/42/rules/branches/main?per_page=100&page=2"},
			"/repositories/42/rules/branches/main": {body: `[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"ci-ok"},{"context":"lint"}]}}]`},
			branch:                                 {body: open},
		}, want: record.BaseRules{Required: []string{"ci-ok", "lint"}}},
		{name: "a strict ruleset", base: "main", routes: map[string]rulesAnswer{
			rules:  {body: `[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"ci"}],"strict_required_status_checks_policy":true}}]`},
			branch: {body: open},
		}, want: record.BaseRules{Required: []string{"ci"}, Strict: true}},
		{name: "a classic rule with contexts and no strict", base: "main", routes: map[string]rulesAnswer{
			rules: {body: `[]`},
			branch: {body: `{"name":"main","protected":true,"protection":{"enabled":true,"required_status_checks":` +
				`{"enforcement_level":"non_admins","contexts":["build","test"],"checks":[{"context":"build","app_id":null},{"context":"deploy","app_id":15368}]}}}`},
		}, want: record.BaseRules{Required: []string{"build", "deploy", "test"}}},
		{name: "a classic rule enforced off requires nothing", base: "main", routes: map[string]rulesAnswer{
			rules: {body: `[]`},
			branch: {body: `{"name":"main","protected":true,"protection":{"enabled":true,"required_status_checks":` +
				`{"enforcement_level":"off","contexts":["build"],"checks":[{"context":"build","app_id":null}]}}}`},
		}, want: record.BaseRules{}},
		{name: "GitHub's refusal of rulesets to a user's plan", base: "main", routes: map[string]rulesAnswer{
			rules:  {status: http.StatusForbidden, body: proPlan},
			branch: {body: open},
		}, want: record.BaseRules{}},
		{name: "GitHub's refusal of rulesets to an organization's plan", base: "main", routes: map[string]rulesAnswer{
			rules:  {status: http.StatusForbidden, body: teamPlan},
			branch: {body: open},
		}, want: record.BaseRules{}},
		{name: "another 403", base: "main", routes: map[string]rulesAnswer{
			rules:  {status: http.StatusForbidden, body: `{"message":"Resource not accessible by integration",` + docs + `}`},
			branch: {body: open},
		}, unread: []string{"GET /rules/branches/main", "403", "Resource not accessible by integration"}},
		{name: "a 5xx of the branch's protection", base: "main", routes: map[string]rulesAnswer{
			rules:  {body: `[]`},
			branch: {status: http.StatusBadGateway, body: `{"message":"Server Error"}`},
		}, unread: []string{"GET /branches/main", "502"}},
		{name: "a base whose name holds # and /", base: "release/#7", routes: map[string]rulesAnswer{
			"/repos/acme/widgets/rules/branches/release/%237": {body: `[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"ci"}]}}]`},
			"/repos/acme/widgets/branches/release/%237":       {body: `{"name":"release/#7","protected":false}`},
		}, want: record.BaseRules{Required: []string{"ci"}},
			asked: []string{"/repos/acme/widgets/rules/branches/release/%237", "/repos/acme/widgets/branches/release/%237"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, seen := rulesStandIn(t, tc.routes)
			fact, logs := appliedRead(t, pool, server.URL, tc.base, false, fmt.Sprintf("rules-%d", i))
			got := fact.Rules
			if got == nil {
				t.Fatalf("the conflicting read's fact carries no rules, want %+v", tc.want)
			}
			t.Logf("%s: %+v", tc.name, *got)
			if !slices.Equal(got.Required, tc.want.Required) || got.Strict != tc.want.Strict {
				t.Fatalf("the conflicting read carried required %q, strict %t; want required %q, strict %t",
					got.Required, got.Strict, tc.want.Required, tc.want.Strict)
			}
			if len(tc.unread) == 0 && got.Unread != "" {
				t.Fatalf("the conflicting read carried unread %q, want rules it read", got.Unread)
			}
			for _, part := range tc.unread {
				if !strings.Contains(got.Unread, part) {
					t.Fatalf("the conflicting read carried unread %q, want it to name %q", got.Unread, part)
				}
			}
			for _, path := range tc.asked {
				if !slices.Contains(seen(), path) {
					t.Fatalf("GitHub was asked for %q, want %q among them", seen(), path)
				}
			}
			if lines := loggedLines(t, logs, rulesReadLine); len(lines) != 1 || lines[0]["base"] != tc.base || lines[0]["unread"] != got.Unread {
				t.Fatalf("the read logged %v, want one line of the rules it read of %s", lines, tc.base)
			}
		})
	}

	t.Run("a mergeable read reads no rules", func(t *testing.T) {
		server, seen := rulesStandIn(t, map[string]rulesAnswer{})
		fact, logs := appliedRead(t, pool, server.URL, "main", true, "rules-mergeable")
		if fact.Rules != nil || len(seen()) != 0 || len(loggedLines(t, logs, rulesReadLine)) != 0 {
			t.Fatalf("a mergeable read carried rules %+v and asked GitHub for %q, want neither", fact.Rules, seen())
		}
	})
}

// secondReadRunner is an outbox over w's database that runs a second read of a conflicting head
// (record.ConflictConfirm) as the daemon's outbox runs it: it reads GitHub, applies the answer and
// waits through w, as attach wires the outbox, applies its facts to w's engine, and logs to w's
// log.
func secondReadRunner(w *workflowRuntime) *outbox {
	return &outbox{pool: w.pool, records: record.NewStore(), handlers: w.handlers, dispatchProject: w.dispatchProject, log: w.log,
		readPullRequest: w.readPullRequest, applyMergeability: w.applyMergeability, mergeStateWait: w.mergeStateWait, now: time.Now}
}

// runSecondReads makes one attempt, with runner's clock at now, of each second read of CAPTURE-1
// due then, and answers how many it ran: the rows of the issue's other kinds stay where they are.
func runSecondReads(t *testing.T, runner *outbox, now time.Time) int {
	t.Helper()
	ctx := context.Background()
	runner.now = func() time.Time { return now }
	rows, err := runner.listDue(ctx, "CAPTURE-1", []record.OutboxKind{record.OutboxKindConflictConfirm}, now)
	if err != nil {
		t.Fatalf("list the due second reads: %v", err)
	}
	for _, row := range rows {
		if err := runner.runRow(ctx, row); err != nil {
			t.Fatalf("run the second read %d: %v", row.ID, err)
		}
	}
	return len(rows)
}

// secondReadRow is CAPTURE-1's second read waiting in the outbox: when it is due and how often it
// has run, and whether there is one.
func secondReadRow(t *testing.T, pool *store.Pool) (due time.Time, attempts int, found bool) {
	t.Helper()
	err := pool.QueryRow(context.Background(), `select next_at, attempts from outbox where issue = 'CAPTURE-1' and kind = $1`,
		string(record.OutboxKindConflictConfirm)).Scan(&due, &attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, 0, false
	}
	if err != nil {
		t.Fatalf("read the second read's row: %v", err)
	}
	return due, attempts, true
}

// secondReadRig is CAPTURE-1 in testing on its pull request 86 at head code, GitHub answering its
// nth read of the pull request as answer says and counting the reads, the base main requiring lint
// (green nowhere) and pr-checks-result, and the runner of its second read.
type secondReadRig struct {
	pool   *store.Pool
	w      *workflowRuntime
	runner *outbox
	logs   *syncBuffer
	reads  atomic.Int32
}

// pullAnswer is GitHub's answer to one read of pull request 86: a status, and a body.
type pullAnswer struct {
	status int
	body   string
}

func newSecondReadRig(t *testing.T, answer func(read int32) pullAnswer) *secondReadRig {
	t.Helper()
	rig := &secondReadRig{pool: isolatedOutboxPool(t), logs: &syncBuffer{}}
	seedCapture(t, rig.pool, phase.Testing, "testing")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/widgets/pulls/86":
			got := answer(rig.reads.Add(1))
			if got.status != 0 {
				w.WriteHeader(got.status)
			}
			_, _ = io.WriteString(w, got.body)
		case "/repos/acme/widgets/rules/branches/main":
			_, _ = io.WriteString(w, `[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"lint"},{"context":"pr-checks-result"}]}}]`)
		case "/repos/acme/widgets/branches/main":
			_, _ = io.WriteString(w, `{"name":"main","protected":false}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	rig.w = readRuntime(rig.pool, server.URL, slog.New(slog.NewJSONHandler(rig.logs, nil)))
	rig.w.mergeStateRetry = 10 * time.Millisecond
	rig.runner = secondReadRunner(rig.w)
	return rig
}

// firstRead applies a first read of the head that found it conflicting with main, read at at, as
// the workflow receives it from any reader of the pull request.
func (rig *secondReadRig) firstRead(t *testing.T, at time.Time) {
	t.Helper()
	applyFact(t, rig.pool, "github", "first-read", intake.PullRequestMergeability{Repo: "acme/widgets", Number: 86, Base: "main",
		HeadSHA: "code", Mergeable: record.MergeabilityConflicting, MergeState: record.MergeStateDirty, ReadAt: at,
		Rules: &record.BaseRules{Required: []string{"lint"}}}, rig.w.handlers...)
	if _, pr := capturePhase(t, rig.pool); pr.ConflictRead == nil || pr.ConflictRead.Head != "code" {
		t.Fatalf("the first read left the record holding first read %+v, want one of head code", pr.ConflictRead)
	}
}

// lines is every line of the rig's log whose message starts with msg.
func (rig *secondReadRig) lines(t *testing.T, msg string) []map[string]any {
	t.Helper()
	return slices.DeleteFunc(loggedLines(t, rig.logs.String(), msg), func(entry map[string]any) bool {
		text, _ := entry["msg"].(string)
		return !strings.HasPrefix(text, msg)
	})
}

// GitHub answers for pull request 86 at head code: `mergeable` null (unknown), false (conflicting)
// or true, with the merge state that goes with each.
var (
	unknownPull     = pullAnswer{body: `{"number":86,"state":"open","base":{"ref":"main"},"head":{"ref":"legion/CAPTURE-1","sha":"code"},"mergeable":null,"mergeable_state":"unknown"}`}
	conflictingPull = pullAnswer{body: `{"number":86,"state":"open","base":{"ref":"main"},"head":{"ref":"legion/CAPTURE-1","sha":"code"},"mergeable":false,"mergeable_state":"dirty"}`}
	unavailablePull = pullAnswer{status: http.StatusBadGateway, body: `{"message":"Server Error"}`}
)

// One CONFLICTING read acts on nothing (dispatch://INFRA-4374 R2): it queues a second read of the
// same head a minute on (record.ConflictConfirm), and that row reads GitHub again. It reads once
// when GitHub answers, and its answer decides: CONFLICTING again confirms the conflict, which R0
// decides on with the base's rules that read carried. GitHub's UNKNOWN is read again, the runtime's
// wait later, five attempts in all, and so is a read that failed; after the last, the row releases
// the first read, so the next conflicting read starts the two reads again. A row whose head the
// record has left, or whose first read a later read already released, reads nothing.
func TestAConflictSecondReadRowReadsTheHeadAgain(t *testing.T) {
	// The record keeps the times to the microsecond.
	first := time.Now().UTC().Truncate(time.Microsecond)

	t.Run("a first read queues it a minute on, and it reads once", func(t *testing.T) {
		rig := newSecondReadRig(t, func(int32) pullAnswer { return conflictingPull })
		rig.firstRead(t, first)
		if due, _, found := secondReadRow(t, rig.pool); !found || !due.Equal(first.Add(time.Minute)) {
			t.Fatalf("the second read is due at %s (queued %t), want a minute after the first read, %s", due, found, first.Add(time.Minute))
		}
		if ran := runSecondReads(t, rig.runner, first.Add(30*time.Second)); ran != 0 {
			t.Fatalf("%d second reads ran half a minute after the first read, want none", ran)
		}
		if ran := runSecondReads(t, rig.runner, first.Add(time.Minute)); ran != 1 {
			t.Fatalf("%d second reads ran a minute after the first read, want one", ran)
		}
		if got := rig.reads.Load(); got != 1 {
			t.Fatalf("GitHub was read %d times, want once: its first answer decided", got)
		}
		if _, _, found := secondReadRow(t, rig.pool); found {
			t.Fatalf("the second read's row is still queued, want it finished on GitHub's answer")
		}
		_, pr := capturePhase(t, rig.pool)
		if pr.ConflictingHead != "code" || pr.ConflictRead != nil {
			t.Fatalf("the record holds %q conflicting with first read %+v, want the conflict of code confirmed", pr.ConflictingHead, pr.ConflictRead)
		}
		if asked := pr.MergeForward; asked == nil || asked.Head != "code" || asked.Why != classify.ConflictCheckNotGreen || asked.Phase != phase.Testing {
			t.Fatalf("the record holds merge forward %+v, want one asked of code in testing for %s: lint is required and green nowhere",
				asked, classify.ConflictCheckNotGreen)
		}
		if lines := rig.lines(t, secondReadLine); len(lines) != 1 || lines[0]["msg"] != secondReadAnswered || lines[0]["mergeable"] != string(record.MergeabilityConflicting) {
			t.Fatalf("the row logged %v, want one line of GitHub's CONFLICTING", lines)
		}
	})

	for _, tc := range []struct {
		name   string
		answer pullAnswer
		unread bool
	}{
		{name: "an unknown answer is read again, five attempts in all, and then the first read is released", answer: unknownPull},
		{name: "a failed read is read again, five attempts in all, and then the first read is released as unread", answer: unavailablePull, unread: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newSecondReadRig(t, func(int32) pullAnswer { return tc.answer })
			rig.firstRead(t, first)
			at := first.Add(time.Minute)
			for attempt := 1; attempt <= mergeStateLadderReads; attempt++ {
				if ran := runSecondReads(t, rig.runner, at); ran != 1 {
					t.Fatalf("attempt %d ran %d second reads, want one", attempt, ran)
				}
				if got := rig.reads.Load(); got != int32(attempt) {
					t.Fatalf("GitHub was read %d times after attempt %d, want one read an attempt", got, attempt)
				}
				due, attempts, found := secondReadRow(t, rig.pool)
				_, pr := capturePhase(t, rig.pool)
				if attempt < mergeStateLadderReads {
					if !found || attempts != attempt || !due.Equal(at.Add(rig.w.mergeStateWait())) || pr.ConflictRead == nil {
						t.Fatalf("after attempt %d the row is queued %t, due %s after %d attempts, and the first read is %+v; want it due the runtime's wait on, %s, the first read held",
							attempt, found, due, attempts, pr.ConflictRead, at.Add(rig.w.mergeStateWait()))
					}
					at = at.Add(time.Minute)
					continue
				}
				if found || pr.ConflictRead != nil || pr.ConflictingHead != "" || pr.MergeForward != nil {
					t.Fatalf("after the last attempt the row is queued %t, the first read is %+v, %q is held conflicting and the merge forward is %+v; want the first read released and nothing acted on",
						found, pr.ConflictRead, pr.ConflictingHead, pr.MergeForward)
				}
			}
			if lines := rig.lines(t, secondReadLine); len(lines) != mergeStateLadderReads {
				t.Fatalf("the row logged %d attempts, want %d", len(lines), mergeStateLadderReads)
			}
			released := rig.lines(t, secondReadReleased)
			if len(released) != 1 || released[0]["head"] != "code" || released[0]["unread"] != tc.unread {
				t.Fatalf("the workflow logged %v, want one release of the first read of code, unread %t", released, tc.unread)
			}
		})
	}

	for _, tc := range []struct {
		name string
		// leave takes the record off the first read before its second read runs.
		leave func(t *testing.T, rig *secondReadRig)
		why   string
	}{
		{name: "a head the record has left is not read", why: "head moved", leave: func(t *testing.T, rig *secondReadRig) {
			recordCapture(t, rig.pool, func(pr *record.PullRequest) { pr.HeadSHA = "moved" })
		}},
		{name: "a first read a mergeable read released is not read", why: "a later read confirmed or released", leave: func(t *testing.T, rig *secondReadRig) {
			applyFact(t, rig.pool, "github", "mergeable-read", intake.PullRequestMergeability{Repo: "acme/widgets", Number: 86, Base: "main",
				HeadSHA: "code", Mergeable: record.MergeabilityMergeable, MergeState: record.MergeStateClean, ReadAt: first.Add(30 * time.Second)}, rig.w.handlers...)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newSecondReadRig(t, func(int32) pullAnswer { return conflictingPull })
			rig.firstRead(t, first)
			tc.leave(t, rig)
			if ran := runSecondReads(t, rig.runner, first.Add(time.Minute)); ran != 1 {
				t.Fatalf("%d second reads ran, want one", ran)
			}
			if got := rig.reads.Load(); got != 0 {
				t.Fatalf("GitHub was read %d times, want none", got)
			}
			if _, _, found := secondReadRow(t, rig.pool); found {
				t.Fatalf("the second read's row is still queued, want it finished")
			}
			skipped := rig.lines(t, secondReadSkipped)
			if len(skipped) != 1 || !strings.Contains(fmt.Sprint(skipped[0]["why"]), tc.why) {
				t.Fatalf("the row logged %v, want one line naming %q", skipped, tc.why)
			}
			if lines := rig.lines(t, secondReadLine); len(lines) != 0 {
				t.Fatalf("the row logged reads %v, want none", lines)
			}
		})
	}
}

// recordCapture writes CAPTURE-1's pull request record as change leaves it, as no fact would: the
// state a test reads a row against.
func recordCapture(t *testing.T, pool *store.Pool, change func(*record.PullRequest)) {
	t.Helper()
	ctx := context.Background()
	records := record.NewStore()
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		pr, err := records.PullRequest(ctx, tx, "CAPTURE-1")
		if err != nil {
			return err
		}
		if pr == nil {
			return fmt.Errorf("the record holds no pull request for CAPTURE-1")
		}
		change(pr)
		return records.PutPullRequest(ctx, tx, *pr)
	}); err != nil {
		t.Fatalf("record CAPTURE-1's pull request: %v", err)
	}
}
