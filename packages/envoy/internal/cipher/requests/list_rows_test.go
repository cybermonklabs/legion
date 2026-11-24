// packages/envoy/internal/cipher/requests/list_rows_test.go
// The review of cybermonklabs/legion#4779 at d7d72138db: a question a row asked before a project
// hid it is still asked once for the list; a row waiting on another row's question holds none of
// the list's question slots, and the list asks at most MaySeeAtOnce questions at once; a list reads
// at most listRowsAtOnce rows at once; a read that panics fails its row closed while the list
// goes on; and, from the review at 5c9c94ec02, once a list's clock has ended its memo asks Dispatch
// nothing more.
package requests

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/identity/kubernetes"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
	"github.com/sjawhar/envoy/internal/cipher/proxyrules"
	"github.com/sjawhar/envoy/internal/logtest"
)

// The projects of these rows: HIDE is private and lists nobody they read as, SHARED is public,
// and BOOM is the project a read panics on.
const (
	hidingProject = "HIDE"
	sharedProject = "SHARED"
	boomProject   = "BOOM"
)

// hiddenMidQuestion is a projectaccess.Access that answers HIDE as a private project the reader is
// not on only once a question about SHARED has arrived, and holds each question about SHARED until
// release is closed; either question ends with its caller's context, answering its error. asked
// counts the questions by project.
type hiddenMidQuestion struct {
	sharedArrived chan struct{}
	arrivedOnce   sync.Once
	release       chan struct{}
	mu            sync.Mutex
	asked         map[string]int
}

func (h *hiddenMidQuestion) Access(ctx context.Context, project, _ string) (projectaccess.Answer, error) {
	h.mu.Lock()
	h.asked[project]++
	h.mu.Unlock()
	switch project {
	case hidingProject:
		select {
		case <-h.sharedArrived:
		case <-ctx.Done():
			return projectaccess.Answer{}, ctx.Err()
		}
		return projectaccess.Answer{Project: project, Visibility: projectaccess.VisibilityPrivate, AskedAt: time.Now()}, nil
	case sharedProject:
		h.arrivedOnce.Do(func() { close(h.sharedArrived) })
		select {
		case <-h.release:
			return projectaccess.Answer{Project: project, Visibility: projectaccess.VisibilityPublic, Visible: true, AskedAt: time.Now()}, nil
		case <-ctx.Done():
			return projectaccess.Answer{}, ctx.Err()
		}
	}
	return projectaccess.Answer{}, fmt.Errorf("the stand-in holds no project %s", project)
}

func (h *hiddenMidQuestion) MemberKind(context.Context, string) (string, error) {
	return projectaccess.MemberFull, nil
}

func (h *hiddenMidQuestion) States(context.Context, []string) (map[string]projectaccess.Answer, error) {
	return nil, errors.New("the stand-in answers no state question")
}

