package supervise

import (
	"bufio"
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"time"
)

// The cost reading (R7 and R9 of dispatch://INFRA-2811): what a window of the daemon's own log
// says a claim's tasks cost, read from the lines this package writes and nothing else. `legion
// task-cost` is its one door (cmd/legion), which reads the daemon container's log from its
// standard input, as the documentation site's page says to pipe it
// (docs/site/src/content/docs/legion/task-delivery-cost.md), and prints TaskCost.Text.
//
// The lines are the daemon's JSON lines (cmd/legion's start writes its log through slog's JSON
// handler): one object per line, with the record's time, its message and its attributes, the
// machine's own claim among them. A line that is no such object — a refusal the command printed
// before its logger existed, a line kubectl added — is passed over, and so is every line of another
// claim than the one asked for, and every line at or after the window's end.
//
// The page's command pipes the current container's whole log, from its start, and not the window
// alone: a number written and seen before the window that no take reaches inside it writes no line
// in the window that names it — the charge its first bound's passing writes (promptFailureLine)
// names no number — so a reading cut at the window's start would never see the stranded number the
// never-taken figure exists to catch. So the lines before From are read for one thing only, where
// each claim and each of its numbers stands at From — its clock, its turn, its latest write and
// seen report, its accepted take — and From and To bound everything counted: every figure counts
// what happened in the window alone. Within one container that state is whole, since a restored
// seen report is written again at its first expiry (decision 20), and a value nobody saw is written
// again once per first bound.
//
// What is counted, and from which line:
//
//   - the turns, from the two turn lines (turnStartedLine, turnEndedLine), by their cause: a task
//     number, or none. The turn-outcome report writes the end message too, with a sequence of its
//     own (turnoutcome.go); that line is the report, not the turn's end, and is passed over. A run
//     the daemon gave a take, whose start line named the take's number and whose report then named
//     no task (notTheTakesTurnLine: decision 8's put-back), was no turn of that number: its start
//     and its end, which names what its start named, count as a turn of no cause, whether the end
//     came before the put-back or after it;
//   - the turns each number took, from the start lines that name it, less the runs put back;
//   - the numbers written, by the origin of the task each names, from the value writes
//     (valueWrittenLine); and of those, the numbers written to a claim that had finished the last
//     task written to it in the session it ran (finished), which is a task written to a claim
//     whose phase waited (R7 and R9's waited figure). The silence repair's writes (OriginSilence)
//     are an origin of their own, read apart from a resume's, and so are the writes of a task of
//     news of the approved design alone (OriginNews), read apart from the phase's work;
//   - the numbers taken, from the accepted answers to a take (takeAnsweredLine), less an answer
//     that repeats one from the record (repeat), which applied nothing;
//   - the numbers no take reached within the first bound of R5's first wait (TaskCostArgs.Take),
//     each time listed with its claim: R5's stranded task, which check c9 reads as none, and which
//     fails the third verdict below as well (the rule is the paragraphs below this list);
//   - the numbers finished, from taskFinishedLine, by what finished each;
//   - the charges, from the two charge lines (promptFailureLine, promptRetiredLine), by wait, and
//     the retirements among them.
//
// A number is owed from a value write of it (valueWrittenLine), unless an accepted take of it
// stands, until a take of it is accepted or dropped (takeAnsweredLine), it finishes
// (taskFinishedLine), its value is deleted (valueDeletedLine), which a drop makes, or a newer
// number is written to its claim, which replaces it. A take-back of it (takenBackLine) owes it
// again, at the revision last written, as a time owed of its own: the process that took it may be
// gone, and the process the claim runs next asks at that revision with no new write. While it is
// owed, its clock counts only time in which R5's first wait runs, so a worker the daemon would not
// charge is not counted:
//
//   - the clock starts at the line that reports the number's latest written revision seen
//     (taskSeenLine): that revision of that epoch, or that revision alone for a line that names no
//     epoch, as an older daemon's does. A newer revision written stops it until that one is
//     reported seen, and so does a seen report of any other revision — another epoch's, which a
//     bucket created again can leave a late report of — since the latest write is unseen again;
//   - a turn's start stops it, and that turn's end starts it again, the bound whole, a turn that
//     began before the window included. A take-back ends the turn its claim stood in, since it is
//     that turn's end or the let-go of its process, which writes no turn end;
//   - a take answered refused, a take the daemon could not decide or record (takeUndecidedLine),
//     and a take-back each stop it until the next value write of the number, whose seen report
//     starts it again, or until the claim's next registration;
//   - a death of the claim's process (processDiedLine) ends its turn and stops the clock until
//     the claim's next registration (agentRegisteredLine);
//   - a registration is a new process, which has seen nothing and asked nothing: the machine
//     forgets the seen report and the take of the process before it (register, decision 6), so
//     the reading ends every stop above and forgets the seen report too, and the clock starts
//     again at the new process's own seen report;
//   - the machine's view of the claim's turns (connview.go) stops it and starts it again, as it
//     stops and arms the first wait: the line where the view stops being whole (viewCutLine) — a
//     close, a daemon's start that restored the claim with a process, a reconnect hello over a
//     view it found whole — stops it, and the line where it is whole again (viewWholeLine) — the
//     new process's first hello, the hello of a shim whose agent has not yet said it is ready
//     (enrolledOnly), a reconnect hello over no pending task, the agent's answer to a hello's ask
//     or to a daemon start's own, a turn frame, a let-go — starts it again, the bound whole. No
//     act of the worker starts it while the view is cut;
//   - a daemon's start stops every clock as of the last line the reading decoded from the daemon
//     before it, since nothing says that daemon lived any longer, and no daemon ran the waits in
//     between. A daemon writes its starting line (DaemonStartingLine) first, and only `legion
//     start`'s configuration warnings, which the reading does not decode, come before it, so every
//     line the reading decodes before it is the daemon before's and every line after it the new
//     one's (daemonStarting); a log with no starting line, an older daemon's, is read with its
//     start line (DaemonStartedLine) as the start. A claim the start restored with a process has
//     its view cut there (viewCutLine), and its clock starts again at its view's whole line, before
//     the start line or after it; every other claim's clock stands still until its worker next
//     acts in the log, as the machine's waits wait for the worker's first act (Machine.awaitsAct):
//     a seen report, a take, a turn line, a report that put a take back, or a registration.
//
// A clock that runs a whole bound has passed it, and that time owed is listed, however it ends
// (a late take included). Only clock time inside the window counts: the bound is never judged
// before From, and a clock that runs at From runs from From, as if its seen report came there
// (enter), so a number stranded since before the window is listed once a bound passes inside it,
// and one taken soon after From is not, however long it waited before. The bound is judged at
// every line of the claim the reading decodes but a value write of the number itself: the first
// bound's own end writes the number again after a daemon's start, over a seen report that daemon
// read from the store (takeBoundOver), so a clock whose bound ends at a write of its number has
// not passed it. A time owed whose bound has not passed at To is not listed, since the window does
// not show whether a take came in time. The daemon writes no line that always carries the bound —
// only a charge of the first wait and the write of a value nobody reported seeing name it — so the
// command is told it (TaskCostArgs.Take).
//
// A seen report of a claim the lines hold no write for — its write fell before the log's first
// line, which the node no longer keeps — owes its number from that report, at the epoch and
// revision the report names, which is the write it saw: so a number written before the log and
// taken back in it is owed again once its next seen report is read.
//
// What this figure cannot see can count a number too many or too few. Too many: no line marks
// when a process the daemon started again after a release, a suspension or a relaunch is ready —
// the let-go makes the claim's view whole for the next process (viewWholeLine), which starts a
// clock the view held still — when a compaction holds a take off, or when a hold stands over a
// task its worker asked nothing of; and a turn that began before the log's first line and is
// still running shows no start. A clock that runs through any of them can list a number no worker
// could have taken. Too few: a let-go other than a death that cuts a turn off with no
// take-back writes no turn end, so a clock stopped by that turn starts again only at the end of
// the claim's next turn or at the seen report of the process the claim registers next; and a
// take-back of a number whose last write fell before the log's first line owes nothing the reading
// can time until a seen report of it is read, since it knows no revision of that number to match
// one against.
//
// And R9's three verdicts, each of which lists what fails it:
//
//   - no charge of a worker whose turn began within the second bound after its take: a charge made
//     while an accepted take of the claim stood, where a turn of the claim started no earlier than
//     the take's second wait began and no later than the bound after that (the charge's own bound,
//     for a charge of the second wait that names one) or, for every other charge, than the charge
//     itself. The second wait begins at the latest of the take's acceptance, its put-back, the
//     end of a turn that ran while the take awaited its turn (the end of the in-turn hold over
//     it), and the point the machine's view of the claim's turns is whole again after a close or
//     a daemon's start (viewWholeLine), as the machine's does (secondWaitNow, connview.go); so a
//     run the daemon gave the take before a put-back is no turn within it, and a take the worker
//     makes while the view is cut — before the agent's answer to the ask its shim's hello makes —
//     begins its wait at that answer. After a daemon's start, a claim whose view no line of that
//     start cut begins it again at its worker's first act too (stopEveryClock), as the machine's
//     waits wait for one (Machine.awaitsAct): the start restored such a claim with no process, so
//     the machine's waits wait for its next process's ready as well, which comes after that act. So
//     the reading's wait never begins after the machine's, and a turn it reads within the bound
//     began within the machine's. It can begin before it, where a hold the reading cannot see —
//     a process not yet ready, a suspension — kept the machine's wait disarmed past the reading's
//     start: its window then ends before the machine's bound did, a turn that began in the rest of
//     that bound goes unseen, and the verdict can miss that fail;
//   - no write of origin resume to a claim that had finished its last task in its session (a value
//     write of origin resume whose finished is true); a silence repair's write is not one;
//   - each number taken once for each time the record owed it: a number is owed once when it is
//     written, and once more at each take-back of it (takenBackLine); no number may be taken
//     more times in the window than it was owed, and none may be owed with no take within the
//     first bound (the figure above, which reads a time owed that no take reached as taken zero
//     times).
//
// Evidence counts verdict by verdict, since the reading never prints a pass it has no evidence
// for and never hides a fail it can prove. A window holds the take model's own evidence when it
// holds, of the claim asked for, a daemon line and a value write (valueWrittenLine) or an answer
// to a take (takeAnsweredLine), and such a window judges each verdict, a pass or a fail. Without
// that evidence, in a window with a daemon line, a fail the reading proves from where the claim
// stood at From is evidence for its own verdict alone, which reads that fail, and each verdict no
// such fail fails reads "no evidence" rather than a pass or a fail (TaskCost.EveryVerdictJudged);
// every other figure is printed as it is read, and `legion task-cost` exits non-zero for such a
// reading. Such a fail is one of two, each of an event inside the window: a time owed whose first
// bound passed in the window with no take (the figure above, which judges clock time inside the
// window alone), which fails the third verdict, and a charge made in the window of a worker whose
// turn began within the second bound after a take accepted before From (the first verdict, which
// counts a charge made inside the window alone). So a bound that passed, or a charge made, before
// From is no evidence. A window that holds no daemon line, of the claim asked for — a wrong
// selector, a window the node no longer keeps — judges no verdict, a carried fail included, since
// nothing in it shows the daemon lived through it. Nor does one whose daemon lines hold neither
// kind of evidence: the log of a daemon from before the task value, of one not yet rolled out, or
// of an older container that `kubectl logs --previous` reads, whose turns name no task and whose
// charges are the push path's, holds no line of the take model, and both carried fails need such
// lines before From.
//
// Every figure is a lower bound but two, which can also count too many: the numbers no take
// reached (above), and the turns each number took, since a run the daemon gave a take inside the
// window whose put-back line falls at or after To stays counted as that number's turn — no line
// from To on is read, so the put-back that would make it a turn of no cause is never seen.
// The log reaches back only as far as the node kept the current container's log, and a take, a
// write or a turn that fell before its first line is not seen.

