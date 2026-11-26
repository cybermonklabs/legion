package daemon

import (
	"fmt"
	"go/ast"
	"go/constant"
	"go/importer"
	"go/printer"
	"go/token"
	"go/types"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/testtypes"
)

// senderModule is the import path every package the walk reads lives under.
const senderModule = "github.com/sjawhar/legion/daemon/internal/"

// decisionPredicate is the one predicate that decides whether a person's recorded decision on a
// design document is still their word (workflow.SupersedingApproval), and decisionRule the rule it
// asks of a changes request (classify.ReviewSuperseded), each as go/types names it
// (types.Func.FullName). The predicate's rule for an approval is a comparison in its own body,
// which no other function can ask, so the walk reads nothing for it.
// resumeSend is the daemon's render of a start that carries a person's answered wait
// (record.SuperviseRequest's Awaiting) at each send of its task, and resumeSendCaller the send of
// a held task that asks it for such a delivery. newsSend is the daemon's ask, at each send of a
// task that carries news of an approved version, whether an item the task carries still stands.
const (
	decisionPredicate = senderModule + "workflow.SupersedingApproval"
	decisionRule      = senderModule + "classify.ReviewSuperseded"
	resumeSend        = "(*" + senderModule + "daemon.workflowRuntime).resumeTaskNow"
	resumeSendCaller  = "(*" + senderModule + "daemon.workflowRuntime).rootTask"
	newsSend          = senderModule + "daemon.itemStands"
)

// ruleAskers is every function of the three packages that may ask the rule itself, with why.
var ruleAskers = map[string]string{
	decisionPredicate:                       "the one predicate, which holds the guards that choose whether the rule is asked of a review",
	senderModule + "workflow.requestStands": "an approval request on the ask mirror, which is the architect's ask and no person's decision",
}

// storedTaskReaders is every function of the three packages that may read the task a phase entry
// stored when it was raised (record.PhaseRow.EntryTask), with why that read sends no person's
// review of a design document.
var storedTaskReaders = map[string]string{
	"(*" + senderModule + "workflow.Engine).sendOwedEntry": "a phase worker's entry, whose task its transition composed from the " +
		"phase (workflow's task and Engine.ResumeTask) and which names no person's review; a root architect's entry is sent by " +
		"workflow.Engine.SendArchitectStart, which sends none of it",
	"(*" + senderModule + "workflow.Engine).enter": "the raise of an entry, which writes the task its caller composed and reads it " +
		"back only to add a reviewer's round sentences (workflow.namingTheReviewRound) and hand that text to its caller; for a root " +
		"architect's entry the caller (workflow.Engine.EnterArchitect) keeps the entry's number alone, and a resume's entry is " +
		"rendered again from its wait at each send (workflow.Engine.SendArchitectStart)",
	senderModule + "workflow.reviewedCommitStays": "it reads only whether the reviewer's row holds an entry's task, to decide " +
		"whether a review moves the commit the reviewer read, and sends no text",
	"(*" + senderModule + "workflow.Engine).ranUnreported": "it reads only whether the role's row holds an entry's task, the " +
		"durable mark that the role started its step in this run (main's #5463), to decide whether a late completion reports " +
		"that run, and sends no text",
	senderModule + "workflow.namingTheReviewRound": "it reads only whether the reviewer's row holds an entry's task, to name that " +
		"entry's number in a reviewer's round sentences; it sends no stored text, and a reviewer's task names no person's review " +
		"of a design document",
}

// keptLoopName names one loop of keptLoopReaders: its function, as go/types names it
// (types.Func.FullName), and its clause as gofmt prints it (loopClause).
type keptLoopName struct{ fn, clause string }

// keptLoopReaders is every loop of the three packages one pass of whose body reads a kept
// decision (record.ParkedDecision or record.DecisionMark) and does not reach the predicate in
// that pass, each named by its function and its clause (keptLoopName), with why the loop renders
// nothing an architect or person reads. A row allows that one loop: another such loop of the same
// function needs a row of its own, a clause two such loops share allows neither, and a row whose
// loop came to ask, changed its clause or went is taken out (unaskedKeptLoops).
var keptLoopReaders = map[keptLoopName]string{
	{senderModule + "workflow.ParkedDecisionTask", "for _, d := range decisions"}: "it turns each unclaimed row into its mark " +
		"(record.ParkedDecision.Mark), the list ForgetSuperseded and KeptDecisionsTask each ask of in a loop of their own, and renders nothing",
	{senderModule + "workflow.ReadKeptGates", "for _, d := range owed.Decisions"}: "it collects the issue each decision was made on, " +
		"to read their gates in one read (record.Store.Gates), and renders nothing",
	{"(*" + senderModule + "workflow.Engine).warnKeptDecisionsOfAClosedTree", "for _, decision := range kept"}: "it writes each " +
		"decision a closing tree still owes into the daemon's log (keptDecisionOfAClosedTree), which no architect or person is " +
		"sent; the controller was told each when it was kept",
	{"(*" + senderModule + "daemon.workflowRuntime).rootTask", "for _, mark := range named.Decisions"}: "it drops the marks a " +
		"held task named that its delivery no longer holds and sorts the rest into those whose issue stands in the tree (stays) " +
		"and those whose issue left it (left), rendering nothing: what stays is asked and written again by " +
		"workflow.KeptDecisionsTask, and what left is released to the tree it is owed to now, whose catch-up asks it " +
		"(workflow.CatchUpTheTreesOwed)",
}

// envoyPublishes are the functions that hand a message to Envoy, each as go/types names it: the
// outbox's publish and its publish with keep, which every executor publishes through, and the two
// of notify.Publisher, which those call. A function that hands one of them a record.Notice it
// holds publishes that notice: the queued notice's delivery (the outbox's deliverNotice) does.
var envoyPublishes = []string{
	"(*" + senderModule + "daemon.outbox).publish",
	"(*" + senderModule + "daemon.outbox).publishKept",
	"(" + senderModule + "notify.Publisher).Publish",
	"(" + senderModule + "notify.Publisher).PublishKept",
}