// TestAQuestionAHiddenRowAskedIsAskedOnceForTheList: two rows of one list, one memo, name SHARED,
// and the first also names HIDE, which hides it from the reader. The first row asks both at once,
// so it is the one asking SHARED, and Dispatch answers HIDE only once that question has arrived:
// HIDE hides the row while SHARED is in flight, and maySee ends the row's context. The second row
// then reads SHARED. The question runs on the list's clock, not the first row's context, so it goes
// on, and the second row takes its answer: Dispatch is asked about SHARED once, and the second row
// is shown. On the first row's context the question ends with that row, and the second row asks it
// again. Every wait here ends with the list's clock, so a maySee that asks its projects one after
// another, HIDE first, fails here rather than holding the test: it never asks SHARED while HIDE waits.
func TestAQuestionAHiddenRowAskedIsAskedOnceForTheList(t *testing.T) {
	dispatch := &hiddenMidQuestion{sharedArrived: make(chan struct{}), release: make(chan struct{}), asked: map[string]int{}}
	m := &Machine{Projects: dispatch}
	clock, cancel := context.WithTimeout(t.Context(), listOwnRowsBound)
	defer cancel()
	list := withAccessMemo(clock)
	type seen struct {
		shown bool
		err   error
	}
	hidden := make(chan seen, 1)
	go func() {
		shown, _, err := m.maySee(list, outsider, nil, hidingProject+"-1 and "+sharedProject+"-1: rotate the deploy key")
		hidden <- seen{shown, err}
	}()
	select {
	case <-dispatch.sharedArrived:
	case <-clock.Done():
		t.Fatal("the row naming HIDE and SHARED did not ask about SHARED before the list's clock ended: it must ask both at once")
	}
	other := make(chan seen, 1)
	go func() {
		shown, _, err := m.maySee(list, outsider, nil, sharedProject+"-2: rotate the wiki key")
		other <- seen{shown, err}
	}()
	if got := <-hidden; got.shown || got.err != nil {
		t.Fatalf("maySee(the row naming HIDE and SHARED) = %v, %v; want false, nil: HIDE hides it", got.shown, got.err)
	}
	close(dispatch.release)
	if got := <-other; !got.shown || got.err != nil {
		t.Errorf("maySee(the row naming SHARED) = %v, %v; want true, nil", got.shown, got.err)
	}
	dispatch.mu.Lock()
	defer dispatch.mu.Unlock()
	if dispatch.asked[hidingProject] != 1 {
		t.Fatalf("Dispatch was asked about HIDE %d times; want once: the test is blind", dispatch.asked[hidingProject])
	}
	if n := dispatch.asked[sharedProject]; n != 1 {
		t.Errorf("Dispatch was asked about SHARED %d times; want once for the list, though the row that asked it first was hidden while it was in flight", n)
	}
}

// listUnderTest is one of the two lists a reader's rows are read on: row makes one row on it from
// a pod of its own (runtime) with reason, and answers its id; listed is the ids outsider's list
// answers; key is the attribute a rowUnansweredMessage line names a row under.
type listUnderTest struct {
	name   string
	row    func(t *testing.T, m *Machine, runtime, reason string) string
	listed func(t *testing.T, m *Machine) []string
	key    string
}

// listsUnderTest are the pending list, whose rows are requests of pods that name no person
// (everyonesPod), and the grant list, whose rows are grants of a service's pods.
var listsUnderTest = []listUnderTest{
	{
		name: "the pending list",
		row: func(t *testing.T, m *Machine, runtime, reason string) string {
			t.Helper()
			pod, key := everyonesPod(t, m, runtime, "")
			req, err := m.Create(t.Context(), pod, signRequest(t, m, key, reason, "SHARED_KEY"), "")
			if err != nil || req.RecordID == nil {
				t.Fatalf("Create(SHARED_KEY, %q) = %+v, %v; want it pending", reason, req, err)
			}
			return *req.RecordID
		},
		listed: func(t *testing.T, m *Machine) []string { return pendingRecords(t, m, outsider) },
		key:    "record",
	},
	{
		name: "the grant list",
		row: func(t *testing.T, m *Machine, runtime, reason string) string {
			t.Helper()
			pod, key := newServiceEnrollment(t, m.Store, fixtureService, fixtureSubject, runtime)
			req, err := m.Create(t.Context(), pod, signRequest(t, m, key, reason, "SERVICE_KEY"), "")
			if err != nil || req.GrantID == nil {
				t.Fatalf("Create(SERVICE_KEY, %q) = %+v, %v; want it granted at once", reason, req, err)
			}
			return *req.GrantID
		},
		listed: func(t *testing.T, m *Machine) []string { return approverGrants(t, m, outsider) },
		key:    "grant",
	},
}

