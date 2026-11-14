package supervise

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
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
// before its logger existed, a line kubectl added — is passed over, and so is every line outside
// the window or of another claim than the one asked for.
//
// What is counted, and from which line:
//
//   - the turns, from the two turn lines (turnStartedLine, turnEndedLine), by their cause: a task
//     number, or none. The turn-outcome report writes the end message too, with a sequence of its
//     own (turnoutcome.go); that line is the report, not the turn's end, and is passed over;
//   - the turns each number took, from the start lines that name it;
//   - the numbers written, by the origin of the task each names, from the value writes
//     (valueWrittenLine); and of those, the numbers written to a claim that had finished the last
//     task written to it in the session it ran (finished), which is a task written to a claim
//     whose phase waited (decision 19 and task T14 of the plan). The silence repair's writes
//     (OriginSilence) are an origin of their own, read apart from a resume's;
//   - the numbers taken, from the accepted answers to a take (takeAnsweredLine), less an answer
//     that repeats one from the record (repeat), which applied nothing;
//   - the numbers finished, from taskFinishedLine, by what finished each;
//   - the charges, from the two charge lines (promptFailureLine, promptRetiredLine), by wait, and
//     the retirements among them.
//
// And R9's three verdicts, each of which lists what fails it:
//
//   - no charge of a worker whose turn began within the second bound after its take: a charge made
//     while an accepted take of the claim stood, where a turn of the claim started after that take
//     and no later than the bound after it (the charge's own bound, for a charge of the second
//     wait) or, for a charge that names no bound, than the charge itself;
//   - no write of origin resume to a claim that had finished its last task in its session (a value
//     write of origin resume whose finished is true); a silence repair's write is not one;
//   - each number taken once for each time the record owed it: a number is owed once when it is
//     written, and once more at each take-back of it (takenBackLine), and no number may be taken
//     more times in the window than it was owed.
//
// Every figure is a lower bound: the log reaches back only as far as the node kept the daemon
// container's log, and a take, a write or a turn that fell before the window's first line is
// not seen.

// TaskCostArgs is what `legion task-cost` is asked: the window of the daemon's log to read, From
// included and To not, and the one claim to read it for, "" for every claim.
type TaskCostArgs struct {
	From  time.Time
	To    time.Time
	Claim string
}