// TaskCostArgs is what `legion task-cost` is asked: the window of the daemon's log to count, From
// included and To not (the lines before From are read for where each claim stands at From alone),
// the one claim to read it for, "" for every claim, and Take, R5's first bound
// (worker_take_timeout_seconds) as the deployment sets it, which the figure of the numbers no take
// reached is judged by.
type TaskCostArgs struct {
	From  time.Time
	To    time.Time
	Claim string
	Take  time.Duration
}

// TaskCost is what one window of the daemon's log says the claims' tasks cost (ReadTaskCost).
// A task is a number of one claim: every claim counts its numbers from 1, so a number is always
// read beside its claim.
type TaskCost struct {
	Args TaskCostArgs
	// Lines is how many of the daemon's lines fell in the window (and were of the claim asked
	// for), and FirstLine and LastLine the times of the first and the last of them.
	Lines               int
	FirstLine, LastLine time.Time
	// TurnsStarted and TurnsEnded are the turns by their cause.
	TurnsStarted, TurnsEnded TurnCauses
	// TurnsOfTask is how many turns each number took, by claim and then number.
	TurnsOfTask []TaskTurns
	// WrittenByOrigin is how many numbers were written, by origin; WaitedByOrigin how many of them
	// were written to a claim whose phase waited.
	WrittenByOrigin, WaitedByOrigin map[string]int
	// Taken and Finished are how many numbers were taken and finished, and FinishedBy the finished
	// by what finished each.
	Taken, Finished int
	FinishedBy      map[string]int
	// takesAnswered is how many answers to a take the window held, accepted, refused or dropped,
	// an answer that repeats one from the record included: with the value writes (WrittenByOrigin),
	// what holdsOwnEvidence reads.
	takesAnswered int
	// Unreached is each time a number was owed and its first-wait clock passed the first bound
	// (Args.Take) with no take, by claim, number and when the bound passed: when the clock last
	// started, when the bound passed, and how the time owed ended.
	Unreached []string
	// ChargesByWait is how many charges were made, by wait ("none" for a line that names none),
	// and Retirements how many of them retired the process.
	ChargesByWait map[string]int
	Retirements   int
	// ChargedWithinBound, ResumeToFinished and TakenNotOncePerOwed are what fails each of R9's
	// three verdicts, in that order: empty for a verdict that passes. The third lists each number
	// taken more times than it was owed, and then each time owed in Unreached.
	ChargedWithinBound, ResumeToFinished, TakenNotOncePerOwed []string
}