// stalledRowsFirst makes, on list, one row whose reason names TEAM-1 and then stalled rows whose
// reasons name SLOW-1, newer and so read first. It points m at a fresh client of a Dispatch that
// holds every question about SLOW for 6 s, past the 5 s Dispatch waits for a list, and answers
// the stand-in, the TEAM row's id and the stalled rows' ids.
func stalledRowsFirst(t *testing.T, m *Machine, list listUnderTest, stalled int) (*stallFor, string, []string) {
	t.Helper()
	team := list.row(t, m, "rows-team", teamProject+"-1: rotate the deploy key")
	var slow []string
	for i := range stalled {
		slow = append(slow, list.row(t, m, fmt.Sprintf("rows-slow-%d", i), stalledProject+"-1: rotate the slow key"))
	}
	answerFrom(t, m, dispatchRows(publicTeam))
	stall := &stallFor{inner: m.Projects, project: stalledProject, hold: 6 * time.Second}
	m.Projects = stall
	return stall, team, slow
}

// TestRowsWaitingOnAStalledProjectHoldNoQuestionSlot: 20 rows whose reasons name SLOW, which
// Dispatch does not answer, are read first, and after them one row whose reason names TEAM,
// public. The list asks SLOW once, and the rows that wait on that question hold none of the
// list's MaySeeAtOnce question slots, so the TEAM row's question takes one and is answered: the
// TEAM row is listed, every SLOW row is left off, and the list answers inside the time Dispatch
// gives it. The list reads its rows at once, so the order in which they reach the memo is not
// fixed, and this row cannot tell on its own whether a waiting row holds a slot:
// TestARowWaitingOnAnotherRowsQuestionHoldsNoSlot does, on the memo itself.
func TestRowsWaitingOnAStalledProjectHoldNoQuestionSlot(t *testing.T) {
	for _, c := range listsUnderTest {
		t.Run(c.name, func(t *testing.T) {
			m, _, _, _ := newProjectFixture(t, proxyrules.Rules{}, publicTeam)
			stall, team, slow := stalledRowsFirst(t, m, c, 20)

			start := time.Now()
			listed := c.listed(t, m)
			took := time.Since(start)
			t.Logf("the list took %s and answered %d rows", took.Round(time.Millisecond), len(listed))

			if n := stall.stalled.Load(); n != 1 {
				t.Errorf("the list asked Dispatch about SLOW %d times; want once, however many rows name it", n)
			}
			if !slices.Contains(listed, team) {
				t.Errorf("the list leaves off the row naming TEAM-1, %s; want it listed: the rows waiting on SLOW hold no question slot", team)
			}
			for _, id := range slow {
				if slices.Contains(listed, id) {
					t.Errorf("the list holds the row naming SLOW-1, %s; want it left off, since Dispatch never answered for it", id)
				}
			}
			if took >= dispatchListWindow-500*time.Millisecond {
				t.Errorf("the list took %s; want it inside the %s Dispatch gives it", took, dispatchListWindow)
			}
		})
	}
}

// publicAnswers is a projectaccess.Access that answers every access question as a public project
// the reader may see, asked now, and once its caller's context has ended answers that context's
// error, as the client does for a question its cache does not hold.
type publicAnswers struct{}

func (publicAnswers) Access(ctx context.Context, project, person string) (projectaccess.Answer, error) {
	if err := ctx.Err(); err != nil {
		return projectaccess.Answer{}, err
	}
	return projectaccess.Answer{Project: project, Person: person, Visibility: projectaccess.VisibilityPublic, Visible: true, AskedAt: time.Now()}, nil
}

func (publicAnswers) MemberKind(context.Context, string) (string, error) {
	return projectaccess.MemberFull, nil
}

func (publicAnswers) States(context.Context, []string) (map[string]projectaccess.Answer, error) {
	return nil, errors.New("the stand-in answers no state question")
}

// listMemo is the memo withAccessMemo put on list, and fails the test when there is none, as a
// withAccessMemo that answers its context unchanged would leave it: the test then fails with a
// message rather than panicking on a nil memo.
func listMemo(t *testing.T, list context.Context) *accessMemo {
	t.Helper()
	memo, ok := list.Value(accessMemoKey{}).(*accessMemo)
	if !ok || memo == nil {
		t.Fatal("withAccessMemo put no memo on the list's context: a list's rows share no question")
	}
	return memo
}

