// Package legiontest seeds one project's Legion history into a Dispatch database and answers
// what it wrote. The per-issue facts (dispatch://INFRA-2915) are counted over hundreds of issues
// at once, and a check of them can count nothing until a history that size exists. It is an
// ordinary package rather than a _test.go file, as store/storetest is, so every package whose
// tests need such a history can import it: Go cannot share test-only code across packages.
//
// Seed writes the rows a running Legion leaves behind: the project and its issues, an event of
// each role's session, the handoffs, and the observations of the phase, the slot, each role's
// claim and the waits the daemon recorded. It writes them with plain pool.Exec, as every other
// Legion test seeder does, and it batches each table into one statement. A history of 200 issues
// is some thousands of rows, and a statement per row takes minutes against a real Postgres.
//
// Fixture answers every key the history holds and which set each key belongs to, so a caller
// asserts against the names Seed wrote and never reads the database back to learn them. Seed
// also logs one line naming the project, the counts and the window, so a reader of a check's
// record can see which history the check ran over.
package legiontest

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/legion"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

const (
	// defaultProject is the project a Spec that names none is written under. The key is what
	// projects.key admits, an upper-case letter and up to nine letters or digits. A caller reads
	// the project it got back off the Fixture, so the name itself is this package's own.
	defaultProject = "LEGION"

	// DefaultTrees is how many trees a Spec that asks for no number writes. Each tree holds
	// issuesPerTree issues and five of them carry Legion phase work, so the default answers
	// facts for 210 issues, above the 200 the facts' own checks count.
	DefaultTrees = 42

	// issuesPerTree is the root and the five children one tree writes. Every one of them but
	// the follow-up a person filed carries Legion phase work.
	issuesPerTree = 6

	// treeSpan is how long before the history's end the first tree opens. Each later tree opens
	// one hour before the tree before it, so the trees overlap as a project's trees do, and the
	// whole window is treeSpan plus one hour per tree.
	treeSpan = 60 * time.Hour

	// slotCap is how many slot numbers the roots share out. A root takes index%slotCap and the
	// child it files first takes slotCap plus that, so one tree holds two numbers and the
	// history uses eight. Two roots slotCap trees apart take one number between them, and the
	// stretches they hold it over overlap, so the number alone never says which issue held it.
	slotCap = 4
)

// person is the actor of every write a person made in the history.
const person = `{"kind":"user","id":"alice"}`

// Spec is the history Seed writes: the project the issues belong to, how many trees of a root
// and its children it holds, and the moment the history ends. The caller gives its own Now, so
// the hours an open issue has run are the hours the caller's own computation measures. A zero
// field takes its default: the package's own project, DefaultTrees, and this moment.
type Spec struct {
	Project string
	Trees   int
	Now     time.Time
}

// Fixture is what Seed wrote, in the terms a caller asserts against.
type Fixture struct {
	// Project is the project every issue below belongs to.
	Project string
	// Keys is every issue of the history, in the order Seed wrote them: each tree's root, then
	// that tree's children.
	Keys []string
	// Roots is each tree's root, in the same order.
	Roots []string
	// PhaseWork is the keys that carry Legion phase work, which is the set metrics.ProjectFacts
	// answers an entry for.
	PhaseWork []string
	// Open is the keys of the issues that never closed, whose hours run to the moment of the
	// computation.
	Open []string
	// NoMeasuredWork is the keys whose work took no measured time, whose facts therefore hold an
	// entry with no phase hours at all.
	NoMeasuredWork []string
	// NoPhaseWork is the keys no Legion role ever worked, which get no facts entry: that is what
	// makes "has an entry" mean "Legion worked it".
	NoPhaseWork []string
	// Phases is the keys the sampler recorded in each phase, by the daemon's own phase name. A
	// caller reads it to state which of legion.PhaseColumns the history reaches.
	Phases map[string][]string
	// SlotHours is how long each issue that held an admission slot of its own held it, from the
	// slot observations Seed wrote as a history of taking one and giving it up.
	SlotHours map[string]float64
	// From and To are the two ends of the window the history falls in. To is the Spec's own Now.
	From, To time.Time
}

