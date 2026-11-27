package api

import (
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// a-c-real-state: GET /api/v1/issues/{key}/legion against a real daemon's state. The test reads a
// capture of one daemon's GET /legion/v1/state (curl -sS "$LEGION_DAEMON_URL/legion/v1/state" >
// "$TMPDIR/state.json"), which LEGION_STATE_CAPTURE names, serves its bytes unchanged as the
// project's daemon, lets the base's own whole read record it, and holds the route's answer for
// every issue of the file to the file's own record of that issue. CI has no capture, so the test
// skips there (packages/envoy/scripts/database-skips.sh names it in EXPECTED_SKIPS); it has no
// build tag, so the race build still compiles it.

// capturedState is the part of a daemon's state the comparison reads, decoded from the capture in
// the daemon's own wire shape (packages/daemon/internal/api/state.go's State, Issue, ClaimView,
// SlotView, WaitingLineView and WaitingOnView; packages/contracts/fixtures/daemon-api/state.json),
// never through Dispatch's decode of it (legion.State), which is what the comparison checks.
type capturedState struct {
	Admission *struct {
		Blocked []struct {
			Issue     string   `json:"issue"`
			BlockedBy []string `json:"blockedBy"`
		} `json:"blocked"`
	} `json:"admission"`
	Issues map[string]capturedIssue `json:"issues"`
}

// capturedClaim is a role's claim, in the one field compared: where it is in its life.
type capturedClaim struct {
	State string `json:"state"`
}

// capturedIssue is one issue of the capture in the fields compared.
type capturedIssue struct {
	Phase     string         `json:"phase"`
	Architect *capturedClaim `json:"architect"`
	Workers   map[string]struct {
		Claim capturedClaim `json:"claim"`
	} `json:"workers"`
	Slot *struct {
		Index int `json:"index"`
	} `json:"slot"`
	WaitingLine *struct {
		Place  int `json:"place"`
		Length int `json:"length"`
	} `json:"waitingLine"`
	WaitingOn json.RawMessage `json:"waitingOn"`
}

// captureDisagreements reports each way the route disagrees with the capture, naming the issue. A
// defect that makes every issue disagree would print some thousands of lines, so the first
// captureDisagreementsShown are printed and the rest counted.
type captureDisagreements struct {
	t     *testing.T
	count int
}

// captureDisagreementsShown is how many disagreements are printed in full.
const captureDisagreementsShown = 50

func (d *captureDisagreements) add(format string, args ...any) {
	d.t.Helper()
	d.count++
	if d.count <= captureDisagreementsShown {
		d.t.Errorf(format, args...)
	}
}

// done reports how many disagreements were not printed.
func (d *captureDisagreements) done() {
	d.t.Helper()
	if d.count > captureDisagreementsShown {
		d.t.Errorf("... and %d more disagreements with the capture, %d in all", d.count-captureDisagreementsShown, d.count)
	}
}

// capturedProject is the one project every key of the capture names, the prefix before its first
// `-`: a daemon keeps the records of its own project only, so a capture of two is no one daemon's
// state.
func capturedProject(t *testing.T, keys []string) string {
	t.Helper()
	projects := map[string]int{}
	for _, key := range keys {
		project, _, found := strings.Cut(key, "-")
		if !found {
			t.Fatalf("the capture holds the key %q, which names no project", key)
		}
		projects[project]++
	}
	if len(projects) != 1 {
		t.Fatalf("the capture's keys name the projects %v, want the one project of one daemon's state", projects)
	}
	return slices.Collect(maps.Keys(projects))[0]
}

// capturedJSON is raw decoded, or nil for a member the daemon left out or sent null.
func capturedJSON(raw json.RawMessage) (any, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var value any
	err := json.Unmarshal(raw, &value)
	return value, err
}

// compareCapturedIssue holds key's answer to the capture's record of it, C-1 to C-6. C-6 compares
// the answer's waiting_on with the capture's waitingOn through the seven members the answer names
// (answerWaitOf): the capture's gateVersion with the answer's gate_version, and so on, and no
// member of another name.
func compareCapturedIssue(report *captureDisagreements, key string, issue capturedIssue, blockedBy []string, answer wireLegionIssue) {
	report.t.Helper()
	if !answer.Configured {
		report.add("C-1 %s: the answer is not configured, and its project names the capture's server as its daemon", key)
	}
	state := answer.State
	if state == nil {
		report.add("C-1 %s: the answer's state is null, want the capture's phase %q", key, issue.Phase)
		return
	}
	if state.Phase != issue.Phase {
		report.add("C-1 %s: the phase is %q, want the capture's %q", key, state.Phase, issue.Phase)
	}
	// A claim whose state is empty is no claim: the daemon writes PhaseView.Claim's zero value for
	// a role that has a phase row and no claim row.
	want := []string{}
	if issue.Architect != nil && issue.Architect.State != "" {
		want = append(want, "architect="+issue.Architect.State)
	}
	for role, worker := range issue.Workers {
		if worker.Claim.State != "" {
			want = append(want, role+"="+worker.Claim.State)
		}
	}
	got := []string{}
	for _, role := range state.Roles {
		got = append(got, role.Role+"="+role.State)
	}
	slices.Sort(want)
	slices.Sort(got)
	if !slices.Equal(got, want) {
		report.add("C-2 %s: the roles are %v, want the capture's %v", key, got, want)
	}
	switch {
	case issue.Slot == nil && state.Slot != nil:
		report.add("C-3 %s: the answer holds slot %d, and the capture's issue holds none", key, *state.Slot)
	case issue.Slot != nil && !sameLegionValue(state.Slot, &issue.Slot.Index):
		report.add("C-3 %s: the slot is %s, want the capture's %d", key, showLegionValue(state.Slot), issue.Slot.Index)
	}
	var line *wireLegionWaitingLine
	if issue.WaitingLine != nil {
		line = &wireLegionWaitingLine{Place: issue.WaitingLine.Place, Length: issue.WaitingLine.Length}
	}
	if !sameLegionValue(state.WaitingLine, line) {
		report.add("C-4 %s: the waiting line is %s, want the capture's %s", key, showLegionValue(state.WaitingLine), showLegionValue(line))
	}
	if !slices.Equal(state.BlockedBy, blockedBy) {
		report.add("C-5 %s: blocked_by is %v, want the capture's admission.blocked entry %v", key, state.BlockedBy, blockedBy)
	}
	wantWait, err := capturedJSON(issue.WaitingOn)
	if err != nil {
		report.add("C-6 %s: decode the capture's waitingOn %s: %v", key, issue.WaitingOn, err)
		return
	}
	if daemon, isObject := wantWait.(map[string]any); isObject {
		wantWait = answerWaitOf(daemon)
	}
	gotWait, err := capturedJSON(state.WaitingOn)
	if err != nil {
		report.add("C-6 %s: decode the answer's waiting_on %s: %v", key, state.WaitingOn, err)
		return
	}
	if !reflect.DeepEqual(gotWait, wantWait) {
		report.add("C-6 %s: waiting_on decodes to %v, want the capture's waitingOn %s under the answer's names, %v", key, gotWait, issue.WaitingOn, wantWait)
	}
}

// TestTheLegionReadMatchesACapturedDaemonState is a-c-real-state. It seeds the capture's project
// and one Dispatch issue for every captured key, names a server of the file's bytes as the
// project's daemon through the settings API, and reads it whole, at the URL that API stored
// (legion.GetSettings), through the Legion intake's own whole read (legion.ReadWhole), which
// records the rows the route reads as production records them. The capture must carry
// `daemon.stateSeq`, `daemon.stateId` and `settings.dispatch_url`: a whole read of a state without
// one of them is a failed read (ErrNoStateSeq, ErrNoStateID, ErrNoDispatchURL), and the test stops
// there. Then for every captured issue it compares the route's answer with the capture (C-1 to
// C-6), and each card of one board answer with that issue's answer (C-7). It prints how many
// issues it compared, which is how many the file holds, the file's size, how many issues carry a
// waitingOn, and the median and largest time of one GET.
func TestTheLegionReadMatchesACapturedDaemonState(t *testing.T) {
	t.Parallel()
	path := os.Getenv("LEGION_STATE_CAPTURE")
	if path == "" {
		t.Skip(`LEGION_STATE_CAPTURE is unset: it names a capture of a daemon's GET /legion/v1/state ` +
			`(curl -sS "$LEGION_DAEMON_URL/legion/v1/state" > "$TMPDIR/state.json"), which CI does not have`)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the capture LEGION_STATE_CAPTURE names: %v", err)
	}
	var captured capturedState
	if err := json.Unmarshal(raw, &captured); err != nil {
		t.Fatalf("the capture %s is not a daemon's state document: %v", path, err)
	}
	keys := slices.Sorted(maps.Keys(captured.Issues))
	if len(keys) == 0 {
		t.Fatalf("the capture %s holds no issue, so nothing would be compared", path)
	}
	project := capturedProject(t, keys)

	handler, database := newTestHandlerWithStore(t)
	seedLegionProjects(t, handler, project)
	// Every captured key as a Dispatch issue, in one statement, with the columns
	// seedLegionIssue writes.
	legionExec(t, database, `
		insert into issues (key, project_key, number, title, status, rank, priority, parent_key, created_by)
		select captured.key, $2, split_part(captured.key, '-', 2)::int, 'The captured issue ' || captured.key,
			'in_progress', lpad(split_part(captured.key, '-', 2), 12, '0'), null, null,
			'{"kind":"user","id":"alice"}'::jsonb
		from unnest($1::text[]) as captured(key)
	`, keys, project)
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/legion/v1/state" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(raw)
	}))
	t.Cleanup(daemon.Close)
	nameLegionDaemon(t, handler, project, daemon.URL)
	readLegionWhole(t, database, project)
	var ok bool
	var failure *string
	if err := database.Pool.QueryRow(t.Context(), `
		select ok, error from legion_reads where project = $1 order by read_at desc, id desc limit 1
	`, project).Scan(&ok, &failure); err != nil {
		t.Fatalf("read the whole read's record: %v", err)
	}
	if !ok {
		t.Fatalf("the whole read of the capture failed: %s", deref(failure))
	}

	blockers := map[string][]string{}
	if captured.Admission != nil {
		for _, blocked := range captured.Admission.Blocked {
			blockers[blocked.Issue] = blocked.BlockedBy
		}
	}
	report := &captureDisagreements{t: t}
	answers := make(map[string]wireLegionIssue, len(keys))
	took := make([]time.Duration, 0, len(keys))
	waiting := 0
	for _, key := range keys {
		began := time.Now()
		response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+key+"/legion", nil, "alice")
		took = append(took, time.Since(began))
		if response.Code != http.StatusOK {
			t.Fatalf("GET /api/v1/issues/%s/legion: status=%d body=%s, want 200", key, response.Code, response.Body.String())
		}
		answer := decodeLegionIssue(t, response.Body.Bytes())
		requireLegionNames(t, key, answer)
		answers[key] = answer
		issue := captured.Issues[key]
		if value, err := capturedJSON(issue.WaitingOn); err == nil && value != nil {
			waiting++
		}
		blockedBy := blockers[key]
		if blockedBy == nil {
			blockedBy = []string{}
		}
		compareCapturedIssue(report, key, issue, blockedBy, answer)
	}
	// C-7: one board answer, each card of which agrees with its issue's answer.
	cards := 0
	for _, column := range legionBoardOf(t, handler, project).Columns {
		for _, card := range column.Cards {
			cards++
			answer, held := answers[card.Key]
			if !held {
				report.add("C-7 the board carries a card for %s, which the capture does not hold", card.Key)
				continue
			}
			for _, disagreement := range legionCardDisagreements(card, answer) {
				report.add("C-7 %s", disagreement)
			}
		}
	}
	report.done()

	slices.Sort(took)
	median := took[len(took)/2]
	if len(took)%2 == 0 {
		median = (took[len(took)/2-1] + took[len(took)/2]) / 2
	}
	t.Logf("compared %d issues of the %d the capture holds, and %d cards of one board answer", len(answers), len(captured.Issues), cards)
	t.Logf("the capture is %d bytes; %d of its issues carry a waitingOn", len(raw), waiting)
	t.Logf("one GET /api/v1/issues/{key}/legion took %s at the median and %s at the most", median, took[len(took)-1])
}
