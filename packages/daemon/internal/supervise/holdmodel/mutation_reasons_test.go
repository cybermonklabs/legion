package holdmodel

// survivorReasons is the written reason each mutant that survives the
// mutation run may survive: what reads the write, and why no behaviour the
// oracles judge differs. The key is the mutated function and statement
// (stateMutation.Stmt); " | <kind> <target>" after it narrows a reason to one
// mutant of that statement (reasonKey). TestStateWriteMutations fails on a
// survivor with no reason here, and, run unfiltered, on a reason no survivor
// uses.
var survivorReasons = map[string]string{
	// Stages and timers of attempts that are not open.
	"invalidate: r.Att[k].Stage = AtInvalid":                          "AtInvalid and AtNone are both not open to every transition. The one transition that reads AtNone (row's D12 Unmatched branch) needs Task.Unknown, and a row with attempts never has it: a legacy row records none, and register needs !Unknown. Only the recovery fence reads AtInvalid, so only its plant goes blind.",
	"outcome: r.Att[k].Stage = AtClosed":                              "AtClosed and AtNone are both not open to every transition. The one transition that reads AtNone (row's D12 Unmatched branch) needs Task.Unknown, and a row with attempts never has it.",
	"answer: r.Att[j].Stage = AtClosed":                               "AtClosed and AtNone are both not open to every transition. The one transition that reads AtNone (row's D12 Unmatched branch) needs Task.Unknown, and a row with attempts never has it.",
	"answer: r.Att[k].Stage = AtClosed":                               "AtClosed and AtNone are both not open to every transition. The one transition that reads AtNone (row's D12 Unmatched branch) needs Task.Unknown, and a row with attempts never has it.",
	"invalidate: r.Att[k].TurnBy = 0":                                 "An attempt's TurnBy is read only while the attempt is open (refreshTurnBy, the turn timer, attemptBoundBad). invalidate leaves no attempt open, and no slot opens again: register writes a new Attempt into a slot never used.",
	"answer: r.Att[j].TurnBy = 0":                                     "A closed attempt's TurnBy is never read (every reader reads open attempts only), and refreshTurnBy right after recomputes the row's bound.",
	"answer: r.Att[k].TurnBy = 0":                                     "A closed attempt's TurnBy is never read (every reader reads open attempts only), and refreshTurnBy right after recomputes the row's bound.",
	"answer: r.RecoverBy, r.TurnBy = 0, 0 | member left out r.TurnBy": "refreshTurnBy on the line before already set TurnBy to the earliest bound of an open attempt, which is 0 when none is outstanding.",
	"restart: r.TurnBy = 0":                                           "forgetMemory has already zeroed TurnBy in the stored row this memory row is read from.",

	// Fields no transition reads.
	"suspend: h.Request = by":             "Hold.Request is never read: every reader of a hold reads By, its holder.",
	"dispatch: w.Conn++":                  "Conn is only copied into WireRequest.Conn, which nothing reads.",
	"prepare: d.Resend = false":           "No transition reads Resend. Only the boot-memory oracle does, at a boot, after restart has cleared it.",
	"row: d.Resend = true":                "No transition reads Resend. Only the boot-memory oracle does, at a boot, after restart has cleared it.",
	"answer: w = RetirementWrite":         "Every reader of a write's kind treats RetirementWrite and EvidenceWrite alike (restart's Unapplied, observeCommit's Negative). The others look for kinds a served answer never writes: register, intent and stop, and closeFatal's failed row.",
	"write: cause.Fault, cause.At = 0, 0": "No reader of the pending cause reads its fault: they read its kind, answer and request. A cause that keeps its fault only makes the state value differ.",

	// Work: WkNone and WkTurn read alike.
	"turn: r.Work = WkTurn | zero r.Work":                               "WkNone and WkTurn are the same to every reader of Work: compact(), the busy probe's guard and compaction's end read only WkBusy, WkCompact and WkCompactTurn.",
	"row: r.Work = WkTurn | zero r.Work":                                "WkNone and WkTurn are the same to every reader of Work: compact(), the busy probe's guard and compaction's end read only WkBusy, WkCompact and WkCompactTurn.",
	"row: r.Phase = PhIdle | zero r.Phase":                              "PhIdle is the zero Phase: the mutant writes the same value.",
	"row: r.Phase, r.Work = PhIdle, WkNone | member left out r.Work":    "On resume the suspended row's Work is already WkNone: stop wrote it, and nothing writes Work on a row that is not live. (Compaction's end, the other write of this statement, is caught by unanswered-wrong-id-compaction.)",
	"stop: r.Phase, r.Work = PhSusp, WkNone | member left out r.Work":   "A suspended row's Work is read only by compaction end's guard, whose step row ignores on a row that is not live, and resume writes WkNone.",
	"fail: r.Phase, r.Work = PhFailed, WkNone | member left out r.Work": "A failed row is never live again. Its Work is read only by compaction end's guard, whose step row ignores on a row that is not live.",
	"leave: r.Phase, r.Work = PhEnded, WkNone | member left out r.Work": "An ended row is never live again. Its Work is read only by compaction end's guard, whose step row ignores on a row that is not live.",

	// Rows that are terminal or suspended.
	"row: d.W.Proc = false":                 "A failed row is terminal and ignores every input a live process enables (frames, reconnect; death needs a live row). Kept, the process is only stopped again at the next fail effects, and a stop of a gone process does nothing.",
	"leave: d.W.Proc = false":               "An ended row is terminal and ignores every input a live process enables (frames, reconnect; death needs a live row).",
	"row: r.SuspendedBy = SrcNone":          "SuspendedBy is read only while the row is suspended (suspend's takeover rule, and RPhaseEnd's check), and the next stop writes it before the row is suspended again.",
	"stop: d.W.Attached = false":            "A suspended row ignores every frame, delivered at once or replayed later, and resume attaches the new process.",
	"stop: d.W.Agent, d.W.Dedupe = 0, 0":    "A suspended row ignores agent frames and sends nothing, and resume, a relaunch or a dead restart resets both before the claim can send or credit a turn again.",
	"install: d.Prepared, d.Sending = 0, 0": "A row leaves live only by a stop after intend (which cleared both), by fail or by claim end. A failed or ended row is terminal, and no reader of Prepared or Sending acts on it.",
	"install: d.Restored = false":           "A row that is not live has no recovery bound (each way out of live invalidates it), so the clear above it for RecoverBy == 0 has already run.",
	"stop: d.M = r":                         "While the stop's write is pending, every decision retries it before it reads the memory row, and defers its input if the write fails again. So the inputs the stale (or empty) live memory enables wait in the deferred slot, and reach the suspended row after install, which ignores them.",
	"row: d.StopOwed, d.StopRetry = false, false | member left out d.StopRetry": "install clears StopRetry once the committed failure has no intent (fail clears it). Until then the failure's write is pending, the fatal item decides first, and a kept StopRetry only disarms the hold's timer, whose step would only retry that write; fail ends the hold.",

	// Writes another step makes again before anything reads them.
	"queue: d.Prepared = 0": "A prepared slot holds nothing until register writes it, and register reads the task then. So registering the old slot for the new task is the same as preparing a new one; only the slot number and one of NA allocations differ.",
	"intend: d.Prepared, d.Sending = 0, 0 | member left out d.Prepared":             "A slot still prepared at a stop holds nothing (register's install moves it to Sending), so registering it later is the same as preparing a new one, up to its number. (Sending is caught by released-stop-sends-again.)",
	"relaunch: d.Prepared, d.Sending = 0, 0 | member left out d.Prepared":           "A slot still prepared at a let-go holds nothing (register's install moves it to Sending), so registering it after ready is the same as preparing a new one, up to its number. (Sending is caught by relaunch-sends-again.)",
	"outcome: d.W.Requests[k].Ack = true":                                           "Ack is read only by the outcome guard, and the outcome that sets it also sets Returned, which disables every later outcome of that request.",
	"pop: w.Backlog[w.N] = Event{}":                                                 "The backlog is read only at its head while N is not 0, and a push overwrites slot N, so an entry at or after N is never read.",
	"dispatch: w.Dedupe = 0":                                                        "The shim dedupes only onto a root sent under the current task id, and the refusal of such a root always moves the task to a new id (answer takes the task or bumps its id), so the cleared Dedupe could not match again; resume, a relaunch and a dead restart reset it too.",
	"dispatch: q.Answered, q.Answer = true, ARefLate | member left out q.Answer":    "A refused send has no alias: the next send needs this outcome first (Sending), and the refusal clears Dedupe. A later refusal frame of the root finds its attempt closed, and observeCommit and the twice oracle read every answer but AOK alike.",
	"dispatch: q.Answer = e.A":                                                      "No transition reads a result's recorded answer: only the frame rule that lets a closed attempt's result be replayed (the replay then changes nothing, since answer needs an open attempt) and the oracles' reading of a negative answer, which the mutant can only make more lenient. A refusal writes ARefLate on the next line.",
	"row: r.Task.L, r.Task.ID = d.H.FatalL, d.H.FatalID | member left out r.Task.L": "Under FatalOpen the task's L is already the receipt's: a pending write that retires or replaces the task clears FatalOpen (judgeReceipt), and every decision between receipt and close applies the fatal item first.",
	"row: d.Restored = false":                                                       "install clears Restored once the committed row has no recovery bound, which this write gives, and every reader of Restored in between first retries the pending write.",
	"stop: d.StopRetry = false":                                                     "stop reaches this line only from finish (which returns while StopRetry is set), after a stop-retry timer cleared it, or for an owed stop, and StopOwed and StopRetry are never both set.",
	"install: d.StopRetry = false":                                                  "stop cleared StopRetry before its write, and nothing sets it between the write and its install.",
	"closeFatal: d.H.FatalPending = false":                                          "install clears FatalPending when it installs the failed row, and a restart over a failed stored row clears it; a receipt between them is settled by the same failure either way.",
	"restart: d.W.Fatal = Event{}":                                                  "The next decision applies the kept item to the failed row: fail rewrites the same failed row, and install then clears the item.",

	// Faults the explorer reaches another way.
	"dispatch: w.LoseReply = true":               "The explorer also loses the reply of every commit (the LostReply cut), so the event's deferred loss gives no state the cut does not; the mutant makes the event a stutter.",
	"flush: d.W.LoseReply = false":               "A flag left set loses the reply of each later commit. The explorer already loses any commit's reply, one a step, as here: a lost reply ends the step's writes.",
	"row: d.crash = d.fault(CrashAfterRuntime)":  "Nothing in the step follows the refused runtime call, so the crash cut ends where the step without it, followed by a clean restart, ends; the explorer takes that too.",
	"stop: d.crash = d.fault(CrashAfterRuntime)": "Nothing in the step follows the refused runtime call (finish returns on StopRetry), so the crash cut ends where the step without it, followed by a clean restart, ends; the explorer takes that too.",
	"row: d.crash = true":                        "Without the crash the step ends with the runtime stopped and the notification still owed (Notify set), which the next fail effects sends: the crash's own outcome without its restart.",

	// The environment: the shim's and the agent's records.
	"row: d.W.Inc++":      "Inc gates only the agent's frames (a turn start names a request written to this process) and the shim's dedupe. Resume also resets Dedupe, and every attempt of the earlier process is invalidated, so its turn start credits nothing: the daemon reads it as a foreign turn start.",
	"relaunch: d.W.Inc++": "Inc gates only the agent's frames (a turn start names a request written to this process) and the shim's dedupe. A relaunch also resets Dedupe and invalidates every attempt of the earlier process, so its turn start credits nothing: the daemon reads it as a foreign turn start.",
	"row: d.W.Dedupe, d.W.Agent = 0, 0 | member left out d.W.Dedupe":               "Inc++ in the same step makes every old root unmatched for dedupe.",
	"relaunch: d.W.Agent, d.W.Dedupe = 0, 0 | member left out d.W.Dedupe":          "Inc++ in the same step makes every old root unmatched for dedupe.",
	"restart: d.W.Agent, d.W.Dedupe, d.W.N = 0, 0, 0 | member left out d.W.Dedupe": "A kept Dedupe matches only a root under the current task id, and RT3 took the task back wherever an attempt under it was open; a closed one was answered, which moved the id.",
	"restart: d.W.Agent, d.W.Dedupe, d.W.N = 0, 0, 0 | member left out d.W.N":      "With the backlog emptied on the next line, a kept N replays empty events, which decide nothing.",
	"restart: d.W.Backlog = [3]Event{}":                                            "With N set to 0 on the line before, the kept entries are never read: the backlog is read only at its head while N is not 0.",
	"restart: d.W.Requests[k].Returned = true":                                     "A callback of the dead daemon's send then still reaches the shim. outcome() ignores it (restart cleared Sending), and the record it changes (answered as refused, Dedupe cleared) is what the request's own refusal frame writes.",
	"dispatch: q.Answered, q.Answer = true, e.A":                                   "The shim's record of a result the crashed daemon took: the mutant lets the shim give that result again, or forget its kind. The restored daemon decides it as the restored prompt's answer (D10 allows it), and once the recovery bound has invalidated the attempt it decides nothing.",

	// Unreachable without the boot-memory plant's load door.
	"restart: d.StopOwed = true":           "Unreachable without the boot-memory plant's load door: restart's memory row is the stored row with its hold forgotten (forgetMemory), so !Proc && r.Hold.By != 0 never holds.",
	"restart: r.Hold.Answer = r.RecoverBy": "Unreachable without the boot-memory plant's load door: forgetMemory removed the hold, so r.Hold.By is 0 here.",

	// Oracle state (the driver's ledgers), not transition state.
	"stop: d.H.Need[i.Pause] |= launchCauses(r)":            "The driver's launch snapshot, which I3 checks later launches against; dropping it can only blind I3, as its two plants show.",
	"observeCommit: d.H.Turned[r.Task.L] |= 1 << p.Cause.R": "The driver's turn-credit ledger, which I1 reads; dropping it can only blind I1, as its two plants show.",
	"observeCommit: d.H.Recovering &^= 1 << l":              "The driver's reading of D10's doubt; keeping the bit after the task left or the claim stopped can only make the twice and replacement oracles stricter, and neither fires.",
	"restart: d.H.Recovering |= 1 << l":                     "The driver's reading of D10's doubt; dropping it can only blind the twice oracle, as the D10 restore plant shows.",
}