// TurnCauses is a count of turns by cause: a task number, or none.
type TurnCauses struct {
	Task, None int
}

// TaskTurns is how many turns the task Task of the claim Claim took.
type TaskTurns struct {
	Claim string
	Task  uint64
	Turns int
}

// costLine is the part of a costed line the reading asks beyond its head (costReading.line): its
// time, its message, and the attributes of the lines it counts. Task, Sequence and Epoch are
// pointers because their absence is the answer (a turn of no cause, a turn's end that is no
// report, a seen report an older daemon wrote with no epoch).
type costLine struct {
	Time     time.Time   `json:"time"`
	Msg      string      `json:"msg"`
	Task     *uint64     `json:"task"`
	Sequence *uint64     `json:"sequence"`
	Epoch    *string     `json:"epoch"`
	Revision uint64      `json:"revision"`
	Origin   string      `json:"origin"`
	Finished bool        `json:"finished"`
	Outcome  TakeOutcome `json:"outcome"`
	Repeat   bool        `json:"repeat"`
	By       string      `json:"by"`
	Wait     string      `json:"wait"`
	Bound    int64       `json:"bound"`
}

// costedLines are the messages of a claim the reading decodes whole. A daemon's starting and start
// lines (DaemonStartingLine, DaemonStartedLine) are read too, from their message alone: they name
// no claim.
var costedLines = map[string]bool{
	turnStartedLine: true, turnEndedLine: true, notTheTakesTurnLine: true, valueWrittenLine: true,
	taskSeenLine: true, valueDeletedLine: true, takeAnsweredLine: true, takeUndecidedLine: true,
	takenBackLine: true, taskFinishedLine: true, processDiedLine: true, agentRegisteredLine: true,
	promptFailureLine: true, promptRetiredLine: true, viewCutLine: true, viewWholeLine: true,
}

