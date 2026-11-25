package supervise

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// workingCopyAnswer is what a test's Deps.WorkingCopy answers at a send, as the daemon's answer
// gives it (workingCopy): the directory the runtime gives the repository the claim's process
// works, or the error the answer fails with, and every delivery the send asked about.
type workingCopyAnswer struct {
	mu    sync.Mutex
	dir   string
	err   error
	asked []string
}

// directoryClause is the directory a task's working copy line names, which the answer writes again:
// the line ends with a full stop before the line break that ends it.
var directoryClause = regexp.MustCompile(`, directory [^ \n]+\.`)

func (r *workingCopyAnswer) answer(_ context.Context, _ Claim, d Delivery) (string, []byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.asked = append(r.asked, d.ID)
	if r.err != nil {
		return "", nil, r.err
	}
	return directoryClause.ReplaceAllLiteralString(d.Task, ", directory "+r.dir+"."), d.News, nil
}

// moves is the answer naming another directory from here on, as it does once the claim's process
// is launched in another repository's workspace, or failing.
func (r *workingCopyAnswer) moves(dir string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dir, r.err = dir, err
}

func (r *workingCopyAnswer) askedAbout() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.asked...)
}

// workingCopyTask is a phase worker's task whose working copy line names dir, on a line of its own
// before the continue line, as the workflow composes one.
func workingCopyTask(dir string) string {
	return "Working copy: branch legion/LEGION-209, directory " + dir +
		".\nContinue Workflow. Issue: LEGION-209. Phase: implementing. Implement the plan."
}

const (
	leftDirectory  = "/workspace/acme/widgets/LEGION-209"
	movedDirectory = "/workspace/zeta/tools/LEGION-209"
)

// A phase worker's task is composed when its start is written and held until a send, and the
// workspace the process it goes to works can differ from the one it was composed for. startSend,
// the one place a prompt goes out, writes the task's working copy line from the answer first
// (currentWorkingCopy, Deps.WorkingCopy), so every send names the directory the answer gives at
// that send: the first send of a task composed for another directory, and a resend after a send
// the transport lost, which went out before the answer moved. The task it writes is persisted
// before it goes, under a new id, so a restart reads what was sent and the shim forwards the new
// text. An answer that fails sends nothing and drops nothing: the task waits, charged nothing, and
// the next trigger sends it naming the answer's directory. A task of no phase, an operator's own,
// is never asked about.
func TestEverySendOfAPhaseTaskNamesTheWorkingCopyTheAnswerGivesAtThatSend(t *testing.T) {
	ready := func(t *testing.T, answer *workingCopyAnswer) *harness {
		h := newBareHarness(t)
		h.deps.WorkingCopy = answer.answer
		if err := h.store.PutClaim(h.ctx, queuedClaim()); err != nil {
			t.Fatal(err)
		}
		h.start(queuedClaim())
		h.reach(StateReady)
		return h
	}
	task := RequestDeliver{Claim: testToken, ID: "outbox:30", Task: workingCopyTask(leftDirectory), Phase: phase.Implementing, Generation: 1}
	wantSentMoved := func(t *testing.T, h *harness, sent string) {
		t.Helper()
		if want := workingCopyTask(movedDirectory); sent != want {
			t.Fatalf("the worker was sent %q, naming the directory the task was composed for; want %q", sent, want)
		}
		p := h.pending()
		stored, ok := h.store.delivery(testToken)
		if p.Task != workingCopyTask(movedDirectory) || !ok || stored.Task != p.Task || stored.ID != p.ID || p.ID == task.ID {
			t.Fatalf("the delivery after the send = %+v, stored %+v (%v); want the task naming %s, persisted, under a new id",
				p, stored, ok, movedDirectory)
		}
	}

	t.Run("a task composed for another directory is sent naming the directory the answer names at its send", func(t *testing.T) {
		answer := &workingCopyAnswer{dir: movedDirectory}
		h := ready(t, answer)
		h.must(task)
		wantSentMoved(t, h, h.wantPrompts(1)[0].Message)
	})

	t.Run("a task sent before the answer moved is sent again naming the directory it names then", func(t *testing.T) {
		answer := &workingCopyAnswer{dir: leftDirectory}
		h := ready(t, answer)
		h.conn.FailPrompt(errors.New("the stream to the shim closed"))
		h.must(task)
		if sent := h.wantPrompts(1)[0]; sent.Message != task.Task || sent.DeliveryID != task.ID {
			t.Fatalf("the send before the move = %+v, want the task as it was queued: the case's own premise", sent)
		}
		h.wantBudgets(Budgets{})

		answer.moves(movedDirectory, nil)
		h.conn.FailPrompt(nil)
		h.observe(runtime.Alive)
		wantSentMoved(t, h, h.wantPrompts(2)[1].Message)
	})

	t.Run("an answer that fails sends nothing and drops nothing", func(t *testing.T) {
		answer := &workingCopyAnswer{err: errors.New("the news the task held does not decode")}
		h := ready(t, answer)
		if err := h.handle(task); err == nil || !strings.Contains(err.Error(), "the news the task held does not decode") {
			t.Fatalf("the send whose answer failed answered %v, want the answer's error", err)
		}
		h.wantPrompts(0)
		if p := h.pending(); p.ID != task.ID || p.Task != task.Task {
			t.Fatalf("the delivery after the failed answer = %+v, want the task kept as it was queued", p)
		}
		h.wantBudgets(Budgets{})

		answer.moves(movedDirectory, nil)
		h.observe(runtime.Alive)
		wantSentMoved(t, h, h.wantPrompts(1)[0].Message)
	})

	t.Run("a task of no phase is sent as it was queued", func(t *testing.T) {
		answer := &workingCopyAnswer{dir: movedDirectory}
		h := ready(t, answer)
		operators := RequestDeliver{Claim: testToken, Task: workingCopyTask(leftDirectory)}
		h.must(operators)
		if sent := h.wantPrompts(1)[0]; sent.Message != operators.Task {
			t.Fatalf("the operator's task was sent as %q, want it as the operator wrote it", sent.Message)
		}
		if asked := answer.askedAbout(); len(asked) != 0 {
			t.Fatalf("the send asked about %v, want nothing asked of a task of no phase", asked)
		}
	})
}