// ParseTaskCostArgs parses `legion task-cost`'s arguments: --from <RFC 3339> --to <RFC 3339>
// [--claim <token>]. Both times are required, To must be after From, and nothing else may be
// given. A help flag answers flag.ErrHelp, which the command answers with its usage.
func ParseTaskCostArgs(args []string) (TaskCostArgs, error) {
	flags := flag.NewFlagSet("task-cost", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	from := flags.String("from", "", "")
	to := flags.String("to", "", "")
	claim := flags.String("claim", "", "")
	if err := flags.Parse(args); err != nil {
		return TaskCostArgs{}, err
	}
	if flags.NArg() > 0 {
		return TaskCostArgs{}, fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	var parsed TaskCostArgs
	for _, bound := range []struct {
		name, value string
		into        *time.Time
	}{{"from", *from, &parsed.From}, {"to", *to, &parsed.To}} {
		if bound.value == "" {
			return TaskCostArgs{}, fmt.Errorf("--%s is required", bound.name)
		}
		at, err := time.Parse(time.RFC3339, bound.value)
		if err != nil {
			return TaskCostArgs{}, fmt.Errorf("--%s %q is not an RFC 3339 time", bound.name, bound.value)
		}
		*bound.into = at
	}
	if !parsed.To.After(parsed.From) {
		return TaskCostArgs{}, fmt.Errorf("--to %s is not after --from %s", *to, *from)
	}
	parsed.Claim = *claim
	return parsed, nil
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
	// ChargesByWait is how many charges were made, by wait ("none" for a line that names none),
	// and Retirements how many of them retired the process.
	ChargesByWait map[string]int
	Retirements   int
	// ChargedWithinBound, ResumeToFinished and TakenPastOwed are what fails each of R9's three
	// verdicts, in that order: empty for a verdict that passes.
	ChargedWithinBound, ResumeToFinished, TakenPastOwed []string
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
// time, its message, and the attributes of the lines it counts. Task and Sequence are pointers
// because their absence is the answer (a turn of no cause, a turn's end that is no report).
type costLine struct {
	Time     time.Time `json:"time"`
	Msg      string    `json:"msg"`
	Task     *uint64   `json:"task"`
	Sequence *uint64   `json:"sequence"`
	Origin   string    `json:"origin"`
	Finished bool      `json:"finished"`
	Outcome  string    `json:"outcome"`
	Repeat   bool      `json:"repeat"`
	By       string    `json:"by"`
	Wait     string    `json:"wait"`
	Bound    int64     `json:"bound"`
}

// costedLines are the messages the reading decodes whole.
var costedLines = map[string]bool{
	turnStartedLine: true, turnEndedLine: true, valueWrittenLine: true, takeAnsweredLine: true,
	takenBackLine: true, taskFinishedLine: true, promptFailureLine: true, promptRetiredLine: true,
}

// costKey is one task: a number of one claim.
type costKey struct {
	claim  string
	number uint64
}

// openTake is a claim's accepted take whose time owed is running: the number and when it was
// accepted.
type openTake struct {
	number uint64
	at     time.Time
}

// costCharge is one charge, with the take that stood when it was made.
type costCharge struct {
	claim string
	at    time.Time
	wait  string
	bound time.Duration
	take  openTake
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
}

// ReadTaskCost reads the daemon's JSON lines from r and answers what the window from (included)
// to to (not included) says the tasks cost, for the claim claim, or for every claim when claim is
// "". It fails only when r does.
func ReadTaskCost(r io.Reader, from, to time.Time, claim string) (TaskCost, error) {
	reading := costReading{
		cost: TaskCost{
			Args:            TaskCostArgs{From: from, To: to, Claim: claim},
			WrittenByOrigin: map[string]int{}, WaitedByOrigin: map[string]int{},
			FinishedBy: map[string]int{}, ChargesByWait: map[string]int{},
		},
		turns: map[costKey]int{}, written: map[string]map[costKey]bool{}, waited: map[string]map[costKey]bool{},
		taken: map[costKey]int{}, owed: map[costKey]int{}, finished: map[costKey]string{},
		resume: map[costKey]time.Time{}, open: map[string]openTake{}, turnStarts: map[string][]time.Time{},
	}
	lines := bufio.NewReader(r)
	for {
		raw, err := lines.ReadBytes('\n')
		if len(raw) > 0 {
			reading.line(raw)
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return TaskCost{}, fmt.Errorf("read the daemon's log: %w", err)
		}
	}
	return reading.done(), nil
}

// line reads one line of the log.
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
	if head.Time.Before(args.From) || !head.Time.Before(args.To) || (args.Claim != "" && claim != args.Claim) {
		return
	}
	if c.cost.Lines == 0 {
		c.cost.FirstLine = head.Time
	}
	c.cost.Lines++
	c.cost.LastLine = head.Time
	if !costedLines[head.Msg] {
		return
	}
	var l costLine
	if json.Unmarshal(raw, &l) != nil {
		return
	}
	c.costed(l, claim)
}

// costed counts one line of the messages the reading decodes whole.
func (c *costReading) costed(l costLine, claim string) {
	var key costKey
	if l.Task != nil {
		key = costKey{claim: claim, number: *l.Task}
	}
	switch l.Msg {
	case turnStartedLine:
		c.turnStarts[claim] = append(c.turnStarts[claim], l.Time)
		if l.Task == nil {
			c.cost.TurnsStarted.None++
			return
		}
		c.cost.TurnsStarted.Task++
		c.turns[key]++
	case turnEndedLine:
		switch {
		case l.Sequence != nil:
			// The turn-outcome report's line (turnoutcome.go): the report, not the turn's end.
		case l.Task == nil:
			c.cost.TurnsEnded.None++
		default:
			c.cost.TurnsEnded.Task++
		}
	case valueWrittenLine:
		if l.Task == nil {
			return
		}
		mark(c.written, l.Origin, key)
		if _, owed := c.owed[key]; !owed {
			c.owed[key] = 1
		}
		if l.Finished {
			mark(c.waited, l.Origin, key)
			if _, listed := c.resume[key]; l.Origin == OriginResume && !listed {
				c.resume[key] = l.Time
			}
		}
	case takeAnsweredLine:
		if l.Task == nil || l.Outcome != string(TakeOutcomeAccepted) || l.Repeat {
			return
		}
		c.taken[key]++
		c.open[claim] = openTake{number: key.number, at: l.Time}
	case takenBackLine:
		if l.Task == nil {
			return
		}
		c.owed[key] = max(c.owed[key], 1) + 1
		c.closeTake(claim, key.number)
	case taskFinishedLine:
		if l.Task == nil {
			return
		}
		c.finished[key] = l.By
		c.closeTake(claim, key.number)
	case promptFailureLine, promptRetiredLine:
		wait := l.Wait
		if wait == "" {
			wait = "none"
		}
		c.cost.ChargesByWait[wait]++
		if l.Msg == promptRetiredLine {
			c.cost.Retirements++
		}
		c.charges = append(c.charges, costCharge{claim: claim, at: l.Time, wait: l.Wait,
			bound: time.Duration(l.Bound), take: c.open[claim]})
	}
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
	for key, turns := range c.turns {
		cost.TurnsOfTask = append(cost.TurnsOfTask, TaskTurns{Claim: key.claim, Task: key.number, Turns: turns})
	}
	slices.SortFunc(cost.TurnsOfTask, func(a, b TaskTurns) int {
		return cmpKey(costKey{a.Claim, a.Task}, costKey{b.Claim, b.Task})
	})
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
	for _, charge := range c.charges {
		if started, ok := c.turnWithinBound(charge); ok {
			wait := "wait " + charge.wait
			if charge.bound > 0 {
				wait += ", bound " + charge.bound.String()
			}
			cost.ChargedWithinBound = append(cost.ChargedWithinBound, fmt.Sprintf(
				"%s task %d: taken %s, a turn started %s, charged %s (%s)",
				charge.claim, charge.take.number, lineTime(charge.take.at), lineTime(started),
				lineTime(charge.at), wait))
		}
	}
	for _, key := range slices.SortedFunc(maps.Keys(c.resume), cmpKey) {
		cost.ResumeToFinished = append(cost.ResumeToFinished,
			fmt.Sprintf("%s task %d: written %s", key.claim, key.number, lineTime(c.resume[key])))
	}
	for _, key := range slices.SortedFunc(maps.Keys(c.taken), cmpKey) {
		owed := max(c.owed[key], 1)
		if c.taken[key] > owed {
			cost.TakenPastOwed = append(cost.TakenPastOwed,
				fmt.Sprintf("%s task %d: taken %d times, owed %d", key.claim, key.number, c.taken[key], owed))
		}
	}
	return cost
}

// turnWithinBound is the first turn of the charged claim that started after the take the charge
// was made under and no later than the bound after it — the charge's own bound, for a charge of
// the second wait that names one, and the charge itself otherwise — and whether there is one.
func (c *costReading) turnWithinBound(charge costCharge) (time.Time, bool) {
	if charge.take.number == 0 {
		return time.Time{}, false
	}
	limit := charge.at
	if charge.wait == WaitTurnStart && charge.bound > 0 {
		limit = charge.take.at.Add(charge.bound)
	}
	for _, started := range c.turnStarts[charge.claim] {
		if !started.Before(charge.take.at) && !started.After(limit) {
			return started, true
		}
	}
	return time.Time{}, false
}

// cmpKey orders tasks by claim and then by number.
func cmpKey(a, b costKey) int {
	if a.claim != b.claim {
		return strings.Compare(a.claim, b.claim)
	}
	switch {
	case a.number < b.number:
		return -1
	case a.number > b.number:
		return 1
	}
	return 0
}

// lineTime is a line's time as the daemon's log writes it, to the millisecond.
func lineTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

// Text is the reading as `legion task-cost` prints it: one figure to a line, each named as the
// documentation site's page defines it, and each verdict with what fails it.
func (c TaskCost) Text() string {
	var b strings.Builder
	whose := "every claim"
	if c.Args.Claim != "" {
		whose = "claim " + c.Args.Claim
	}
	fmt.Fprintf(&b, "window: %s to %s, %s\n", c.Args.From.UTC().Format(time.RFC3339),
		c.Args.To.UTC().Format(time.RFC3339), whose)
	if c.Lines == 0 {
		fmt.Fprintln(&b, "daemon lines in the window: 0")
	} else {
		fmt.Fprintf(&b, "daemon lines in the window: %d, the first at %s, the last at %s\n",
			c.Lines, lineTime(c.FirstLine), lineTime(c.LastLine))
	}
	for _, turns := range []struct {
		name   string
		causes TurnCauses
	}{{"turns started", c.TurnsStarted}, {"turns ended", c.TurnsEnded}} {
		fmt.Fprintf(&b, "%s: %d (by a task number %d, with no cause %d)\n", turns.name,
			turns.causes.Task+turns.causes.None, turns.causes.Task, turns.causes.None)
	}
	if len(c.TurnsOfTask) == 0 {
		fmt.Fprintln(&b, "turns each number took: none")
	} else {
		fmt.Fprintln(&b, "turns each number took:")
		for _, t := range c.TurnsOfTask {
			fmt.Fprintf(&b, "  %s task %d: %d\n", t.Claim, t.Task, t.Turns)
		}
	}
	fmt.Fprintf(&b, "numbers written by origin: %s\n", byKey(c.WrittenByOrigin))
	fmt.Fprintf(&b, "numbers written to a claim whose phase waited, by origin: %s\n", byKey(c.WaitedByOrigin))
	fmt.Fprintf(&b, "silence repair (origin %s), counted apart from resume: numbers written %d, to a claim whose phase waited %d\n",
		OriginSilence, c.WrittenByOrigin[OriginSilence], c.WaitedByOrigin[OriginSilence])
	fmt.Fprintf(&b, "numbers taken: %d\n", c.Taken)
	fmt.Fprintf(&b, "numbers finished, by what finished each: %s\n", byKey(c.FinishedBy))
	fmt.Fprintf(&b, "charges by wait: %s\n", byKey(c.ChargesByWait))
	fmt.Fprintf(&b, "retirements: %d\n", c.Retirements)
	for _, verdict := range []struct {
		name  string
		fails []string
	}{
		{"no charge of a worker whose turn began within the second bound after its take", c.ChargedWithinBound},
		{"no write of origin resume to a claim that finished its last task in its session", c.ResumeToFinished},
		{"each number taken once for each time the record owed it", c.TakenPastOwed},
	} {
		if len(verdict.fails) == 0 {
			fmt.Fprintf(&b, "verdict: %s: pass\n", verdict.name)
			continue
		}
		fmt.Fprintf(&b, "verdict: %s: fail\n", verdict.name)
		for _, fail := range verdict.fails {
			fmt.Fprintf(&b, "  %s\n", fail)
		}
	}
	return b.String()
}

// byKey is counts as `key n, key n` in key order, with their sum, or "none".
func byKey(counts map[string]int) string {
	if len(counts) == 0 {
		return "none"
	}
	var parts []string
	total := 0
	for _, key := range slices.Sorted(maps.Keys(counts)) {
		parts = append(parts, fmt.Sprintf("%s %d", key, counts[key]))
		total += counts[key]
	}
	return fmt.Sprintf("%s (%d in all)", strings.Join(parts, ", "), total)
}