// Seed writes one project's Legion history into the database and answers what it wrote.
func Seed(t testing.TB, pool *store.Pool, spec Spec) Fixture {
	t.Helper()
	if spec.Project == "" {
		spec.Project = defaultProject
	}
	if spec.Trees <= 0 {
		spec.Trees = DefaultTrees
	}
	if spec.Now.IsZero() {
		spec.Now = time.Now().UTC()
	}
	run := &seeding{
		spec:      spec,
		seq:       map[string]int32{},
		phaseWork: map[string]bool{},
		fixture: Fixture{
			Project:   spec.Project,
			Phases:    map[string][]string{},
			SlotHours: map[string]float64{},
			From:      spec.Now.Add(-(treeSpan + time.Duration(spec.Trees)*time.Hour)),
			To:        spec.Now,
		},
	}
	for index := range spec.Trees {
		run.tree(index)
	}
	run.write(t, pool)
	fixture := run.fixture
	t.Logf("seeded %s: %d issues, %d with Legion phase work, %d open, %d with no measured work, from %s to %s",
		fixture.Project, len(fixture.Keys), len(fixture.PhaseWork), len(fixture.Open),
		len(fixture.NoMeasuredWork), fixture.From.Format(time.RFC3339), fixture.To.Format(time.RFC3339))
	return fixture
}

// seeding is one run of Seed: the spec it was given, what it has written so far, the next event
// sequence number of each issue, which events.seq is unique on, and the issues it has already
// recorded Legion phase work for.
type seeding struct {
	spec      Spec
	fixture   Fixture
	rows      rows
	seq       map[string]int32
	phaseWork map[string]bool
}

// rows is the four tables one seed writes, each held as the columns of one batched insert.
type rows struct {
	issueKeys       []string
	issueNumbers    []int32
	issueTitles     []string
	issueStatuses   []string
	issueRanks      []string
	issuePriorities []int16
	issueParents    []*string
	issueOpened     []time.Time
	issueClosed     []*time.Time

	eventIssues   []string
	eventSeqs     []int32
	eventTypes    []string
	eventActors   []string
	eventPayloads []string
	eventAt       []time.Time

	handoffIssues  []string
	handoffPhases  []string
	handoffAuthors []string
	handoffAt      []time.Time

	observeIssues []string
	observeFields []string
	observeValues []string
	observeAt     []time.Time
}

// session is the actor of a Legion role's session working issue, carrying the title Dispatch
// learns the session by. Every reader of the phase-work rule resolves a session's newest title
// off the session's own events (delivery.ResolveSessionTitles), so the title belongs on the
// actor of each event it writes.
func session(role, issue string) string {
	return fmt.Sprintf(`{"kind":"session","id":"s-%s-%s","origin":{"session_title":%q}}`,
		role, issue, "Legion "+role+" · "+issue)
}

// phaseMark is one phase the sampler recorded of an issue, and how many hours into its tree's
// history it recorded it.
type phaseMark struct {
	hour  float64
	phase string
}

