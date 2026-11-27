package daemon

import (
	"fmt"
	"go/ast"
	"go/constant"
	"go/importer"
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
	senderModule + "workflow.entryWork": "StopForChildren reads only a phase worker's entry for the held start; " +
		"acceptance uses a separate builder and no architect entry with a person's answered wait comes through here",
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
	{"of the %s, and the %s is now at version", "workflow.KeptDecisionsTask"},
}

// Every sender of a person's design decision asks the one predicate whether that decision is
// still the person's word (workflow.SupersedingApproval), so no sender decides it for itself and
// no sender asks the changes-request rule itself (dispatch://INFRA-3136/spec R2).
// A sender is a function of internal/workflow, internal/daemon or internal/admit, outside its
// tests, whose body
//
//   - writes a record.Notice whose kind tells a person's decision on a design document: a kind in
//     record.SpecDecision's set, or one the outbox's one rule for which queued rows it asks again
//     takes as a person's changes request (record.ChangesRequest, through record's
//     changesRequestKind). Each set is read from its function's own switch and checked against
//     the function, so a kind added to either is covered here;
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
// A person's changes request on the version of an ancestor's document that a step of a root
// stands open on is told as basis-changes-requested (workflow.Engine.tellBasisChanges), which is
// no spec decision and holds no phrase of decisionReviewSentences: the walk sees its writer by the
// first rule, since record.ChangesRequest takes the kind. The notice is queued twice. The copy
// keyed by the ancestor, where the daemon records the ancestor in a live tree, names the
// ancestor's own document, so it is a sender's under R2: tellBasisChanges asks the predicate of
// the ancestor's gates before it queues it (workflow.NoticeSuperseded), and the delivery and the
// clear of a generation ask it again. TestTheAncestorsCopyOfABasisChangesRequestIsToldOnlyWhileItStands
// drives the queue and the delivery; the clear asks through the same rule (record.ChangesRequest,
// whose rows record's TestTheClearsSelectOfChangesRequestsNamesTheRowsChangesRequestAccepts holds
// the clear's select to). The copy keyed by the root stays outside R2: its Ref is the ancestor's
// step reference, so workflow.RefSuperseded, asked of the root's gates, finds no step of the root
// in another issue's reference (refStep) and answers that the request stands. The walk cannot tell
// the two copies apart, since one literal writes both: what holds the ask to the ancestor's copy,
// and not to the root's, is that behavior.
//
// A sender passes when it reaches the predicate: it calls it, or calls a function of the three
// packages that reaches it, however many calls down. The walk type-checks the three packages
// (and record, for the switches of SpecDecision and changesRequestKind) against the compiler's
// export data of their dependencies (testtypes.CompiledDependencies) and resolves every call, and
// every function value a body names, through types.Info to the function it is, so a method that
// shares a name with another is never mistaken for it.
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
// A list of kept decisions is held to more, by the rule decision_senders_kept_loops_test.go holds
// it to, which also says what that rule does not follow. The rule is there because a task named a
// changes request that the person's approval had superseded: a function asked about one kept
// decision, or about the kept approval, and rendered the rest of the list unasked.
// TestDecisionSendersSeeEveryLoopThatReadsAKeptDecision and
// TestDecisionSendersSeeEveryKeptListWrittenWhole plant each shape that rule must read, and
// TestDecisionSendersAllowAnUnaskedKeptLoopOnlyByItsOwnRow the rows of keptLoopReaders.
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
	// The kinds of a notice that tells a person's decision on a design document, read from the
	// switch of each record function that sets them out, and held to that function's answer.
	for _, set := range []struct {
		switchOf, rule string
		takes          func(record.NoticeKind) bool
	}{
		{switchOf: "SpecDecision", rule: "record.SpecDecision", takes: func(kind record.NoticeKind) bool {
			_, spec := record.SpecDecision(kind)
			return spec
		}},
		{switchOf: "changesRequestKind", rule: "record.ChangesRequest", takes: func(kind record.NoticeKind) bool {
			_, asks := record.ChangesRequest(record.Notice{Kind: kind})
			return asks
		}},
	} {
		for _, kind := range walk.recordSwitchKinds(t, set.switchOf) {
			if !set.takes(record.NoticeKind(kind)) {
				t.Errorf("record.%s's switch names %q, but %s does not take a notice of it: the walk read the set wrongly", set.switchOf, kind, set.rule)
			}
			if !slices.ContainsFunc(walk.funcs, func(f *decisionFunc) bool { return slices.Contains(f.kinds, kind) }) {
				t.Errorf("no function of internal/workflow, internal/daemon or internal/admit writes a record.Notice of kind %q: the walk no longer sees where that "+
					"decision is sent, so it proves nothing of its sender", kind)
			}
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
	for _, failure := range walk.wholeListRenders() {
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
	// The one wording of kept decisions (workflow.KeptDecisionsTask), and the forget of those that
	// are no longer the person's word (workflow.ForgetSuperseded), each ask of every decision in a
	// loop: the walk has to see both ask, or it proves nothing of any loop.
	for _, name := range []string{"workflow.KeptDecisionsTask", "workflow.ForgetSuperseded"} {
		if f := walk.named(name); f == nil || !slices.ContainsFunc(f.keptLoops, func(l keptLoop) bool { return l.asks }) {
			t.Errorf("the walk found no kept-decision loop in %s that asks %s of each decision it reads", name, short(decisionPredicate))
		}
	}
	// Both ask through keptSuperseded, which hands the decision's fields to RefSuperseded in a
	// workflow.Decision, which hands that to the predicate: the walk has to follow the decision down
	// each, or it proves nothing of a loop that asks through a function.
	for _, ask := range []struct{ fn, param string }{{"workflow.keptSuperseded", "mark"}, {"workflow.RefSuperseded", "d"}} {
		if !walk.asksOfParameter(ask.fn, ask.param) {
			t.Errorf("the walk does not read %s as asking %s of its parameter %s", ask.fn, short(decisionPredicate), ask.param)
		}
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
	// info is the type checker's record of the function's package, params its receiver and
	// parameters by position (funcParams), and sites every call it makes of a function it names
	// (callSites): what the walk reads, once it has read every function, of what the function asks
	// the predicate of and writes into text through the functions it calls (resolve).
	info   *types.Info
	params []types.Object
	sites  []callSite
	// renders are the lists of kept decisions the body writes whole into text (resolve).
	renders []wholeRender
}

func (f *decisionFunc) short() string { return short(f.name) }

// short is a function's FullName without the module's path: workflow.SupersedingApproval,
// (*workflow.Engine).tellChanges.
func short(name string) string { return strings.ReplaceAll(name, senderModule, "") }

type decisionSender struct {
	fn  *decisionFunc
	why string
}

type decisionWalk struct {
	funcs  []*decisionFunc
	byName map[string]*decisionFunc // FullName → function
	record *checkedPackage
	// asking and rendering are, by a function's FullName, each position of a call of it
	// (callSite) whose argument the function asks the predicate of, or writes whole into text,
	// itself or through the functions it calls by name (resolve).
	asking, rendering map[string]map[int]bool
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

// recordSwitchKinds is the set of notice kinds the record function name sets out in its switch, as
// the switch spells it: every case value, as the type checker reads its constant. The walk reads
// two: record.SpecDecision's, the kinds a person's spec decision writes, and changesRequestKind's,
// the kinds record.ChangesRequest takes as a person's changes request, which the outbox asks again.
func (w *decisionWalk) recordSwitchKinds(t *testing.T, name string) []string {
	t.Helper()
	var kinds []string
	for _, file := range w.record.files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || fn.Name.Name != name {
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
						t.Fatalf("record.%s has a case the walk cannot read as a constant: %s", name, w.record.pos(expr))
					}
					kinds = append(kinds, constant.StringVal(value))
				}
				return true
			})
		}
	}
	if len(kinds) == 0 {
		t.Fatalf("read no kind from record.%s's switch", name)
	}
	return kinds
}