// workerActs are the messages of the worker's own acts the reading decodes, which a claim's clock
// waits for after a daemon's start (claimClock.awaitsAct), as the machine's waits wait for the
// worker's first act (Machine.awaitsAct): its seen report, its take, its turn, its report that put
// a take back, its registration. Such an act starts the clock and begins a take's second wait again
// only for a claim no view line shows cut, which the start restored with no process. Over a cut
// view no act starts anything: the view's whole line does, and ends the wait for an act too, since
// the shim's hello, the worker's first act there, came before it (viewWhole).
var workerActs = map[string]bool{
	taskSeenLine: true, takeAnsweredLine: true, takeUndecidedLine: true, turnStartedLine: true,
	turnEndedLine: true, notTheTakesTurnLine: true, agentRegisteredLine: true,
}

// costKey is one task: a number of one claim.
type costKey struct {
	claim  string
	number uint64
}

// openTake is a claim's accepted take whose time owed is running: the number, when it was
// accepted, whether it awaits its turn — from its acceptance and its put-back until a turn naming
// its number starts — and when its second wait began as the reading places it (secondWaitNow): the
// latest of its acceptance, its put-back, the end of a turn that ended while it awaited its turn,
// the point the machine's view of the claim's turns was whole again (viewWholeLine), and, for a
// claim no view line shows cut, the worker's first act after a daemon's start. The machine's can
// begin later, where a hold the reading cannot see stood over the take, and never earlier (the
// first verdict's paragraph above).
type openTake struct {
	number   uint64
	at, from time.Time
	awaits   bool
}