// tree writes one root and its five children: one Legion tree from the architect's first comment
// to the merger's close, with every phase the daemon reports recorded somewhere in it.
//
// The five children are the five shapes a reader of the facts must handle. The first is
// implemented, tested and sent back once, so its facts hold two review rounds. The second is
// reviewed and merged, and a person answered an ask of it, so its hours land on waiting for a
// person as well as on its phases. The third never closed, so its hours run to the moment of the
// computation, and it sat parked for six of them. The fourth recorded its one handoff at the
// moment it closed, so its work took no measured time and its facts hold an entry with no phase
// hours. The fifth is a follow-up a person filed and no Legion role worked, so it gets no facts
// entry at all.
func (s *seeding) tree(index int) {
	start := s.spec.Now.Add(-(treeSpan + time.Duration(index)*time.Hour))
	at := func(hours float64) time.Time {
		return start.Add(time.Duration(hours * float64(time.Hour)))
	}
	number := index*issuesPerTree + 1

	// The root. Its architect designs it while it holds a slot, its planner plans it, it waits on
	// its children through its accepting phase, a person answers its ask, and its merger closes
	// it once the merge is done.
	root := s.issue(number, "A Legion root", "", 1, at(0), new(at(56)))
	s.fixture.Roots = append(s.fixture.Roots, root)
	s.event(root, "issue.created", person, `{"status":"triage"}`, at(0))
	s.roleEvent(root, "architect", "comment.created", `{"status":"in_progress"}`, at(1))
	s.roleEvent(root, "planner", "comment.created", `{}`, at(6))
	s.handoff(root, "plan", "planner", at(8))
	s.ask(root, "architect", at(30), at(34))
	s.roleEvent(root, "merger", "issue.closed", `{"status":"done"}`, at(56))
	s.slot(root, index%slotCap, at(1), at(50))
	s.claim(root, "architect", at(1), at(6))
	s.claim(root, "planner", at(6), at(8))
	s.phases(root, at, []phaseMark{
		{1, legion.PhaseAdmitted},
		{6, legion.PhasePlanning},
		{20, legion.PhaseAccepting},
		{30, legion.PhaseAwaitingPerson},
		{34, legion.PhaseAccepting},
		{50, legion.PhaseAwaitingMerge},
		{56, legion.PhaseDone},
	})
	s.waiting(root, "gate", at(30), at(34))

	// The child the architect filed first: implemented, tested, sent back once and reviewed
	// again, so its facts hold two review rounds. It holds a slot of its own, which is what
	// keeps a reader from taking every slot holder for a root.
	implemented := s.issue(number+1, "An implemented child", root, 2, at(2), new(at(24)))
	s.event(implemented, "issue.created", session("architect", root), filing(root), at(2))
	s.roleEvent(implemented, "implementer", "comment.created", `{"status":"in_progress"}`, at(4))
	s.handoff(implemented, "implement", "implementer", at(12))
	s.roleEvent(implemented, "tester", "comment.created", `{}`, at(14))
	s.handoff(implemented, "test", "tester", at(15))
	s.roleEvent(implemented, "merger", "issue.closed", `{"status":"done"}`, at(24))
	s.slot(implemented, slotCap+index%slotCap, at(4), at(24))
	s.claim(implemented, "implementer", at(4), at(12))
	s.claim(implemented, "tester", at(14), at(15))
	s.phases(implemented, at, []phaseMark{
		{4, legion.PhaseImplementing},
		{14, legion.PhaseTesting},
		{16, legion.PhaseReviewing},
		{18, legion.PhaseImplementing},
		{20, legion.PhaseReviewing},
		{24, legion.PhaseDone},
	})

	// The child a reviewer and a merger took through: a person answered its ask, and it is the
	// one issue of the tree that reaches all three of the merger's phases.
	reviewed := s.issue(number+2, "A reviewed child", root, 2, at(5), new(at(40)))
	s.event(reviewed, "issue.created", session("architect", root), filing(root), at(5))
	s.roleEvent(reviewed, "implementer", "comment.created", `{"status":"in_progress"}`, at(10))
	s.roleEvent(reviewed, "reviewer", "comment.created", `{"status":"needs_review"}`, at(26))
	s.ask(reviewed, "reviewer", at(28), at(31))
	s.handoff(reviewed, "review", "reviewer", at(33))
	s.roleEvent(reviewed, "merger", "comment.created", `{}`, at(34))
	s.event(reviewed, "issue.closed", session("merger", reviewed), `{"status":"done"}`, at(40))
	s.claim(reviewed, "implementer", at(10), at(26))
	s.claim(reviewed, "reviewer", at(26), at(33))
	s.claim(reviewed, "merger", at(34), at(38))
	s.phases(reviewed, at, []phaseMark{
		{10, legion.PhaseImplementing},
		{26, legion.PhaseReviewing},
		{28, legion.PhaseAwaitingPerson},
		{31, legion.PhaseAccepting},
		{34, legion.PhaseRetro},
		{36, legion.PhaseMerging},
		{40, legion.PhaseDone},
	})
	s.waiting(reviewed, "ask", at(28), at(31))

	// The child nobody closed. Its work time runs to the moment of the computation, and the six
	// hours it sat parked are off it. It ends in `held`, the one phase that leaves its root free
	// to accept and to merge while the child is still open.
	open := s.issue(number+3, "An open child", root, 2, at(8), nil)
	s.fixture.Open = append(s.fixture.Open, open)
	s.event(open, "issue.created", session("architect", root), filing(root), at(8))
	s.roleEvent(open, "implementer", "comment.created", `{"status":"in_progress"}`, at(12))
	s.event(open, "issue.updated", person, `{"status":"backlog"}`, at(20))
	s.event(open, "issue.updated", person, `{"status":"in_progress"}`, at(26))
	s.claim(open, "implementer", at(12), at(20))
	s.claim(open, "tester", at(38), at(44))
	s.phases(open, at, []phaseMark{
		{12, legion.PhaseImplementing},
		{20, legion.PhaseHeld},
		{26, legion.PhaseImplementing},
		{38, legion.PhaseTesting},
		{44, legion.PhaseHeld},
	})

	// The child whose work took no measured time: its one handoff is the moment it closed, so
	// its work time starts and ends together and its facts hold no phase hours. The sampler
	// never saw it, which is why a handoff alone has to carry it into the facts.
	instant := s.issue(number+4, "A child that closed where it opened", root, 3, at(46), new(at(46)))
	s.fixture.NoMeasuredWork = append(s.fixture.NoMeasuredWork, instant)
	s.event(instant, "issue.created", person, `{"status":"triage"}`, at(46))
	s.handoff(instant, "implement", "implementer", at(46))
	s.event(instant, "issue.closed", person, `{"status":"done"}`, at(46))

	// The follow-up a person filed and closed. No role session wrote on it and it carries no
	// handoff, so it has no Legion phase work and must get no facts entry.
	untouched := s.issue(number+5, "A follow-up nobody worked", root, 3, at(48), new(at(52)))
	s.fixture.NoPhaseWork = append(s.fixture.NoPhaseWork, untouched)
	s.event(untouched, "issue.created", person, `{"status":"triage"}`, at(48))
	s.event(untouched, "issue.closed", person, `{"status":"done"}`, at(52))
}