// senderPackages are the three packages that hold senders of a person's design decision: the
// workflow, which writes the notices and renders the resume and the kept-decisions tasks, the
// daemon, which sends a held task at each send (rootTask), publishes a queued notice
// (deliverNotice), writes a catch-up's decisions again at its delivery and asks a changes request
// again there (readNoticeTree), relays a decision nobody in its tree hears (parkDecision) and
// asks a relayed changes request again at its publish to the controller (controllerNotice), and
// admission, which composes a re-admitted root's start from what was kept for it.
var senderPackages = []string{"workflow", "daemon", "admit"}

// keptDecisionTypes are the record types that hold one kept decision: the row the daemon keeps
// (record.ParkedDecision) and the mark a task names it by (record.DecisionMark). A loop that
// reads a value of either type reads a kept decision, whatever the loop runs over (readKeptLoop).
var keptDecisionTypes = []string{"ParkedDecision", "DecisionMark"}

// decisionReviewSentences is every fixed phrase a task says about a person's review of a design
// document, each with the function that renders it. A function whose string constants hold one
// renders that review: it is a sender, or the template its callers send (decisionSenders). The
// test holds each named function to its phrase, so a phrase reworded out of the list fails here
// rather than leaving its renderer unread.
var decisionReviewSentences = []struct{ phrase, renderer string }{
	// The changes request that answered the wait.
	{"A person requested changes to your", "workflow.resumeTask"},
	// That changes request, when the person's approval followed it.
	{"asks for no revision now", "workflow.resumeTask"},
	// A changes request the gate records as told after the wait began.
	{"A person then requested changes to your", "workflow.resumeTask"},
	// The changes request that answered the wait, of a document its step no longer holds.
	{"The review was of a document since replaced", "workflow.resumeTask"},
	// The approval that answered the wait, when the person's word has moved past it, or its step
	// holds another document now.
	{"That approval no longer stands", "workflow.writeApprovalReplaced"},
	// That approval, when it stands below a newer version.
	{"The approval of version", "workflow.resumeTask"},
	// The decisions kept while nobody in the tree could hear them.
	{"a person decided on the tree", "workflow.KeptDecisionsTask"},
	// The kept approval of a version the document has moved past.
	{"and is now at version", "workflow.KeptDecisionsTask"},
}

