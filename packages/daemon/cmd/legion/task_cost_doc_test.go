package main

// The documentation site's page of the cost reading and the sample log committed beside
// internal/supervise hold each other to the reader: the page's commands, parsed as `legion
// task-cost` parses them (parseTaskCostArgs) and read by the daemon's own reader
// (supervise.ReadTaskCost), print every figure the page shows.

import (
	"bytes"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/supervise"
)

const (
	// taskCostPage is the documentation site's page of the reading, from this package's directory:
	// the repository root is four directories up.
	taskCostPage = "../../../../docs/site/src/content/docs/legion/task-delivery-cost.md"
	// taskCostSample is the sample log the page's examples read, from this package's directory,
	// and taskCostSampleFromRoot the same file as the page names it, from the repository root. It
	// is named .jsonl, the daemon's JSON lines, because the repository's .gitignore ignores *.log.
	taskCostSample         = "../../internal/supervise/testdata/task-cost.jsonl"
	taskCostSampleFromRoot = "packages/daemon/internal/supervise/testdata/task-cost.jsonl"
	// taskCostCommand is the command the page gives in one fenced block: it pipes the current
	// container's whole log, from its start, with no --since-time.
	taskCostCommand = "kubectl -n legion logs deploy/legion-<project>-daemon -c legion | legion task-cost --from <from> --to <to> [--claim <token>] [--take-timeout <seconds>]"
	// startedLine and endedLine are the messages of the daemon's two turn lines (supervise's
	// turnStartedLine and turnEndedLine).
	startedLine = "supervise: a turn started"
	endedLine   = "supervise: a turn ended"
)

// docExample is one example of the page: the arguments its fenced `legion task-cost` command
// gives, and the output the page shows for it in the fenced block after it.
type docExample struct {
	command string
	args    []string
	output  string
}

// fencedBlock is one fenced code block of a Markdown page: its language and its text.
type fencedBlock struct {
	lang, text string
}

// fencedBlocks is every fenced code block of page, in order.
func fencedBlocks(page string) []fencedBlock {
	var blocks []fencedBlock
	var open *fencedBlock
	var body []string
	for _, line := range strings.Split(page, "\n") {
		switch {
		case open == nil && strings.HasPrefix(line, "```"):
			open, body = &fencedBlock{lang: strings.TrimPrefix(line, "```")}, nil
		case open != nil && line == "```":
			open.text = strings.Join(body, "\n") + "\n"
			blocks = append(blocks, *open)
			open = nil
		case open != nil:
			body = append(body, line)
		}
	}
	return blocks
}

// taskCostExamples reads the page: it fails unless the page gives the reading's command in one
// fenced block, and answers each example — a fenced `legion task-cost` command reading the sample,
// followed by a fenced block of its output.
func taskCostExamples(t *testing.T) []docExample {
	t.Helper()
	page, err := os.ReadFile(taskCostPage)
	if err != nil {
		t.Fatalf("read the page: %v", err)
	}
	blocks := fencedBlocks(string(page))
	commands := 0
	var examples []docExample
	for i, block := range blocks {
		if block.lang == "sh" && block.text == taskCostCommand+"\n" {
			commands++
		}
		line, ok := strings.CutPrefix(strings.TrimSuffix(block.text, "\n"), "legion task-cost ")
		if block.lang != "sh" || !ok {
			continue
		}
		args, input, redirected := strings.Cut(line, " < ")
		if !redirected || input != taskCostSampleFromRoot {
			t.Fatalf("the page's example %q reads %q, want the sample %s on its standard input", block.text, input, taskCostSampleFromRoot)
		}
		if i+1 == len(blocks) || blocks[i+1].lang != "text" {
			t.Fatalf("the page's example %q is followed by no fenced text block of its output", block.text)
		}
		examples = append(examples, docExample{command: line, args: strings.Fields(args), output: blocks[i+1].text})
	}
	if commands != 1 {
		t.Fatalf("the page gives %q in %d fenced blocks, want one", taskCostCommand, commands)
	}
	claims := 0
	for _, example := range examples {
		if slices.Contains(example.args, "--claim") {
			claims++
		}
	}
	if claims == 0 || claims == len(examples) {
		t.Fatalf("the page's examples %q read the sample whole %d times and one claim %d times, want both", examples, len(examples)-claims, claims)
	}
	return examples
}

// readSample is the sample log the page's examples read (taskCostSampleFromRoot names the same
// file from the repository root).
func readSample(t *testing.T) []byte {
	t.Helper()
	sample, err := os.ReadFile(taskCostSample)
	if err != nil {
		t.Fatalf("read the sample: %v", err)
	}
	return sample
}

// readExample runs the example's command over log, as `legion task-cost` runs it.
func readExample(t *testing.T, example docExample, log []byte) supervise.TaskCost {
	t.Helper()
	args, err := parseTaskCostArgs(example.args)
	if err != nil {
		t.Fatalf("the page's example %q: %v", example.command, err)
	}
	cost, err := supervise.ReadTaskCost(bytes.NewReader(log), args)
	if err != nil {
		t.Fatalf("read the sample for %q: %v", example.command, err)
	}
	return cost
}

// figure is one figure of the reading's output: its name, and its value — the rest of its line,
// with the indented lines under it (each task of a list, each failure of a verdict).
type figure struct {
	name, value string
}