// checkedPackage is one package's non-test source, type-checked (testtypes.CheckPackage), with
// its files in the order of their names, so the walk reads them, and logs what it finds, in the
// same order on every run; or the planted source, type-checked by the same check
// (testtypes.CheckFiles, plantedDecisionSource.walk).
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

// readDecisionWalk type-checks record and the three sender packages against the compiler's
// export data of their dependencies (compiledImports), and reads every function declaration of
// the three (newDecisionWalk).
func readDecisionWalk(t *testing.T) *decisionWalk {
	t.Helper()
	fset, imports := compiledImports(t)
	recordPackage := checkPackage(t, fset, imports, "record")
	pkgs := make([]*checkedPackage, 0, len(senderPackages))
	for _, name := range senderPackages {
		pkgs = append(pkgs, checkPackage(t, fset, imports, name))
	}
	return newDecisionWalk(recordPackage, pkgs...)
}

// compiledImports is a FileSet and the importer, over it, of the types the compiler left for the
// daemon's dependencies, as supervise's source tests read them (testtypes.CompiledDependencies):
// the build that runs this test has written them into its cache, so the walk reads those rather
// than type-checking the dependency tree from its source. This package imports the other two
// sender packages and record, so the one listing of its dependencies serves the four packages the
// walk checks and the planted source alike (plantedDecisionSource).
func compiledImports(t *testing.T) (*token.FileSet, types.Importer) {
	t.Helper()
	compiled, err := testtypes.CompiledDependencies(senderModule + "daemon")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	return fset, importer.ForCompiler(fset, "gc", compiled)
}

