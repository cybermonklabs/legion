package daemon

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/texts"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// goldenRendering is the values of one of the two renderings of every kind. Each value of one is
// drawn apart from the other's, so no field of the two shares a run of more than 40 characters
// that the builder did not write itself.
type goldenRendering struct {
	n       int
	prefix  string
	project string
	repo    string
	base    time.Time
}

// goldenRenderings are the two renderings: sample 1 and sample 2.
var goldenRenderings = [2]goldenRendering{
	{n: 1, prefix: "ALPHA", project: "alpha", repo: "alpha-works/alpha-tower", base: time.Date(2026, 11, 18, 9, 30, 0, 0, time.UTC)},
	{n: 2, prefix: "BRAVO", project: "bravo", repo: "bravo-labs/bravo-yard", base: time.Date(2027, 4, 7, 16, 45, 0, 0, time.UTC)},
}

// goldenHostile are the values TOON must quote or escape, which the two renderings carry among
// their titles and words: true, null, -x, #x, 1e5, 0123, 123456e78901, a comma, a colon, a tab, a
// quote, a backslash, a character outside the Basic Multilingual Plane, an empty string.
var goldenHostile = [2][]string{
	{"true", "-x", "1e5", "123456e78901", "a, b", "a: b", "tab\there", "\U0001F600 outside the plane"},
	{"null", "#x", "0123", "", `say "so"`, `back\slash`, "\U0001F680 rocket", "0x1F"},
}

// key is the key of issue i of this rendering's project.
func (r goldenRendering) key(i int) string { return fmt.Sprintf("%s-%d", r.prefix, r.n*1000+i) }

// at is the rendering's base time, minutes on.
func (r goldenRendering) at(minutes int) time.Time {
	return r.base.Add(time.Duration(minutes) * time.Minute)
}

// stamp is at(minutes) as the texts write a time.
func (r goldenRendering) stamp(minutes int) string { return r.at(minutes).Format(time.RFC3339Nano) }

// random is the source of label's values in this rendering: the same values at every run.
func (r goldenRendering) random(label string) *rand.Rand {
	h := fnv.New64a()
	fmt.Fprintf(h, "%d/%s", r.n, label)
	seed := h.Sum64()
	return rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
}

// words is n characters of words drawn for label.
func (r goldenRendering) words(label string, n int) string {
	random := r.random(label)
	var b strings.Builder
	for b.Len() < n {
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		for range 3 + random.IntN(7) {
			b.WriteByte(byte('a' + random.IntN(26)))
		}
	}
	return b.String()[:n]
}