// TestARowWaitingOnAnotherRowsQuestionHoldsNoSlot: on one list's memo, under its 3 s clock, 20
// rows name SLOW, which Dispatch holds until the clock ends. The first row asks it, in one of the
// list's MaySeeAtOnce (16) slots, and the others wait on its question. Once the question has
// reached Dispatch (stallFor's arrived) and 15 rows wait on it (the memo's joined hook), a row
// naming TEAM asks: a waiting row holds no slot, so TEAM's question takes one of the 15 left and
// the row is shown at once. SLOW is asked once: the count is read while the question is still in
// flight, before the clock ends, when no waiting row can ask it again. Were a waiting row to take a
// slot, the 15 would hold every slot the question leaves, TEAM's question could take none before
// the clock ran out, and the row would be ErrProjectAccessUnavailable.
func TestARowWaitingOnAnotherRowsQuestionHoldsNoSlot(t *testing.T) {
	stall := &stallFor{inner: publicAnswers{}, project: stalledProject, hold: time.Hour, arrived: make(chan struct{}, 1)}
	m := &Machine{Projects: stall}
	clock, cancel := context.WithTimeout(t.Context(), listOwnRowsBound)
	defer cancel()
	list := withAccessMemo(clock)
	// The hook drops a signal it has no room for rather than block: once the clock ends, the rows
	// that find the question cut short each join again as they ask afresh, more often than any
	// buffer counts, and a blocked hook would hold its row, and this test, for good.
	joined := make(chan struct{}, 32)
	listMemo(t, list).joined = func() {
		select {
		case joined <- struct{}{}:
		default:
		}
	}
	var slow sync.WaitGroup
	for range 20 {
		slow.Go(func() { _, _, _ = m.maySee(list, outsider, nil, stalledProject+"-1: rotate the slow key") })
	}
	select {
	case <-stall.arrived:
	case <-clock.Done():
		t.Fatal("the question about SLOW did not reach Dispatch before the list's clock ended")
	}
	for range MaySeeAtOnce - 1 {
		select {
		case <-joined:
		case <-clock.Done():
			t.Fatal("15 rows did not wait on SLOW's question before the list's clock ended")
		}
	}

	shown, _, err := m.maySee(list, outsider, nil, teamProject+"-1: rotate the deploy key")
	asked := stall.stalled.Load()
	cancel()
	slow.Wait()

	if !shown || err != nil {
		t.Errorf("maySee(the row naming TEAM-1) while 15 rows wait on SLOW = %v, %v; want true, nil: a waiting row holds no slot", shown, err)
	}
	if asked != 1 {
		t.Errorf("Dispatch was asked about SLOW %d times; want once, however many rows name it", asked)
	}
}

// liveCalls is a projectaccess.Access that counts the access questions it is asked on a context
// still live when they arrive, the ones a client would send to Dispatch, and passes each to inner.
type liveCalls struct {
	inner projectaccess.Access
	live  atomic.Int32
}

func (l *liveCalls) Access(ctx context.Context, project, person string) (projectaccess.Answer, error) {
	if ctx.Err() == nil {
		l.live.Add(1)
	}
	return l.inner.Access(ctx, project, person)
}

func (l *liveCalls) MemberKind(ctx context.Context, person string) (string, error) {
	return l.inner.MemberKind(ctx, person)
}

func (l *liveCalls) States(ctx context.Context, projects []string) (map[string]projectaccess.Answer, error) {
	return l.inner.States(ctx, projects)
}