// filing is the payload of the `issue.created` event a root's architect writes when it files a
// child: the status the child starts at, and the tree it is filed under.
func filing(root string) string {
	return fmt.Sprintf(`{"status":"triage","parent":%q,"ancestors":[%q]}`, root, root)
}

// issue writes one issue of the project and answers its key. An issue with no close is open, and
// a rank of its padded number puts Dispatch's rank order over the history in key order.
func (s *seeding) issue(number int, title, parent string, priority int16, opened time.Time, closed *time.Time) string {
	key := fmt.Sprintf("%s-%d", s.spec.Project, number)
	status := "in_progress"
	if closed != nil {
		status = "done"
	}
	var parentKey *string
	if parent != "" {
		parentKey = &parent
	}
	s.rows.issueKeys = append(s.rows.issueKeys, key)
	s.rows.issueNumbers = append(s.rows.issueNumbers, int32(number))
	s.rows.issueTitles = append(s.rows.issueTitles, title)
	s.rows.issueStatuses = append(s.rows.issueStatuses, status)
	s.rows.issueRanks = append(s.rows.issueRanks, fmt.Sprintf("%08d", number))
	s.rows.issuePriorities = append(s.rows.issuePriorities, priority)
	s.rows.issueParents = append(s.rows.issueParents, parentKey)
	s.rows.issueOpened = append(s.rows.issueOpened, opened)
	s.rows.issueClosed = append(s.rows.issueClosed, closed)
	s.fixture.Keys = append(s.fixture.Keys, key)
	return key
}

