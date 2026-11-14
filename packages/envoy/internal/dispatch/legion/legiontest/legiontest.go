// Package legiontest seeds one project's Legion history into a Dispatch database and answers
// what it wrote. The per-issue facts (dispatch://INFRA-2915) are counted over hundreds of issues
// at once, and a check of them can count nothing until a history that size exists. It is an
// ordinary package rather than a _test.go file, as store/storetest is, so every package whose
// tests need such a history can import it (Go cannot share test-only code across packages), and
// a command can write the same history through Write (evals/evalfixture/cmd).
//
// Write stores the rows a running Legion leaves behind: the project and its issues, an event of
// each role's session, the handoffs, and the observations of the phase, the slot, each role's
// claim and the waits the daemon recorded. It writes them with plain Exec, as every other Legion
// test seeder does, and it batches each table into one statement. A history of 200 issues is
// some thousands of rows, and a statement per row takes minutes against a real Postgres. Its
// events bypass events.Broker.Append, so a page that was open when they were written may not see
// them until it reloads; a reader of the tables, which every check of them is, sees them all.
//
// Fixture answers every key the history holds and which set each key belongs to, so a caller
// asserts against the names Write wrote and never reads the database back to learn them. Seed
// also logs one line naming the project, the counts and the window (Fixture.Summary), so a
// reader of a check's record can see which history the check ran over.
package legiontest

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/legion"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// askNamespace is the uuid namespace every ask id this rig writes is derived under (askID), so
// the ids are deterministic, are this rig's own, and read as the uuid every reader of an ask
// casts them to.
var askNamespace = uuid.MustParse("6f1b4b0e-2f1a-4a5f-9a3d-8c7c1f0b2d41")

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

	// FinishedPerTree is how many issues of one tree finish with Legion phase work, which are the
	// issues a Legion measure counts: the root and three of its children, since one child never
	// closes and the follow-up a person filed carries no phase work.
	FinishedPerTree = 4

	// treeSpan is how long before the history's end the first tree opens. Each later tree opens
	// one hour (Spec.Spacing) before the tree before it, so the trees overlap as a project's trees
	// do, and the whole window is treeSpan plus one hour per tree.
	treeSpan = 60 * time.Hour

	// slotCap is how many slot numbers the roots share out. A root takes index%slotCap and the
	// child it files first takes slotCap plus that, so one tree holds two numbers and the
	// history uses eight. Two roots slotCap trees apart take one number between them, and the
	// stretches they hold it over overlap, so the number alone never says which issue held it.
	slotCap = 4
)

// person is the actor of every write a person made in the history.
const person = `{"kind":"user","id":"alice"}`

// Spec is the history Write writes: the project the issues belong to, how many trees of a root
// and its children it holds, and the moment the history ends. The caller gives its own Now, so
// the hours an open issue has run are the hours the caller's own computation measures. A zero
// field takes its default: the package's own project, DefaultTrees, and this moment.
//
// The other fields are off where zero, and a Spec that sets none of them writes the history every
// golden file of this change was recorded over, row for row. They shape the history a check of
// the eval registry's Legion measures reads (dispatch://INFRA-2916, k6 and k17), which needs a
// window before the project's first observation and one after it, finished issues that sat
// parked, merged pull requests and filings of every class.
type Spec struct {
	Project string
	Trees   int
	Now     time.Time

	// Spacing is how long before the tree after it each tree opens: one hour where zero. A wider
	// spacing spreads the same trees over more days.
	Spacing time.Duration
	// Unobserved is how many of the oldest trees the daemon's sampler never saw: their events
	// and handoffs are written and no observation of them is, so the project's first
	// observation falls partway through the history and the issues before it carry the event
	// log alone, as a project's issues did before Dispatch read its daemon.
	Unobserved int
	// Queued has each observed root wait for a slot: the sampler records it admitted from its
	// creation, an hour before it takes its slot, and the root of each odd tree gives its slot
	// up for the fourth hour of its history and waits again. Without it no issue of the history
	// ever waits for a slot.
	Queued bool
	// IssueOffset is how many of the project's issue numbers another history took already: the
	// issues are numbered from IssueOffset+1, so this history can be written beside that one.
	IssueOffset int
	// Parked has a finished child of every tree sit parked partway through its work, each park
	// both a Dispatch status and what the daemon observes: in an even tree the implemented child
	// sits in `icebox` for the two hours between its implementer's handoff and its tester, put
	// there by a person, and in an odd tree the reviewed child is parked in `backlog` for three
	// hours of its implementing by its tree's architect, as the daemon's parkChild moves a child.
	// The sampler records phase `held` over each park, and the phase it returns to after it.
	Parked bool
	// Merged gives each issue the merger closes (each root and its implemented and reviewed
	// children) a merged pull request naming it, merged an hour before its close, so the facts
	// read its merge (metrics' Issue.MergedAt) and store it.
	Merged bool
	// Filings has each tree's Legion sessions file one issue of each class metric 4 counts besides
	// the architect's own at design (metrics.ClassInTreeAtDesign): the implemented child's
	// implementer one into the tree after its first implementer event (in_tree_later), the
	// reviewed child's reviewer one with no parent (out_of_tree), and the project's controller one
	// (by_the_controller). A person closes each an hour after it is filed, and no Legion role works
	// it. They are numbered after every tree's issues, so the trees keep their numbers.
	Filings bool
}