// Every sender of a person's design decision asks the one predicate whether that decision is
// still the person's word (workflow.SupersedingApproval), so no sender decides it for itself and
// no sender asks the changes-request rule itself (dispatch://INFRA-3136/spec R2).
// A sender is a function of internal/workflow, internal/daemon or internal/admit, outside its
// tests, whose body
//
//   - writes a record.Notice whose kind is in record.SpecDecision's set, which is read from
//     SpecDecision's own switch and checked against the function, so a kind added there is
//     covered here;
//   - asks record.SpecDecision of a notice's kind, to send the decision it names on (the outbox's
//     parkDecision relays it to the controller);
//   - holds a review sentence (decisionReviewSentences) in a string constant, which is rendering
//     the task that says it;
//   - calls such a renderer that does not reach the predicate itself: the renderer is then a
//     template, its callers are its senders, and a template nothing calls is a sender itself;
//   - writes a record.SuperviseRequest that carries a person's answered wait (its Awaiting): the
//     start of a resume, whose task the daemon renders at each send from that wait (resumeSend),
//     so such a function reaches what that render reaches. The resume itself writes one
//     (workflow.Engine.ResumeAwaitedRoot), and so does the sweep's send of a resume's entry whose
//     start was lost (workflow.Engine.SendArchitectStart);
//   - publishes a record.Notice it holds: hands a value of that type, however it was made (a
//     row's payload, its Published copy, a conversion), to one of envoyPublishes. A notice the
//     workflow queued is published where it is delivered (the outbox's deliverNotice), and a
//     relay to the controller where its row runs (the outbox's controllerNotice): each can be long
//     after the workflow or the relay asked, so the publish is a send of its own; or
//   - is the news's ask of an item it tells (newsSend, the daemon's itemStands): an item names a
//     person's approval of a version, and a send of the task that carries it tells that approval
//     to its audience, so the ask of whether it still stands is a sender's.
//
// A sender passes when it reaches the predicate: it calls it, or calls a function of the three
// packages that reaches it, however many calls down. The walk type-checks the three packages
// (and record, for SpecDecision's switch) against the compiler's export data of their
// dependencies (testtypes.CompiledDependencies) and resolves every call, and every function value
// a body names, through types.Info to the function it is, so a method that shares a name with
// another is never mistaken for it.
//
// The reach is transitive, which bounds what the rule for a publisher proves: it fails a publisher
// that reaches the predicate through nothing it calls, and no more. The delivery of a notice
// (deliverNotice) reaches it four ways: through its ask of a changes request (the stillOwed that
// readNoticeTree calls, through workflow.NoticeSuperseded), through the catch-up's Decided that
// readNoticeTree writes again (workflow.KeptDecisionsTask), through the hand-over of a catch-up's
// decisions to a fresh catch-up (workflow.HandOverKept) once a newer one superseded it
// (supersededKept) or its re-holds are spent (the re-hold policy's spentKept), and through the
// relay of a kept decision (parkDecision). Take the first out and the walk still passes. What
// holds that ask is its behavior, which TestAQueuedChangesNoticeAnApprovalFollowedIsNotPublished
// drives. The publish of a relay to the controller (controllerNotice) reaches it one way alone,
// through its ask of a relayed changes request (stillOwed, through workflow.NoticeSuperseded), so the
// walk fails it if that ask goes; that the ask is of the review's mark and document the relay
// carries, and of nothing kept beside the row, is its behavior too, which
// TestAChangesRequestRelayedToTheControllerIsNotToldOnceApproved drives.
//
// A list of kept decisions is held to more: every loop, a for or a range over anything (a slice,
// a map, an iterator, or a count that indexes the list), one pass of whose body reads a kept
// decision (keptDecisionTypes) — a field of it, a method of it, the value whole as an argument or
// a switch's tag, a conversion or a copy — must reach the predicate in that pass (readKeptLoop):
// through what the pass itself calls, not through a call before or after the loop, and not
// through a loop nested in it, whose body is a pass of that loop's own. A loop that renders
// nothing a person or architect reads passes only where keptLoopReaders names it, by its
// function and its clause, with why; a row allows that one loop, so a second such loop of the
// function fails, and so does a row that names no such loop or two of them (unaskedKeptLoops). A
// function that asks about one decision, or about the kept approval, and renders the rest of the
// list unasked — naming only Ref and Actor, handing the whole decision to a helper or a method,
// switching on Decision to write constants, or printing the value whole — is what this test
// fails on: a task named a changes request that the person's approval had superseded that way.
// TestDecisionSendersSeeEveryLoopThatReadsAKeptDecision plants each of those shapes, and
// TestDecisionSendersAllowAnUnaskedKeptLoopOnlyByItsOwnRow the list's rows, and holds the walk to
// what each must read.
//
// A task a phase entry stored when it was raised is held to more as well: a function that reads
// record.PhaseRow.EntryTask can send a text rendered at the raise, and no call it makes at the send
// can ask the predicate about what that text already says. The resume renders a person's review
// into its architect's entry (workflow.Engine.ResumeAwaitedRoot), so a send of that stored text
// again names the review whether or not the person's word has moved since, which is how the
// sweep's send of a lost resume's start named a changes request the person's approval had
// superseded. The resume still writes that text, which this release never sends, for the release
// a rollback past contract entry 20261125181104 returns to (record.PhaseRow.EntryTask says so), so
// the text is there for a new sender to read. Such a function passes only where storedTaskReaders
// names it, with why its read sends no person's review.
//
// And the changes-request rule (classify.ReviewSuperseded) is asked by the predicate and by the
// one reading of an approval request alone (ruleAskers), so the guards the predicate holds, which
// its doc lists, live in one place. That rule is classify's, so it is exported, and two functions
// of workflow ask it, so no shape of the code keeps a third function from asking it; only this
// reading does. The rule for an approval needs no such reading: it is a comparison in the
// predicate's own body, which no other function can ask.
//
// This test reads its sites from the source because no chokepoint can make a sender ask: the
// predicate is the one place the answer is decided, but a notice is a plain struct literal any
// function can write and a publish takes it as any other payload, the review sentences are built
// in several functions, a list of kept decisions is a plain slice any function can range over and
// write into text, and a stored task is a plain field any function can read, and no type can make
// any of them ask the predicate first.
// So this test stands until one can; delete it then.
func TestDecisionSendersAskWhetherTheReviewIsStillThePersonsWord(t *testing.T) {
	t.Parallel()
	walk := readDecisionWalk(t)
	kinds := walk.specDecisionKinds(t)
	for _, kind := range kinds {
		if _, ok := record.SpecDecision(record.NoticeKind(kind)); !ok {
			t.Errorf("record.SpecDecision's switch names %q, but SpecDecision(%q) is no decision: the walk read the set wrongly", kind, kind)
		}
		if !slices.ContainsFunc(walk.funcs, func(f *decisionFunc) bool { return slices.Contains(f.kinds, kind) }) {
			t.Errorf("no function of internal/workflow, internal/daemon or internal/admit writes a record.Notice of kind %q: the walk no longer sees where that "+
				"decision is sent, so it proves nothing of its sender", kind)
		}
	}
	for _, sentence := range decisionReviewSentences {
		f := walk.named(sentence.renderer)
		if f == nil || !slices.Contains(f.phrases, sentence.phrase) {
			t.Errorf("%s no longer holds the review sentence %q: name the phrase it renders now in decisionReviewSentences", sentence.renderer, sentence.phrase)
		}
	}
	if predicate := walk.byName[decisionPredicate]; predicate == nil || !predicate.asks {
		t.Fatalf("the walk found no %s asking %s: it reads the predicate under that name", short(decisionPredicate),
			short(decisionRule))
	}
	senders := walk.decisionSenders()
	for _, s := range senders {
		t.Logf("sender %s (%s): %s", s.fn.short(), s.fn.at, s.why)
		if !walk.reaches(s.fn.calls) {
			t.Errorf("%s (%s) %s, but does not reach %s, directly or through any function it calls: it decides for itself whether "+
				"the person's review is still their word", s.fn.short(), s.fn.at, s.why, short(decisionPredicate))
		}
	}
	for _, f := range walk.funcs {
		for _, at := range f.unread {
			t.Errorf("%s writes a record.Notice at %s whose kind the walk cannot read: give it a constant kind, or a local assigned only "+
				"constants, so the walk can tell whether it sends a person's decision", f.short(), at)
		}
		if f.asks {
			if _, ok := ruleAskers[f.name]; !ok {
				t.Errorf("%s (%s) asks %s itself: ask %s, which holds the guards that choose whether the rule is asked of a review, so "+
					"no copy of them can drift", f.short(), f.at, short(decisionRule), short(decisionPredicate))
			}
		}
		if len(f.storedTasks) > 0 {
			why, named := storedTaskReaders[f.name]
			t.Logf("reader of a stored task %s (%s), at %s: %s", f.short(), f.at, strings.Join(f.storedTasks, ", "), why)
			if !named {
				t.Errorf("%s (%s) reads the task a phase entry stored when it was raised (record.PhaseRow.EntryTask, at %s): that text was "+
					"rendered at the raise, and the resume of a root a person answered renders the person's review into its architect's "+
					"entry (workflow.Engine.ResumeAwaitedRoot), so sent again it names that review whether or not the person's word has "+
					"moved since. Send what the task was built from, which the send renders (record.PhaseRow.EntryAwaiting), or name the "+
					"function in storedTaskReaders with why its read sends no person's review", f.short(), f.at, strings.Join(f.storedTasks, ", "))
			}
		}
	}
	for name := range storedTaskReaders {
		if f := walk.byName[name]; f == nil || len(f.storedTasks) == 0 {
			t.Errorf("storedTaskReaders names %s, which reads no stored entry task: take it out, so the list names every read of a stored "+
				"task and no other", short(name))
		}
	}
	notes, failures := walk.unaskedKeptLoops(keptLoopReaders)
	for _, note := range notes {
		t.Log(note)
	}
	for _, failure := range failures {
		t.Error(failure)
	}
	// A queued notice is published where it is delivered (the outbox's deliverNotice), and a relay
	// to the controller where its row runs (controllerNotice): the walk has to see both publishes,
	// or the rule for a publisher of a notice proves nothing.
	for _, name := range []string{"(*daemon.outbox).deliverNotice", "(*daemon.outbox).controllerNotice"} {
		if !slices.ContainsFunc(senders, func(s decisionSender) bool { return s.fn.short() == name && len(s.fn.publishes) > 0 }) {
			t.Errorf("the walk found no publish of a record.Notice in %s", name)
		}
	}
	// The one wording of kept decisions (workflow.KeptDecisionsTask) renders them in a loop: the
	// walk has to see it, or it proves nothing of any renderer.
	if f := walk.named("workflow.KeptDecisionsTask"); f == nil || !slices.ContainsFunc(f.keptLoops, func(l keptLoop) bool { return walk.reaches(l.calls) }) {
		t.Error("the walk found no kept-decision loop reaching the predicate in workflow.KeptDecisionsTask")
	}
	// The resume of an answered root is rendered at each send from the gate as it stands
	// (workflow.ResumeAwaitedRootTask, the daemon's rootTask), so it is a sender too: the walk has
	// to see it.
	if !slices.ContainsFunc(senders, func(s decisionSender) bool { return s.fn.short() == "workflow.resumeTask" }) {
		t.Error("the walk found no resume sender (workflow.resumeTask) in internal/workflow")
	}
	// The resume's start, and the sweep's send of a resume's entry whose start was lost, each
	// carry the answered wait and are rendered at the send (resumeSend, which rootTask asks), so
	// each is a sender: the walk has to see both of them, and the send that renders them.
	if f := walk.byName[resumeSendCaller]; f == nil || !slices.Contains(f.calls, resumeSend) {
		t.Errorf("the walk found %s asking no %s: a start that carries a person's answered wait is no longer rendered at its send",
			short(resumeSendCaller), short(resumeSend))
	}
	for _, name := range []string{"(*workflow.Engine).ResumeAwaitedRoot", "(*workflow.Engine).SendArchitectStart"} {
		if !slices.ContainsFunc(senders, func(s decisionSender) bool { return s.fn.short() == name && s.fn.queuesResume }) {
			t.Errorf("the walk found no start carrying a person's answered wait in %s: a resume's start it writes no longer carries "+
				"the wait the send renders it from", name)
		}
	}
	// The news's ask of an item it tells (newsSend) is a sender of a person's approval: the walk
	// has to see it, or it proves nothing of the news.
	if !slices.ContainsFunc(senders, func(s decisionSender) bool { return s.fn.name == newsSend }) {
		t.Errorf("the walk found no news sender (%s) in internal/daemon", short(newsSend))
	}
	if !slices.ContainsFunc(senders, func(s decisionSender) bool { return s.fn.pkg == "daemon" }) {
		t.Error("the walk found no sender in internal/daemon")
	}
}