// event writes one event of an issue, at that issue's next sequence number.
func (s *seeding) event(issue, kind, actor, payload string, at time.Time) {
	s.seq[issue]++
	s.rows.eventIssues = append(s.rows.eventIssues, issue)
	s.rows.eventSeqs = append(s.rows.eventSeqs, s.seq[issue])
	s.rows.eventTypes = append(s.rows.eventTypes, kind)
	s.rows.eventActors = append(s.rows.eventActors, actor)
	s.rows.eventPayloads = append(s.rows.eventPayloads, payload)
	s.rows.eventAt = append(s.rows.eventAt, at)
}

// roleEvent writes one event of a session titled for the role working that same issue, which is
// what makes the issue carry that role's phase work (legion.RoleSessionWorks). An event of a
// root's architect on a child it filed goes through event instead, because that title names the
// root and so is no phase work of the child.
func (s *seeding) roleEvent(issue, role, kind, payload string, at time.Time) {
	s.event(issue, kind, session(role, issue), payload, at)
	s.carriesPhaseWork(issue)
}

// ask writes one ask of the issue that the role's session opened and a person answered, which is
// the stretch the facts count as waiting on a person.
func (s *seeding) ask(issue, role string, opened, answered time.Time) {
	payload := fmt.Sprintf(`{"id":"ask-%s"}`, issue)
	s.roleEvent(issue, role, "ask.opened", payload, opened)
	s.event(issue, "ask.answered", person, payload, answered)
}

// handoff writes one phase handoff of the issue. A handoff is Legion phase work of its own, so
// an issue whose only Legion record is a handoff still has facts.
func (s *seeding) handoff(issue, phase, role string, at time.Time) {
	s.rows.handoffIssues = append(s.rows.handoffIssues, issue)
	s.rows.handoffPhases = append(s.rows.handoffPhases, phase)
	s.rows.handoffAuthors = append(s.rows.handoffAuthors, session(role, issue))
	s.rows.handoffAt = append(s.rows.handoffAt, at)
	s.carriesPhaseWork(issue)
}

// carriesPhaseWork records the issue as one metrics.ProjectFacts answers an entry for, once
// however many role events and handoffs it carries.
func (s *seeding) carriesPhaseWork(issue string) {
	if s.phaseWork[issue] {
		return
	}
	s.phaseWork[issue] = true
	s.fixture.PhaseWork = append(s.fixture.PhaseWork, issue)
}

// observe writes one observation of one field of one issue, as the sampler records a value it
// found had moved.
func (s *seeding) observe(issue, field, value string, at time.Time) {
	s.rows.observeIssues = append(s.rows.observeIssues, issue)
	s.rows.observeFields = append(s.rows.observeFields, field)
	s.rows.observeValues = append(s.rows.observeValues, value)
	s.rows.observeAt = append(s.rows.observeAt, at)
}

// phases writes the phases the sampler recorded of one issue, in the order it recorded them.
func (s *seeding) phases(issue string, at func(float64) time.Time, marks []phaseMark) {
	for _, mark := range marks {
		s.observe(issue, legion.FieldPhase, fmt.Sprintf("%q", mark.phase), at(mark.hour))
		s.fixture.Phases[mark.phase] = append(s.fixture.Phases[mark.phase], issue)
	}
}

// slot writes the history of one admission slot: the moment the issue took it, and the moment it
// gave it up. Two rows and not one, because a reader that measures how long the issue held a
// slot needs an end as much as a start.
func (s *seeding) slot(issue string, index int, taken, released time.Time) {
	s.observe(issue, legion.FieldSlot, fmt.Sprintf("%d", index), taken)
	s.observe(issue, legion.FieldSlot, "null", released)
	s.fixture.SlotHours[issue] = released.Sub(taken).Hours()
}