// repositoryText is a claim's working repository as a failure names it.
func repositoryText(repository *ghrepo.Repository) string {
	switch {
	case repository == nil:
		return "none recorded"
	case repository.IsZero():
		return "the zero Repository"
	}
	return repository.String()
}

// The working copy line of every phase task names the workspace the process it goes to works, so
// the launch that records a process records that repository with it, in the same write
// (launchTries, workingRepository): the repository the place the process runs in names
// (runtime.Runtime.PodRepository) where the runtime answers one, since a role launched into a live
// pod works that pod's clone whatever its spec names, and the spec's own repository where the
// runtime answers none, as tmux does, or cannot answer, which the launch says at Warn naming both.
// A launch that names no repository records the zero Repository, which is not none recorded. The
// let-go that clears the locator clears it too, so it never outlives the process it describes, and
// the next launch records the repository of its own process.
func TestALaunchRecordsTheRepositoryItsProcessWorks(t *testing.T) {
	acme, zeta := ghrepo.MustParse("acme/widgets"), ghrepo.MustParse("zeta/tools")
	launched := func(t *testing.T, repository ghrepo.Repository, place func(h *harness)) *harness {
		t.Helper()
		h := newBareHarness(t)
		h.specs.names(repository)
		if err := h.store.PutClaim(h.ctx, queuedClaim()); err != nil {
			t.Fatal(err)
		}
		place(h)
		h.start(queuedClaim())
		h.launch()
		return h
	}
	wantWorking := func(t *testing.T, h *harness, want *ghrepo.Repository) {
		t.Helper()
		for _, read := range []struct {
			what string
			c    Claim
		}{{"the claim", h.claim()}, {"the claim the store holds", h.store.load(h.token)}} {
			got := read.c.WorkingRepository
			if (got == nil) != (want == nil) || (got != nil && *got != *want) {
				t.Fatalf("%s records the working repository %s, want %s", read.what, repositoryText(got), repositoryText(want))
			}
		}
	}
	nothing := func(*harness) {}

	t.Run("a runtime whose processes share no place records the spec's repository", func(t *testing.T) {
		h := launched(t, acme, nothing)
		wantWorking(t, h, &acme)
	})

	t.Run("the place's own repository comes before the spec's", func(t *testing.T) {
		h := launched(t, zeta, func(h *harness) { h.rt.ScriptPodRepository(h.token, acme.String()) })
		wantWorking(t, h, &acme)
		if asked := h.wantCalls("PodRepository", 1)[0].Pod; asked.Locator == nil || asked.Locator.Incarnation != h.locator().Incarnation {
			t.Fatalf("the launch asked the runtime about %+v, want the process it launched, %s", asked, h.locator().Incarnation)
		}
	})

	t.Run("a runtime that cannot answer leaves the spec's repository, said at Warn", func(t *testing.T) {
		h := launched(t, zeta, func(h *harness) { h.rt.FailPodRepository(errors.New("the cluster did not answer")) })
		wantWorking(t, h, &zeta)
		if lines := h.logs.lines("level=WARN", "did not say which repository", "specRepository=zeta/tools", "the cluster did not answer"); len(lines) != 1 {
			t.Fatalf("logged %v, want one warning naming the spec's repository and the runtime's error", lines)
		}
	})

	t.Run("a launch that names no repository records the zero Repository", func(t *testing.T) {
		h := launched(t, ghrepo.Repository{}, nothing)
		wantWorking(t, h, &ghrepo.Repository{})
	})

	t.Run("the let-go that clears the locator clears it, and the next launch records its own", func(t *testing.T) {
		h := launched(t, acme, nothing)
		h.reach(StateReady)
		wantWorking(t, h, &acme)

		h.must(RequestSuspend{Claim: h.token, By: SuspendedByOperator})
		h.wantState(StateSuspended)
		if loc := h.claim().Locator; loc != nil {
			t.Fatalf("the suspended claim records the process %+v, want none: the case's own premise", loc)
		}
		wantWorking(t, h, nil)

		h.specs.names(zeta)
		h.must(RequestResume{Claim: h.token})
		h.wantState(StateLaunching)
		wantWorking(t, h, &zeta)
	})
}