// A loop that reads a kept decision and asks nothing passes only where a row of keptLoopReaders
// names it, by its function and its clause, and a row allows that one loop (unaskedKeptLoops).
// Each row here plants a function render beside the planted rows' helpers
// (plantedDecisionPreamble), allows loops of it in a list of its own, and names a phrase of each
// failure the rule must write, in order.
func TestDecisionSendersAllowAnUnaskedKeptLoopOnlyByItsOwnRow(t *testing.T) {
	t.Parallel()
	planted := newPlantedDecisionSource(t)
	const collects = `
func render(marks []record.DecisionMark) []string {
	var issues []string
	for _, d := range marks {
		issues = append(issues, d.Issue)
	}
	return issues
}
`
	const twoLoops = `
func render(marks []record.DecisionMark) (string, []string) {
	var issues []string
	for _, d := range marks {
		issues = append(issues, d.Issue)
	}
	var named []string
	for _, other := range marks {
		named = append(named, fmt.Sprintf("%s by %s", other.Ref, other.Actor))
	}
	return joined(named), issues
}
`
	const sharedClause = `
func render(marks []record.DecisionMark) (string, []string) {
	var issues []string
	for _, d := range marks {
		issues = append(issues, d.Issue)
	}
	var named []string
	for _, d := range marks {
		named = append(named, fmt.Sprintf("%s by %s", d.Ref, d.Actor))
	}
	return joined(named), issues
}
`
	const asking = `
func render(gate *record.DesignGate, marks []record.DecisionMark) string {
	var named []string
	for _, d := range marks {
		if stands(gate, d) {
			named = append(named, line(d))
		}
	}
	return joined(named)
}
`
	collectsLoop := keptLoopName{plantedDecisionRender, "for _, d := range marks"}
	for _, row := range []struct {
		name    string
		source  string
		allowed []keptLoopName
		fails   []string
	}{
		{name: "a row allows its one loop", source: collects, allowed: []keptLoopName{collectsLoop}},
		{name: "a second loop of the function that asks nothing fails", source: twoLoops, allowed: []keptLoopName{collectsLoop},
			fails: []string{"in the loop `for _, other := range marks`"}},
		{name: "a row whose clause two such loops share fails", source: sharedClause, allowed: []keptLoopName{collectsLoop},
			fails: []string{"holds 2 loops of that clause"}},
		{name: "a row whose loop asks is taken out", source: asking, allowed: []keptLoopName{collectsLoop},
			fails: []string{"holds no loop of that clause"}},
		{name: "a row whose clause its loop no longer has is taken out", source: collects,
			allowed: []keptLoopName{{plantedDecisionRender, "for _, mark := range marks"}},
			fails:   []string{"in the loop `for _, d := range marks`", "holds no loop of that clause"}},
		{name: "a row of a function the walk does not hold is taken out", source: collects,
			allowed: []keptLoopName{collectsLoop, {plantedDecisionPackage + ".gone", "for _, d := range marks"}},
			fails:   []string{"holds no loop of that clause"}},
	} {
		t.Run(row.name, func(t *testing.T) {
			allowed := map[keptLoopName]string{}
			for _, name := range row.allowed {
				allowed[name] = "planted"
			}
			_, failures := planted.walk(t, row.source).unaskedKeptLoops(allowed)
			if len(failures) != len(row.fails) {
				t.Fatalf("the rule wrote %d failures, want %d (%q): %q", len(failures), len(row.fails), row.fails, failures)
			}
			for i, phrase := range row.fails {
				if !strings.Contains(failures[i], phrase) {
					t.Errorf("failure %d is %q, want it to say %q", i, failures[i], phrase)
				}
			}
		})
	}
}

