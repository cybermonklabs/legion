//go:build e2e

// The live proof's driver for the rating of each plan: each retro of the whole daemon rates the
// plan its issue ran, the daemon writes the plan and the rating onto the issue in Dispatch, and a
// person lists the finished issues of the project with the real `dispatch plans` command.
// scripts/e2e/plan-ratings.sh builds this driver with `-tags e2e` and owns the nats-server it runs
// against; it starts no database, and the PostgreSQL is the server LEGION_TEST_PG_DSN names.
//
// Nothing of the record path is stood in for. Dispatch is the real server this checkout builds
// (startHandoffDispatch), on a database of its own on LEGION_TEST_PG_DSN's server, with NATS off.
// The daemon is the whole daemon (run, through startDaemonLogging) with dispatch_url at that
// server, so each plan and each rating it writes goes through its outbox and its Dispatch client to
// the real routes. The issues walk as the package's whole-daemon tests walk them (stepRecordsRig,
// step_records_test.go, and walkThreeRated, plan_ratings_test.go). The reader is the real
// `dispatch` CLI under bun, run as a person of the project with the personal token Dispatch
// minted for that person (stepRecordsLive.cli, step_records_live_test.go).
//
// What is stood in for lies outside the records, as for the step records' driver: GitHub's REST
// API, the Apps' tokens, Envoy's listener, and the panes, which the package's fake runtime
// launches and the driver plays.
package daemon

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/execfile"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// plansAnswerEnv names the file a run keeps the answer of the person's plans read in, for a run
// that later serves a recorded answer of Dispatch's plans route in its place. With it set,
// readByProject reads `dispatch plans --project <PROJECT>` through plansKept, writes the body
// Dispatch answered that read with to the file once every check of the step has held, and prints
// the body in the driver's output (plansAnswerBlock), which a reader of the run's log decodes.
// Unset, the read is plans', and nothing is written or printed.
const plansAnswerEnv = "PLAN_RATINGS_PLANS_ANSWER"

// The lines that print the kept answer, each a whole line of the driver's output with no prefix:
// plansAnswerBegin, the body in standard base64 with padding in lines of plansAnswerWidth
// characters (the last one shorter or equal), plansAnswerEnd, then plansAnswerSum followed by the
// body's sha256 in lower-case hex.
const (
	plansAnswerBegin = "plans answer begin"
	plansAnswerEnd   = "plans answer end"
	plansAnswerSum   = "plans answer sha256 "
	plansAnswerWidth = 76
)

// planRatingsLive is one run of the proof: the walk's rig over four issues of one project, and the
// person's CLI.
type planRatingsLive struct {
	*stepRecordsLive
}

// TestPlanRatingsLive drives the proof's steps against one booted daemon and one Dispatch, over
// four issues of one project: three walk to done and rate their plans, and the fourth walks to
// its retro, whose completion is refused until it rates the plan. The fourth goes no further than
// merging, so the read by project lists the three alone.
func TestPlanRatingsLive(t *testing.T) {
	// Not parallel: it gives the process back the operator's own environment, as t.Setenv does (fixtureenv.Live, in newStepRecordsLive).
	l := &planRatingsLive{newStepRecordsLive(t, "scripts/e2e/plan-ratings.sh", 4, "plans-reader")}
	keys := make([]string, 0, len(l.issues))
	for _, issue := range l.issues {
		keys = append(keys, issue.key)
	}
	l.note("Dispatch %s (database of its own, NATS off), project %s with default repository %s, issues %s; daemon at dispatch_url %s; the reader is the person's personal token",
		l.dispatch.base, l.cfg.Project, liveRepo, strings.Join(keys, ", "), l.cfg.DispatchURL)
	l.step("three-finished-issues-each-rated", l.readByProject)
	l.step("a-retro-with-no-rating-is-refused", l.ratingRequired)
}