// figures is the reading's output figure by figure, in order. A figure's name is its line up to
// the first ": ", or a verdict's up to its last, since a verdict's name holds no colon and its
// value is pass or fail; a line that ends in ":" names a figure whose value is the indented lines
// under it.
func figures(text string) []figure {
	var out []figure
	for _, line := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
		if strings.HasPrefix(line, "  ") && len(out) > 0 {
			last := &out[len(out)-1]
			last.value = strings.TrimPrefix(last.value+"\n"+strings.TrimSpace(line), "\n")
			continue
		}
		cut := strings.Index(line, ": ")
		if strings.HasPrefix(line, "verdict: ") {
			cut = strings.LastIndex(line, ": ")
		}
		if cut < 0 {
			out = append(out, figure{name: strings.TrimSuffix(line, ":")})
			continue
		}
		out = append(out, figure{name: line[:cut], value: line[cut+2:]})
	}
	return out
}

// figureDiff is every figure where the page's output and the reading's part, or "" where the two
// are the same text: a figure one shows with another value than the other, or that only one of
// them shows, and, where every figure agrees, the text itself.
func figureDiff(want, got string) string {
	gave, showed := map[string]string{}, map[string]bool{}
	read := figures(got)
	for _, f := range read {
		gave[f.name] = f.value
	}
	var diff []string
	for _, f := range figures(want) {
		showed[f.name] = true
		switch value, ok := gave[f.name]; {
		case !ok:
			diff = append(diff, fmt.Sprintf("%s: the page shows %q, the reading gives no such figure", f.name, f.value))
		case value != f.value:
			diff = append(diff, fmt.Sprintf("%s: the page shows %q, the reading gives %q", f.name, f.value, value))
		}
	}
	for _, f := range read {
		if !showed[f.name] {
			diff = append(diff, fmt.Sprintf("%s: the reading gives %q, the page shows no such figure", f.name, f.value))
		}
	}
	if len(diff) == 0 && want != got {
		diff = append(diff, fmt.Sprintf("every figure agrees, but the page shows the text %q and the reading gives %q", want, got))
	}
	return strings.Join(diff, "\n")
}

// turnCause is the task attribute of a turn line, which a sample whose turn lines carry no cause
// lacks (TestDocExampleTaskCost's negative control).
var turnCause = regexp.MustCompile(`,"task":\d+`)

// The page's command, run as the page gives it over the sample committed beside internal/supervise,
// prints every figure the page shows: for the whole sample and for one claim. The sample holds a
// task written to a claim whose phase waited and a silence repair's write to such a claim, so the
// waited figure and the silence count are not zero, and the resume verdict is seen not to count
// the silence repair's write. It holds a number written and seen before the window and taken late
// inside it too, which the reading lists from the window's start; a reading cut at the window's
// start, the never-taken test's control, lists no such number, and the page's figures do not hold
// over it.
//
// The negative control is the same reading over the sample with the cause taken out of every turn
// line: the figures the page shows do not hold, which is the proof that they come from the turn
// lines' causes. And a window the log does not reach reads no line and no turn at all, so the
// figures come from the lines and from no constant.
func TestDocExampleTaskCost(t *testing.T) {
	examples := taskCostExamples(t)
	sample := readSample(t)

	t.Run("the whole sample and one claim", func(t *testing.T) {
		for _, example := range examples {
			cost := readExample(t, example, sample)
			if diff := figureDiff(example.output, cost.Text()); diff != "" {
				t.Errorf("legion task-cost %s gives figures the page does not show:\n%s", example.command, diff)
			}
			if !slices.Contains(example.args, "--claim") {
				if cost.WaitedByOrigin[supervise.OriginSilence] == 0 || cost.WrittenByOrigin[supervise.OriginSilence] == 0 {
					t.Errorf("the whole sample reads %v written and %v waited by origin, want the silence repair's write to a waiting claim in both",
						cost.WrittenByOrigin, cost.WaitedByOrigin)
				}
				if len(cost.ResumeToFinished) != 0 {
					t.Errorf("the resume verdict fails on %q, want the silence repair's write left out of it", cost.ResumeToFinished)
				}
			}
		}
	})

	t.Run("a sample whose turn lines carry no cause", func(t *testing.T) {
		var stripped []string
		for _, line := range strings.Split(string(sample), "\n") {
			if strings.Contains(line, `"msg":"`+startedLine+`"`) || strings.Contains(line, `"msg":"`+endedLine+`"`) {
				line = turnCause.ReplaceAllString(line, "")
			}
			stripped = append(stripped, line)
		}
		uncaused := []byte(strings.Join(stripped, "\n"))
		for _, example := range examples {
			diff := figureDiff(example.output, readExample(t, example, uncaused).Text())
			if diff == "" {
				t.Fatalf("legion task-cost %s over turn lines with no cause gives the page's figures, want them not to hold", example.command)
			}
			t.Logf("legion task-cost %s over turn lines with no cause does not give the page's figures:\n%s", example.command, diff)
		}
	})

	t.Run("a window the log does not reach", func(t *testing.T) {
		for _, example := range examples {
			args, err := parseTaskCostArgs(example.args)
			if err != nil {
				t.Fatal(err)
			}
			day := 24 * time.Hour
			args.From, args.To = args.From.Add(-day), args.To.Add(-day)
			cost, err := supervise.ReadTaskCost(bytes.NewReader(sample), args)
			if err != nil {
				t.Fatal(err)
			}
			if cost.Lines != 0 || cost.TurnsStarted != (supervise.TurnCauses{}) || cost.TurnsEnded != (supervise.TurnCauses{}) || cost.Taken != 0 || len(cost.Unreached) != 0 {
				t.Fatalf("a day before the sample reads %d lines, turns %+v and %+v, %d taken, %q owed past the first bound; want nothing", cost.Lines,
					cost.TurnsStarted, cost.TurnsEnded, cost.Taken, cost.Unreached)
			}
		}
	})
}
