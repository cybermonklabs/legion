package intake

//ci:reads packages/contracts/fixtures/checks-settlement/* fixtureSettlement

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// checksFixtureSettlement is the settlement a fixture of packages/contracts/fixtures/checks-settlement
// says Envoy's listener publishes: each check's run with the word GitHub gave it, and the names
// each group of the settlement holds.
type checksFixtureSettlement struct {
	CheckRuns []struct {
		Name       fixtureName     `json:"name"`
		ID         int64           `json:"id"`
		Conclusion json.RawMessage `json:"conclusion"`
	} `json:"check_runs"`
	Failed    []fixtureName `json:"failed"`
	Cancelled []fixtureName `json:"cancelled"`
	Skipped   []fixtureName `json:"skipped"`
}

// fixtureSettlement is the settlement the shared fixture named file holds.
func fixtureSettlement(t *testing.T, file string) checksFixtureSettlement {
	t.Helper()
	encoded, err := os.ReadFile(filepath.Join("..", "..", "..", "contracts", "fixtures", "checks-settlement", file))
	if err != nil {
		t.Fatalf("read the shared fixture %s: %v", file, err)
	}
	var fixture struct {
		Settlement checksFixtureSettlement `json:"settlement"`
	}
	if err := json.Unmarshal(encoded, &fixture); err != nil {
		t.Fatalf("decode the shared fixture %s: %v", file, err)
	}
	return fixture.Settlement
}

// fixtureNames is the names list holds, each built as the fixture writes it.
func fixtureNames(list []fixtureName) []string {
	out := make([]string, 0, len(list))
	for _, name := range list {
		out = append(out, string(name))
	}
	return out
}

// checkGroup is names as the listener writes one group of a settlement: {count, checks}.
func checkGroup(names []string) map[string]any {
	return map[string]any{"count": len(names), "checks": append([]string{}, names...)}
}

// byName is runs sorted by name, the order the decoder gives a settlement's runs in.
func byName(runs []record.AttemptRun) []record.AttemptRun {
	sorted := slices.Clone(runs)
	slices.SortFunc(sorted, func(a, b record.AttemptRun) int { return strings.Compare(a.Name, b.Name) })
	return sorted
}

// runNamed is the run of runs named name, failing the test when there is none.
func runNamed(t *testing.T, runs []record.AttemptRun, name string) record.AttemptRun {
	t.Helper()
	i := slices.IndexFunc(runs, func(run record.AttemptRun) bool { return run.Name == name })
	if i < 0 {
		t.Fatalf("the decoded runs %+v hold no run of %s", runs, name)
	}
	return runs[i]
}