// claim writes one role's turn on the issue: the moment the role started to work and the moment
// it stopped, which is the stretch the facts count as time in a turn.
func (s *seeding) claim(issue, role string, started, ended time.Time) {
	s.observe(issue, legion.ClaimFieldPrefix+role, `"working"`, started)
	s.observe(issue, legion.ClaimFieldPrefix+role, `"idle"`, ended)
}

// waiting writes the wait the daemon recorded of the issue: the wait a read found while the
// person still held it, and the same wait answered.
func (s *seeding) waiting(issue, kind string, since, answered time.Time) {
	held := fmt.Sprintf(`{"kind":%q,"since":%q}`, kind, since.Format(time.RFC3339))
	done := fmt.Sprintf(`{"kind":%q,"since":%q,"answeredAt":%q}`,
		kind, since.Format(time.RFC3339), answered.Format(time.RFC3339))
	s.observe(issue, legion.FieldWaitingOn, held, since)
	s.observe(issue, legion.FieldWaitingOn, done, answered)
}

// write stores everything the trees built, one statement per table. Each statement takes its
// table's columns as arrays and unnests them, so a history of thousands of rows costs four round
// trips rather than thousands.
func (s *seeding) write(t testing.TB, pool *store.Pool) {
	t.Helper()
	ctx := context.Background()
	exec := func(what, statement string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, statement, args...); err != nil {
			t.Fatalf("seed the %s of %s (%s): %v", what, s.spec.Project, statement, err)
		}
	}
	exec("project", `insert into projects (key, name, next_number) values ($1, $1, $2)`,
		s.spec.Project, len(s.rows.issueKeys)+1)
	exec("issues", `
		insert into issues (key, project_key, number, title, status, rank, priority, parent_key,
			created_by, created_at, closed_at)
		select key, $1, number, title, status, rank, priority, parent, $2::jsonb, opened, closed
		from unnest($3::text[], $4::int[], $5::text[], $6::text[], $7::text[], $8::smallint[],
			$9::text[], $10::timestamptz[], $11::timestamptz[])
			as seeded(key, number, title, status, rank, priority, parent, opened, closed)`,
		s.spec.Project, person, s.rows.issueKeys, s.rows.issueNumbers, s.rows.issueTitles,
		s.rows.issueStatuses, s.rows.issueRanks, s.rows.issuePriorities, s.rows.issueParents,
		s.rows.issueOpened, s.rows.issueClosed)
	exec("events", `
		insert into events (issue_key, seq, type, actor, payload, notify, created_at)
		select issue, seq, kind, actor::jsonb, payload::jsonb, false, at
		from unnest($1::text[], $2::int[], $3::text[], $4::text[], $5::text[], $6::timestamptz[])
			as seeded(issue, seq, kind, actor, payload, at)`,
		s.rows.eventIssues, s.rows.eventSeqs, s.rows.eventTypes, s.rows.eventActors,
		s.rows.eventPayloads, s.rows.eventAt)
	exec("handoffs", `
		insert into handoffs (issue_key, phase, version, body, author, created_at)
		select issue, phase, 1, '{}', author::jsonb, at
		from unnest($1::text[], $2::text[], $3::text[], $4::timestamptz[])
			as seeded(issue, phase, author, at)`,
		s.rows.handoffIssues, s.rows.handoffPhases, s.rows.handoffAuthors, s.rows.handoffAt)
	exec("observations", `
		insert into legion_observations (project, issue_key, field, value, observed_at)
		select $1, issue, field, value::jsonb, at
		from unnest($2::text[], $3::text[], $4::text[], $5::timestamptz[])
			as seeded(issue, field, value, at)`,
		s.spec.Project, s.rows.observeIssues, s.rows.observeFields, s.rows.observeValues,
		s.rows.observeAt)
}