// costCharge is one charge, with the take that stood when it was made.
type costCharge struct {
	claim string
	at    time.Time
	wait  string
	bound time.Duration
	take  openTake
}

// owedNumber is one time a number was owed to a claim, with the clock of R5's first wait on it
// (the rule is the reading's doc comment above): whether a take refused or undecided, or a
// take-back, stopped the clock until the number's next write or the claim's next registration;
// when the clock last started, zero while it stands still; when its bound passed, zero while it
// has not; and, once the time owed ended, how.
type owedNumber struct {
	key          costKey
	stopped      bool
	from, passed time.Time
	ended        string
}

// costTurn is the claim's latest turn whose start or end line named a task, which a put-back may
// make a turn of no cause (costReading.putBack): the number its lines name, whether its end line
// came, and whether its start line and its end line were counted, each as a turn of that number
// (only a line inside the window is).
type costTurn struct {
	number                   uint64
	over                     bool
	startCounted, endCounted bool
}

// costReading is a reading under way.
type costReading struct {
	cost       TaskCost
	turns      map[costKey]int
	written    map[string]map[costKey]bool
	waited     map[string]map[costKey]bool
	taken      map[costKey]int
	owed       map[costKey]int
	finished   map[costKey]string
	resume     map[costKey]time.Time
	open       map[string]openTake
	turnStarts map[string][]time.Time
	charges    []costCharge
	// clocks is each claim's first wait, unreached each time owed whose clock passed the bound,
	// once that time owed ended, and alive the time of the last line the reading decodes, of any
	// claim, which is the last the reading knows a daemon to have lived: a line it does not decode
	// can be the next daemon process's, written before its starting line (`legion start`'s
	// configuration warnings).
	clocks    map[string]*claimClock
	unreached []owedNumber
	alive     time.Time
	// starting is whether a daemon's starting line has come since the last start line
	// (daemonStarting).
	starting bool
	// counting is whether the line being read is inside the window, which every figure counts
	// from.
	counting bool
}

// ReadTaskCost reads the daemon's JSON lines from r — the current container's log from its start,
// as the page's command pipes it — and answers what the window from args.From (included) to
// args.To (not included) says the tasks cost, for the claim args.Claim, or for every claim when it
// is "", with args.Take as the first bound; the lines before args.From say only where each claim
// stands when the window opens. It fails only when r does.
func ReadTaskCost(r io.Reader, args TaskCostArgs) (TaskCost, error) {
	reading := costReading{
		cost: TaskCost{
			Args:            args,
			WrittenByOrigin: map[string]int{}, WaitedByOrigin: map[string]int{},
			FinishedBy: map[string]int{}, ChargesByWait: map[string]int{},
		},
		turns: map[costKey]int{}, written: map[string]map[costKey]bool{}, waited: map[string]map[costKey]bool{},
		taken: map[costKey]int{}, owed: map[costKey]int{}, finished: map[costKey]string{},
		resume: map[costKey]time.Time{}, open: map[string]openTake{}, turnStarts: map[string][]time.Time{},
		clocks: map[string]*claimClock{},
	}
	lines := bufio.NewReader(r)
	for {
		raw, err := lines.ReadBytes('\n')
		reading.line(raw)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return TaskCost{}, fmt.Errorf("read the daemon's log: %w", err)
		}
	}
	return reading.done(), nil
}