// The listener names each check run's word as GitHub gave it (check_runs[].conclusion) and the
// checks GitHub concluded skipped in a group of their own (skipped, {count, checks}). The daemon
// keeps each word on its run, display only, and makes each run the skipped group names skipped
// (record.RunSkipped), the one conclusion it puts on a run at decode; every other run is left to
// the lists, as before. On both shared fixtures, as the listener's own tests publish them: on the
// head of this repository whose held-back suites skipped, the 28 runs GitHub concluded skipped are
// skipped with their word and every other run carries its word, success, with no conclusion; on
// the fixture of the listener's rules, docs is skipped, advice carries neutral and future a word
// GitHub may add later, passed on as it is.
//
// A word is kept only when it is lower-case letters and underscores, one to 32 of them, as each
// of GitHub's words and the one it may add later are; anything else, which no listener writes, is
// dropped from its run and named in the unread list the consumer logs, and so is a skipped group
// that cannot be read, which then names no run skipped: neither costs the settlement anything
// else, so the fact stands with every run. An absent or null word is no word and nothing unread,
// and a name the skipped group gives with no run changes nothing.
func TestDecodeChecksReadsTheSkippedGroupAndEachConclusionWord(t *testing.T) {
	t.Parallel()
	for _, file := range []string{"held-back-suites-skip.json", "listener-rules.json"} {
		t.Run(file, func(t *testing.T) {
			settlement := fixtureSettlement(t, file)
			failed, cancelled, skipped := fixtureNames(settlement.Failed), fixtureNames(settlement.Cancelled), fixtureNames(settlement.Skipped)
			entries := make([]map[string]any, 0, len(settlement.CheckRuns))
			words := make(map[string]string, len(settlement.CheckRuns))
			for _, run := range settlement.CheckRuns {
				var word string
				if err := json.Unmarshal(run.Conclusion, &word); err != nil || word == "" {
					t.Fatalf("the fixture gives the run of %s the word %s, want a word on every run", run.Name, run.Conclusion)
				}
				words[string(run.Name)] = word
				// The entry as the fixture writes it, its word byte for byte, as the listener does.
				entries = append(entries, map[string]any{"name": string(run.Name), "id": run.ID, "conclusion": run.Conclusion})
			}
			payload := settlementPayload(nil, failed, cancelled)
			payload["check_runs"], payload["skipped"] = entries, checkGroup(skipped)
			decoded := decodeSettlement(t, payload)
			checks := settledChecks(t, decoded)
			if len(decoded.Unread) != 0 || !reflect.DeepEqual(checks.Failing, failed) || !reflect.DeepEqual(checks.Cancelled, cancelled) {
				t.Fatalf("the fixture's settlement decoded failing %v, cancelled %v, reported %q; want its %v and %v and nothing unread",
					checks.Failing, checks.Cancelled, decoded.Unread, failed, cancelled)
			}
			if len(checks.CheckRuns) != len(entries) {
				t.Fatalf("the fixture's settlement decoded %d runs, want its %d", len(checks.CheckRuns), len(entries))
			}
			for _, run := range checks.CheckRuns {
				want := record.AttemptRun{Name: run.Name, ID: run.ID, GitHubConclusion: words[run.Name]}
				if slices.Contains(skipped, run.Name) {
					want.Conclusion = record.RunSkipped
				}
				if run != want {
					t.Fatalf("the run of %s decoded to %+v, want %+v", run.Name, run, want)
				}
			}

			switch file {
			case "held-back-suites-skip.json":
				var concludedSkipped []string
				for _, run := range checks.CheckRuns {
					if words[run.Name] == "skipped" {
						concludedSkipped = append(concludedSkipped, run.Name)
					}
					wantConclusion := record.RunConclusion("")
					if words[run.Name] == "skipped" {
						wantConclusion = record.RunSkipped
					}
					if run.Conclusion != wantConclusion || run.GitHubConclusion != words[run.Name] {
						t.Fatalf("the run of %s, which GitHub concluded %s, decoded to %+v, want the conclusion %q and its word",
							run.Name, words[run.Name], run, wantConclusion)
					}
				}
				if len(concludedSkipped) != 28 || !reflect.DeepEqual(concludedSkipped, slices.Sorted(slices.Values(skipped))) {
					t.Fatalf("the runs GitHub concluded skipped are %v, want the 28 the skipped group names, %v", concludedSkipped, skipped)
				}
			case "listener-rules.json":
				for _, want := range []record.AttemptRun{
					{Name: "docs", ID: 117, Conclusion: record.RunSkipped, GitHubConclusion: "skipped"},
					{Name: "advice", ID: 118, GitHubConclusion: "neutral"},
					{Name: "future", ID: 115, GitHubConclusion: "a_conclusion_github_adds_later"},
				} {
					if got := runNamed(t, checks.CheckRuns, want.Name); got != want {
						t.Fatalf("the run of %s decoded to %+v, want %+v", want.Name, got, want)
					}
				}
			}
		})
	}

	// Each of GitHub's words as the listener passes it on, one it may add later, and the longest a
	// word may be, each kept on its run.
	kept := []struct{ name, word string }{
		{"lint", "success"}, {"advice", "neutral"}, {"docs", "skipped"}, {"image", "cancelled"}, {"unit", "failure"},
		{"timeout", "timed_out"}, {"approval", "action_required"}, {"boot", "startup_failure"}, {"old", "stale"},
		{"future", "a_conclusion_github_adds_later"}, {"longest", strings.Repeat("a", 32)},
	}
	failed := []string{"approval", "boot", "future", "old", "timeout", "unit"}
	cancelled := []string{"image"}
	wordedRuns := func() ([]map[string]any, []record.AttemptRun) {
		entries := make([]map[string]any, 0, len(kept))
		runs := make([]record.AttemptRun, 0, len(kept))
		for i, run := range kept {
			id := int64(100 + i)
			entries = append(entries, map[string]any{"name": run.name, "id": id, "conclusion": run.word})
			runs = append(runs, record.AttemptRun{Name: run.name, ID: id, GitHubConclusion: run.word})
		}
		return entries, runs
	}

	t.Run("each word, beside what is no word", func(t *testing.T) {
		entries, want := wordedRuns()
		// What is no word, each dropped from its run and named in the unread list.
		dropped := []struct {
			name string
			word any
		}{
			{"numbered", 7}, {"overlong", strings.Repeat("a", 33)}, {"object", map[string]any{"word": "success"}},
			{"capitals", "SUCCESS"}, {"spaced", "timed out"}, {"empty", ""},
		}
		for i, run := range dropped {
			id := int64(200 + i)
			entries = append(entries, map[string]any{"name": run.name, "id": id, "conclusion": run.word})
			want = append(want, record.AttemptRun{Name: run.name, ID: id})
		}
		// No word: none named, and none written as null.
		entries = append(entries, map[string]any{"name": "unworded", "id": 300}, map[string]any{"name": "nulled", "id": 301, "conclusion": nil})
		want = append(want, record.AttemptRun{Name: "unworded", ID: 300}, record.AttemptRun{Name: "nulled", ID: 301})

		payload := settlementPayload(nil, failed, cancelled)
		payload["check_runs"] = entries
		// A skipped group whose count and list disagree, read as no group.
		payload["skipped"] = map[string]any{"count": 2, "checks": []string{"docs"}}
		decoded := decodeSettlement(t, payload)
		checks := settledChecks(t, decoded)
		if !reflect.DeepEqual(checks.CheckRuns, byName(want)) || !reflect.DeepEqual(checks.Failing, failed) || !reflect.DeepEqual(checks.Cancelled, cancelled) {
			t.Fatalf("decoded runs %+v, failing %v, cancelled %v; want every run, %+v, each word kept and none other, and %v and %v",
				checks.CheckRuns, checks.Failing, checks.Cancelled, byName(want), failed, cancelled)
		}
		wantUnread := make([]string, 0, len(dropped)+1)
		for _, run := range dropped {
			wantUnread = append(wantUnread, "check_runs["+run.name+"].conclusion ")
		}
		wantUnread = append(wantUnread, "skipped ")
		if len(decoded.Unread) != len(wantUnread) {
			t.Fatalf("reported %q, want one line for each of %q", decoded.Unread, wantUnread)
		}
		for i, prefix := range wantUnread {
			if !strings.HasPrefix(decoded.Unread[i], prefix) {
				t.Fatalf("unread line %d is %q, want it to name %q", i, decoded.Unread[i], prefix)
			}
		}
		if line := decoded.Unread[len(decoded.Unread)-1]; !strings.Contains(line, statusesHead) {
			t.Fatalf("the skipped group's line %q names no head, want %s", line, statusesHead)
		}
		if strings.Contains(strings.Join(decoded.Unread, "\n"), strings.Repeat("a", 33)) {
			t.Fatalf("reported %q, want an overlong word counted rather than quoted", decoded.Unread)
		}
	})

	for _, tc := range []struct {
		name  string
		group any
		// skipped is the name the group makes skipped, "" for a group read as none.
		skipped string
	}{
		{"a group naming docs and a check with no run", checkGroup([]string{"docs", "ghost"}), "docs"},
		{"a null group", nil, ""},
		{"an empty group", checkGroup(nil), ""},
		{"a group with an empty name", checkGroup([]string{"docs", ""}), ""},
		{"a group with a negative count", map[string]any{"count": -1, "checks": []string{}}, ""},
		{"a group that is a list", []string{"docs"}, ""},
		{"a group that is a string", "docs", ""},
		{"a group whose names are no strings", map[string]any{"count": 1, "checks": []int{7}}, ""},
	} {
		t.Run("skipped: "+tc.name, func(t *testing.T) {
			entries, want := wordedRuns()
			for i := range want {
				if want[i].Name == tc.skipped {
					want[i].Conclusion = record.RunSkipped
				}
			}
			payload := settlementPayload(nil, failed, cancelled)
			payload["check_runs"], payload["skipped"] = entries, tc.group
			decoded := decodeSettlement(t, payload)
			if got := settledChecks(t, decoded); !reflect.DeepEqual(got.CheckRuns, byName(want)) {
				t.Fatalf("decoded runs %+v, want %+v", got.CheckRuns, byName(want))
			}
			// A group read whole, a null one among them, is nothing unread; any other is named.
			read := tc.skipped != "" || tc.group == nil || reflect.DeepEqual(tc.group, checkGroup(nil))
			if read && len(decoded.Unread) != 0 {
				t.Fatalf("reported %q, want nothing unread", decoded.Unread)
			}
			if !read && (len(decoded.Unread) != 1 || !strings.HasPrefix(decoded.Unread[0], "skipped ")) {
				t.Fatalf("reported %q, want the skipped group named once", decoded.Unread)
			}
		})
	}
}

// A listener of an earlier release names no word on a run and, at its oldest, no skipped group;
// another names the group empty or null. Each decodes as it did before the daemon read either:
// every run with its name and id alone, no conclusion and no word, the failing and cancelled
// names as given, and nothing unread.
func TestDecodeChecksReadsASettlementWithNoSkippedGroupOrConclusionWordAsToday(t *testing.T) {
	t.Parallel()
	runs := []record.AttemptRun{{Name: "unit", ID: 12}, {Name: "build", ID: 11}, {Name: "docs", ID: 13}}
	want := PullRequestChecks{Repo: "sjawhar/legion", Number: 42, HeadSHA: statusesHead, CheckRuns: byName(runs), Generation: 3,
		Snapshot: "snapshot-3", Failing: []string{"unit"}, Cancelled: []string{}}
	for _, tc := range []struct {
		name  string
		group any
	}{
		{"no skipped group", nil},
		{"a null skipped group", json.RawMessage("null")},
		{"an empty skipped group", checkGroup(nil)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := settlementPayload(runs, []string{"unit"}, []string{})
			delete(payload, "skipped")
			if tc.group != nil {
				payload["skipped"] = tc.group
			}
			for _, entry := range payload["check_runs"].([]map[string]any) {
				if _, worded := entry["conclusion"]; worded {
					t.Fatalf("the old listener's entry %v names a word", entry)
				}
			}
			decoded := decodeSettlement(t, payload)
			if got := settledChecks(t, decoded); !reflect.DeepEqual(got, want) || len(decoded.Unread) != 0 {
				t.Fatalf("decoded %#v, reported %q; want %#v and nothing unread", got, decoded.Unread, want)
			}
		})
	}
}