// TestNoQuestionIsAskedOnceTheListsClockHasEnded: once a list's clock has ended, its memo asks
// Dispatch nothing more, even for a read whose own context still reads as live, as a row's does
// for a moment while the clock's end reaches the contexts derived from it one after another. SLOW's
// question reaches Dispatch on the list's clock; the clock ends and cuts it short. Then, on a
// context the clock does not end, a row naming SLOW finds the question cut, and a row naming 16
// projects the memo never held asks each: no question reaches Dispatch on a live context, and both
// rows are ErrProjectAccessUnavailable. Were a question started after the clock's end, it would
// reach Dispatch unless onList's cancel, which runs on a goroutine of its own, ended it first, a
// race each of the 17 questions runs. Dispatch holds SLOW for the length of a list's clock, so a
// read that wrongly asks it on a live context fails here rather than holding the test.
func TestNoQuestionIsAskedOnceTheListsClockHasEnded(t *testing.T) {
	stall := &stallFor{inner: publicAnswers{}, project: stalledProject, hold: listOwnRowsBound, arrived: make(chan struct{}, 1)}
	dispatch := &liveCalls{inner: stall}
	m := &Machine{Projects: dispatch}
	clock, cancel := context.WithCancel(t.Context())
	list := withAccessMemo(clock)
	memo := listMemo(t, list)
	first := make(chan struct{})
	go func() {
		defer close(first)
		_, _, _ = m.maySee(list, outsider, nil, stalledProject+"-1: rotate the slow key")
	}()
	<-stall.arrived
	cancel()
	<-first
	memo.mu.Lock()
	cutShort := memo.entries[accessMemoQuestion{project: stalledProject, person: outsider}]
	memo.mu.Unlock()
	if cutShort == nil {
		t.Fatal("the memo holds no question about SLOW: the test is blind")
	}
	<-cutShort.done
	if !cutShort.cut {
		t.Fatalf("SLOW's question ended with %v and was not cut short by the clock: the test is blind", cutShort.err)
	}
	before := dispatch.live.Load()
	if before != 1 {
		t.Fatalf("Dispatch was asked %d questions on a live context before the clock ended; want 1, SLOW's: the test is blind", before)
	}

	live := context.WithValue(context.Background(), accessMemoKey{}, memo)
	keys := make([]string, 16)
	for i := range keys {
		keys[i] = fmt.Sprintf("E%c-1", 'A'+rune(i))
	}
	for _, reason := range []string{stalledProject + "-1: rotate the slow key", strings.Join(keys, " ")} {
		if shown, _, err := m.maySee(live, outsider, nil, reason); shown || !errors.Is(err, ErrProjectAccessUnavailable) {
			t.Errorf("maySee(%q) once the list's clock has ended = %v, %v; want false, ErrProjectAccessUnavailable", reason, shown, err)
		}
	}
	if asked := dispatch.live.Load() - before; asked != 0 {
		t.Errorf("Dispatch was asked %d questions on a live context once the list's clock had ended; want none", asked)
	}
}

// TestAListReadsAtMostSixtyFourRowsAtOnce: a list reads at most listRowsAtOnce (64) rows at once,
// and a row waiting on a project Dispatch does not answer keeps its place until the list's clock
// runs out. With 63 rows naming SLOW read first, the row naming TEAM after them is read at once,
// and listed. With 64, it starts only once the clock has run out, when its question can no longer
// be asked, so it is left off and logged with the SLOW rows: the threshold listRowsAtOnce's doc
// states. With no bound on the rows read at once, the TEAM row is listed after 64 too.
func TestAListReadsAtMostSixtyFourRowsAtOnce(t *testing.T) {
	for _, c := range listsUnderTest {
		for _, k := range []struct {
			stalled int
			listed  bool
		}{
			{listRowsAtOnce - 1, true},
			{listRowsAtOnce, false},
		} {
			t.Run(fmt.Sprintf("%s, %d rows naming SLOW read first", c.name, k.stalled), func(t *testing.T) {
				m, _, _, _ := newProjectFixture(t, proxyrules.Rules{}, publicTeam)
				stall, team, slow := stalledRowsFirst(t, m, c, k.stalled)
				capture := &logtest.Recorder{}
				logtest.SetDefault(t, capture)

				listed := c.listed(t, m)

				if stall.stalled.Load() == 0 {
					t.Fatal("the list asked Dispatch nothing about SLOW, so nothing stalled: the test is blind")
				}
				for _, id := range slow {
					if slices.Contains(listed, id) {
						t.Fatalf("the list holds the row naming SLOW-1, %s; want it left off", id)
					}
				}
				if got := slices.Contains(listed, team); got != k.listed {
					t.Errorf("after %d rows naming SLOW, the list lists the row naming TEAM-1: %v; want %v", k.stalled, got, k.listed)
				}
				var logged bool
				for _, line := range linesOf(capture, rowUnansweredMessage) {
					logged = logged || line[c.key] == team
				}
				if logged == k.listed {
					t.Errorf("a %q line names the TEAM row %s: %v; want %v", rowUnansweredMessage, team, logged, !k.listed)
				}
			})
		}
	}
}