// spacing is how long before the tree after it each tree opens.
func (s Spec) spacing() time.Duration {
	if s.Spacing <= 0 {
		return time.Hour
	}
	return s.Spacing
}

// Execer is the database connection Write needs: one that runs a statement. A pool, a connection
// or a transaction is one.
type Execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Fixture is what Write wrote, in the terms a caller asserts against.
type Fixture struct {
	// Project is the project every issue below belongs to.
	Project string
	// Keys is every issue of the history, in the order Write wrote them: each tree's root, then
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
	// slot observations Write wrote as a history of taking one and giving it up.
	SlotHours map[string]float64
	// From and To are the two ends of the window the history falls in. To is the Spec's own Now.
	From, To time.Time
	// FirstObserved is the moment of the oldest observation the history holds, which is where
	// the project's observed history begins; zero for a history with none. FirstFinished is the
	// earliest close of an issue with Legion phase work, the first day a Legion measure has
	// input.
	FirstObserved, FirstFinished time.Time
}

// Summary is one line naming the history and its counts, which Seed logs and a command prints.
func (f Fixture) Summary() string {
	return fmt.Sprintf("seeded %s: %d issues, %d with Legion phase work, %d open, %d with no measured work, from %s to %s",
		f.Project, len(f.Keys), len(f.PhaseWork), len(f.Open), len(f.NoMeasuredWork),
		f.From.Format(time.RFC3339), f.To.Format(time.RFC3339))
}

// Seed writes one project's Legion history into the database (Write), fails the test if it
// cannot, logs one line naming what it wrote, and answers it.
func Seed(t testing.TB, pool *store.Pool, spec Spec) Fixture {
	t.Helper()
	fixture, err := Write(t.Context(), pool, spec)
	if err != nil {
		t.Fatalf("seed the Legion history: %v", err)
	}
	t.Log(fixture.Summary())
	return fixture
}

// Write writes the history spec names into the database q is on and answers what it wrote. The
// project's row is written where the project has none and kept where it has one, so the history
// can join a project another history, or a person, made first.
func Write(ctx context.Context, q Execer, spec Spec) (Fixture, error) {
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
			From:      spec.Now.Add(-(treeSpan + time.Duration(spec.Trees)*spec.spacing())),
			To:        spec.Now,
		},
	}
	for index := range spec.Trees {
		run.tree(index)
	}
	for index, key := range run.rows.issueKeys {
		closed := run.rows.issueClosed[index]
		if closed != nil && run.phaseWork[key] && (run.fixture.FirstFinished.IsZero() || closed.Before(run.fixture.FirstFinished)) {
			run.fixture.FirstFinished = *closed
		}
	}
	if err := run.write(ctx, q); err != nil {
		return Fixture{}, err
	}
	return run.fixture, nil
}

// seeding is one run of Write: the spec it was given, what it has written so far, the next event
// sequence number of each issue, which events.seq is unique on, the issues it has already
// recorded Legion phase work for, and whether the sampler saw the tree it is writing.
type seeding struct {
	spec      Spec
	fixture   Fixture
	rows      rows
	seq       map[string]int32
	phaseWork map[string]bool
	observed  bool
}