// line reads one line of the log: one before the window for where its claim stands at From, one
// inside it for that and for the figures, and none at or after To.
func (c *costReading) line(raw []byte) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '{' {
		return
	}
	var head struct {
		Time  time.Time `json:"time"`
		Msg   string    `json:"msg"`
		Claim any       `json:"claim"`
	}
	if json.Unmarshal(raw, &head) != nil || head.Time.IsZero() {
		return
	}
	claim, _ := head.Claim.(string)
	args := c.cost.Args
	if !head.Time.Before(args.To) {
		return
	}
	c.counting = !head.Time.Before(args.From)
	switch head.Msg {
	case DaemonStartingLine:
		c.daemonStarting()
	case DaemonStartedLine:
		c.daemonStarted()
	}
	costed := costedLines[head.Msg]
	if costed {
		c.alive = head.Time
	}
	if args.Claim != "" && claim != args.Claim {
		return
	}
	if c.counting {
		if c.cost.Lines == 0 {
			c.cost.FirstLine = head.Time
		}
		c.cost.Lines++
		c.cost.LastLine = head.Time
	}
	if !costed {
		return
	}
	var l costLine
	if json.Unmarshal(raw, &l) != nil {
		return
	}
	c.costed(l, claim)
}

// costed reads one line of the messages the reading decodes whole: for where its claim stands,
// and, for a line inside the window (counting), for the figures.
func (c *costReading) costed(l costLine, claim string) {
	if l.Msg == turnEndedLine && l.Sequence != nil {
		// The turn-outcome report's line (turnoutcome.go): the report, not the turn's end.
		return
	}
	var key costKey
	if l.Task != nil {
		key = costKey{claim: claim, number: *l.Task}
	}
	k := c.clocks[claim]
	if k == nil {
		k = &claimClock{}
		c.clocks[claim] = k
	}
	if c.counting {
		c.enter(k)
	}
	// The bound is judged at every line of the claim but a write of the number its clock is on,
	// which is the first bound's own end after a daemon's start (the reading's doc comment).
	if o := k.owed; o == nil || l.Msg != valueWrittenLine || l.Task == nil || *l.Task != o.key.number {
		c.boundPassed(k, l.Time)
	}
	if k.awaitsAct && workerActs[l.Msg] {
		k.awaitsAct = false
		if !k.cut {
			c.restart(k, l.Time)
			c.secondWaitFrom(claim, l.Time)
		}
	}
	switch l.Msg {
	case turnStartedLine:
		c.turnStarts[claim] = append(c.turnStarts[claim], l.Time)
		c.turnStarted(k)
		if l.Task == nil {
			if c.counting {
				c.cost.TurnsStarted.None++
			}
			return
		}
		if c.counting {
			c.cost.TurnsStarted.Task++
			c.turns[key]++
		}
		k.taskTurn = costTurn{number: key.number, startCounted: c.counting}
		if open, ok := c.open[claim]; ok && open.number == key.number {
			open.awaits = false
			c.open[claim] = open
		}
	case turnEndedLine:
		cause := l.Task
		if cause != nil && *cause == k.putBackEnd {
			// The end of a run a put-back gave back (putBack): a turn of no cause.
			cause = nil
		}
		k.putBackEnd = 0
		switch {
		case cause == nil:
			if c.counting {
				c.cost.TurnsEnded.None++
			}
		default:
			if c.counting {
				c.cost.TurnsEnded.Task++
			}
			if t := &k.taskTurn; t.number != *cause || t.over {
				// A turn whose start fell before the log's first line.
				*t = costTurn{number: *cause}
			}
			k.taskTurn.over, k.taskTurn.endCounted = true, c.counting
		}
		// A turn that ends while the claim's take awaits its turn is the in-turn hold over that
		// take, whose end begins its second wait again.
		c.secondWaitFrom(claim, l.Time)
		c.turnEnded(k, l.Time)
	case notTheTakesTurnLine:
		if l.Task != nil {
			c.putBack(k, key, l.Time)
		}
	case valueWrittenLine:
		if l.Task == nil {
			return
		}
		// A claim's value names its one pending task, so a write of another number than the open
		// take's is of a task queued after that take's task went, whether or not a line said so: a
		// queue retires a task whose turn has run with no take-back or finish line (retirePending).
		if open, ok := c.open[claim]; ok && open.number != key.number {
			delete(c.open, claim)
		}
		if c.counting {
			mark(c.written, l.Origin, key)
			if l.Finished {
				mark(c.waited, l.Origin, key)
				if _, listed := c.resume[key]; l.Origin == OriginResume && !listed {
					c.resume[key] = l.Time
				}
			}
		}
		c.owe(k, key, revisionOf(l), l.Time)
	case taskSeenLine:
		if l.Task == nil {
			return
		}
		if k.written.Number == 0 {
			// A report of a write that fell before the log's first line: it owes the number it
			// saw from here (the reading's doc comment).
			c.owe(k, key, revisionOf(l), l.Time)
		}
		if k.seen = k.sawLatest(l); k.seen {
			c.restart(k, l.Time)
		} else if o := k.owed; o != nil {
			// A report of another revision leaves the latest write unseen, which charges nobody.
			o.from = time.Time{}
		}
	case valueDeletedLine:
		if l.Task != nil {
			c.endOwed(k, key.number, l.Time, "its value deleted")
		}
	case takeAnsweredLine:
		if l.Task == nil {
			return
		}
		if c.counting {
			c.cost.takesAnswered++
		}
		switch l.Outcome {
		case TakeOutcomeRefused:
			c.stop(k, key.number)
			return
		case TakeOutcomeAccepted:
			c.endOwed(k, key.number, l.Time, "taken")
		default:
			c.endOwed(k, key.number, l.Time, "a take answered dropped")
			return
		}
		if l.Repeat {
			return
		}
		if c.counting {
			c.taken[key]++
		} else {
			// A charge the window counts is judged by the turns since its take's second wait
			// began, so the turns before this take, all before the window, are no longer asked.
			c.turnStarts[claim] = c.turnStarts[claim][:0]
		}
		c.open[claim] = openTake{number: key.number, at: l.Time, from: l.Time, awaits: true}
	case takeUndecidedLine:
		if l.Task != nil {
			c.stop(k, key.number)
		}
	case takenBackLine:
		if l.Task == nil {
			return
		}
		if c.counting {
			c.owed[key] = max(c.owed[key], 1) + 1
		}
		c.closeTake(claim, key.number)
		// No turn outlives a take-back: it is the turn's end, or its process's let-go.
		k.inTurn, k.putBackEnd = false, 0
		c.takenBack(k, key)
	case taskFinishedLine:
		if l.Task == nil {
			return
		}
		if c.counting {
			c.finished[key] = l.By
		}
		c.closeTake(claim, key.number)
		c.endOwed(k, key.number, l.Time, "finished")
	case processDiedLine:
		// The process and its turn are gone; the next process is the next registration's.
		k.inTurn, k.awaitsRegistration, k.putBackEnd = false, true, 0
		if o := k.owed; o != nil {
			o.from = time.Time{}
		}
	case agentRegisteredLine:
		c.registered(k)
	case promptFailureLine, promptRetiredLine:
		if !c.counting {
			return
		}
		wait := cmp.Or(l.Wait, "none")
		c.cost.ChargesByWait[wait]++
		if l.Msg == promptRetiredLine {
			c.cost.Retirements++
		}
		c.charges = append(c.charges, costCharge{claim: claim, at: l.Time, wait: wait,
			bound: time.Duration(l.Bound), take: c.open[claim]})
	case viewCutLine:
		c.viewCut(k)
	case viewWholeLine:
		c.viewWhole(k, claim, l.Time)
	}
}