// TestTheQuestionsAListAsksAtOnceNeverExceedTheCap: 24 rows, each whose reason names 4 projects of
// its own, 96 questions Dispatch holds 50 ms each, are read on one list. The list asks each once,
// and never more than MaySeeAtOnce at once, though its rows' reads would ask all 96 together; it
// asks MaySeeAtOnce at once, so the count sees questions held together. Each project is one
// Dispatch does not hold, which hides nothing, so every row is listed.
func TestTheQuestionsAListAsksAtOnceNeverExceedTheCap(t *testing.T) {
	const rows, projectsEach = 24, 4
	m, _, _, dispatch := newProjectFixture(t, proxyrules.Rules{}, publicTeam)
	pending := listsUnderTest[0]
	var ids []string
	for r := range rows {
		keys := make([]string, projectsEach)
		for k := range keys {
			keys[k] = fmt.Sprintf("Q%c%c-1", 'A'+rune(r), 'A'+rune(k))
		}
		ids = append(ids, pending.row(t, m, fmt.Sprintf("rows-cap-%d", r), strings.Join(keys, " ")))
	}
	dispatch.SetStall(50 * time.Millisecond)
	log := &fanOutLog{inner: projectaccess.New(dispatch.URL, dispatch.Account, nil)}
	m.Projects = log

	listed := pending.listed(t, m)
	t.Logf("the list asked %d questions, at most %d at once", len(log.asked), log.most)

	for _, id := range ids {
		if !slices.Contains(listed, id) {
			t.Errorf("the list leaves off %s; want every row listed, since no project it names hides it", id)
		}
	}
	if len(log.asked) != rows*projectsEach {
		t.Errorf("the list asked %d distinct questions; want %d", len(log.asked), rows*projectsEach)
	}
	for question, n := range log.asked {
		if n != 1 {
			t.Errorf("the list asked %q %d times; want once", question, n)
		}
	}
	if log.most > MaySeeAtOnce {
		t.Errorf("the list held %d questions at once; want at most MaySeeAtOnce (%d)", log.most, MaySeeAtOnce)
	}
	if log.most < MaySeeAtOnce {
		t.Errorf("the list held at most %d questions at once; want MaySeeAtOnce (%d), which 96 questions wanted together reach, or the count is blind", log.most, MaySeeAtOnce)
	}
}

// panicsAbout is a projectaccess.Access that panics on every access question about project, and on
// every state question while states is set; every other question goes to inner.
type panicsAbout struct {
	inner   projectaccess.Access
	project string
	states  bool
}

func (p panicsAbout) Access(ctx context.Context, project, person string) (projectaccess.Answer, error) {
	if project == p.project {
		panic("the stand-in's access question about " + project + " panicked")
	}
	return p.inner.Access(ctx, project, person)
}

func (p panicsAbout) MemberKind(ctx context.Context, person string) (string, error) {
	return p.inner.MemberKind(ctx, person)
}

func (p panicsAbout) States(ctx context.Context, projects []string) (map[string]projectaccess.Answer, error) {
	if p.states {
		panic(fmt.Sprintf("the stand-in's state question about %v panicked", projects))
	}
	return p.inner.States(ctx, projects)
}

// panicLogged reports whether capture holds a readPanickedMessage line at ERROR whose error names
// errReadPanicked and whose stack is written.
func panicLogged(capture *logtest.Recorder) bool {
	for _, line := range linesOf(capture, readPanickedMessage) {
		if line["level"] == "ERROR" && strings.Contains(line["error"], errReadPanicked.Error()) && line["stack"] != "" {
			return true
		}
	}
	return false
}

