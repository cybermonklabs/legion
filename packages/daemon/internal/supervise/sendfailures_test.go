package supervise

// A take whose adoption of the working copy fails leaves two marks: the claim's prompt budget,
// which only the far side's own answer about this workspace spends, and the pending task's record
// of every failed adoption, which is kept whether the failure was charged or not. These tests pin
// both (INFRA-1480).

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/store/storetext"
)

// staleLockRefusal is what the shim answers an adoption with when jj is wedged on a git lock
// another role's container left: the shim's own words, which name the lock and its age.
const staleLockRefusal = "Command failed (exit 1): jj metaedit: the git lock " +
	"/legion/workspaces/acme/widgets/legion-480/.git/index.lock was last written 31m ago"

// staleLockRefusalWithNUL is that same answer with a NUL byte in it, which is what reaches the
// daemon when the wedged command printed one: the Go shim relays the command's own bytes whatever
// they are. PostgreSQL refuses a NUL in text ("invalid byte sequence for encoding \"UTF8\":
// 0x00"), so the record of failed working-copy adoptions holds U+FFFD in its place, as every text
// the daemon stores does (storetext.FarSideError).
const staleLockRefusalWithNUL = "Command failed (exit 1): jj metaedit: the git lock " +
	"/legion/workspaces/acme/widgets/legion-480/.git/index.lock\x00 was last written 31m ago"

// staleLockRefusalWithRawByte is that answer carrying a byte that begins no UTF-8 sequence, as a
// command whose output is not text at all gives it.
const staleLockRefusalWithRawByte = "Command failed (exit 1): jj metaedit: the git lock " +
	"/legion/workspaces/acme/widgets/legion-480/.git/index.lock\xff was last written 31m ago"

// adoptingHarness is a machine whose role has an App identity to adopt its working copy with, at
// ready with nothing pending: without an Identity the daemon adopts nothing at all (Machine.adopt)
// and no adoption could fail. Every task given through it names a phase, because a task that is
// no phase task adopts nothing either (Machine.adopts): the adoption is made for the commits a
// phase writes, and a message or a reminder would only take the workspace from whichever role is
// working in it (dispatch://INFRA-714).
func adoptingHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	h.deps.Identity = adoptingIdentity
	h.restart()
	h.reach(StateReady)
	return h
}