// newDecisionWalk reads every function declaration of pkgs (readDecisionFunc), then what each
// asks the predicate of and writes into text through the functions it calls (resolve). It is the
// one reader of a package's declarations, for the three sender packages (readDecisionWalk) and
// the planted source (plantedDecisionSource.walk) alike. recordPackage is the record package the
// walk reads the kinds of a person's decision from (recordSwitchKinds), nil for planted source.
func newDecisionWalk(recordPackage *checkedPackage, pkgs ...*checkedPackage) *decisionWalk {
	w := &decisionWalk{byName: map[string]*decisionFunc{}, record: recordPackage}
	for _, pkg := range pkgs {
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
	w.resolve()
	return w
}

// readDecisionFunc reads what fn calls, by name and with what at each position (callSites), the
// record.Notice literals it writes, the review sentences its string constants hold, its loops that
// read a kept decision (readKeptLoop), the stored entry tasks it reads, the starts it writes that
// carry a person's answered wait and the notices it publishes.
func readDecisionFunc(pkg *checkedPackage, fn *ast.FuncDecl) *decisionFunc {
	info := pkg.info
	f := &decisionFunc{pkg: pkg.name, name: info.Defs[fn.Name].(*types.Func).FullName(), at: pkg.pos(fn), info: info,
		params: funcParams(info, fn)}
	body := func(visit func(ast.Node) bool) { ast.Inspect(fn.Body, visit) }
	f.calls = namedCalls(info, fn.Body)
	f.sites = callSites(pkg, body)
	f.asks = slices.Contains(f.calls, decisionRule)
	f.picks = slices.Contains(f.calls, senderModule+"record.SpecDecision")
	assigned := localAssignments(info, fn.Body)
	written := writtenExprs(fn.Body)
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch node := n.(type) {
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
			if loop, ok := readKeptLoop(pkg, node, written); ok {
				f.keptLoops = append(f.keptLoops, loop)
			}
		}
		return true
	})
	return f
}

// writtenExprs is the left-hand side of every plain assignment and definition (= and :=) in
// body, function literals included: what each writes is not read there, though what is inside it
// is (an index, the base of a selector). A compound assignment (x.F += y) reads what it writes,
// so its left-hand side is read. The walk takes one such map of each function, for the stored
// entry tasks it reads and for its loops that read a kept decision alike.
func writtenExprs(body *ast.BlockStmt) map[ast.Expr]bool {
	written := map[ast.Expr]bool{}
	ast.Inspect(body, func(n ast.Node) bool {
		if assign, ok := n.(*ast.AssignStmt); ok && (assign.Tok == token.ASSIGN || assign.Tok == token.DEFINE) {
			for _, lhs := range assign.Lhs {
				written[ast.Unparen(lhs)] = true
			}
		}
		return true
	})
	return written
}

// readNoticeKind adds to f the kind of a person's decision a record.Notice literal writes
// (decisionNoticeKind), or the literal's position to f.unread when its kind is no constant the
// walk can read: a constant, or a local every assignment of which is one.
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
			if decisionNoticeKind(record.NoticeKind(value)) && !slices.Contains(f.kinds, value) {
				f.kinds = append(f.kinds, value)
			}
		}
	}
}

// decisionNoticeKind says whether a notice of kind tells a person's decision on a design document:
// record.SpecDecision names kind, or record.ChangesRequest takes a notice of it as a person's
// changes request, which the outbox asks again.
func decisionNoticeKind(kind record.NoticeKind) bool {
	if _, spec := record.SpecDecision(kind); spec {
		return true
	}
	_, asks := record.ChangesRequest(record.Notice{Kind: kind})
	return asks
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

// namedCalls is the FullName of every function the identifiers of body name, each once: a call of
// the function, or the function taken as a value.
func namedCalls(info *types.Info, body *ast.BlockStmt) []string {
	var calls []string
	ast.Inspect(body, func(n ast.Node) bool {
		if ident, ok := n.(*ast.Ident); ok {
			if fun, ok := info.Uses[ident].(*types.Func); ok && !slices.Contains(calls, fun.Origin().FullName()) {
				calls = append(calls, fun.Origin().FullName())
			}
		}
		return true
	})
	return calls
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
