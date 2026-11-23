package supervise

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// The cases here are where news of the approved design meets the work an entry hands a role
// (dispatch://INFRA-175 meeting dispatch://INFRA-2614): which of a task of news alone and a task
// of an entry a worker is sent first, what a task of news alone raises of the claim's entry and
// handshake, and what a resent task of an entry records of the news its own prompt carried.

// newsAloneTask is the task a start queued for news of the approved design alone hands token's
// claim (RequestDeliver.NewsOnly, the daemon's news-only start): of the run at generation, naming
// phase p (none for an architect, whose starts name none), and carrying no entry.
func newsAloneTask(token claim.Token, row int64, generation uint64, p phase.Phase) RequestDeliver {
	return RequestDeliver{Claim: token, ID: fmt.Sprintf("outbox:%d", row), Row: row, Phase: p, Generation: generation,
		Task: "The approved design of LEGION-208 changed.", News: []byte(newsOnlyNews), NewsOnly: true}
}

// newsRole is one claim a task of news alone can reach, and the phase its starts name.
type newsRole struct {
	name  string
	claim Claim
	phase phase.Phase
}

// newsRoles is the two kinds of claim a task of news alone reaches: a tree's root architect, which
// is told the summary of its tree, and a phase worker, which is told its own issue's news.
var newsRoles = []newsRole{
	{"a tree's root architect", rootClaim(), ""},
	{"a phase worker", queuedClaim(), phase.Implementing},
}

// newsHolding is one way a claim comes to hold a task of news alone that no turn has taken, with
// no send of it in flight and no prompt of it waiting on a result, and how its agent then comes to
// take the next prompt.
type newsHolding struct {
	name string
	hold func(h *harness, task RequestDeliver)
	up   func(h *harness)
}

var newsHoldings = []newsHolding{
	// The start's task reached a claim whose launch has not come up, so nothing has sent it.
	{"queued while the launch runs", func(h *harness, task RequestDeliver) {
		h.t.Helper()
		h.launch()
		h.must(task)
	}, bringUp},
	// The task was sent into a turn of the agent's own, which answered it busy: the claim works
	// that turn, and the turn's end makes the next send.
	{"sent into a turn of the agent's own and answered busy", func(h *harness, task RequestDeliver) {
		h.t.Helper()
		h.reach(StateReady)
		h.conn.SetStreaming(true)
		h.conn.RefusePrompt(busyAnswer)
		h.must(task)
		h.wantState(StateWorking)
	}, func(h *harness) {
		h.t.Helper()
		h.conn.FailPrompt(nil)
		h.conn.SetStreaming(false)
		h.mustQuiet(StreamTurnEnd{Claim: h.token})
	}},
	// The task was sent and acknowledged, no turn took it, and its process went, and with it the
	// wait for that prompt's result (letGo); the claim relaunches holding it.
	{"sent and acknowledged, then its process went before any turn", func(h *harness, task RequestDeliver) {
		h.t.Helper()
		h.reach(StateReady)
		h.must(task)
		if len(h.outstanding()) != 1 {
			h.t.Fatalf("prompts %v await a result after the send, want the one acknowledged", h.outstanding())
		}
		h.observe(runtime.Gone)
	}, func(h *harness) {
		h.t.Helper()
		h.relaunched()
	}},
}

// A claim that holds a task of news alone that no turn has taken, with nothing of it in the
// agent's hands, has it superseded by a task of an entry, whatever the role: the entry's task is
// pending in its place, written with its entry and that entry's handshake, the task of news alone
// is retired unsent, and the one prompt the agent is sent once it comes up is the entry's task.
// The start of an entry carries the news itself (the daemon's newsForStart reads what the record
// owes when the start runs, and the untaken task raised none of it), so nothing of the news is
// lost, and the work the role is owed does not wait a turn and a backoff behind the news. Refused
// instead, as a task of no entry refuses a newer one under the root architect's rule (a wake's),
// the entry's task would wait for a turn of the news first.
func TestAnEntrysTaskTakesTheUntakenNewsTasksPlaceForEveryRole(t *testing.T) {
	for _, role := range newsRoles {
		for _, held := range newsHoldings {
			t.Run(role.name+"/"+held.name, func(t *testing.T) {
				h := newHarnessOf(t, role.claim)
				news := newsAloneTask(h.token, 3, 2, role.phase)
				held.hold(h, news)
				heldUnconfirmed(h, news)
				sent := len(h.prompts())
				entry := startTask(h.token, 15, 2, 2)
				entry.Phase, entry.News = role.phase, []byte(queuedNews)

				if err := h.handle(entry); err != nil {
					t.Fatalf("the entry's task was answered %v, want it queued in place of the news task %s", err, news.ID)
				}

				p := h.pending()
				if p.ID != entry.ID || p.NewsOnly || p.Entry != entry.Entry || !bytes.Equal(p.News, entry.News) {
					t.Fatalf("pending %+v, want the entry's task %s carrying its news", p, entry.ID)
				}
				if got := h.claim(); got.GivenEntry != entry.Entry || got.Handshake.Entry != entry.Entry {
					t.Fatalf("the claim records entry %d given with handshake %+v, want entry %d", got.GivenEntry, got.Handshake, entry.Entry)
				}
				log := h.store.deliveryLog()
				want := []string{"retire " + news.ID, "put " + entry.ID}
				if len(log) < 2 || !slices.Equal(log[len(log)-2:], want) {
					t.Fatalf("the delivery writes = %v, want them to end %v: the news task retired, then the entry's written", log, want)
				}
				if lines := h.logs.lines("gives way to the newer entry's task", "superseded="+news.ID, "delivery="+entry.ID); len(lines) != 1 {
					t.Fatalf("logged %v, want one line naming both deliveries", lines)
				}

				held.up(h)

				prompts := h.prompts()[sent:]
				if len(prompts) != 1 || prompts[0].DeliveryID != entry.ID || !strings.Contains(prompts[0].Message, entry.Task) {
					t.Fatalf("prompts once the agent came up = %+v, want the entry's task alone", prompts)
				}
			})
		}
	}
}