// readByProject is the read of a project's finished issues by a person: the rig's first
// three issues walk to done, the first with one failed review, and their retros rate the plan 3, 4
// and 5 with a reason. `dispatch plans --project <PROJECT>` run as a person of the project then
// prints the three, newest close first, each with the default plan, its one rating with its
// reason, its time and its count of failed steps, 1, 0 and 0. The read after the merge,
// scripts/e2e/plan-ratings-read.sh, judges the same command's output for the first issue and
// passes, naming its rating and reason; its control names the fourth issue, which is not
// finished, and the script must fail on it. The control of the read deletes the failed review's
// record from Dispatch's own store, its node and every tuple that names it, and the same parse of
// the same command must then report the first issue's failed steps 0 where 1 is wanted. With
// plansAnswerEnv set, the first `dispatch plans` goes through plansKept, and the body Dispatch
// answered it with is kept (keepPlansAnswer) once the step's every check, the controls among them,
// has held.
func (l *planRatingsLive) readByProject() {
	t := l.t
	want := l.walkThreeRated()
	l.note("%s, %s and %s walked to done, the first with one failed review; their retros rated the plan %d, %d and %d",
		want[0].key, want[1].key, want[2].key, want[0].rating.Rating, want[1].rating.Rating, want[2].rating.Rating)
	// The ratings and the closes ride the outbox, so the read by project holds them a moment after
	// the walks end: GET /api/v1/projects/{key}/plans is waited on for the whole of what it must
	// list before the CLI reads it once.
	l.waitPlans("the three finished issues, each rated", want)

	keep := os.Getenv(plansAnswerEnv)
	var answer []byte
	var out string
	var code int
	if keep == "" {
		out, code = l.plans()
	} else {
		out, code, answer = l.plansKept()
	}
	if code != 0 {
		t.Fatalf("`dispatch plans --project %s` exited %d: %s", l.cfg.Project, code, out)
	}
	lines, err := plansPrintedHold(l.cfg.Project, out, want)
	if err != nil {
		t.Fatalf("`dispatch plans --project %s` printed\n%s\nand the read does not hold the three: %v", l.cfg.Project, out, err)
	}
	l.note("`dispatch plans --project %s` as the person, exit 0:", l.cfg.Project)
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		l.note("  %s", line)
	}

	out, code = l.readScript(want[0].key)
	if code != 0 || !readScriptPassed(out, lines[0]) {
		t.Fatalf("scripts/e2e/plan-ratings-read.sh %s %s exited %d:\n%s\nwant exit 0 and a PASS line naming the plan and the ratings of %q", l.cfg.Project, want[0].key, code, out, lines[0].text)
	}
	l.note("scripts/e2e/plan-ratings-read.sh %s %s, exit 0: %s", l.cfg.Project, want[0].key, lastLine(out))
	unfinished := l.issues[3].key
	out, code = l.readScript(unfinished)
	if failed := fmt.Sprintf("FAIL: the read lists 3 finished issues of %s and no line of %s", l.cfg.Project, unfinished); code != 1 || !strings.Contains(out, failed) {
		t.Fatalf("control: scripts/e2e/plan-ratings-read.sh %s %s exited %d:\n%s\nwant exit 1 and %q", l.cfg.Project, unfinished, code, out, failed)
	}
	l.note("control: scripts/e2e/plan-ratings-read.sh %s %s, which is not finished, exit 1: %s", l.cfg.Project, unfinished, lastLine(out))

	// stepsHold passed over want[0].steps, and it refuses a read that holds no record of step 4
	// attempt 1 (stepRecordsTen), so this find answers that record's id.
	deleted := want[0].steps.find("step", 4, 1).ID
	l.deleteNode(deleted)
	l.note("control: deleted %s's failed review's record %s from Dispatch's database (its workflow_nodes row and its workflow_tuples rows)", want[0].key, deleted)
	out, code = l.plans()
	if code != 0 {
		t.Fatalf("control: `dispatch plans --project %s` after the delete exited %d: %s", l.cfg.Project, code, out)
	}
	_, err = plansPrintedHold(l.cfg.Project, out, want)
	if err == nil {
		t.Fatalf("control: the parse of `dispatch plans --project %s` passed with the failed review's record deleted:\n%s", l.cfg.Project, out)
	}
	if wanted := failedStepsPrinted(want[0].key, 0, 1); !strings.Contains(err.Error(), wanted) {
		t.Fatalf("control: the parse with the failed review's record deleted reports %q, want it to say %q", err, wanted)
	}
	l.note("control: the same parse now fails: %v", err)
	if keep != "" {
		l.keepPlansAnswer(keep, answer)
	}
}