// decisionFunc is what the walk reads of one function declaration, its function literals
// included: they run as part of it (a transaction's body is one).
type decisionFunc struct {
	pkg       string // the package's last element
	name      string // types.Func.FullName
	at        string // file:line of the declaration
	asks      bool   // names classify.ReviewSuperseded
	picks     bool   // names record.SpecDecision
	kinds     []string
	unread    []string // positions of record.Notice literals whose kind the walk cannot read
	phrases   []string
	calls     []string // the FullName of every function the body calls or names
	keptLoops []keptLoop
	// storedTasks are the positions where the body reads record.PhaseRow.EntryTask, a task an
	// entry stored when it was raised.
	storedTasks []string
	// queuesResume is whether the body writes a record.SuperviseRequest that carries a person's
	// answered wait, whose task the daemon renders at each send (resumeSend).
	queuesResume bool
	// publishes are the positions where the body hands a record.Notice to one of envoyPublishes.
	publishes []string
}

func (f *decisionFunc) short() string { return short(f.name) }

// short is a function's FullName without the module's path: workflow.SupersedingApproval,
// (*workflow.Engine).tellChanges.
func short(name string) string { return strings.ReplaceAll(name, senderModule, "") }

// keptLoop is a loop of a function one pass of whose body reads a kept decision
// (keptDecisionTypes): where it is, its clause (loopClause), and what that pass calls.
type keptLoop struct {
	at     string
	clause string
	calls  []string
}

type decisionSender struct {
	fn  *decisionFunc
	why string
}

type decisionWalk struct {
	funcs  []*decisionFunc
	byName map[string]*decisionFunc // FullName → function
	record *checkedPackage
}

// named is the function short names (short), nil when the walk read none.
func (w *decisionWalk) named(name string) *decisionFunc {
	for _, f := range w.funcs {
		if f.short() == name {
			return f
		}
	}
	return nil
}