// A start of no entry carries no news (the daemon's startNewsOf), so a task of news alone is not
// superseded by it: the claim refuses it behind the untaken news task, as behind any untaken task,
// and sends the news task first. Once that task's turn is over the start's retry is queued and
// sent. So the agent is sent the two in that order: the news, then the task of no entry — a root
// architect's wake (workflow's wakeRow), or a phase worker's resume task.
func TestAStartOfNoEntryWaitsBehindAnUntakenNewsTask(t *testing.T) {
	for _, role := range newsRoles {
		t.Run(role.name, func(t *testing.T) {
			h := newHarnessOf(t, role.claim)
			h.launch()
			news := newsAloneTask(h.token, 3, 2, role.phase)
			h.must(news)
			later := RequestDeliver{Claim: h.token, ID: "outbox:15", Row: 15, Phase: role.phase, Generation: 2,
				Task: "Your pod was released and has come back: a message was published to your role."}

			if err := h.handle(later); !errors.Is(err, ErrDeliveryPending) {
				t.Fatalf("the start of no entry was answered %v, want it refused behind the news task (%v)", err, ErrDeliveryPending)
			}
			if p := h.pending(); p.ID != news.ID {
				t.Fatalf("pending %+v, want the news task still held", p)
			}

			bringUp(h)
			takeTurn(h, news.ID)
			h.must(later)

			var order []string
			for _, prompt := range h.prompts() {
				order = append(order, prompt.DeliveryID)
			}
			if want := []string{news.ID, later.ID}; !slices.Equal(order, want) {
				t.Fatalf("the agent was prompted %v, want %v: the news first, then the start of no entry", order, want)
			}
		})
	}
}

// A task of news alone hands the claim no entry's work, so it is no part of what the claim was
// given or of the handshake over its entry: queued to a claim given entry 2 whose handshake of
// that entry no turn has confirmed, it leaves the given entry and the handshake as they stand, and
// a turn that takes it confirms nothing of that entry, so the state does not read the entry as one
// the worker took (dispatch://INFRA-2614 R6), and the sweep still reads the claim as owed the
// entry's work while it holds nothing else. Its retirement after that turn leaves the served entry
// too.
func TestATaskOfNewsAloneRaisesNoEntryAndConfirmsNoHandshake(t *testing.T) {
	queued := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	for _, role := range newsRoles {
		t.Run(role.name, func(t *testing.T) {
			c := role.claim
			c.GivenEntry, c.ServedEntry = 2, 1
			c.Handshake = EntryHandshake{Entry: 2, Generation: 2, QueuedAt: queued}
			h := newHarnessOf(t, c)
			h.reach(StateReady)
			news := newsAloneTask(h.token, 3, 2, role.phase)

			h.must(news)
			if got := h.claim(); got.GivenEntry != c.GivenEntry || got.Handshake != c.Handshake {
				t.Fatalf("after the news task was queued the claim records entry %d given with handshake %+v, want entry %d and %+v",
					got.GivenEntry, got.Handshake, c.GivenEntry, c.Handshake)
			}
			takeTurn(h, news.ID)

			for name, got := range map[string]Claim{"in memory": h.claim(), "in the store": h.store.load(h.token)} {
				if got.GivenEntry != c.GivenEntry || got.ServedEntry != c.ServedEntry || got.Handshake != c.Handshake {
					t.Fatalf("%s, after the news task's turn the claim records entry %d given and %d served with handshake %+v, want %d, %d and %+v",
						name, got.GivenEntry, got.ServedEntry, got.Handshake, c.GivenEntry, c.ServedEntry, c.Handshake)
				}
			}
		})
	}
}