// TestAReadThatPanicsFailsItsRowClosedAndTheListGoesOn: a panic in a goroutine a read of the person
// rule starts is that read's failure, never the broker's end. A question of a list's memo that
// panics (BOOM, on both lists) leaves off the row naming it, logged as one Dispatch did not answer
// for, and lists the row naming TEAM. A row's own read that panics (the state question its tagged
// secret asks: PRIV_HUMAN pending, and PRIV_KEY granted at once, which every reader's grant list
// reads) leaves off that row and lists the untagged one beside it. A decision's question that
// panics, asked outside any list, is ErrProjectAccessUnavailable. Each panic is logged at ERROR
// with its stack. Unrecovered, each panic ends the test binary.
func TestAReadThatPanicsFailsItsRowClosedAndTheListGoesOn(t *testing.T) {
	for _, c := range listsUnderTest {
		t.Run("a question of "+c.name, func(t *testing.T) {
			m, _, _, _ := newProjectFixture(t, proxyrules.Rules{}, publicTeam)
			team := c.row(t, m, "panic-team", teamProject+"-1: rotate the deploy key")
			boom := c.row(t, m, "panic-boom", boomProject+"-1: rotate the boom key")
			m.Projects = panicsAbout{inner: m.Projects, project: boomProject}
			capture := &logtest.Recorder{}
			logtest.SetDefault(t, capture)

			listed := c.listed(t, m)

			if !slices.Contains(listed, team) {
				t.Errorf("the list leaves off the row naming TEAM-1, %s; want it listed: the list goes on", team)
			}
			if slices.Contains(listed, boom) {
				t.Errorf("the list holds the row naming BOOM-1, %s; want it left off, failing closed", boom)
			}
			if !panicLogged(capture) {
				t.Errorf("the log holds %v; want an ERROR line of %q with its stack", linesOf(capture, readPanickedMessage), readPanickedMessage)
			}
			var unanswered bool
			for _, line := range linesOf(capture, rowUnansweredMessage) {
				unanswered = unanswered || line[c.key] == boom
			}
			if !unanswered {
				t.Errorf("the log holds %v; want a %q line naming %s %s", linesOf(capture, rowUnansweredMessage), rowUnansweredMessage, c.key, boom)
			}
		})
	}

	// privatePod is a pod of a service whose account an operator's entry maps to PRIV for insider
	// (BROKER_EXCHANGE_ACCOUNTS), as TestAPrivateProjectsPodRequestIsDecidedListedAndShownOnlyToItsPeople
	// makes it: its requests and grants are PRIV's.
	privatePod := func(t *testing.T, m *Machine) (string, *ecdsa.PrivateKey) {
		t.Helper()
		set, err := kubernetesPolicy([]kubernetes.AccountEntry{{Account: fixtureSubject, Project: privProject, Person: insider}})
		if err != nil {
			t.Fatalf("exchange.NewSet: %v", err)
		}
		m.Exchange = set
		return newServiceEnrollment(t, m.Store, fixtureService, fixtureSubject, "pod-priv-panic")
	}

	t.Run("a row's own read on the pending list", func(t *testing.T) {
		m, _, _, _ := newProjectFixture(t, proxyrules.Rules{}, publicTeam)
		pod, key := privatePod(t, m)
		plain, err := m.Create(t.Context(), pod, signRequest(t, m, key, "rotate the vendor key", "SHARED_KEY"), "")
		if err != nil || plain.RecordID == nil {
			t.Fatalf("PRIV's pod: Create(SHARED_KEY) = %+v, %v; want pending", plain, err)
		}
		tagged, err := m.Create(t.Context(), pod, signRequest(t, m, key, "deploy", "PRIV_HUMAN"), "")
		if err != nil || tagged.RecordID == nil {
			t.Fatalf("PRIV's pod: Create(PRIV_HUMAN) = %+v, %v; want pending", tagged, err)
		}
		if listed := pendingRecords(t, m, insider); !slices.Contains(listed, *tagged.RecordID) || !slices.Contains(listed, *plain.RecordID) {
			t.Fatalf("PendingForApprover(%s) before any panic = %v; want both of PRIV's pod's requests: the test is blind", insider, listed)
		}
		m.Projects = panicsAbout{inner: m.Projects, states: true}
		capture := &logtest.Recorder{}
		logtest.SetDefault(t, capture)

		listed := pendingRecords(t, m, insider)

		if !slices.Contains(listed, *plain.RecordID) {
			t.Errorf("the list leaves off the untagged request %s; want it listed: the list goes on", *plain.RecordID)
		}
		if slices.Contains(listed, *tagged.RecordID) {
			t.Errorf("the list holds the PRIV_HUMAN request %s, whose read panicked; want it left off, failing closed", *tagged.RecordID)
		}
		if !panicLogged(capture) {
			t.Errorf("the log holds %v; want an ERROR line of %q with its stack", linesOf(capture, readPanickedMessage), readPanickedMessage)
		}
	})

	t.Run("a row's own read on the grant list", func(t *testing.T) {
		m, _, _, _ := newProjectFixture(t, proxyrules.Rules{}, publicTeam)
		pod, key := privatePod(t, m)
		plain, err := m.Create(t.Context(), pod, signRequest(t, m, key, "rotate the service key", "SERVICE_KEY"), "")
		if err != nil || plain.GrantID == nil {
			t.Fatalf("PRIV's pod: Create(SERVICE_KEY) = %+v, %v; want it granted at once", plain, err)
		}
		tagged, err := m.Create(t.Context(), pod, signRequest(t, m, key, "deploy", "PRIV_KEY"), "")
		if err != nil || tagged.GrantID == nil {
			t.Fatalf("PRIV's pod: Create(PRIV_KEY) = %+v, %v; want it granted at once", tagged, err)
		}
		if listed := approverGrants(t, m, fixtureOperator); !slices.Contains(listed, *tagged.GrantID) || !slices.Contains(listed, *plain.GrantID) {
			t.Fatalf("GrantsForApprover(%s) before any panic = %v; want both of PRIV's pod's grants: the test is blind", fixtureOperator, listed)
		}
		m.Projects = panicsAbout{inner: m.Projects, states: true}
		capture := &logtest.Recorder{}
		logtest.SetDefault(t, capture)

		listed := approverGrants(t, m, fixtureOperator)

		if !slices.Contains(listed, *plain.GrantID) {
			t.Errorf("the list leaves off the untagged grant %s; want it listed: the list goes on", *plain.GrantID)
		}
		if slices.Contains(listed, *tagged.GrantID) {
			t.Errorf("the list holds the PRIV_KEY grant %s, whose read panicked; want it left off, failing closed", *tagged.GrantID)
		}
		if !panicLogged(capture) {
			t.Errorf("the log holds %v; want an ERROR line of %q with its stack", linesOf(capture, readPanickedMessage), readPanickedMessage)
		}
	})

	t.Run("a decision's question", func(t *testing.T) {
		m, _, _, _ := newProjectFixture(t, proxyrules.Rules{}, publicTeam)
		boom := listsUnderTest[0].row(t, m, "panic-decision", boomProject+"-1: rotate the boom key")
		m.Projects = panicsAbout{inner: m.Projects, project: boomProject}
		capture := &logtest.Recorder{}
		logtest.SetDefault(t, capture)

		if err := m.HideUnseen(t.Context(), boom, outsider); !errors.Is(err, ErrProjectAccessUnavailable) {
			t.Errorf("HideUnseen(the request naming BOOM-1, %s) = %v; want ErrProjectAccessUnavailable, failing closed", outsider, err)
		}
		if !panicLogged(capture) {
			t.Errorf("the log holds %v; want an ERROR line of %q with its stack", linesOf(capture, readPanickedMessage), readPanickedMessage)
		}
	})
}