// reaches is whether calls name the predicate, or a function of the walk from which a chain of
// calls within the three packages reaches it.
func (w *decisionWalk) reaches(calls []string) bool {
	seen := map[string]bool{}
	next := slices.Clone(calls)
	for len(next) > 0 {
		name := next[len(next)-1]
		next = next[:len(next)-1]
		if name == decisionPredicate {
			return true
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		if f := w.byName[name]; f != nil {
			next = append(next, f.calls...)
		}
	}
	return false
}

// unaskedKeptLoops holds w's loops that read a kept decision to the rule for them (the test's doc
// comment), given allowed, the loops that may ask nothing (keptLoopReaders): notes says why each
// allowed loop asks nothing, and failures names each loop that asks nothing in its own pass and no
// row allows, then each row of allowed that names no such loop or more than one.
func (w *decisionWalk) unaskedKeptLoops(allowed map[keptLoopName]string) (notes, failures []string) {
	found := map[keptLoopName][]string{}
	for _, f := range w.funcs {
		for _, loop := range f.keptLoops {
			if w.reaches(loop.calls) {
				continue
			}
			name := keptLoopName{f.name, loop.clause}
			why, named := allowed[name]
			if !named {
				failures = append(failures, fmt.Sprintf("%s (%s) reads kept decisions in the loop `%s` at %s, but that loop asks %s "+
					"about none of them, directly or through a function it calls: it can name a decision without knowing whether it is "+
					"still the person's word. Ask the predicate in the loop's own body, or name the loop in keptLoopReaders with why it "+
					"renders nothing", f.short(), f.at, loop.clause, loop.at, short(decisionPredicate)))
				continue
			}
			found[name] = append(found[name], loop.at)
			notes = append(notes, fmt.Sprintf("kept-decision loop that asks nothing, `%s` of %s at %s: %s", loop.clause, f.short(), loop.at, why))
		}
	}
	var rows []string
	for name := range allowed {
		switch at := found[name]; {
		case len(at) == 0:
			rows = append(rows, fmt.Sprintf("keptLoopReaders names the loop `%s` of %s, which holds no loop of that clause that reads "+
				"a kept decision and asks nothing: take the row out, so the list names every such loop and no other",
				name.clause, short(name.fn)))
		case len(at) > 1:
			rows = append(rows, fmt.Sprintf("keptLoopReaders names the loop `%s` of %s, which holds %d loops of that clause that read "+
				"a kept decision and ask nothing (at %s): a row allows one loop, so ask the predicate in the others or give each "+
				"a clause of its own", name.clause, short(name.fn), len(at), strings.Join(at, ", ")))
		}
	}
	slices.Sort(rows)
	return notes, append(failures, rows...)
}

// decisionSenders is every sender the walk finds (the test's doc comment), each once, with what
// makes it one.
func (w *decisionWalk) decisionSenders() []decisionSender {
	var senders []decisionSender
	add := func(f *decisionFunc, why string) {
		for i := range senders {
			if senders[i].fn == f {
				senders[i].why += "; " + why
				return
			}
		}
		senders = append(senders, decisionSender{f, why})
	}
	for _, f := range w.funcs {
		if len(f.kinds) > 0 {
			add(f, fmt.Sprintf("writes a record.Notice of kind %s", strings.Join(f.kinds, ", ")))
		}
		if f.picks {
			add(f, "asks record.SpecDecision of a notice's kind and sends the decision it names on")
		}
		if f.queuesResume {
			add(f, fmt.Sprintf("writes a start that carries a person's answered wait, whose task the daemon renders at each send (%s)",
				short(resumeSend)))
		}
		if f.name == newsSend {
			add(f, "asks whether the approval a news item names still stands, at each send of the task that tells it")
		}
		if len(f.publishes) > 0 {
			add(f, fmt.Sprintf("publishes a record.Notice it holds, at %s", strings.Join(f.publishes, ", ")))
		}
		if len(f.phrases) == 0 {
			continue
		}
		if w.reaches(f.calls) {
			add(f, fmt.Sprintf("renders %q", f.phrases))
			continue
		}
		var callers []*decisionFunc
		for _, g := range w.funcs {
			if g != f && slices.Contains(g.calls, f.name) {
				callers = append(callers, g)
			}
		}
		if len(callers) == 0 {
			add(f, fmt.Sprintf("renders %q, and nothing calls it to ask the predicate for it", f.phrases))
		}
		for _, g := range callers {
			add(g, fmt.Sprintf("sends what %s renders, %q", f.short(), f.phrases))
		}
	}
	return senders
}

// specDecisionKinds is record.SpecDecision's set as its switch spells it: every case value, as
// the type checker reads its constant.
func (w *decisionWalk) specDecisionKinds(t *testing.T) []string {
	t.Helper()
	var kinds []string
	for _, file := range w.record.files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || fn.Name.Name != "SpecDecision" {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				clause, ok := n.(*ast.CaseClause)
				if !ok {
					return true
				}
				for _, expr := range clause.List {
					value := w.record.info.Types[expr].Value
					if value == nil || value.Kind() != constant.String {
						t.Fatalf("record.SpecDecision has a case the walk cannot read as a constant: %s", w.record.pos(expr))
					}
					kinds = append(kinds, constant.StringVal(value))
				}
				return true
			})
		}
	}
	if len(kinds) == 0 {
		t.Fatal("read no kind from record.SpecDecision's switch")
	}
	return kinds
}

// checkedPackage is one package's non-test source, type-checked (testtypes.CheckPackage), with
// its files in the order of their names, so the walk reads them, and logs what it finds, in the
// same order on every run.
type checkedPackage struct {
	name  string
	files []*ast.File
	fset  *token.FileSet
	info  *types.Info
}

func (p *checkedPackage) pos(node ast.Node) string {
	position := p.fset.Position(node.Pos())
	return fmt.Sprintf("%s:%d", filepath.Join(p.name, filepath.Base(position.Filename)), position.Line)
}

// checkPackage type-checks the non-test source of internal/<name> against the compiler's export
// data of its dependencies.
func checkPackage(t *testing.T, fset *token.FileSet, imports types.Importer, name string) *checkedPackage {
	t.Helper()
	checked, err := testtypes.CheckPackage(fset, imports, senderModule+name, filepath.Join("..", name))
	if err != nil {
		t.Fatal(err)
	}
	return &checkedPackage{name: name, files: checked.Files, fset: fset, info: checked.Info}
}

// readDecisionWalk type-checks record and the three sender packages and reads every function
// declaration of the three. It reads their dependencies' types where the compiler left them, as
// supervise's source tests read them (testtypes.CompiledDependencies): the build that runs this
// test has written them into its cache, so the walk reads those rather than type-checking the
// dependency tree from its source. This package imports the other two sender packages and record,
// so the one listing of its dependencies serves the four packages the walk checks.
func readDecisionWalk(t *testing.T) *decisionWalk {
	t.Helper()
	compiled, err := testtypes.CompiledDependencies(senderModule + "daemon")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	imports := importer.ForCompiler(fset, "gc", compiled)
	w := &decisionWalk{byName: map[string]*decisionFunc{}, record: checkPackage(t, fset, imports, "record")}
	for _, name := range senderPackages {
		pkg := checkPackage(t, fset, imports, name)
		for _, file := range pkg.files {
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				f := readDecisionFunc(pkg, fn)
				w.funcs = append(w.funcs, f)
				w.byName[f.name] = f
			}
		}
	}
	return w
}

// readDecisionFunc reads what fn calls, the record.Notice literals it writes, the review
// sentences its string constants hold, its loops that read a kept decision (readKeptLoop), the
// stored entry tasks it reads, the starts it writes that carry a person's answered wait and the
// notices it publishes.
func readDecisionFunc(pkg *checkedPackage, fn *ast.FuncDecl) *decisionFunc {
	info := pkg.info
	f := &decisionFunc{pkg: pkg.name, name: info.Defs[fn.Name].(*types.Func).FullName(), at: pkg.pos(fn)}
	f.calls = namedCalls(info, func(visit func(ast.Node) bool) { ast.Inspect(fn.Body, visit) })
	f.asks = slices.Contains(f.calls, decisionRule)
	f.picks = slices.Contains(f.calls, senderModule+"record.SpecDecision")
	assigned := localAssignments(info, fn.Body)
	// written is every field selector an assignment writes, which reads nothing of the field. The
	// walk visits an assignment before its left-hand selectors, so each is marked before it is read.
	written := map[*ast.SelectorExpr]bool{}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.AssignStmt:
			for _, lhs := range node.Lhs {
				if sel, ok := ast.Unparen(lhs).(*ast.SelectorExpr); ok {
					written[sel] = true
				}
			}
		case ast.Expr:
			if value := info.Types[node].Value; value != nil && value.Kind() == constant.String {
				text := constant.StringVal(value)
				for _, sentence := range decisionReviewSentences {
					if strings.Contains(text, sentence.phrase) && !slices.Contains(f.phrases, sentence.phrase) {
						f.phrases = append(f.phrases, sentence.phrase)
					}
				}
				return false
			}
			if lit, ok := node.(*ast.CompositeLit); ok && isRecordType(info.Types[lit].Type, "Notice") {
				readNoticeKind(pkg, f, lit, assigned)
			}
			// The send renders such a start from the wait it carries, so the function reaches
			// whatever that render reaches.
			if lit, ok := node.(*ast.CompositeLit); ok && isRecordType(info.Types[lit].Type, "SuperviseRequest") && setsField(lit, "Awaiting") {
				f.queuesResume = true
				if !slices.Contains(f.calls, resumeSend) {
					f.calls = append(f.calls, resumeSend)
				}
			}
			if sel, ok := node.(*ast.SelectorExpr); ok && !written[sel] && sel.Sel.Name == "EntryTask" && isRecordType(info.TypeOf(sel.X), "PhaseRow") {
				f.storedTasks = append(f.storedTasks, pkg.pos(sel))
			}
			if call, ok := node.(*ast.CallExpr); ok && publishesNotice(info, call) {
				f.publishes = append(f.publishes, pkg.pos(call))
			}
		case *ast.RangeStmt, *ast.ForStmt:
			if loop, ok := readKeptLoop(pkg, node); ok {
				f.keptLoops = append(f.keptLoops, loop)
			}
		}
		return true
	})
	return f
}

// readNoticeKind adds to f the spec-decision kind a record.Notice literal writes, or the literal's
// position to f.unread when its kind is no constant the walk can read: a constant, or a local
// every assignment of which is one.
func readNoticeKind(pkg *checkedPackage, f *decisionFunc, lit *ast.CompositeLit, assigned map[types.Object][]ast.Expr) {
	for _, elt := range lit.Elts {
		keyed, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			f.unread = append(f.unread, pkg.pos(lit))
			return
		}
		if key, ok := keyed.Key.(*ast.Ident); !ok || key.Name != "Kind" {
			continue
		}
		values, ok := constantStrings(pkg.info, keyed.Value, assigned)
		if !ok {
			f.unread = append(f.unread, pkg.pos(lit))
			return
		}
		for _, value := range values {
			if _, spec := record.SpecDecision(record.NoticeKind(value)); spec && !slices.Contains(f.kinds, value) {
				f.kinds = append(f.kinds, value)
			}
		}
	}
}

// constantStrings is every string expr can hold: its constant value, or the constant value of
// each assignment of the local variable it names.
func constantStrings(info *types.Info, expr ast.Expr, assigned map[types.Object][]ast.Expr) ([]string, bool) {
	if value := info.Types[expr].Value; value != nil && value.Kind() == constant.String {
		return []string{constant.StringVal(value)}, true
	}
	ident, ok := ast.Unparen(expr).(*ast.Ident)
	if !ok {
		return nil, false
	}
	values := assigned[info.Uses[ident]]
	if len(values) == 0 {
		return nil, false
	}
	var all []string
	for _, value := range values {
		if value == nil {
			return nil, false
		}
		held := info.Types[value].Value
		if held == nil || held.Kind() != constant.String {
			return nil, false
		}
		all = append(all, constant.StringVal(held))
	}
	return all, true
}

// localAssignments is every value body assigns to each variable it declares or assigns, function
// literals included; a value no single expression gives (a tuple's) is nil.
func localAssignments(info *types.Info, body *ast.BlockStmt) map[types.Object][]ast.Expr {
	assigned := map[types.Object][]ast.Expr{}
	object := func(ident *ast.Ident) types.Object {
		if defined := info.Defs[ident]; defined != nil {
			return defined
		}
		return info.Uses[ident]
	}
	ast.Inspect(body, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.AssignStmt:
			for i, lhs := range node.Lhs {
				ident, ok := lhs.(*ast.Ident)
				if !ok || ident.Name == "_" {
					continue
				}
				var value ast.Expr
				if len(node.Lhs) == len(node.Rhs) {
					value = node.Rhs[i]
				}
				assigned[object(ident)] = append(assigned[object(ident)], value)
			}
		case *ast.ValueSpec:
			for i, ident := range node.Names {
				var value ast.Expr
				if i < len(node.Values) {
					value = node.Values[i]
				}
				assigned[object(ident)] = append(assigned[object(ident)], value)
			}
		}
		return true
	})
	return assigned
}

// namedCalls is the FullName of every function the identifiers walk visits name, each once: a
// call of the function, or the function taken as a value. walk is ast.Inspect of a node, or one
// pass of a loop (ownPass).
func namedCalls(info *types.Info, walk func(visit func(ast.Node) bool)) []string {
	var calls []string
	walk(func(n ast.Node) bool {
		if ident, ok := n.(*ast.Ident); ok {
			if fun, ok := info.Uses[ident].(*types.Func); ok && !slices.Contains(calls, fun.Origin().FullName()) {
				calls = append(calls, fun.Origin().FullName())
			}
		}
		return true
	})
	return calls
}

// readKeptLoop reads loop, a for or a range statement, when one pass of its body reads a kept
// decision (keptDecisionTypes): a field of it, a method of it, the value whole as an argument or a
// switch's tag, a conversion or a copy, whatever the loop runs over (a slice, a map, an iterator)
// or however it counts (a C-style for that indexes the list). A pass is the body as ownPass walks
// it, and what that pass calls is what the loop reaches the predicate through: a call before or
// after the loop asks nothing of its decisions, and an ask in a loop nested in it answers for
// that nested loop alone.
func readKeptLoop(pkg *checkedPackage, loop ast.Node) (keptLoop, bool) {
	var body *ast.BlockStmt
	switch loop := loop.(type) {
	case *ast.RangeStmt:
		body = loop.Body
	case *ast.ForStmt:
		body = loop.Body
	}
	pass := func(visit func(ast.Node) bool) { ownPass(body, visit) }
	if !passReadsKeptDecision(pkg.info, pass) {
		return keptLoop{}, false
	}
	return keptLoop{at: pkg.pos(loop), clause: loopClause(pkg.fset, loop), calls: namedCalls(pkg.info, pass)}, true
}

// passReadsKeptDecision is whether pass, one pass of a loop (ownPass), reads an expression whose
// type is a kept decision (keptDecisionTypes). The left-hand side of a plain assignment or a
// definition is written, not read; what is inside it, an index or the base of a selector, is read.
func passReadsKeptDecision(info *types.Info, pass func(visit func(ast.Node) bool)) bool {
	written := map[ast.Expr]bool{}
	found := false
	pass(func(n ast.Node) bool {
		if found {
			return false
		}
		switch node := n.(type) {
		case *ast.AssignStmt:
			if node.Tok == token.ASSIGN || node.Tok == token.DEFINE {
				for _, lhs := range node.Lhs {
					written[ast.Unparen(lhs)] = true
				}
			}
		case ast.Expr:
			if tv, ok := info.Types[node]; ok && !written[node] && tv.IsValue() && isKeptDecision(tv.Type) {
				found = true
				return false
			}
		}
		return true
	})
	return found
}

// ownPass walks one pass of the loop whose body is body, as ast.Inspect walks a node: every
// statement of the body, and of a loop nested in it only what runs in that pass, its range
// expression or its init, condition and post. The nested loop's body is a pass of that loop's
// own, which readDecisionFunc reads as a loop of its own, and the key and value a nested range
// assigns are written, not read.
func ownPass(body *ast.BlockStmt, visit func(ast.Node) bool) {
	var inspect func(ast.Node) bool
	inspect = func(n ast.Node) bool {
		var clause []ast.Node
		switch loop := n.(type) {
		case *ast.RangeStmt:
			clause = []ast.Node{loop.X}
		case *ast.ForStmt:
			clause = []ast.Node{loop.Init, loop.Cond, loop.Post}
		default:
			return visit(n)
		}
		for _, part := range clause {
			if part != nil {
				ast.Inspect(part, inspect)
			}
		}
		return false
	}
	ast.Inspect(body, inspect)
}

// loopClause is loop's clause as gofmt prints it, without its body: `for _, d := range
// owed.Decisions`. keptLoopReaders names a loop by it, so a row stays on its loop as lines move,
// and names no loop once the clause changes.
func loopClause(fset *token.FileSet, loop ast.Node) string {
	var bare ast.Node
	switch loop := loop.(type) {
	case *ast.RangeStmt:
		cut := *loop
		cut.Body = &ast.BlockStmt{}
		bare = &cut
	case *ast.ForStmt:
		cut := *loop
		cut.Body = &ast.BlockStmt{}
		bare = &cut
	}
	var text strings.Builder
	if err := printer.Fprint(&text, fset, bare); err != nil {
		return fmt.Sprintf("the loop at %s, whose clause did not print: %v", fset.Position(loop.Pos()), err)
	}
	clause := text.String()
	if brace := strings.LastIndex(clause, "{"); brace >= 0 {
		clause = clause[:brace]
	}
	return strings.TrimSpace(clause)
}

// publishesNotice is whether call hands a record.Notice to one of envoyPublishes: an argument of
// that type is the notice the call publishes, whatever made the value.
func publishesNotice(info *types.Info, call *ast.CallExpr) bool {
	fun := calledFunc(info, call)
	if fun == nil || !slices.Contains(envoyPublishes, fun.Origin().FullName()) {
		return false
	}
	return slices.ContainsFunc(call.Args, func(arg ast.Expr) bool { return isRecordType(info.TypeOf(arg), "Notice") })
}

// calledFunc is the function call calls by name, a method by its selector, and nil for a call of
// anything else (a function value, a conversion).
func calledFunc(info *types.Info, call *ast.CallExpr) *types.Func {
	var ident *ast.Ident
	switch fun := ast.Unparen(call.Fun).(type) {
	case *ast.Ident:
		ident = fun
	case *ast.SelectorExpr:
		ident = fun.Sel
	default:
		return nil
	}
	fun, _ := info.Uses[ident].(*types.Func)
	return fun
}

// isRecordType is whether typ is the record package's type name.
func isRecordType(typ types.Type, name string) bool {
	if pointer, ok := typ.(*types.Pointer); ok {
		typ = pointer.Elem()
	}
	named, ok := typ.(*types.Named)
	return ok && named.Obj().Pkg() != nil && named.Obj().Pkg().Path() == senderModule+"record" && named.Obj().Name() == name
}

// isKeptDecision is whether typ is one of the record types that hold one kept decision.
func isKeptDecision(typ types.Type) bool {
	return slices.ContainsFunc(keptDecisionTypes, func(name string) bool { return isRecordType(typ, name) })
}

// setsField is whether the struct literal lit names field among its keys.
func setsField(lit *ast.CompositeLit, field string) bool {
	return slices.ContainsFunc(lit.Elts, func(elt ast.Expr) bool {
		keyed, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			return false
		}
		key, ok := keyed.Key.(*ast.Ident)
		return ok && key.Name == field
	})
}