// revisionOf is the value write a write line or a seen line names: its number, its epoch where
// it names one, and its revision.
func revisionOf(l costLine) TaskValue {
	r := TaskValue{Number: *l.Task, Revision: l.Revision}
	if l.Epoch != nil {
		r.Epoch = *l.Epoch
	}
	return r
}

// closeTake ends the time owed of the claim's accepted take of number, if that take stands.
func (c *costReading) closeTake(claim string, number uint64) {
	if c.open[claim].number == number {
		delete(c.open, claim)
	}
}

// mark records key under origin in sets.
func mark(sets map[string]map[costKey]bool, origin string, key costKey) {
	if sets[origin] == nil {
		sets[origin] = map[costKey]bool{}
	}
	sets[origin][key] = true
}

// done derives the figures and the verdicts from what the lines left.
func (c *costReading) done() TaskCost {
	cost := c.cost
	for _, key := range slices.SortedFunc(maps.Keys(c.turns), cmpKey) {
		cost.TurnsOfTask = append(cost.TurnsOfTask, TaskTurns{Claim: key.claim, Task: key.number, Turns: c.turns[key]})
	}
	for origin, keys := range c.written {
		cost.WrittenByOrigin[origin] = len(keys)
	}
	for origin, keys := range c.waited {
		cost.WaitedByOrigin[origin] = len(keys)
	}
	cost.Taken, cost.Finished = len(c.taken), len(c.finished)
	for _, by := range c.finished {
		cost.FinishedBy[by]++
	}
	// A number still owed at the window's end is judged there: listed where its clock passed the
	// bound before To, and left out while it has not. A claim with no line in the window has the
	// window opened here, so a clock that ran through the whole window is judged from From. The
	// list is sorted below, so the order the claims are read in here does not show.
	for _, k := range c.clocks {
		c.enter(k)
		c.boundPassed(k, c.cost.Args.To)
		if o := k.owed; o != nil && !o.passed.IsZero() {
			o.ended = "still owed at the window's end"
			c.unreached = append(c.unreached, *o)
		}
	}
	slices.SortFunc(c.unreached, func(a, b owedNumber) int {
		return cmp.Or(cmpKey(a.key, b.key), a.passed.Compare(b.passed))
	})
	for _, owed := range c.unreached {
		cost.Unreached = append(cost.Unreached, fmt.Sprintf("%s task %d: the clock started %s, the bound passed %s, %s",
			owed.key.claim, owed.key.number, lineTime(owed.passed.Add(-c.cost.Args.Take)), lineTime(owed.passed), owed.ended))
	}
	for _, charge := range c.charges {
		if started, ok := c.turnWithinBound(charge); ok {
			wait := "wait " + charge.wait
			if charge.bound > 0 {
				wait += ", bound " + charge.bound.String()
			}
			taken := "taken " + lineTime(charge.take.at)
			if !charge.take.from.Equal(charge.take.at) {
				taken += ", its second wait begun again " + lineTime(charge.take.from)
			}
			cost.ChargedWithinBound = append(cost.ChargedWithinBound, fmt.Sprintf(
				"%s task %d: %s, a turn started %s, charged %s (%s)",
				charge.claim, charge.take.number, taken, lineTime(started), lineTime(charge.at), wait))
		}
	}
	for _, key := range slices.SortedFunc(maps.Keys(c.resume), cmpKey) {
		cost.ResumeToFinished = append(cost.ResumeToFinished,
			fmt.Sprintf("%s task %d: written %s", key.claim, key.number, lineTime(c.resume[key])))
	}
	for _, key := range slices.SortedFunc(maps.Keys(c.taken), cmpKey) {
		owed := max(c.owed[key], 1)
		if c.taken[key] > owed {
			cost.TakenNotOncePerOwed = append(cost.TakenNotOncePerOwed,
				fmt.Sprintf("%s task %d: taken %d times, owed %d", key.claim, key.number, c.taken[key], owed))
		}
	}
	for _, owed := range c.unreached {
		cost.TakenNotOncePerOwed = append(cost.TakenNotOncePerOwed, fmt.Sprintf("%s task %d: owed, and no take within the first bound, which passed %s",
			owed.key.claim, owed.key.number, lineTime(owed.passed)))
	}
	return cost
}

// turnWithinBound is the first turn of the charged claim that started no earlier than the second
// wait of the take the charge was made under began (openTake.from) and no later than the bound
// after that — the charge's own bound, for a charge of the second wait that names one, and the
// charge itself otherwise — and whether there is one.
func (c *costReading) turnWithinBound(charge costCharge) (time.Time, bool) {
	if charge.take.number == 0 {
		return time.Time{}, false
	}
	limit := charge.at
	if charge.wait == WaitTurnStart && charge.bound > 0 {
		limit = charge.take.from.Add(charge.bound)
	}
	for _, started := range c.turnStarts[charge.claim] {
		if !started.Before(charge.take.from) && !started.After(limit) {
			return started, true
		}
	}
	return time.Time{}, false
}

// cmpKey orders tasks by claim and then by number.
func cmpKey(a, b costKey) int {
	return cmp.Or(strings.Compare(a.claim, b.claim), cmp.Compare(a.number, b.number))
}

// lineTime is a line's time as the daemon's log writes it, to the millisecond.
func lineTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }
