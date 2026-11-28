package main

import (
	"cmp"
	"fmt"
	"io"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/envoy/going/agent"
	"github.com/sjawhar/envoy/going/eval"
)

// reportLevels is the levels in the order the report gives them.
var reportLevels = []eval.Level{eval.LevelFast, eval.LevelStandard, eval.LevelDeep}

// writeLevelCalls writes, for each level of the tasks run, how many model calls its tries made and
// the p99 and the max of one call's output tokens, its reasoning among them, and of its reasoning
// tokens alone, over every model call of every try of the level as its reply frame says it
// (tryOutcome.ModelCalls), beside the level's bound of output tokens and the reasoning tokens its
// pacer reserves for a call. It then names each run that stopped at a model call's output tokens
// (agent.ReasonOutputTokens), with the tokens of its last model call, the one cut, and each run
// that stopped at its working time (agent.ReasonTimeBudget), with how long the agent's pacer held
// its model calls, of the time it worked.
func writeLevelCalls(w io.Writer, tasks []eval.Task, byTask map[string][]tryResult) {
	fmt.Fprintln(w, "## Model calls of each level")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Each model call's output tokens, its reasoning among them, and of those its reasoning tokens, as its reply frame says them, over every model call of every try of the level. "+
		"The bound is the output tokens a call of the level may write (max_tokens), which bounds the tokens that are not reasoning where the provider counts the reasoning apart; the pacer reserves the level's reasoning tokens beside it.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "| level | model calls | output tokens p99 | output tokens max | reasoning tokens p99 | reasoning tokens max | bound | reasoning reserved |")
	fmt.Fprintln(w, "| --- | --- | --- | --- | --- | --- | --- | --- |")
	for _, level := range reportLevels {
		var output, reasoning []int
		ran := false
		for _, task := range tasks {
			if task.Level != level {
				continue
			}
			ran = true
			for _, r := range byTask[task.ID] {
				for _, call := range r.Outcome.ModelCalls {
					output = append(output, call.OutputTokens)
					reasoning = append(reasoning, call.ReasoningTokens)
				}
			}
		}
		if !ran {
			continue
		}
		bound := agent.Levels[string(level)]
		fmt.Fprintf(w, "| %s | %d | %s | %s | %s | %s | %d | %d |\n", level, len(output), rankCell(output, 0.99), rankCell(output, 1),
			rankCell(reasoning, 0.99), rankCell(reasoning, 1), bound.OutputTokens, bound.ReasoningTokens)
	}
	fmt.Fprintln(w)

	var cut, timed []string
	for _, task := range tasks {
		for _, r := range byTask[task.ID] {
			switch stopCode(r.Outcome.Reason) {
			case agent.ReasonOutputTokens:
				line := fmt.Sprintf("- %s try %d, %s: no reply frame says the tokens of its model calls", task.ID, r.Try, task.Level)
				if calls := r.Outcome.ModelCalls; len(calls) > 0 {
					last := calls[len(calls)-1]
					line = fmt.Sprintf("- %s try %d, %s: its last model call wrote %d output tokens, %d of them reasoning",
						task.ID, r.Try, task.Level, last.OutputTokens, last.ReasoningTokens)
				}
				cut = append(cut, line)
			case agent.ReasonTimeBudget:
				timed = append(timed, fmt.Sprintf("- %s try %d, %s: the pacer held its model calls %s of the %s it worked",
					task.ID, r.Try, task.Level, msText(r.Outcome.PaceMS), msText(r.Outcome.WorkingMS)))
			}
		}
	}
	fmt.Fprintf(w, "Runs stopped at a model call's output tokens (%s): %d. An answer cut that should not have been is a reason to raise its level's bound.\n",
		agent.ReasonOutputTokens, len(cut))
	for _, line := range cut {
		fmt.Fprintln(w, line)
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "Runs stopped at their working time (%s): %d, each with how long the agent's pacer held its model calls, waiting for the model's quota.\n",
		agent.ReasonTimeBudget, len(timed))
	for _, line := range timed {
		fmt.Fprintln(w, line)
	}
	fmt.Fprintln(w)
}

// checkKinds is the kinds of task in the order the checks' table gives them: answer and status
// first, whose every turn is a question that pays for a check, then the rest in eval.Kinds' order.
func checkKinds() []eval.Kind {
	kinds := []eval.Kind{eval.KindAnswer, eval.KindStatus}
	for _, kind := range eval.Kinds {
		if !slices.Contains(kinds, kind.Kind) {
			kinds = append(kinds, kind.Kind)
		}
	}
	return kinds
}

// checkTally is what the no-plan guard did in the tries of some tasks: the turns their runs ended,
// the checks made and the tokens they spent, for each turn that made a check the milliseconds its
// checks took, summed, the turns whose check it skipped and that made none, which add nothing, and
// the turns it decided on with no line from Jev, those whose line call was still out among them.
type checkTally struct {
	turns, calls, tokens int
	added                []int64
	skipped, unread      int
	late                 int
}

// guardedTurn is what the guard did in one turn: the milliseconds its checks took, summed,
// whether it checked a reply and whether it skipped one, and what Jev read the turn's words as
// when it last decided (proseCheck.Jev), which only moves from late to a line.
type guardedTurn struct {
	ms               int64
	checked, skipped bool
	jev              string
}

// tallyChecks is the checkTally of the tries of tasks.
func tallyChecks(tasks []eval.Task, byTask map[string][]tryResult) checkTally {
	var tally checkTally
	for _, task := range tasks {
		for _, r := range byTask[task.ID] {
			tally.turns += r.Outcome.Turns
			byTurn := map[int]*guardedTurn{}
			for _, check := range r.Outcome.Checks {
				turn := byTurn[check.Turn]
				if turn == nil {
					turn = &guardedTurn{}
					byTurn[check.Turn] = turn
				}
				turn.jev = check.Jev
				if check.Skipped != "" {
					turn.skipped = true
					continue
				}
				turn.checked = true
				turn.ms += check.MS
				tally.calls++
				tally.tokens += check.Tokens
			}
			for _, turn := range byTurn {
				switch {
				case turn.checked:
					tally.added = append(tally.added, turn.ms)
				case turn.skipped:
					tally.skipped++
				}
				switch turn.jev {
				case jevLate:
					tally.late++
					tally.unread++
				case jevNone:
					tally.unread++
				}
			}
		}
	}
	return tally
}

// The lines a reply frame says Jev read the turn's words as where Jev gave the guard none (going/
// agent's replyFrame.JevLine): no line, and a line call still out when the guard decided.
const (
	jevNone = "none"
	jevLate = "late"
)

// writeChecks writes what the no-plan guard did with replies in prose (tryOutcome.Checks): each
// check is one more call of the chat model on a turn that would end in prose with no plan, and a
// turn whose words the decision model Jev read as Ask skips it. For each kind of the tasks run, in
// checkKinds' order, and for all of them, it writes the turns its tries' runs ended, how many of
// those made a check, the checks they made, the turns that skipped it and made none, the turns
// the guard decided on with no line from Jev, and the median and the p99 of the time a turn's
// checks took, summed, over the turns that made one; then how many checks the run made, the
// tokens they spent, the turns it skipped, and of the turns with no line from Jev those whose
// line call was still out. A run whose server names no decision model reads no line for any turn.
func writeChecks(w io.Writer, tasks []eval.Task, byTask map[string][]tryResult) {
	fmt.Fprintln(w, "## The no-plan guard's checks")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "A turn that would end in prose with no plan first makes one more call of the chat model, which checks the reply; a turn whose reply the check holds back makes a second. "+
		"A turn whose words the decision model Jev read as Ask skips the check and adds nothing to its time; a turn Jev gave no line by the time the guard decided is checked. "+
		"The time the check added to a turn is its checks' milliseconds summed; the median and the p99 are over the turns that made a check.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "| kind | turns | turns checked | check calls | turns skipped | turns with no line from Jev | median ms | p99 ms |")
	fmt.Fprintln(w, "| --- | --- | --- | --- | --- | --- | --- | --- |")
	row := func(name string, tally checkTally) {
		fmt.Fprintf(w, "| %s | %d | %d | %d | %d | %d | %s | %s |\n", name, tally.turns, len(tally.added), tally.calls, tally.skipped, tally.unread,
			rankCell(tally.added, 0.5), rankCell(tally.added, 0.99))
	}
	for _, kind := range checkKinds() {
		var of []eval.Task
		for _, task := range tasks {
			if task.Kind == kind {
				of = append(of, task)
			}
		}
		if len(of) > 0 {
			row(string(kind), tallyChecks(of, byTask))
		}
	}
	all := tallyChecks(tasks, byTask)
	row("all", all)
	fmt.Fprintf(w, "\nThe run made %d check calls, which spent %d tokens, and skipped the check of %d turns. "+
		"%d turns had no line from Jev when the guard decided, %d of them with the line call still out.\n\n",
		all.calls, all.tokens, all.skipped, all.unread, all.late)
}

// rankCell is the nearest-rank share q of values (nearestRank) as a cell of the report's tables,
// "none" for no values.
func rankCell[T cmp.Ordered](values []T, q float64) string {
	if len(values) == 0 {
		return "none"
	}
	return fmt.Sprint(nearestRank(values, q))
}

// nearestRank is the least of values that a share q of them, from 0 to 1, is at or under: the
// median at 0.5, the lower of the two middle values for an even count, and the max at 1. values
// holds at least one.
func nearestRank[T cmp.Ordered](values []T, q float64) T {
	sorted := slices.Sorted(slices.Values(values))
	return sorted[max(0, int(math.Ceil(q*float64(len(sorted))))-1)]
}

// stopCode is the code a run's stop reason starts with, before its colon: agent.ReasonTimeBudget
// for a run stopped at its working time.
func stopCode(reason string) string {
	code, _, _ := strings.Cut(reason, ":")
	return code
}

// msText is a time in milliseconds as the report writes it, to a tenth of a second.
func msText(ms int64) string {
	return (time.Duration(ms) * time.Millisecond).Round(100 * time.Millisecond).String()
}