// The whole split a failed adoption is charged on, in one table: the prompt budget pays for the
// answer the far side gave about this very workspace — an adoption the shim refused — and for
// nothing else. A call the transport lost, and an App identity the daemon could not mint before
// any call left it, say nothing about the workspace or the agent, cost nothing, and the value is
// written again for as long as they keep failing. Charging them would retire a healthy worker for
// its cluster's weather; charging none leaves a claim refused for ever, which is what held two
// planning tasks 31 and 33 hours (INFRA-1480). Each take is refused with the adoption's hold.
//
// Each row names its own failure in the log, the uncharged ones included: an operator reading a
// claim that waits is told whether the adoption was lost on the way or the GitHub App would mint
// no identity for it, which are looked for in different places.
func TestTakeWhoseAdoptionFailsIsRefusedAndOnlyTheShimsRefusalIsCharged(t *testing.T) {
	for _, tc := range []struct {
		name string
		// fail arranges the one failure the take's adoption meets.
		fail func(h *harness)
		// logged is the message of the log line the failure writes, which carries the failure's
		// own text beside it; the charge writes a line of its own, so the message is matched
		// whole rather than by a fragment both lines hold.
		logged  string
		text    string
		budgets Budgets
	}{
		{name: "the shim refuses the adoption",
			fail:    func(h *harness) { h.rt.FailAdoptWorkingCopy(adoptRefusal(staleLockRefusal)) },
			logged:  `msg="` + adoptRefusedLine + `"`,
			text:    staleLockRefusal,
			budgets: Budgets{PromptFailures: 1}},
		{name: "the transport loses the adoption",
			fail:   func(h *harness) { h.rt.FailAdoptWorkingCopy(errBoom) },
			logged: `msg="supervise: the adoption of the working copy was lost to the transport; the take is refused and the task waits"`, text: errBoom.Error()},
		{name: "the role's App identity cannot be minted",
			fail: func(h *harness) {
				h.deps.Identity = func(context.Context, claim.Role, string) (runtime.GitIdentity, error) {
					return runtime.GitIdentity{}, errBoom
				}
				h.restart()
				h.reach(StateReady)
			},
			logged: `msg="supervise: the role's GitHub App identity could not be minted; the take is refused and the task waits"`,
			text:   errBoom.Error()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := adoptingHarness(t)
			tc.fail(h)

			h.must(RequestDeliver{Claim: testToken, Task: "the task", Phase: phase.Implementing})

			h.wantBudgets(tc.budgets)
			h.wantPrompts(0)
			if answers := h.worker.answers; len(answers) != 1 || answers[0].Outcome != TakeOutcomeRefused || answers[0].Hold != HoldAdoption {
				t.Fatalf("the take was answered %+v, want it refused by %q", answers, HoldAdoption)
			}
			if p := h.pending(); p.Task != "the task" || p.taken() {
				t.Fatalf("pending %+v, want the task waiting untaken for the next take", p)
			}
			if lines := h.logs.lines("level=WARN", tc.logged, tc.text); len(lines) != 1 {
				t.Fatalf("logged %v, want one warning naming %q and carrying %q", lines, tc.logged, tc.text)
			}
		})
	}

	// Refused adoptions spend that budget to its end rather than being refused for ever: with the
	// shipped limits (prompt_failure_limit 3, prompt_retire_limit 2) three charges retire the
	// process and relaunch the session, and the sixth consecutive charge fails the claim, which
	// `legion state` and the stranded report then name. A failed claim adopts nothing more.
	t.Run("consecutive refused adoptions end in a failed claim", func(t *testing.T) {
		h := adoptingHarness(t)
		h.rt.FailAdoptWorkingCopy(adoptRefusal(staleLockRefusal))

		h.must(RequestDeliver{Claim: testToken, Task: "the task", Phase: phase.Implementing}) // the first charge
		h.observe(runtime.Alive)                                                              // the second, at the value the sweep writes again
		h.wantBudgets(Budgets{PromptFailures: 2})
		h.observe(runtime.Alive) // the third: the retirement

		h.wantState(StateLaunching)
		h.wantBudgets(Budgets{PromptRetires: 1})
		h.wantCalls("Suspend", 1)
		h.relaunched()           // the fourth charge, on the relaunched session's ready
		h.observe(runtime.Alive) // the fifth
		h.observe(runtime.Alive) // the sixth: the retirement that has none left

		h.wantState(StateFailed)
		h.wantBudgets(Budgets{PromptFailures: 3, PromptRetires: 2})
		// One adoption per take and no seventh: the claim failed where it would otherwise have
		// been offered the task again, and no take was ever accepted.
		h.wantCalls("AdoptWorkingCopy", 6)
		h.wantPrompts(0)
		if lines := h.logs.lines("level=WARN",
			`msg="`+adoptRefusedLine+`"`,
			staleLockRefusal); len(lines) != 6 {
			t.Fatalf("logged %d adoption refusals, want one per take naming the shim's own words:\n%s",
				len(lines), strings.Join(lines, "\n"))
		}
		if p := h.pending(); p.Task != "the task" {
			t.Fatalf("pending %+v, want the undelivered task kept on the failed claim", p)
		}
	})

	// The charge is what bounds the takes refused, and the record of the failed adoption beside it
	// is only a diagnosis, so a record the store will not take decides nothing about the charge.
	// The write of that record used to return before the split that charges, and a refused
	// adoption whose text the store refused was then charged nothing and offered again with no
	// bound — the very stall the split exists to end (INFRA-1480). The store refuses the write
	// itself here, rather than the text being one a UTF8 server rejects, so the claim holds for
	// every reason a write can fail and not for one encoding's quirk.
	t.Run("a refused adoption is charged though its record is not written", func(t *testing.T) {
		h := adoptingHarness(t)
		h.rt.FailAdoptWorkingCopy(adoptRefusal(staleLockRefusalWithNUL))

		// The first take writes its record: this same call queues the task, and a store refusing
		// every delivery write would refuse the queue's own write and leave no task to take.
		h.must(RequestDeliver{Claim: testToken, Task: "the task", Phase: phase.Implementing})
		h.wantBudgets(Budgets{PromptFailures: 1})

		h.store.fail("PutDelivery", errBoom)
		h.observe(runtime.Alive) // the sweep's offer, whose take's record the store refuses

		h.wantBudgets(Budgets{PromptFailures: 2})
		if p := h.pending(); p.SendFailures != 2 {
			t.Fatalf("pending %+v, want the second failed adoption counted in memory", p)
		}
		if lines := h.logs.lines("level=WARN",
			`msg="supervise: the failed adoption was not recorded on the delivery; settle writes it again and the charge goes on"`,
			errBoom.Error()); len(lines) != 1 {
			t.Fatalf("logged %v, want the unwritten record named once", lines)
		}

		// The other half of the same fix, and the half a guard written the obvious way does not
		// reach: carrying the failed write past loses nothing, because settle writes the record
		// again at the next decision. Reading the stored row after another sweep would prove
		// none of it — that sweep's own take fails too, and its putPending writes the row — so
		// the decision after the store recovers must be one that touches no delivery. An
		// operator's suspension is that decision, and it retires nothing because an operator's
		// suspension keeps the pending task whatever its phase (SuspendSource.RetiresPhaseTask,
		// which settle's `suspended` branch reads), which is the one line this case depends on: a
		// suspension by a source that retires the task leaves no delivery to read.
		//
		// The assertion is that the stored record equals the record memory holds, not that it
		// holds some count, so it reads the same against whichever write settle makes
		// (putClaimAndPending today) and fails if settle stops making one at all.
		h.store.fail("PutDelivery", nil)
		h.must(RequestSuspend{Claim: testToken, By: SuspendedByOperator})

		stored := h.store.load(testToken).Pending
		if stored == nil {
			t.Fatal("the store holds no delivery at all after the suspension settled")
		}
		if mem := h.pending(); stored.SendFailures != mem.SendFailures || stored.LastSendError != mem.LastSendError {
			t.Fatalf("the store holds %+v, want the record memory holds, %+v: the carried-past write is lost", stored, mem)
		}
	})

	// The far side's text reaches the delivery as the daemon keeps any far side's error text
	// (storetext.FarSideError), in memory as in the store. It is the wedged command's own output,
	// relayed whole, and the column holds a diagnosis rather than a log: each NUL and each byte
	// that is not UTF-8 reads U+FFFD, and the text is cut to its start. The warning beside the
	// record carries that same text, and it is the one line of its message however long the text
	// is: a check that counts refused adoptions by that message counts one. What the cut left out
	// follows it in the log at Info, in lines one log entry holds, under a message of their own
	// that names the warning's (farSideErrorInFull, storetext.LogWhole), so nothing is lost and
	// only moved.
	t.Run("the text a failed adoption records is cleaned and bounded", func(t *testing.T) {
		// About a megabyte of relayed stderr, as a jj wedged on a lock writes it, ending in a
		// line the log must still carry and the record must not.
		hugeRefusal := "Command failed (exit 1): jj metaedit: " +
			strings.Repeat("the git lock is held by another container; ", 24000) +
			"and this is the last line of the megabyte"
		for _, tc := range []struct {
			name string
			// reason is what the shim's `ok: false` answer carries.
			reason string
			// want is the whole text the delivery must hold.
			want string
			// whole is the far side's whole text, which the log's parts after the warning carry
			// when the record holds only its start, and "" when the record holds all of it.
			whole string
		}{
			{name: "a NUL byte, which PostgreSQL refuses in text",
				reason: staleLockRefusalWithNUL,
				want:   adoptRefusal(strings.ReplaceAll(staleLockRefusalWithNUL, "\x00", "\uFFFD")).Error()},
			// Not the row above again, and not to be deleted as a duplicate of it: a NUL is valid
			// UTF-8 and a lone 0xff begins no UTF-8 sequence at all, so the two are refused by
			// different servers. An SQL_ASCII server takes the 0xff, a UTF8 server — production's
			// encoding — refuses it ("invalid byte sequence for encoding "UTF8": 0xff"), and every
			// encoding refuses the NUL. The record holds valid UTF-8 with no NUL, and this is the
			// row that reads the first half of that.
			{name: "a byte that begins no UTF-8 sequence",
				reason: staleLockRefusalWithRawByte,
				want:   adoptRefusal(strings.ReplaceAll(staleLockRefusalWithRawByte, "\xff", "\uFFFD")).Error()},
			{name: "about a megabyte of the wedged command's stderr",
				reason: hugeRefusal,
				want:   string([]rune(adoptRefusal(hugeRefusal).Error())[:storetext.FarSideErrorRunes-1]) + "…",
				whole:  adoptRefusal(hugeRefusal).Error()},
		} {
			t.Run(tc.name, func(t *testing.T) {
				h := adoptingHarness(t)
				h.rt.FailAdoptWorkingCopy(adoptRefusal(tc.reason))

				h.must(RequestDeliver{Claim: testToken, Task: "the task", Phase: phase.Implementing})

				got := h.pending().LastSendError
				if got != tc.want {
					t.Fatalf("recorded %.300q, want %.300q", got, tc.want)
				}
				if !utf8.ValidString(got) || strings.ContainsRune(got, 0) || utf8.RuneCountInString(got) > storetext.FarSideErrorRunes {
					t.Fatalf("recorded %.300q of %d runes, want valid UTF-8, no NUL, and at most %d runes",
						got, utf8.RuneCountInString(got), storetext.FarSideErrorRunes)
				}
				if stored, ok := h.store.delivery(testToken); !ok || stored.LastSendError != got {
					t.Fatalf("stored delivery's text = %.300q (held %t), want the recorded text persisted with it",
						stored.LastSendError, ok)
				}
				records := h.records.of(adoptRefusedLine)
				if len(records) != 1 || records[0].level != slog.LevelWarn {
					t.Fatalf("logged %d records of %q, want the refusal's one warning, however long its text", len(records), adoptRefusedLine)
				}
				warned, _ := records[0].attr("error")
				task, _ := records[0].attr("task")
				if warned.String() != got {
					t.Fatalf("the warning carries %.300q, want the text the delivery records", warned.String())
				}
				var whole strings.Builder
				for i, part := range h.records.of(farSideErrorInFull) {
					number, _ := part.attr("part")
					of, _ := part.attr("of")
					partTask, _ := part.attr("task")
					text, _ := part.attr("error")
					if part.level != slog.LevelInfo || number.Kind() != slog.KindInt64 || number.Int64() != int64(i+1) ||
						of.String() != adoptRefusedLine || partTask.String() != task.String() {
						t.Fatalf("part record %d is %s part %v of %q for task %v, want part %d at INFO of %q for task %v",
							i+1, part.level, number, of, partTask, i+1, adoptRefusedLine, task)
					}
					whole.WriteString(text.String())
				}
				if whole.String() != tc.whole {
					t.Fatalf("the log's parts after the warning join to %d bytes ending %q, want the far side's whole text of %d bytes ending %q",
						whole.Len(), whole.String()[max(0, whole.Len()-48):], len(tc.whole), tc.whole[max(0, len(tc.whole)-48):])
				}
			})
		}
	})
}

// Every adoption that fails is recorded on the pending task itself — how many have failed, when
// the last of them failed by the machine's clock, and the error it failed with — and the record is
// kept for a failure the budget charged and for one it did not. The uncharged failures are the
// ones it exists for: a transport loss costs nothing and is offered again for ever, so without
// this the only trace of a claim waiting days on a take that never lands is a log line inside its
// pod. An accepted take clears all three, so what an operator reads on a pending task is the run
// of failures standing in the way of the take it is still waiting for.
func TestTakeRecordsItsAdoptionFailuresAndAnAcceptedTakeClearsThem(t *testing.T) {
	for _, tc := range []struct {
		name string
		// fail arranges the failure, and succeed takes it away so the next take is accepted.
		fail    func(h *harness)
		succeed func(h *harness)
		text    string
		charged Budgets
	}{
		{name: "an adoption the shim refuses, which is charged",
			fail:    func(h *harness) { h.rt.FailAdoptWorkingCopy(adoptRefusal(staleLockRefusal)) },
			succeed: func(h *harness) { h.rt.FailAdoptWorkingCopy(nil) },
			text:    staleLockRefusal,
			charged: Budgets{PromptFailures: 2}},
		{name: "an adoption the transport loses, which is not",
			fail:    func(h *harness) { h.rt.FailAdoptWorkingCopy(errBoom) },
			succeed: func(h *harness) { h.rt.FailAdoptWorkingCopy(nil) },
			text:    errBoom.Error()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := adoptingHarness(t)
			tc.fail(h)

			h.must(RequestDeliver{Claim: testToken, Task: "the task", Phase: phase.Implementing})

			first := h.clock.Now()
			p := h.pending()
			if p.SendFailures != 1 || !p.LastSendFailureAt.Equal(first) || !strings.Contains(p.LastSendError, tc.text) {
				t.Fatalf("pending after one failed adoption = %+v, want one failure at %v carrying %q", p, first, tc.text)
			}
			if p.taken() {
				t.Fatalf("pending %+v, want the task left untaken by the failed adoption", p)
			}
			// The record rides out on the delivery's own write, so a restart reads it rather than
			// starting the count again.
			if stored, ok := h.store.delivery(testToken); !ok || stored.SendFailures != 1 || !stored.LastSendFailureAt.Equal(first) {
				t.Fatalf("stored delivery = %+v (held %t), want the failure persisted with it", stored, ok)
			}

			h.advance(time.Second)
			second := h.clock.Now()
			h.observe(runtime.Alive)

			if p := h.pending(); p.SendFailures != 2 || !p.LastSendFailureAt.Equal(second) {
				t.Fatalf("pending after two failed adoptions = %+v, want two failures, the last at %v", p, second)
			}
			h.wantBudgets(tc.charged)

			tc.succeed(h)
			h.observe(runtime.Alive)

			p = h.pending()
			if !p.awaitsTurn() {
				t.Fatalf("pending %+v, want the take the sweep offered accepted", p)
			}
			if p.SendFailures != 0 || !p.LastSendFailureAt.IsZero() || p.LastSendError != "" {
				t.Fatalf("pending after the accepted take = %+v, want its failure record cleared", p)
			}
			if stored, ok := h.store.delivery(testToken); !ok || stored.SendFailures != 0 ||
				!stored.LastSendFailureAt.IsZero() || stored.LastSendError != "" {
				t.Fatalf("stored delivery = %+v (held %t), want the cleared record persisted", stored, ok)
			}
		})
	}
}