// title is a title of 1,000 characters drawn for label, with a comma, a colon, a tab, a quote, a
// backslash and a character outside the Basic Multilingual Plane among its words.
func (r goldenRendering) title(label string) string {
	marks := []string{", ", ": ", "\t", `"`, `\`, "\U0001F9ED"}
	runes := []rune(r.words(label, 1000))
	random := r.random(label + "/marks")
	for _, mark := range marks {
		at := 10 + random.IntN(len(runes)-20)
		runes = slices.Insert(runes, at, []rune(mark)...)
	}
	return string(runes[:1000])
}

// sha is a commit drawn for label.
func (r goldenRendering) sha(label string) string {
	random := r.random(label)
	const hex = "0123456789abcdef"
	b := make([]byte, 40)
	for i := range b {
		b[i] = hex[random.IntN(16)]
	}
	return string(b)
}

// id is an id drawn for label, the shape of a Dispatch uuid.
func (r goldenRendering) id(label string) string {
	s := r.sha(label)
	return s[:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:32]
}

// token is the claim token of role on issue in this rendering's project.
func (r goldenRendering) token(t *testing.T, issue string, role claim.Role) claim.Token {
	t.Helper()
	token, err := claim.NewToken(r.project, issue, role)
	if err != nil {
		t.Fatalf("claim token of %s's %s: %v", issue, role, err)
	}
	return token
}

// goldenBuild collects the samples as each builder writes them.
type goldenBuild struct {
	t       *testing.T
	samples []goldenSample
}

// goldenSamples is every sample of the golden file, each kind in each of its variants rendered
// twice, in the order of the list.
func goldenSamples(t *testing.T) []goldenSample {
	t.Helper()
	b := &goldenBuild{t: t}
	for _, r := range goldenRenderings {
		b.tasks(r)
		b.startParts(r)
		b.notices(r)
		b.controllerNotices(r)
		b.roleMessages(r)
	}
	order := map[texts.Kind]int{}
	for i, entry := range texts.Entries() {
		order[entry.Kind] = i
	}
	slices.SortStableFunc(b.samples, func(x, y goldenSample) int {
		if c := order[x.Kind] - order[y.Kind]; c != 0 {
			return c
		}
		if c := strings.Compare(x.Variant, y.Variant); c != 0 {
			return c
		}
		return x.Sample - y.Sample
	})
	return b.samples
}

// task adds a task: rec rendered (texts.Render), as the builder's caller sends it.
func (b *goldenBuild) task(r goldenRendering, variant string, reader claim.Role, issue string, rec any, want map[string]string) {
	b.t.Helper()
	text, err := texts.Render(rec)
	if err != nil {
		b.t.Fatalf("render %T: %v", rec, err)
	}
	b.taskText(r, kindOf(b.t, rec), variant, reader, issue, text, want)
}

// taskText adds a task the builder wrote as its text.
func (b *goldenBuild) taskText(r goldenRendering, kind texts.Kind, variant string, reader claim.Role, issue, text string, want map[string]string) {
	b.samples = append(b.samples, goldenSample{Kind: kind, Variant: variant, Sample: r.n, Reader: reader, Form: texts.Task,
		Issue: issue, Text: text, want: want})
}

// kindOf is the kind a record names.
func kindOf(t *testing.T, rec any) texts.Kind {
	t.Helper()
	for _, entry := range texts.Entries() {
		if fmt.Sprintf("%T", entry.Record) == strings.TrimPrefix(fmt.Sprintf("%T", rec), "*") {
			return entry.Kind
		}
	}
	t.Fatalf("%T is the record of no kind on the list", rec)
	return ""
}

// notice adds a notice to an architect or to the controller: rec as the builder's caller queues it
// (record.NoticeOf, record.NewOutboxRow), read back from its row, and published as the outbox
// publishes it (deliverNotice's Published, controllerNotice's Notice), at the urgency the list
// gives the kind for reader and the payload's variant (noticeUrgency).
func (b *goldenBuild) notice(r goldenRendering, variant string, reader claim.Role, rec any, want map[string]string) {
	b.t.Helper()
	notice, err := record.NoticeOf(rec)
	if err != nil {
		b.t.Fatalf("NoticeOf %T: %v", rec, err)
	}
	b.queuedNotice(r, variant, reader, notice, want)
}

// queuedNotice adds a notice a builder queued.
func (b *goldenBuild) queuedNotice(r goldenRendering, variant string, reader claim.Role, notice record.Notice, want map[string]string) {
	b.t.Helper()
	var payload record.OutboxPayload = notice
	if texts.IsControllerNotice(notice.Kind) || reader == claim.RoleController {
		payload = record.ControllerNotice(notice)
	}
	row, err := record.NewOutboxRow(notice.Issue, payload, r.base)
	if err != nil {
		b.t.Fatalf("queue the %s notice of %s: %v", notice.Kind, notice.Issue, err)
	}
	var queued record.Notice
	if err := json.Unmarshal(row.Payload, &queued); err != nil {
		b.t.Fatalf("read the %s notice back: %v", notice.Kind, err)
	}
	published := queued
	if _, controller := payload.(record.ControllerNotice); !controller {
		published, _ = queued.Published(1)
	}
	encoded, err := json.Marshal(published)
	if err != nil {
		b.t.Fatalf("encode the %s notice: %v", notice.Kind, err)
	}
	b.samples = append(b.samples, goldenSample{Kind: notice.Kind, Variant: variant, Sample: r.n, Reader: reader,
		Form: goldenFormOf(b.t, notice.Kind), Urgency: noticeUrgency(published, reader), Issue: row.Issue,
		Summary: noticeSummary(notice.Kind, row.Issue), Payload: encoded, want: want})
}

// goldenFormOf is the form the list gives kind.
func goldenFormOf(t *testing.T, kind texts.Kind) texts.Form {
	t.Helper()
	entry, ok := texts.Lookup(kind)
	if !ok {
		t.Fatalf("kind %s is on no list", kind)
	}
	return entry.Form
}

// roleMessage adds a role message: rec queued for reader's claim on issue at urgency
// (record.NewRoleMessage), read back from its row and published as its record
// (record.RoleMessage.Record, the outbox's roleMessage).
func (b *goldenBuild) roleMessage(r goldenRendering, variant string, reader claim.Role, issue string, rec any, urgency texts.Urgency, want map[string]string) {
	b.t.Helper()
	message, err := record.NewRoleMessage(string(r.token(b.t, issue, reader)), rec, urgency)
	if err != nil {
		b.t.Fatalf("NewRoleMessage %T: %v", rec, err)
	}
	row, err := record.NewOutboxRow(issue, message, r.base)
	if err != nil {
		b.t.Fatalf("queue the %s message of %s: %v", message.Kind, issue, err)
	}
	var queued record.RoleMessage
	if err := json.Unmarshal(row.Payload, &queued); err != nil {
		b.t.Fatalf("read the %s message back: %v", message.Kind, err)
	}
	published, err := queued.Record()
	if err != nil {
		b.t.Fatalf("the %s message's record: %v", message.Kind, err)
	}
	b.published(r, message.Kind, variant, reader, issue, published, queued.Urgency, want)
}

// published adds a role message the outbox publishes as payload, which the daemon encodes with
// json.Marshal (notify.HTTPPublisher.Publish).
func (b *goldenBuild) published(r goldenRendering, kind texts.Kind, variant string, reader claim.Role, issue string, payload any, urgency texts.Urgency, want map[string]string) {
	b.t.Helper()
	encoded, err := json.Marshal(payload)
	if err != nil {
		b.t.Fatalf("encode the %s message: %v", kind, err)
	}
	b.samples = append(b.samples, goldenSample{Kind: kind, Variant: variant, Sample: r.n, Reader: reader, Form: texts.RoleMessage,
		Urgency: urgency, Issue: issue, Summary: noticeSummary(kind, issue), Payload: encoded, want: want})
}

// queuedRow is row as a builder queues it, read back from its outbox row into a fresh value of its
// type, which the outbox's executor decodes.
func queuedRow[P record.OutboxPayload](t *testing.T, issue string, row P) P {
	t.Helper()
	encoded, err := record.NewOutboxRow(issue, row, time.Now())
	if err != nil {
		t.Fatalf("queue %T for %s: %v", row, issue, err)
	}
	var queued P
	if err := json.Unmarshal(encoded.Payload, &queued); err != nil {
		t.Fatalf("read %T back: %v", row, err)
	}
	return queued
}

// goldenWant is a want map: each value as the sample names it.
func goldenWant(pairs ...any) map[string]string {
	want := map[string]string{}
	for i := 0; i+1 < len(pairs); i += 2 {
		key := pairs[i].(string)
		switch v := pairs[i+1].(type) {
		case string:
			want[key] = v
		case int:
			want[key] = strconv.Itoa(v)
		case uint64:
			want[key] = strconv.FormatUint(v, 10)
		case int64:
			want[key] = strconv.FormatInt(v, 10)
		case bool:
			want[key] = strconv.FormatBool(v)
		case phase.Phase:
			want[key] = string(v)
		case claim.Role:
			want[key] = string(v)
		case record.ConflictWhy:
			want[key] = string(v)
		case time.Time:
			want[key] = v.UTC().Format(time.RFC3339Nano)
		case []string:
			want[key] = strings.Join(v, ",")
		default:
			panic(fmt.Sprintf("goldenWant: %s is a %T", key, v))
		}
	}
	return want
}

// tasks adds every task kind: the phase assignment in each of its moves, the resume of a phase,
// the resume of an answered root in each act, the look task, each shape of the architect's start,
// the kept decisions, each wake of a pod, and the controller's start.
func (b *goldenBuild) tasks(r goldenRendering) {
	hostile := goldenHostile[r.n-1]
	pr := func(number int, label string) *record.PullRequest {
		return &record.PullRequest{Repo: r.repo, Number: number, Base: "main-" + r.words(label+"/base", 6), HeadSHA: r.sha(label + "/head")}
	}

	// phase-assignment, the start of a phase after the tester's handoff.
	{
		issue := record.Issue{Key: r.key(1), Title: hostile[0], Phase: phase.Reviewing}
		handoff := record.PhaseRow{Role: claim.RoleTester, HandoffRecord: r.id("handoff"), HandoffVersion: 3 + 4*r.n,
			HandoffCommit: r.sha("handoff/commit"), Verdict: []string{"pass", "fail"}[r.n-1]}
		if r.n == 1 {
			handoff.HandoffCommit = "123456e78901"
		}
		request := pr(40+r.n, "handoff")
		b.task(r, "handoff", claim.RoleReviewer, issue.Key, workflow.PhaseAssignment(issue, workflow.WorkingCopy(issue, nil), handoff, request, workflow.PhaseMove{}),
			goldenWant("issue", issue.Key, "title", issue.Title, "phase", issue.Phase, "handoff_phase", "test",
				"handoff_version", handoff.HandoffVersion, "handoff_commit", handoff.HandoffCommit, "tester_verdict", handoff.Verdict,
				"pull_request", texts.PullRequestRef(r.repo, request.Number)))
	}
	// A worker's move back, with its words.
	{
		issue := record.Issue{Key: r.key(2), Title: hostile[1], Phase: phase.Implementing}
		from := []phase.Phase{phase.Testing, phase.Reviewing}[r.n-1]
		words := r.words("moved-back", 300) + "\n" + hostile[4] + " </reason " + hostile[6]
		b.task(r, "moved-back", claim.RoleImplementer, issue.Key, workflow.PhaseAssignment(issue, workflow.WorkingCopy(issue, nil), record.PhaseRow{}, pr(50+r.n, "back"),
			workflow.PhaseMove{BackFrom: from, Reason: words, At: r.at(5)}),
			goldenWant("issue", issue.Key, "title", issue.Title, "phase", issue.Phase, "moved_back_from", from,
				"pull_request", texts.PullRequestRef(r.repo, 50+r.n), "reason", words, "reason@by", phase.RoleFor(from),
				"reason@at", r.at(5)))
	}
	// A review's request for changes: the review App's in one rendering, a person's in the other.
	{
		issue := record.Issue{Key: r.key(3), Title: hostile[2], Phase: phase.Implementing}
		head := r.sha("rejected/head")
		review := record.ReviewDecision{State: "changes_requested", Body: r.words("rejected/body", 900), Head: head,
			Author: "legion-reviewer[bot]", SubmittedAt: r.at(9), ID: 3100000000 + int64(r.n)}
		by, body := string(claim.RoleReviewer), review.Body
		if r.n == 2 {
			// A person's review is cut to texts.OneAct, the rest counted.
			review.Author, review.Person, review.Body = "a-person-"+r.words("person", 5), true, r.words("rejected/body", 4500)
			by, body = review.Author, review.Body[:texts.OneAct]
		}
		b.task(r, "review-rejected", claim.RoleImplementer, issue.Key, workflow.PhaseAssignment(issue, workflow.WorkingCopy(issue, nil), record.PhaseRow{}, pr(60+r.n, "rejected"),
			workflow.PhaseMove{Review: &review}),
			goldenWant("issue", issue.Key, "title", issue.Title, "phase", issue.Phase, "reviewed_head", head, "review_id", review.ID,
				"pull_request", texts.PullRequestRef(r.repo, 60+r.n), "body", body, "body@by", by, "body@at", r.at(9)))
	}
	// A merger started in merging at the head a merge forward landed after the review, while the
	// review that landing asked of the reviewer stands for that head.
	{
		issue := record.Issue{Key: r.key(4), Title: hostile[3], Phase: phase.Merging}
		request := pr(70+r.n, "merge-forward")
		request.MergeForward = &record.MergeForward{Head: r.sha("merge-forward/old"), Base: request.Base, Phase: phase.Retro,
			Why: record.ConflictMergeNext, Landed: request.HeadSHA, ReviewAsked: true}
		b.task(r, "merge-forward-review", claim.RoleMerger, issue.Key, workflow.PhaseAssignment(issue, workflow.WorkingCopy(issue, nil), record.PhaseRow{}, request, workflow.PhaseMove{}),
			goldenWant("issue", issue.Key, "title", issue.Title, "phase", issue.Phase, "merged_forward", request.Base,
				"approval_owed", request.HeadSHA, "pull_request", texts.PullRequestRef(r.repo, request.Number)))
	}
	// A retry of a held phase, with the architect's words or none (the operator's).
	{
		issue := record.Issue{Key: r.key(5), Title: hostile[4], Phase: phase.Testing}
		retried := []string{workflow.RetriedByArchitect, workflow.RetriedByOperator}[r.n-1]
		words := r.words("retried", 200)
		b.task(r, "retried", claim.RoleTester, issue.Key, workflow.PhaseAssignment(issue, workflow.WorkingCopy(issue, nil), record.PhaseRow{}, nil,
			workflow.PhaseMove{Retried: retried, Reason: words}),
			goldenWant("issue", issue.Key, "title", issue.Title, "phase", issue.Phase, "retried", retried, "reason", words))
	}
	// A stopped task started again at its first step (workflow's restartTask): by a todo written on
	// the held issue, and by a later version of the owner's plan that changed the stopped tasks.
	{
		issue := record.Issue{Key: r.key(8), Title: hostile[6], Phase: phase.Implementing}
		b.task(r, "restarted-todo", claim.RoleImplementer, issue.Key, workflow.PhaseAssignment(issue, workflow.WorkingCopy(issue, nil), record.PhaseRow{}, nil,
			workflow.PhaseMove{Restarted: workflow.RestartedByTodo}),
			goldenWant("issue", issue.Key, "title", issue.Title, "phase", issue.Phase, "restarted", workflow.RestartedByTodo))
		version := 3 + 4*r.n
		stopped := []string{"task-" + r.words("restarted/first", 3), "task-" + r.words("restarted/second", 3)}[:r.n]
		b.task(r, "restarted-plan", claim.RoleImplementer, issue.Key, workflow.PhaseAssignment(issue, workflow.WorkingCopy(issue, nil), record.PhaseRow{}, nil,
			workflow.PhaseMove{Restarted: workflow.RestartedByPlan, PlanVersion: version, StoppedTasks: stopped}),
			goldenWant("issue", issue.Key, "title", issue.Title, "phase", issue.Phase, "restarted", workflow.RestartedByPlan, "plan_version", version,
				"stopped_tasks", stopped))
	}
	// A retry of a review round that nothing ends, in each of its causes.
	for i, stuck := range []struct {
		variant, cause string
		ends           bool
	}{
		{"stuck-undecided", texts.CauseUndecided, true},
		{"stuck-approved-other-code", texts.CauseApprovedOtherCode, true},
		{"stuck-merge-state", texts.CauseMergeState, false},
	} {
		issue := record.Issue{Key: r.key(6), Title: hostile[5+i], Phase: phase.Reviewing}
		head := r.sha(stuck.variant + "/head")
		request := pr(80+r.n, stuck.variant)
		round := &workflow.StuckRound{Cause: stuck.cause, Head: head, EndsRound: stuck.ends}
		want := goldenWant("issue", issue.Key, "title", issue.Title, "phase", issue.Phase, "retried", workflow.RetriedByArchitect,
			"stuck_round", stuck.cause, "head", head, "pull_request", texts.PullRequestRef(r.repo, 80+r.n))
		switch stuck.cause {
		case texts.CauseUndecided:
			round.SetAside = r.words(stuck.variant+"/aside", 120)
			want["set_aside"] = round.SetAside
		case texts.CauseApprovedOtherCode:
			round.Approved = r.sha(stuck.variant + "/approved")
			want["reviewed_head"] = round.Approved
		case texts.CauseMergeState:
			checks := goldenChecks(r, stuck.variant)
			request.CheckRuns, request.CheckedHead, request.Statuses = checks.runs, checks.head, checks.statuses
			round.MergeState = string([]record.MergeState{record.MergeStateBlocked, record.MergeStateBehind}[r.n-1])
			want["merge_state"], want["not_passed#"] = round.MergeState, strconv.Itoa(len(checks.notPassed))
			want["list_passed"] = texts.ListPassed(r.repo, request.Number)
			checks.skippedFacts(checks.statusFacts(want))
		}
		if stuck.ends {
			want["ends_round"] = "APPROVE of head " + head
		}
		b.task(r, stuck.variant, claim.RoleReviewer, issue.Key, workflow.PhaseAssignment(issue, workflow.WorkingCopy(issue, nil), record.PhaseRow{}, request,
			workflow.PhaseMove{Retried: workflow.RetriedByArchitect, Stuck: round}), want)
	}

	// phase-resume.
	{
		issue := record.Issue{Key: r.key(7), Title: r.title("resume"), Phase: []phase.Phase{phase.Planning, phase.Merging}[r.n-1]}
		b.task(r, "resume", phase.RoleFor(issue.Phase), issue.Key, workflow.PhaseResume(issue, workflow.WorkingCopy(issue, nil)),
			goldenWant("issue", issue.Key, "title", issue.Title, "phase", issue.Phase))
	}

	// The standing of a step of an owner's plan (workflow.StandingOf, as standingPart composes it):
	// a step's worker's phase assignment and its phase-resume, each with the task, its place, the
	// steps, the report of each earlier step and, last, the owner's text of the task's block, which
	// holds a line that closes the block's tag and a line that reads like a field; and the merger's
	// task, which names the task and its steps alone.
	{
		issue, w, rows := goldenStepWalk(b.t, r)
		at, err := workflow.StandingOf(issue, nil, w, rows)
		if err != nil {
			b.t.Fatalf("the standing of %s's step: %v", issue.Key, err)
		}
		want := goldenWant("issue", issue.Key, "title", issue.Title, "phase", issue.Phase, "task", w.Task, "step", 3, "steps#", 3,
			"steps", []string{w.Steps[0].Role, w.Steps[1].Role, w.Steps[2].Role}, "reported#", 2,
			"task_text", strings.TrimSpace(w.Text), "task_text@by", string(w.Version.GivenBy), "task_text@at", w.Version.GivenAt)
		for i, row := range rows {
			want[fmt.Sprintf("reported[%d].step", i)] = strconv.Itoa(i + 1)
			want[fmt.Sprintf("reported[%d].role", i)] = w.Steps[i].Role
			word, _, _ := phase.Handoff(goldenStepPhase[row.Role])
			want[fmt.Sprintf("reported[%d].handoff_phase", i)] = word
			want[fmt.Sprintf("reported[%d].handoff_version", i)] = strconv.Itoa(row.HandoffVersion)
		}
		reader := phase.RoleFor(issue.Phase)
		b.task(r, "step", reader, issue.Key, workflow.PhaseAssignment(issue, at, record.PhaseRow{}, nil, workflow.PhaseMove{}), want)
		b.task(r, "step", reader, issue.Key, workflow.PhaseResume(issue, at), want)
		merging := issue
		merging.Phase = phase.Merging
		merger, err := workflow.StandingOf(merging, nil, w, rows)
		if err != nil {
			b.t.Fatalf("the standing of %s's merge: %v", issue.Key, err)
		}
		b.task(r, "merge", claim.RoleMerger, issue.Key, workflow.PhaseAssignment(merging, merger, record.PhaseRow{}, nil, workflow.PhaseMove{}),
			goldenWant("issue", issue.Key, "title", issue.Title, "phase", phase.Merging, "task", w.Task, "steps#", 3,
				"steps", []string{w.Steps[0].Role, w.Steps[1].Role, w.Steps[2].Role}))
	}

	b.rootResumes(r)
	b.checkLooks(r)
	b.architectStarts(r)
	b.keptDecisions(r)
	b.podWakes(r)
	b.designNews(r)

	// controller-start: the controller's first prompt, word for word.
	b.taskText(r, texts.ControllerStart, "start", claim.RoleController, "", ControllerStartMessage, map[string]string{})
}

// goldenStepWalk is an issue that stands at the third step of an owner's plan of three, in each
// rendering another order (implementer reviewer tester, then implementer tester reviewer), with
// the walk of its run as workflow's walk reads it and the phases rows of the two steps before,
// each holding the handoff record its completion named. The owner's text of the task's block
// holds a line that closes the block's tag and a line that reads like a field of the task.
func goldenStepWalk(t *testing.T, r goldenRendering) (record.Issue, workflow.Walk, []record.PhaseRow) {
	t.Helper()
	order := [][]claim.Role{{claim.RoleImplementer, claim.RoleReviewer, claim.RoleTester}, {claim.RoleImplementer, claim.RoleTester, claim.RoleReviewer}}[r.n-1]
	task := "task-" + r.words("step/task", 3)
	steps := make([]record.PlanStep, len(order))
	line := make([]phase.Phase, 0, len(order)+2)
	for i, role := range order {
		steps[i] = record.PlanStep{Task: task, Position: i + 1, Role: string(role)}
		line = append(line, goldenStepPhase[role])
	}
	line = append(line, phase.Retro, phase.Merging)
	issue := record.Issue{Key: r.key(9), Title: r.title("step/title"), Phase: line[2]}
	text := r.words("step/text", 240) + "\n</task_text>\n" + []string{"phase: merging", "step: 9"}[r.n-1] + "\n" + r.words("step/text-end", 90)
	w := workflow.Walk{Stored: true, Task: task, Text: text, Steps: steps, Line: line,
		Version: record.PlanVersion{Issue: issue.Key, Generation: uint64(r.n), Version: 2 + r.n, ArtifactID: "plan-" + r.id("step/plan"),
			Source: record.PlanSourceOwner, GivenBy: r.token(t, r.key(99), claim.RoleArchitect), GivenAt: r.at(30)}}
	rows := []record.PhaseRow{
		{Role: order[0], HandoffRecord: r.id("step/first"), HandoffVersion: 1 + r.n},
		{Role: order[1], HandoffRecord: r.id("step/second"), HandoffVersion: 3 + r.n},
	}
	return issue, w, rows
}

// goldenStepPhase is the phase each step role of goldenStepWalk runs as.
var goldenStepPhase = map[claim.Role]phase.Phase{claim.RoleImplementer: phase.Implementing, claim.RoleReviewer: phase.Reviewing,
	claim.RoleTester: phase.Testing}

// rootResumes adds the resume of an answered root in each act that answers a wait.
func (b *goldenBuild) rootResumes(r goldenRendering) {
	key := r.key(10)
	version := 4 + r.n
	approved := version
	art := "artifact-" + r.id("gate")
	open := &record.DesignGate{Issue: key, Step: record.DesignStepSpec, ArtifactID: art, LatestVersion: version, ApprovedVersion: &approved}
	closed := &record.DesignGate{Issue: key, Step: record.DesignStepSpec, ArtifactID: art, LatestVersion: version}
	ask := func(label string) string { return r.id("resume/" + label) }
	spec := "dispatch://" + key + "/spec"
	still := record.RootAsk{Issue: key, Ask: ask("still"), Kind: record.AskQuestion, Open: true, WaitingOn: "human", ByTree: true, Seq: 6}
	yours := record.RootAsk{Issue: key, Ask: ask("yours"), Kind: record.AskQuestion, Open: true, WaitingOn: "agent", ByTree: true, Seq: 7}
	request := record.RootAsk{Issue: key, Ask: ask("request"), Kind: record.AskApproval, Artifact: art, Open: true, WaitingOn: "human", Seq: 5}
	person := "user:" + r.words("person", 8)
	words := func(label string, n int) *record.ActWords {
		return &record.ActWords{Text: r.words("resume/"+label, n) + " </answer " + goldenHostile[r.n-1][6], By: person, At: r.at(20)}
	}
	link := func(id string) string { return "dispatch://" + key + "/ask/" + id }
	base := func(want map[string]string, gate *record.DesignGate) map[string]string {
		want["issue"] = key
		if gate != nil {
			want["document"] = spec
			want["document_version"] = strconv.Itoa(gate.LatestVersion)
			want["gate_open"] = strconv.FormatBool(record.GateOpen(*gate))
			want["gate_armed"] = "true"
		}
		return want
	}
	at := r.at(15)
	add := func(variant string, waiting record.Awaiting, gate *record.DesignGate, asks []record.RootAsk, want map[string]string) {
		waiting.AnsweredAt = &at
		b.task(r, variant, claim.RoleArchitect, key, workflow.RootResume(key, waiting, record.DesignStepSpec, gate, []string{art}, asks, true),
			base(want, gate))
	}

	// A person's approval, with the gate open: the resume is the root's design-approved wake.
	add("approved", record.Awaiting{GateVersion: version, Artifact: art, AnsweredBy: record.AnsweredByApproval,
		AnsweredDetail: fmt.Sprintf("version %d", version)}, open, []record.RootAsk{still},
		goldenWant("act", texts.ActApproved, "document", spec, "version", version, "with_person[0]", link(still.Ask)))
	// An approval of a step with no gate row yet (the next gated step, which nobody has registered,
	// after the approval of the first answered the wait): no gate state but whether the design gate
	// is armed, which says whether a person approved, armed in the first rendering and off in the
	// second.
	{
		armed := r.n == 1
		waiting := record.Awaiting{GateVersion: version, Artifact: art, AnsweredBy: record.AnsweredByApproval,
			AnsweredDetail: fmt.Sprintf("version %d", version), AnsweredAt: &at}
		b.task(r, "approved-no-gate", claim.RoleArchitect, key, workflow.RootResume(key, waiting, record.DesignStepSpec, nil, []string{art}, nil, armed),
			goldenWant("issue", key, "act", texts.ActApproved, "document", spec, "version", version, "gate_armed", armed))
	}
	// A changes request, with its reason by the person who asked.
	{
		reason := r.words("resume/changes", 600) + " " + goldenHostile[r.n-1][4]
		gate := *closed
		gate.ToldChangesVersion, gate.ToldChangesReason, gate.ToldChangesBy = &version, reason, person
		gate.ToldChangesMark = record.EventMark{Seq: 9, At: r.at(18)}
		add("changes-requested", record.Awaiting{GateVersion: version, Artifact: art, AnsweredBy: record.AnsweredByChanges,
			AnsweredDetail: fmt.Sprintf("version %d: %q", version, reason), Since: record.EventMark{Seq: 3}}, &gate,
			[]record.RootAsk{request, yours},
			goldenWant("act", texts.ActChangesRequested, "document", spec, "version", version, "by", person, "at", r.at(18),
				"reason", reason, "approval_request", version, "request_open", true, "waiting_on_you[0]", link(yours.Ask)))
	}
	// A newer version that handed the approval request back.
	add("version", record.Awaiting{GateVersion: version, Artifact: art, AnsweredBy: record.AnsweredByVersion,
		AnsweredDetail: fmt.Sprintf("version %d", version+1)}, closed, []record.RootAsk{request},
		goldenWant("act", texts.ActVersion, "document", spec, "version", version+1, "approval_request", version))
	// An ask retracted with its decision block.
	add("retracted", record.Awaiting{Ask: ask("retracted"), AnsweredBy: record.AnsweredByRetraction}, open, nil,
		goldenWant("act", texts.ActRetracted, "ask", link(ask("retracted"))))
	// An ask closed by an agent.
	closer := "session:" + r.id("closer")
	add("closed", record.Awaiting{Ask: ask("closed"), AnsweredBy: record.AnsweredByClosed, AnsweredDetail: closer}, open, nil,
		goldenWant("act", texts.ActClosed, "ask", link(ask("closed")), "closed_by", closer))
	// A person's answer of the awaited ask, with the options it chose.
	{
		answer := words("answer", 700)
		answer.Options = []string{"option " + r.words("option/1", 6), "option " + r.words("option/2", 6)}
		row := record.RootAsk{Issue: key, Ask: ask("answered"), Kind: record.AskQuestion, Open: false, ByTree: true, Seq: 8,
			PersonAnswerSeq: 9, PersonAnswerWords: answer}
		add("ask", record.Awaiting{Ask: row.Ask, AnsweredBy: record.AnsweredByAsk, AnsweredDetail: row.Ask}, open,
			[]record.RootAsk{row, still},
			goldenWant("act", texts.ActAsk, "ask", link(row.Ask), "by", person, "at", r.at(20), "options", answer.Options,
				"answer", answer.Text, "with_person[0]", link(still.Ask)))
	}
	// A person's reply in the thread of another ask.
	{
		reply := words("reply", 120)
		row := record.RootAsk{Issue: key, Ask: ask("replied"), Kind: record.AskQuestion, Open: true, WaitingOn: "agent", Seq: 8,
			PersonSeq: 11, PersonWords: reply}
		add("reply", record.Awaiting{Ask: ask("awaited"), AnsweredBy: record.AnsweredByReply, AnsweredDetail: row.Ask}, open,
			[]record.RootAsk{row},
			goldenWant("act", texts.ActReply, "ask", link(row.Ask), "by", person, "at", r.at(20), "reply", reply.Text))
	}
	// A session's reply that handed the question to the architect's turn.
	add("turn", record.Awaiting{Ask: ask("awaited"), AnsweredBy: record.AnsweredByTurn, AnsweredDetail: ask("turned")}, closed,
		[]record.RootAsk{yours},
		goldenWant("act", texts.ActTurn, "ask", link(ask("turned")), "waiting_on_you[0]", link(yours.Ask)))
	// A person's decline, with the sentences Dispatch wrote for the asking agent and their note.
	{
		note := words("note", 400)
		row := record.RootAsk{Issue: key, Ask: ask("declined"), Kind: record.AskQuestion, Open: false, Seq: 8,
			ClosedBy: record.ClosedByPerson, ClosedSeq: 12, PersonSeq: 12, PersonWords: note,
			Decline: &record.AskDecline{Note: note.Text, Words: goldenDeclineDispatchWords}}
		add("declined", record.Awaiting{Ask: ask("awaited"), AnsweredBy: record.AnsweredByDecline, AnsweredDetail: row.Ask}, closed,
			[]record.RootAsk{row},
			goldenWant("act", texts.ActDeclined, "ask", link(row.Ask), "by", person, "at", r.at(20), "dispatch_words", row.Decline.Words,
				"note", note.Text))
	}
}

// goldenDeclineDispatchWords is what Dispatch writes beside a person's note when they decline to
// answer, word for word as packages/envoy/internal/dispatch/model/model.go's DeclineDispatchWords
// holds it. This package cannot import that internal package, so the fixture carries a copy: the
// production resume takes the words from the ask.resolved event (intake's decode), and so every
// declined resume carries these same four sentences, which goldenKept holds by dispatch://INFRA-1282's R7.
const goldenDeclineDispatchWords = "The person chose not to answer this question. " +
	"Decide the next step without an answer. " +
	"Do not ask this question again unless the facts change. " +
	"Write in your work what you assumed."

// checkLooks adds the look task of a production check, for each thing that wakes a look.
func (b *goldenBuild) checkLooks(r goldenRendering) {
	issue := record.Issue{Key: r.key(20), Title: r.title("look")}
	check := record.ProductionCheck{Issue: issue.Key, Repo: r.repo, Number: 300 + r.n, MergeSHA: r.sha("look/merge"),
		OpenedAt: r.at(0), BoundAt: r.at(0).Add(time.Duration(2+r.n) * 24 * time.Hour), Role: []claim.Role{claim.RoleTester, claim.RoleImplementer}[r.n-1],
		Looks: 2 + r.n}
	want := func(woken string) map[string]string {
		return goldenWant("issue", issue.Key, "title", issue.Title, "look", check.Looks, "woken_by", woken,
			"pull_request", texts.PullRequestRef(r.repo, check.Number), "merge_commit", check.MergeSHA,
			"bound_days", 2+r.n, "last_look", check.BoundAt)
	}
	b.task(r, "merge", check.Role, issue.Key, workflow.CheckLook(issue, check, "merge", "", false), want(texts.WokenByMerge))
	summary := r.words("look/summary", 500) + " " + goldenHostile[r.n-1][5]
	named := want(texts.WokenByNamed)
	named["summary"], named["summary@by"] = summary, string(check.Role)
	reported := check
	reportedAt := r.at(7)
	reported.ReportedAt = &reportedAt
	for key, value := range goldenWant("summary@at", reportedAt) {
		named[key] = value
	}
	b.task(r, "named", check.Role, issue.Key, workflow.CheckLook(issue, reported, "check wake", summary, false), named)
	retry := r.words("look/retry", 80)
	retried := want(texts.WokenByRetry)
	retried["retry_of"] = retry
	b.task(r, "retry", check.Role, issue.Key, workflow.CheckLook(issue, check, "check retry", "the retry of a look nobody reported: "+retry, false), retried)
	unmerged := check
	unmerged.MergeSHA = ""
	last := want(texts.WokenByBound)
	last["last"] = "true"
	delete(last, "merge_commit")
	b.task(r, "bound-last", check.Role, issue.Key, workflow.CheckLook(issue, unmerged, "bound", "", true), last)
	// A bound that is not whole days, as production_check_hours takes one: 37 hours, then an hour
	// and a half, each named as Go writes the duration.
	hours := check
	hours.BoundAt = check.OpenedAt.Add([]time.Duration{37 * time.Hour, 90 * time.Minute}[r.n-1])
	inHours := want(texts.WokenByMerge)
	delete(inHours, "bound_days")
	inHours["bound_duration"], inHours["last_look"] = []string{"37h0m0s", "1h30m0s"}[r.n-1], hours.BoundAt.UTC().Format(time.RFC3339Nano)
	b.task(r, "hours", check.Role, issue.Key, workflow.CheckLook(issue, hours, "merge", "", false), inHours)
}

// architectStarts adds the root architect's start in each of its shapes (dispatch://INFRA-2614's
// R3), the kept decisions rendered as a part of their own (keptDecisions).
func (b *goldenBuild) architectStarts(r goldenRendering) {
	key := r.key(30)
	generation := uint64(3 + 5*r.n)
	steps := [][]record.DesignStep{{record.DesignStepSpec}, nil}[r.n-1]
	root := record.Issue{Key: key, Tree: key, Phase: phase.Admitted, Generation: generation}
	add := func(variant string, start workflow.ArchitectStart, want map[string]string) {
		b.t.Helper()
		want["issue"], want["generation"] = key, strconv.FormatUint(generation, 10)
		b.taskText(r, texts.ArchitectStart, variant, claim.RoleArchitect, key, workflow.ArchitectStartTask(start), want)
	}
	gateActs := []string{workflow.FirstActRequestApproval, workflow.FirstActRegisterGate}
	if len(steps) == 0 {
		gateActs = []string{workflow.FirstActRegisterGate}
	}
	gated := strings.Join(record.StepNames(steps), ",")

	fresh := goldenWant("shape", texts.ShapeFreshTree, "phase", phase.Admitted, "holds_slot", true, "decompose", true,
		"first_act", append([]string{workflow.FirstActClaim, workflow.FirstActWriteSpec, workflow.FirstActDecompose}, gateActs...),
		"nothing_else_starts_it", true)
	if gated != "" {
		fresh["gated"] = gated
	}
	add("fresh-tree", workflow.ArchitectStart{Root: root, Gate: record.TreeGate{Steps: steps}}, fresh)

	carried := root
	carried.Phase = []phase.Phase{phase.Testing, phase.Merging}[r.n-1]
	add("carried-phase", workflow.ArchitectStart{Root: carried, Carried: true, GateCarried: r.n == 1, PhaseFinished: r.n == 2,
		Gate: record.TreeGate{Steps: steps}},
		goldenWant("shape", texts.ShapeCarriedPhase, "phase", carried.Phase, "gate_carried", r.n == 1, "daemon_starts_worker", r.n == 1,
			"first_act", []string{workflow.FirstActClaim, workflow.FirstActResumePhase}, "decompose", false))

	version := 2 + r.n
	changes := version
	standing := record.TreeGate{Steps: []record.DesignStep{record.DesignStepDecisions, record.DesignStepSpec},
		Rows: map[record.DesignStep]record.DesignGate{
			record.DesignStepDecisions: {Issue: key, Step: record.DesignStepDecisions, ArtifactID: r.id("decisions"), LatestVersion: version,
				ToldChangesVersion: &changes},
		}}
	add("gate-standing", workflow.ArchitectStart{Root: root, Gate: standing},
		goldenWant("shape", texts.ShapeGateStanding, "phase", phase.Admitted, "decompose", false,
			"first_act", []string{workflow.FirstActClaim, workflow.FirstActCarryGate, workflow.FirstActTakeUpTree}, "first_phase_waits_for", []string{workflow.WaitsForGate},
			"gated", "decisions,spec",
			"gates[0].step", "decisions", "gates[0].document", "dispatch://"+key+"/artifact/"+dispatch.DecisionsSlug,
			"gates[0].version", version, "gates[0].state", texts.GateChangesRequested,
			"gates[1].step", "spec", "gates[1].document", "dispatch://"+key+"/spec", "gates[1].version", 0, "gates[1].state", texts.GateUnregistered))

	approved := version
	openGate := record.TreeGate{Steps: []record.DesignStep{record.DesignStepSpec},
		Rows: map[record.DesignStep]record.DesignGate{
			record.DesignStepSpec: {Issue: key, Step: record.DesignStepSpec, ArtifactID: r.id("spec"), LatestVersion: version, ApprovedVersion: &approved},
		}}
	add("gate-open", workflow.ArchitectStart{Root: root, Gate: openGate},
		goldenWant("shape", texts.ShapeGateOpen, "phase", phase.Admitted, "decompose", false,
			"first_act", []string{workflow.FirstActClaim, workflow.FirstActTakeUpTree},
			"first_phase_waits_for", []string{workflow.WaitsForSlot, workflow.WaitsForChildren, workflow.WaitsForBlockers}, "gated", "spec",
			"gates[0].step", "spec", "gates[0].document", "dispatch://"+key+"/spec", "gates[0].version", version, "gates[0].state", texts.GateApproved))

	earlier := goldenWant("shape", texts.ShapeEarlierGeneration, "phase", phase.Admitted, "holds_slot", true, "decompose", false,
		"first_act", append([]string{workflow.FirstActClaim}, gateActs...), "first_phase_waits_for", []string{workflow.WaitsForGate})
	if gated != "" {
		earlier["gated"] = gated
	}
	add("earlier-generation", workflow.ArchitectStart{Root: root, Served: generation - 1, Gate: record.TreeGate{Steps: steps}}, earlier)
}

// keptDecisions adds the task that names what a person decided on a tree nobody could hear: one
// decision, cut at texts.OneAct, and several with a kept approval, each cut at texts.EachOfSeveral.
func (b *goldenBuild) keptDecisions(r goldenRendering) {
	root := r.key(40)
	person := "user:" + r.words("kept/person", 7)
	mark := func(i int, decision record.DecisionKind, n int) record.DecisionMark {
		return record.DecisionMark{Ref: fmt.Sprintf("dispatch://%s/ask/%s", root, r.id(fmt.Sprintf("kept/%d", i))), At: r.at(30 + i),
			Issue: r.key(41 + i), Decision: decision, Actor: person, Options: []string{"keep " + r.words(fmt.Sprintf("kept/%d/option", i), 5)},
			Answer: r.words(fmt.Sprintf("kept/%d/answer", i), n) + " " + goldenHostile[r.n-1][i%len(goldenHostile[0])], AnswerAt: r.at(40 + i)}
	}
	one := record.KeptDecisions{Decisions: []record.DecisionMark{mark(0, record.DecisionAskAnswered, 4500)}}
	answer, _ := texts.Bound(one.Decisions[0].Answer, texts.OneAct)
	b.taskText(r, texts.KeptDecisions, "one", claim.RoleArchitect, root, workflow.KeptDecisionsTask(root, one),
		goldenWant("issue", root, "kept[0].ref", one.Decisions[0].Ref, "kept[0].issue", one.Decisions[0].Issue,
			"kept[0].decision", string(record.DecisionAskAnswered), "kept[0].by", person, "kept[0].at", r.at(40),
			"kept[0].answer", answer))
	several := record.KeptDecisions{Decisions: []record.DecisionMark{mark(1, record.DecisionAskReply, 300), mark(2, record.DecisionAskResolved, 1500)},
		KeptApproval: record.KeptApproval{Step: record.DesignStepSpec, Approved: 2 + r.n, Latest: 4 + r.n}}
	second, _ := texts.Bound(several.Decisions[1].Answer, texts.EachOfSeveral)
	b.taskText(r, texts.KeptDecisions, "several", claim.RoleArchitect, root, workflow.KeptDecisionsTask(root, several),
		goldenWant("issue", root, "kept[0].ref", several.Decisions[0].Ref, "kept[0].issue", several.Decisions[0].Issue,
			"kept[0].decision", string(record.DecisionAskReply), "kept[1].ref", several.Decisions[1].Ref,
			"kept[1].decision", string(record.DecisionAskResolved), "kept[1].answer", second,
			"approval_step", "spec", "approval_document", "dispatch://"+root+"/spec", "approved", 2+r.n, "latest", 4+r.n))
}

// podWakes adds the task of a role whose released pod a wake brought back, for each wake: the
// first rendering's wakes bring back a session an idle release stopped, the second's one a
// project's pause stopped.
func (b *goldenBuild) podWakes(r goldenRendering) {
	issue := r.key(50)
	stop, stoppedBy := []workflow.WakeStop{workflow.StoppedByRelease, workflow.StoppedByPause}[r.n-1],
		[]string{texts.StoppedByRelease, texts.StoppedByPause}[r.n-1]
	checks := goldenChecks(r, "wake")
	settled := workflow.SettlementWake(issue, phase.Testing, r.repo, intake.PullRequestChecks{Repo: r.repo, Number: 500 + r.n,
		HeadSHA: r.sha("wake/head"), CheckRuns: checks.runs, Failing: checks.failing, Cancelled: checks.cancelled, Statuses: checks.statuses})
	settled.StoppedBy = stoppedBy
	want := checks.skippedFacts(checks.statusFacts(goldenWant("issue", issue, "wake", pods.WakeChecks, "stopped_by", stoppedBy,
		"pull_request", texts.PullRequestRef(r.repo, 500+r.n), "head", r.sha("wake/head"),
		"phase", phase.Testing, "list_passed", texts.ListPassed(r.repo, 500+r.n), "not_passed#", len(checks.notPassed))))
	b.task(r, "checks", claim.RoleTester, issue, settled, want)
	token := r.token(b.t, issue, claim.RoleImplementer)
	b.taskText(r, texts.PodWake, "role-message", claim.RoleImplementer, issue, workflow.WakeTask(issue, pods.WakeRoleMessage, string(token), stop),
		goldenWant("issue", issue, "wake", pods.WakeRoleMessage, "stopped_by", stoppedBy, "topic", "notifications.role."+string(token)))
	session := r.id("wake/session")
	b.taskText(r, texts.PodWake, "agent-message", claim.RoleArchitect, issue, workflow.WakeTask(issue, pods.WakeAgentMessage, session, stop),
		goldenWant("issue", issue, "wake", pods.WakeAgentMessage, "stopped_by", stoppedBy, "session", session))
	role := []claim.Role{claim.RolePlanner, claim.RoleMerger}[r.n-1]
	b.taskText(r, texts.PodWake, "backstop", role, issue, workflow.WakeTask(issue, pods.WakeBackstop, string(role), stop),
		goldenWant("issue", issue, "wake", pods.WakeBackstop, "stopped_by", stoppedBy, "armed_for", role))
}

// goldenChecks is a head's 60 check runs: 15 that failed and 10 that were cancelled, the 25 that
// did not pass, 5 that GitHub skipped (dispatch://INFRA-4135's R1), and 30 that passed. The first
// rendering's runs each carry the word GitHub gave them, as the listener sends it since
// cybermonklabs/legion#4541 - failure or timed_out, cancelled, skipped, success or neutral - and a
// text names a check that did not pass by that word; the second rendering's carry none, as a
// listener of an earlier release sends them, and a text names such a check by the daemon's
// conclusion. Hostile names are among them. Beside them the head carries 30 commit statuses
// (cybermonklabs/legion#4299, dispatch://INFRA-1997's R7), each stamped with the head as the pull
// request keeps it: 3 in state failure or error, the ones that did not pass, a hostile context
// among them, and 27 that passed, in context order.
type goldenCheckRuns struct {
	runs               []record.AttemptRun
	failing, cancelled []string
	notPassed          []string
	skipped            []string
	passed             int
	// conclusionOf is what a text names each check as: its word, else its conclusion.
	conclusionOf      map[string]string
	head              string
	statuses          []record.CommitStatus
	notPassedStatuses []string
	statusesPassed    int
	stateOf           map[string]record.CommitStatusState
}

func goldenChecks(r goldenRendering, label string) goldenCheckRuns {
	checks := goldenCheckRuns{head: r.sha(label + "/head"), conclusionOf: map[string]string{},
		stateOf: map[string]record.CommitStatusState{}}
	hostile := goldenHostile[r.n-1]
	for i := range 60 {
		name := fmt.Sprintf("%s-%02d %s", r.words(label+"/check", 4+r.n), i, r.words(fmt.Sprintf("%s/check/%d", label, i), 8))
		if i < len(hostile) && hostile[i] != "" {
			name = hostile[i] + fmt.Sprintf(" %02d", i)
		}
		conclusion, word := record.RunSucceeded, []string{"success", "neutral"}[i%2]
		switch {
		case i%12 < 3:
			conclusion, word = record.RunFailed, []string{"failure", "timed_out"}[i%2]
		case i%12 < 5:
			conclusion, word = record.RunCancelled, "cancelled"
		case i%12 == 5:
			conclusion, word = record.RunSkipped, "skipped"
		}
		if r.n != 1 {
			word = ""
		}
		checks.runs = append(checks.runs, record.AttemptRun{Name: name, ID: int64(1000*r.n + i), Head: checks.head, Conclusion: conclusion,
			GitHubConclusion: word})
		checks.conclusionOf[name] = string(conclusion)
		if word != "" {
			checks.conclusionOf[name] = word
		}
		switch conclusion {
		case record.RunFailed:
			checks.failing = append(checks.failing, name)
			checks.notPassed = append(checks.notPassed, name)
		case record.RunCancelled:
			checks.cancelled = append(checks.cancelled, name)
			checks.notPassed = append(checks.notPassed, name)
		case record.RunSkipped:
			checks.skipped = append(checks.skipped, name)
		default:
			checks.passed++
		}
	}
	slices.Sort(checks.notPassed)
	slices.Sort(checks.skipped)
	for i := range 30 {
		statusContext := fmt.Sprintf("status/%s-%02d %s", r.words(label+"/status", 3+r.n), i, r.words(fmt.Sprintf("%s/status/%d", label, i), 6))
		if i == 3 {
			statusContext = hostile[4] + fmt.Sprintf(" status %02d", i)
		}
		state := record.CommitStatusSuccess
		switch i {
		case 3, 23:
			state = record.CommitStatusFailure
		case 13:
			state = record.CommitStatusError
		}
		checks.statuses = append(checks.statuses, record.CommitStatus{Context: statusContext, State: state, CreatedAt: r.at(i), Head: checks.head})
		checks.stateOf[statusContext] = state
		if state == record.CommitStatusSuccess {
			checks.statusesPassed++
		} else {
			checks.notPassedStatuses = append(checks.notPassedStatuses, statusContext)
		}
	}
	slices.SortFunc(checks.statuses, func(a, b record.CommitStatus) int { return strings.Compare(a.Context, b.Context) })
	slices.Sort(checks.notPassedStatuses)
	return checks
}

// skippedFacts is the want of a sample that names the head's checks GitHub skipped (skipped):
// their count, the first of them, and the field that says once that a skipped check ran nothing
// on the head (each_skipped_check).
func (c goldenCheckRuns) skippedFacts(want map[string]string) map[string]string {
	want["skipped#"] = strconv.Itoa(len(c.skipped))
	want["skipped[0]"] = c.skipped[0]
	want["each_skipped_check"] = texts.RanNothing
	return want
}

// statusFacts is the want of a sample that names the head's commit statuses that did not pass
// (not_passed_statuses), the first of them with its state, the count of the checks that passed
// (passed) and, apart from it, the count of the statuses that passed (passed_statuses).
func (c goldenCheckRuns) statusFacts(want map[string]string) map[string]string {
	want["not_passed_statuses#"] = strconv.Itoa(len(c.notPassedStatuses))
	want["not_passed_statuses[0].context"] = c.notPassedStatuses[0]
	want["not_passed_statuses[0].state"] = string(c.stateOf[c.notPassedStatuses[0]])
	want["passed"] = strconv.Itoa(c.passed)
	want["passed_statuses"] = strconv.Itoa(c.statusesPassed)
	return want
}

// startParts adds the parts a start puts before a task, each a task of its own kind, rendered
// part by part as Delivery.message joins them: the relaunch note, the cut-turn note, the
// abandoned-session note, the out-of-memory note and the provider-error note.
func (b *goldenBuild) startParts(r goldenRendering) {
	issue := r.key(60)
	cause := "the process died: " + r.words("ended/cause", 40)
	ended := supervise.EndedWork{At: r.at(1), Cause: cause, Turn: true, TaskTurn: r.n == 1, Reported: r.n == 2,
		Kinds: [][]string{{"subagent", "async-job"}, {"claude-background-task"}}[r.n-1], Background: r.n == 2}
	b.task(r, "in-front-of-a-task", claim.RoleImplementer, issue, supervise.RelaunchNote(issue, ended, true),
		goldenWant("issue", issue, "cause", cause, "in_turn", true, "task", true))
	// Alone, as queueRelaunch delivers it to a claim that holds no task: a worker or an architect,
	// never the controller, whose start message carries the note in front of it.
	b.task(r, "alone", claim.RoleArchitect, issue, supervise.RelaunchNote(issue, ended, false),
		goldenWant("issue", issue, "cause", cause, "in_turn", true))
	// With no report of the process's last turn: a process in no turn (sample 1) and a turn cut off
	// whose record names no work that stopped (sample 2), of each of which main's note said that
	// every subagent and background job of the process is gone.
	unreported := supervise.EndedWork{At: r.at(2), Cause: cause, Turn: r.n == 2}
	want := goldenWant("issue", r.key(63), "cause", cause, "process_background_work", texts.ProcessBackgroundWorkGone)
	if unreported.Turn {
		want["in_turn"] = "true"
	}
	b.task(r, "no-report", claim.RoleReviewer, r.key(63), supervise.RelaunchNote(r.key(63), unreported, false), want)

	b.task(r, "cut-turn", claim.RoleTester, issue, supervise.CutTurnNote(issue, cause), goldenWant("issue", issue, "cause", cause))
	b.task(r, "cut-turn-no-cause", claim.RoleReviewer, r.key(61), supervise.CutTurnNote(r.key(61), ""), goldenWant("issue", r.key(61), "cause", ""))

	session := r.id("abandoned/session")
	why := r.words("abandoned/why", 160) + " \"" + goldenHostile[r.n-1][5] + "\""
	b.task(r, "worker", claim.RoleImplementer, issue, supervise.AbandonedSessionNote(issue, claim.RoleImplementer, session, why),
		goldenWant("issue", issue, "session", session, "why", why, "branch", claim.IssueBranch(issue)))
	b.task(r, "controller", claim.RoleController, "", supervise.AbandonedSessionNote("", claim.RoleController, session, why),
		goldenWant("issue", "", "session", session, "why", why))

	b.task(r, "out-of-memory", []claim.Role{claim.RoleTester, claim.RoleArchitect}[r.n-1], issue, texts.NewOutOfMemoryNote(issue),
		goldenWant("issue", issue, "reason", texts.OutOfMemoryReason))

	status := []int{529, 503}[r.n-1]
	b.task(r, "status", claim.RolePlanner, issue, supervise.ProviderErrorNote(issue, status), goldenWant("issue", issue, "http_status", status))
	b.task(r, "no-status", claim.RoleMerger, r.key(62), supervise.ProviderErrorNote(r.key(62), 0), goldenWant("issue", r.key(62)))
}