// ratingRequired is the rule of the rating for a plugin at the new contract: the rig's fourth issue
// walks to its retro, and the implementer's completion of the retro with no rating, from a claim
// at api.PlanRatingContract or later, is refused 400 PLAN_RATING_REQUIRED, and the issue stays in
// retro. The control sends the same completion with planRating 4 and a reason, which moves the
// issue to merging, so the refusal is the rating's and no refusal of every retro.
func (l *planRatingsLive) ratingRequired() {
	t := l.t
	l.on(l.issues[3].key)
	l.walkToRetro()
	l.note("planning, implementing, testing, an approving review and the architect's sign-off walked %s to its retro", l.key)

	c := l.worker(claim.RoleImplementer)
	c.take()
	body := l.unratedRetroRefused(c)
	if got := string(l.d.state().Issues[l.key].Phase); got != string(phase.Retro) {
		t.Fatalf("after the refused completion %s is in %s, want retro", l.key, got)
	}
	l.note("the retro's completion with no rating, from the implementer at plugin contract %d: %d %s; %s still in retro",
		l.d.claim(c.token).PluginContract, http.StatusBadRequest, strings.TrimSpace(body), l.key)

	l.finish(c, "", l.retroCompletion(&record.PlanRating{Rating: 4, Reason: "The plan fit the work once its retro was asked for a rating."}), phase.Merging)
	l.note("control: the same completion with planRating 4 and a reason: 200; %s in merging", l.key)
}

// plans runs `dispatch plans --project <PROJECT>` as the person and returns both streams and its
// exit code.
func (l *planRatingsLive) plans() (string, int) {
	l.t.Helper()
	return l.cli("plans", "--project", l.cfg.Project)
}

// plansKept runs `dispatch plans --project <PROJECT>` as the person, as plans does, with the CLI's
// Dispatch a proxy of the test's own: it passes each request to the run's Dispatch as it came and
// each answer back as it went, and keeps the answer Dispatch gave the CLI's
// GET /api/v1/projects/<PROJECT>/plans. That one request goes to Dispatch with Accept-Encoding
// identity, so Dispatch answers it with the plain body, and the CLI is sent the bytes that are kept:
// Go's transport neither asks for a compression nor decodes one for a request that sets the header
// itself. It returns both streams, the exit code and that body. It fails the test unless the CLI
// made that read once, and Dispatch answered it 200 with a body and no Content-Encoding but
// identity.
func (l *planRatingsLive) plansKept() (string, int, []byte) {
	t := l.t
	t.Helper()
	target, err := url.Parse(l.dispatch.base)
	if err != nil {
		t.Fatalf("parse Dispatch's address %s: %v", l.dispatch.base, err)
	}
	route := "/api/v1/projects/" + l.cfg.Project + "/plans"
	var mu sync.Mutex
	var reads []keptPlansRead
	proxy := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(target)
			if r.In.Method == http.MethodGet && r.In.URL.Path == route {
				r.Out.Header.Set("Accept-Encoding", "identity")
			}
		},
		ModifyResponse: func(response *http.Response) error {
			if response.Request.Method != http.MethodGet || response.Request.URL.Path != route {
				return nil
			}
			body, err := io.ReadAll(response.Body)
			if closed := response.Body.Close(); err == nil {
				err = closed
			}
			read := keptPlansRead{status: response.StatusCode, body: body, err: plainCoding(response.Header)}
			if err != nil {
				read.err = fmt.Errorf("read the answer: %w", err)
			}
			response.Body = io.NopCloser(bytes.NewReader(body))
			mu.Lock()
			defer mu.Unlock()
			reads = append(reads, read)
			return err
		},
	}
	server := httptest.NewServer(proxy)
	shell := l.personsShell()
	shell.base = server.URL
	output, code := shell.bun(nil, dispatchCLI(l.root), "plans", "--project", l.cfg.Project)
	server.Close()
	out := string(output.stdout) + string(output.stderr)
	mu.Lock()
	defer mu.Unlock()
	if len(reads) != 1 {
		t.Fatalf("`dispatch plans --project %s` made %d reads of GET %s, want 1 (exit %d): %s", l.cfg.Project, len(reads), route, code, out)
	}
	read := reads[0]
	switch {
	case read.err != nil:
		t.Fatalf("the answer of GET %s to `dispatch plans --project %s`: %v", route, l.cfg.Project, read.err)
	case read.status != http.StatusOK:
		t.Fatalf("GET %s answered `dispatch plans --project %s` %d, want 200: %s", route, l.cfg.Project, read.status, read.body)
	case len(read.body) == 0:
		t.Fatalf("GET %s answered `dispatch plans --project %s` 200 with no body", route, l.cfg.Project)
	}
	return out, code, read.body
}