// The admission sweep sends a root architect the same entry and run again once the claim's task
// of it was retired unconfirmed, which puts the given entry back (retirePending): here a turn
// confirms the entry's task, the turn ends in a provider error, which takes the task back
// unconfirmed under the ladder's wait, and a wake's task of no entry takes that owed task's place
// (replaceable). The sweep's start of the same entry is a row of its own, so its task is a new
// delivery. That task carries news of the approved design, and the record moved since the first
// start read it, so its send renders the news again under a new id (currentNews, rotateTaskID):
// one rendering per id. Its acknowledgement records the news that resent prompt carried as the
// copy its confirmation raises (Delivery.Carried, acked), never the first task's, and its turn's
// confirmation carries that copy. No delivery id is prompted twice, so the shim forwards each and
// answers none from its own record (shimwire's Dedupe): no prompt is taken twice.
func TestAResentEntryTaskRecordsTheNewsItsOwnPromptCarried(t *testing.T) {
	const (
		firstNews  = `{"task":"the work of generation 2, entry 2","items":[{"issue":"LEGION-208","audience":"architect","step":"spec","artifact":"artifact-spec","from":4,"to":5}]}`
		resentNews = `{"task":"the work of generation 2, entry 2","items":[{"issue":"LEGION-208","audience":"architect","step":"spec","artifact":"artifact-spec","from":4,"to":6}]}`
		resentTask = "the work of generation 2, entry 2\n\nThe approved spec of LEGION-208 changed from version 4 to version 6."
	)
	answer := &designNewsAnswer{}
	h := newBareHarness(t)
	h.deps.DesignNews = answer.answer
	c := rootClaim()
	h.token = c.Token
	if err := h.store.PutClaim(h.ctx, c); err != nil {
		t.Fatal(err)
	}
	h.start(c)
	h.reach(StateReady)

	first := startTask(rootToken, 15, 2, 2)
	first.News = []byte(firstNews)
	answer.task, answer.news = first.Task, first.News
	h.must(first)
	if stored := h.store.load(rootToken).Pending; stored == nil || !bytes.Equal(stored.Carried, first.News) {
		t.Fatalf("the first task after its acknowledgement = %+v, want it holding the copy of the news its prompt carried", stored)
	}
	h.must(StreamTurnStart{Claim: rootToken})
	h.errorTurn(StreamPromptResult{Error: "overloaded_error", HTTPStatus: 529})
	if p := h.pending(); !p.ConfirmedAt.IsZero() || len(p.Carried) != 0 {
		t.Fatalf("the task after the provider error = %+v, want its confirmation and its copy taken back", p)
	}
	wake := wakeTask(20, 2)
	h.must(wake)
	if got := h.claim(); got.GivenEntry != 0 || h.pending().ID != wake.ID {
		t.Fatalf("after the wake's task the claim holds %+v given entry %d, want the wake's task and entry 2 owed again", h.claim().Pending, got.GivenEntry)
	}
	takeTurn(h, wake.ID)

	resend := startTask(rootToken, 31, 2, 2)
	resend.News = first.News
	answer.task, answer.news = resentTask, []byte(resentNews)
	h.must(resend)

	prompts := h.prompts()
	last := prompts[len(prompts)-1]
	if last.DeliveryID == resend.ID || last.Message != resentTask {
		t.Fatalf("the resent task was prompted %s: %q, want the news rendered again under a new id, %q", last.DeliveryID, last.Message, resentTask)
	}
	stored := h.store.load(rootToken).Pending
	if stored == nil || stored.ID != last.DeliveryID || !bytes.Equal(stored.Carried, []byte(resentNews)) {
		t.Fatalf("the resent task after its acknowledgement = %+v, want it under %s holding the copy of the news its own prompt carried, %s",
			stored, last.DeliveryID, resentNews)
	}
	seen := map[string]bool{}
	for _, prompt := range prompts {
		if seen[prompt.DeliveryID] {
			t.Fatalf("delivery %s was prompted twice in %+v, want every prompt under an id of its own", prompt.DeliveryID, prompts)
		}
		seen[prompt.DeliveryID] = true
	}
	h.must(StreamTurnStart{Claim: rootToken})
	if confirmed, ok := h.store.delivery(rootToken); !ok || confirmed.ConfirmedAt.IsZero() || !bytes.Equal(confirmed.Carried, []byte(resentNews)) {
		t.Fatalf("the resent task after its turn started = %+v (held %t), want it confirmed carrying %s", confirmed, ok, resentNews)
	}
}