// rows is the five tables one seed writes beside its project's row (issues, the pull requests,
// events, handoffs and observations), each held as the columns of one batched insert.
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

	prRepos   []string
	prNumbers []int32
	prTitles  []string
	prURLs    []string
	prIssues  []string
	prCreated []time.Time
	prMerged  []time.Time
}

// session is the actor of a Legion role's session working issue, carrying the title Dispatch
// learns the session by. Every reader of the phase-work rule resolves a session's newest title
// (delivery.ResolveSessionTitles) from the session's own events, whose insert records it
// (migration 20261113090412's trigger, which fires for this package's bare inserts too), so the
// title belongs on the actor of each event it writes.
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
	start := s.spec.Now.Add(-(treeSpan + time.Duration(index)*s.spec.spacing()))
	at := func(hours float64) time.Time {
		return start.Add(time.Duration(hours * float64(time.Hour)))
	}
	number := s.spec.IssueOffset + index*issuesPerTree + 1
	// The oldest Spec.Unobserved trees are the ones the sampler never saw.
	s.observed = index < s.spec.Trees-s.spec.Unobserved

	// The root. Its architect designs it while it holds a slot, its planner plans it, it waits on
	// its children through its accepting phase, a person answers its ask, and its merger closes
	// it once the merge is done. Under Spec.Queued it waits for its slot first, from its creation
	// to its architect's first hour, and the root of an odd tree waits again for the fourth hour
	// of its history.
	root := s.issue(number, "A Legion root", "", 1, at(0), new(at(56)))
	s.fixture.Roots = append(s.fixture.Roots, root)
	s.event(root, "issue.created", person, `{"status":"triage"}`, at(0))
	s.roleEvent(root, "architect", "comment.created", `{"status":"in_progress"}`, at(1))
	s.roleEvent(root, "planner", "comment.created", `{}`, at(6))
	s.handoff(root, "plan", "planner", at(8))
	s.ask(root, "architect", at(30), at(34))
	s.roleEvent(root, "merger", "issue.closed", `{"status":"done"}`, at(56))
	if s.spec.Merged {
		s.pullRequest(root, number, at(1), at(55))
	}
	admitted := 1.0
	if s.spec.Queued {
		admitted = 0
	}
	if s.spec.Queued && index%2 == 1 {
		s.slot(root, index%slotCap, at(1), at(3))
		s.slot(root, index%slotCap, at(4), at(50))
	} else {
		s.slot(root, index%slotCap, at(1), at(50))
	}
	s.claim(root, "architect", at(1), at(6))
	s.claim(root, "planner", at(6), at(8))
	s.phases(root, at, []phaseMark{
		{admitted, legion.PhaseAdmitted},
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
	// Under Spec.Parked an even tree's implemented child waits in `icebox` from its implementer's
	// handoff to its tester, a person's park.
	icebox := s.spec.Parked && index%2 == 0
	if icebox {
		s.event(implemented, "issue.updated", person, `{"status":"icebox"}`, at(12))
		s.event(implemented, "issue.updated", person, `{"status":"in_progress"}`, at(14))
	}
	s.roleEvent(implemented, "tester", "comment.created", `{}`, at(14))
	s.handoff(implemented, "test", "tester", at(15))
	s.roleEvent(implemented, "merger", "issue.closed", `{"status":"done"}`, at(24))
	if s.spec.Merged {
		s.pullRequest(implemented, number+1, at(4), at(23))
	}
	s.slot(implemented, slotCap+index%slotCap, at(4), at(24))
	s.claim(implemented, "implementer", at(4), at(12))
	s.claim(implemented, "tester", at(14), at(15))
	implementedPhases := []phaseMark{{4, legion.PhaseImplementing}}
	if icebox {
		implementedPhases = append(implementedPhases, phaseMark{12, legion.PhaseHeld})
	}
	s.phases(implemented, at, append(implementedPhases,
		phaseMark{14, legion.PhaseTesting},
		phaseMark{16, legion.PhaseReviewing},
		phaseMark{18, legion.PhaseImplementing},
		phaseMark{20, legion.PhaseReviewing},
		phaseMark{24, legion.PhaseDone},
	))

	// The child a reviewer and a merger took through: a person answered its ask, and it is the
	// one issue of the tree that reaches all three of the merger's phases.
	reviewed := s.issue(number+2, "A reviewed child", root, 2, at(5), new(at(40)))
	s.event(reviewed, "issue.created", session("architect", root), filing(root), at(5))
	s.roleEvent(reviewed, "implementer", "comment.created", `{"status":"in_progress"}`, at(10))
	// Under Spec.Parked an odd tree's reviewed child is parked in `backlog` for three hours of its
	// implementing by its tree's architect, as the daemon's parkChild moves a child, and put back.
	backlog := s.spec.Parked && index%2 == 1
	if backlog {
		s.event(reviewed, "issue.updated", session("architect", root), `{"status":"backlog"}`, at(16))
		s.event(reviewed, "issue.updated", session("architect", root), `{"status":"in_progress"}`, at(19))
	}
	s.roleEvent(reviewed, "reviewer", "comment.created", `{"status":"needs_review"}`, at(26))
	s.ask(reviewed, "reviewer", at(28), at(31))
	s.handoff(reviewed, "review", "reviewer", at(33))
	s.roleEvent(reviewed, "merger", "comment.created", `{}`, at(34))
	s.event(reviewed, "issue.closed", session("merger", reviewed), `{"status":"done"}`, at(40))
	if s.spec.Merged {
		s.pullRequest(reviewed, number+2, at(10), at(39))
	}
	s.claim(reviewed, "implementer", at(10), at(26))
	s.claim(reviewed, "reviewer", at(26), at(33))
	s.claim(reviewed, "merger", at(34), at(38))
	reviewedPhases := []phaseMark{{10, legion.PhaseImplementing}}
	if backlog {
		reviewedPhases = append(reviewedPhases, phaseMark{16, legion.PhaseHeld}, phaseMark{19, legion.PhaseImplementing})
	}
	s.phases(reviewed, at, append(reviewedPhases,
		phaseMark{26, legion.PhaseReviewing},
		phaseMark{28, legion.PhaseAwaitingPerson},
		phaseMark{31, legion.PhaseAccepting},
		phaseMark{34, legion.PhaseRetro},
		phaseMark{36, legion.PhaseMerging},
		phaseMark{40, legion.PhaseDone},
	))
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

	// Under Spec.Filings, one filing of each class but the architect's at design, each numbered
	// after every tree's issues and closed by a person an hour after it was filed.
	if s.spec.Filings {
		extra := s.spec.IssueOffset + s.spec.Trees*issuesPerTree + index*filingsPerTree + 1
		// Into the tree, under the implemented child, after its implementer's first event.
		later := s.issue(extra, "A follow-up the implementer filed", implemented, 3, at(10), new(at(11)))
		s.event(later, "issue.created", session("implementer", implemented),
			fmt.Sprintf(`{"status":"triage","parent":%q,"ancestors":[%q,%q]}`, implemented, implemented, root), at(10))
		s.event(later, "issue.closed", person, `{"status":"done"}`, at(11))
		// Under no parent at all, so in no tree of the reviewer's.
		outside := s.issue(extra+1, "An issue the reviewer filed outside the tree", "", 3, at(30), new(at(31)))
		s.event(outside, "issue.created", session("reviewer", reviewed), `{"status":"triage"}`, at(30))
		s.event(outside, "issue.closed", person, `{"status":"done"}`, at(31))
		// By the project's controller, which works no issue.
		controlled := s.issue(extra+2, "An issue the controller filed", "", 3, at(48), new(at(49)))
		s.event(controlled, "issue.created", controller(s.spec.Project), `{"status":"triage"}`, at(48))
		s.event(controlled, "issue.closed", person, `{"status":"done"}`, at(49))
		s.fixture.NoPhaseWork = append(s.fixture.NoPhaseWork, later, outside, controlled)
	}
}

// filingsPerTree is how many issues Spec.Filings files in each tree.
const filingsPerTree = 3

// controller is the actor of the project's Legion controller session, titled as Legion titles it
// (`Legion controller · <PROJECT>`).
func controller(project string) string {
	return fmt.Sprintf(`{"kind":"session","id":"s-controller-%s","origin":{"session_title":%q}}`, project, "Legion controller · "+project)
}

// pullRequest records the merged pull request of issue, numbered as the issue is, in the project's
// own repository: opened at its first worker event and merged at merged.
func (s *seeding) pullRequest(issue string, number int, opened, merged time.Time) {
	repo := "acme/" + strings.ToLower(s.spec.Project)
	s.rows.prRepos = append(s.rows.prRepos, repo)
	s.rows.prNumbers = append(s.rows.prNumbers, int32(number))
	s.rows.prTitles = append(s.rows.prTitles, "The work of "+issue)
	s.rows.prURLs = append(s.rows.prURLs, fmt.Sprintf("https://github.com/%s/pull/%d", repo, number))
	s.rows.prIssues = append(s.rows.prIssues, issue)
	s.rows.prCreated = append(s.rows.prCreated, opened)
	s.rows.prMerged = append(s.rows.prMerged, merged)
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
//
// The id is a uuid, because every reader of an ask casts it to one: refs/ask_blocks.go's
// askPartRows reads `where s.ask_id = any($1::uuid[])`, and the event stream relay runs that for
// each event it carries, so an id of any other shape stops the relay on every event of this
// history and no event reaches a subscribed page again. It is derived from the issue key rather
// than drawn fresh, so two seedings of one history write one id and a test can name it.
func (s *seeding) ask(issue, role string, opened, answered time.Time) {
	payload := fmt.Sprintf(`{"id":%q}`, askID(issue))
	s.roleEvent(issue, role, "ask.opened", payload, opened)
	s.event(issue, "ask.answered", person, payload, answered)
}

// askID is the id of the one ask this rig writes for an issue: a uuid derived from the issue's
// key under askNamespace, so it is this rig's own and no deployment's ask can carry it.
func askID(issue string) string {
	return uuid.NewSHA1(askNamespace, []byte(issue)).String()
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
	if s.fixture.FirstObserved.IsZero() || at.Before(s.fixture.FirstObserved) {
		s.fixture.FirstObserved = at
	}
}

// phases writes the phases the sampler recorded of one issue, in the order it recorded them. It,
// slot, claim and waiting write nothing of a tree the sampler never saw (Spec.Unobserved).
func (s *seeding) phases(issue string, at func(float64) time.Time, marks []phaseMark) {
	if !s.observed {
		return
	}
	for _, mark := range marks {
		s.observe(issue, legion.FieldPhase, fmt.Sprintf("%q", mark.phase), at(mark.hour))
		s.fixture.Phases[mark.phase] = append(s.fixture.Phases[mark.phase], issue)
	}
}

// slot writes the history of one admission slot: the moment the issue took it, and the moment it
// gave it up. Two rows and not one, because a reader that measures how long the issue held a
// slot needs an end as much as a start. An issue that takes a slot twice holds the two stretches'
// hours together.
func (s *seeding) slot(issue string, index int, taken, released time.Time) {
	if !s.observed {
		return
	}
	s.observe(issue, legion.FieldSlot, fmt.Sprintf("%d", index), taken)
	s.observe(issue, legion.FieldSlot, "null", released)
	s.fixture.SlotHours[issue] += released.Sub(taken).Hours()
}

// claim writes one role's turn on the issue: the moment the role started to work and the moment
// it stopped, which is the stretch the facts count as time in a turn.
func (s *seeding) claim(issue, role string, started, ended time.Time) {
	if !s.observed {
		return
	}
	s.observe(issue, legion.ClaimFieldPrefix+role, `"working"`, started)
	s.observe(issue, legion.ClaimFieldPrefix+role, `"idle"`, ended)
}

// waiting writes the wait the daemon recorded of the issue: the wait a read found while the
// person still held it, and the same wait answered.
func (s *seeding) waiting(issue, kind string, since, answered time.Time) {
	if !s.observed {
		return
	}
	held := fmt.Sprintf(`{"kind":%q,"since":%q}`, kind, since.Format(time.RFC3339))
	done := fmt.Sprintf(`{"kind":%q,"since":%q,"answeredAt":%q}`,
		kind, since.Format(time.RFC3339), answered.Format(time.RFC3339))
	s.observe(issue, legion.FieldWaitingOn, held, since)
	s.observe(issue, legion.FieldWaitingOn, done, answered)
}

// write stores everything the trees built, one statement per table. Each statement takes its
// table's columns as arrays and unnests them, so a history of thousands of rows costs six round
// trips (the project's row and the five tables) rather than thousands. The project's row is kept
// where it exists already.
func (s *seeding) write(ctx context.Context, q Execer) error {
	exec := func(what, statement string, args ...any) error {
		if _, err := q.Exec(ctx, statement, args...); err != nil {
			return fmt.Errorf("write the %s of %s: %w", what, s.spec.Project, err)
		}
		return nil
	}
	if err := exec("project", `insert into projects (key, name, next_number) values ($1, $1, $2) on conflict (key) do nothing`,
		s.spec.Project, s.spec.IssueOffset+len(s.rows.issueKeys)+1); err != nil {
		return err
	}
	if err := exec("issues", `
		insert into issues (key, project_key, number, title, status, rank, priority, parent_key,
			created_by, created_at, closed_at)
		select key, $1, number, title, status, rank, priority, parent, $2::jsonb, opened, closed
		from unnest($3::text[], $4::int[], $5::text[], $6::text[], $7::text[], $8::smallint[],
			$9::text[], $10::timestamptz[], $11::timestamptz[])
			as seeded(key, number, title, status, rank, priority, parent, opened, closed)`,
		s.spec.Project, person, s.rows.issueKeys, s.rows.issueNumbers, s.rows.issueTitles,
		s.rows.issueStatuses, s.rows.issueRanks, s.rows.issuePriorities, s.rows.issueParents,
		s.rows.issueOpened, s.rows.issueClosed); err != nil {
		return err
	}
	// The pull requests of Spec.Merged, each naming its issue as the attribution read would (its
	// title's key), so nothing later reads it again. A history with none writes no row.
	if len(s.rows.prRepos) > 0 {
		if err := exec("pull requests", `
			insert into delivery_pull_requests (repo, number, title, url, author, created_at, merged_at, issue_key,
				attribution_title_keys, attribution_cited_issues, attribution_branch_keys, attribution_commit_keys)
			select repo, number, title, url, 'legion-bot', opened, merged, issue, array[issue], '{}', '{}', '{}'
			from unnest($1::text[], $2::int[], $3::text[], $4::text[], $5::text[], $6::timestamptz[], $7::timestamptz[])
				as seeded(repo, number, title, url, issue, opened, merged)`,
			s.rows.prRepos, s.rows.prNumbers, s.rows.prTitles, s.rows.prURLs, s.rows.prIssues,
			s.rows.prCreated, s.rows.prMerged); err != nil {
			return err
		}
	}
	if err := exec("events", `
		insert into events (issue_key, seq, type, actor, payload, notify, created_at)
		select issue, seq, kind, actor::jsonb, payload::jsonb, false, at
		from unnest($1::text[], $2::int[], $3::text[], $4::text[], $5::text[], $6::timestamptz[])
			as seeded(issue, seq, kind, actor, payload, at)`,
		s.rows.eventIssues, s.rows.eventSeqs, s.rows.eventTypes, s.rows.eventActors,
		s.rows.eventPayloads, s.rows.eventAt); err != nil {
		return err
	}
	if err := exec("handoffs", `
		insert into handoffs (issue_key, phase, version, body, author, created_at)
		select issue, phase, 1, '{}', author::jsonb, at
		from unnest($1::text[], $2::text[], $3::text[], $4::timestamptz[])
			as seeded(issue, phase, author, at)`,
		s.rows.handoffIssues, s.rows.handoffPhases, s.rows.handoffAuthors, s.rows.handoffAt); err != nil {
		return err
	}
	return exec("observations", `
		insert into legion_observations (project, issue_key, field, value, observed_at)
		select $1, issue, field, value::jsonb, at
		from unnest($2::text[], $3::text[], $4::text[], $5::timestamptz[])
			as seeded(issue, field, value, at)`,
		s.spec.Project, s.rows.observeIssues, s.rows.observeFields, s.rows.observeValues,
		s.rows.observeAt)
}
