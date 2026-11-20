package main

// legion: tool - replays every edit a replay of a production bundle plans against a model of Dispatch's edit rules and fails on one Dispatch would refuse, for a person who picks a bundle for a load run and must know it replays whole; run with LOADTEST_REPLAY_BUNDLE=<bundle dir> go test -count=1 -run '^$' -bench '^BenchmarkReplayEditsBundle$' -benchtime 1x ./loadtest

import (
	"flag"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"
)

// BenchmarkReplayEditsBundle plans the bundle LOADTEST_REPLAY_BUNDLE names (its input: it skips
// without it) at rate 1 over its whole window (loadgen replay-plan -replay-rate 1, its other flags
// at their defaults) and replays every planned edit, document by document in plan order, against
// applyAsDispatch from the text the client holds: the document's text at the bundle's instant
// (what the seed leaves Dispatch holding), a created document's or spec's text, an upload's
// content, or the text the edit before it left. It fails on any edit Dispatch's rules refuse, on
// any whose result is not the text the plan then holds, and on an anchored root's quote the
// rendered text holds more than once. It logs counts only, never a text. It is a benchmark, not a
// test, since it reads a production bundle, and no CI step passes -bench; applyAsDispatch's rules
// are tested on generated inputs. Run it once, with -benchtime 1x.
func BenchmarkReplayEditsBundle(b *testing.B) {
	dir := os.Getenv("LOADTEST_REPLAY_BUNDLE")
	if dir == "" {
		b.Skip("LOADTEST_REPLAY_BUNDLE is not set")
	}
	bundle, err := readBundle(dir)
	if err != nil {
		b.Fatal(err)
	}
	fs := flag.NewFlagSet("replay-plan", flag.ContinueOnError)
	rf := registerReplayFlags(fs)
	if err := fs.Parse([]string{"-replay-inputs", dir, "-replay-rate", "1"}); err != nil {
		b.Fatal(err)
	}
	o, err := rf.options(1)
	if err != nil {
		b.Fatal(err)
	}
	plan, err := planReplay(bundle, o)
	if err != nil {
		b.Fatal(err)
	}
	texts := map[string]string{}
	for i := range bundle.State.Issues {
		for _, a := range bundle.State.Issues[i].Artifacts {
			if a.Kind == "doc" && a.TextPath != "" {
				raw, err := bundle.readFile(a.TextPath)
				if err != nil {
					b.Fatal(err)
				}
				texts[a.ID] = string(raw)
			}
		}
	}
	outcomes, ops := map[string]int{}, map[string]int{}
	edits := 0
	for _, act := range plan.Actions {
		if !act.Write || act.Doc == "" {
			continue
		}
		var body map[string]any
		for _, stage := range act.Stages {
			for _, r := range stage {
				if r.Method != "GET" {
					body = r.Body
				}
			}
		}
		switch act.Kind {
		case rkIssueCreate:
			texts[act.Doc] = body["spec"].(string)
		case rkDocCreate, rkUpload:
			texts[act.Doc] = body["content"].(string)
		case rkEdit, rkEditUnnamed:
			edits++
			batch := body["ops"].([]replayEditOp)
			for _, op := range batch {
				ops[op.Op]++
			}
			text, known := texts[act.Doc]
			if !known {
				b.Errorf("%s event %d: an edit of document %s the client never held", act.Issue, act.EventID, act.Doc)
				continue
			}
			got, code := applyAsDispatch(text, batch)
			switch {
			case code != "":
				outcomes[code]++
				b.Errorf("%s event %d: %d ops on document %s: Dispatch would answer %s", act.Issue, act.EventID, len(batch), act.Doc, code)
			case got != act.text:
				outcomes["applied, text differs"]++
				b.Errorf("%s event %d: document %s holds %d bytes after the edit, the plan %d", act.Issue, act.EventID, act.Doc, len(got), len(act.text))
			default:
				outcomes["applied"]++
			}
			texts[act.Doc] = act.text
		case rkAnchored:
			// An anchored root's quote must match once in the rendered text it lands in: Dispatch
			// answers a quote it matches twice with 409 TARGET_AMBIGUOUS.
			anchor, ok := body["anchor"].(map[string]any)
			if !ok {
				continue
			}
			quote := anchor["quote"].(string)
			if n := strings.Count(scanMarkdown(texts[act.Doc]).rendered, renderedLine(quote)); n > 1 {
				outcomes["anchor 409 TARGET_AMBIGUOUS"]++
				b.Errorf("%s event %d: the anchor quote on document %s matches %d places", act.Issue, act.EventID, act.Doc, n)
			} else {
				outcomes["anchor matches once"]++
			}
		}
	}
	for _, k := range slices.Sorted(maps.Keys(outcomes)) {
		b.Logf("%-24s %5d", k, outcomes[k])
	}
	b.Logf("%d edits (%d from the diff, %d substitutes); ops %v; %d anchored roots unanchorable", edits, plan.Counts.Edits.Diff, plan.Counts.Edits.Substitute, ops, plan.Counts.Unanchorable)
	if edits == 0 {
		b.Fatal("the plan holds no edit")
	}
}