// keptPlansRead is one answer of GET /api/v1/projects/<PROJECT>/plans as plansKept's proxy passed
// it to the CLI: its status, its body as Dispatch sent it, and the error that the reading met, or
// that plainCoding found in its headers.
type keptPlansRead struct {
	status int
	body   []byte
	err    error
}

// plainCoding is nil for an answer whose headers name no Content-Encoding but identity, and
// otherwise an error naming the Content-Encoding the answer carries.
func plainCoding(header http.Header) error {
	codings := header.Values("Content-Encoding")
	for _, coding := range codings {
		if !strings.EqualFold(strings.TrimSpace(coding), "identity") {
			return fmt.Errorf("the answer carries Content-Encoding %q to a request that sent Accept-Encoding identity", strings.Join(codings, ", "))
		}
	}
	return nil
}

// keepPlansAnswer writes answer, the body of the person's plans read, to file, which plansAnswerEnv
// names, and prints plansAnswerBlock of it in one write.
func (l *planRatingsLive) keepPlansAnswer(file string, answer []byte) {
	t := l.t
	t.Helper()
	if err := os.WriteFile(file, answer, 0o644); err != nil {
		t.Fatalf("write the answer of the person's plans read to %s, which %s names: %v", file, plansAnswerEnv, err)
	}
	fmt.Print(plansAnswerBlock(answer))
	l.note("kept the answer of the person's plans read, %d bytes, in %s, which %s names, and printed it above", len(answer), file, plansAnswerEnv)
}

// plansAnswerBlock is the lines that print answer, each ended by a newline: plansAnswerBegin,
// answer in standard base64 with padding, plansAnswerWidth characters a line, plansAnswerEnd, and
// plansAnswerSum followed by answer's sha256 in lower-case hex.
func plansAnswerBlock(answer []byte) string {
	encoded := base64.StdEncoding.EncodeToString(answer)
	sum := sha256.Sum256(answer)
	var block strings.Builder
	block.WriteString(plansAnswerBegin + "\n")
	for len(encoded) > 0 {
		n := min(len(encoded), plansAnswerWidth)
		block.WriteString(encoded[:n] + "\n")
		encoded = encoded[n:]
	}
	block.WriteString(plansAnswerEnd + "\n")
	block.WriteString(plansAnswerSum + hex.EncodeToString(sum[:]) + "\n")
	return block.String()
}

// readScript runs scripts/e2e/plan-ratings-read.sh <PROJECT> issue in the person's shell, as the
// operator runs it after the merge, and returns both streams and its exit code. The `dispatch` on
// its PATH runs this checkout's CLI under bun, as the installed command does.
func (l *planRatingsLive) readScript(issue string) (string, int) {
	t := l.t
	t.Helper()
	cli := dispatchCLI(l.root)
	if strings.Contains(cli, "'") {
		t.Fatalf("the checkout's path %s holds a quotation mark the dispatch wrapper cannot quote", l.root)
	}
	bin := filepath.Join(l.cliHome, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := execfile.Write(filepath.Join(bin, "dispatch"), []byte("#!/bin/sh\nexec bun '"+cli+"' \"$@\"\n"), 0o700); err != nil {
		t.Fatalf("write the dispatch command of the person's shell: %v", err)
	}
	out, code := l.personsShell().run([]string{"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH")},
		"bash", filepath.Join(l.root, "scripts", "e2e", "plan-ratings-read.sh"), l.cfg.Project, issue)
	return string(out.stdout) + string(out.stderr), code
}

// lastLine is the last line of out.
func lastLine(out string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	return lines[len(lines)-1]
}
