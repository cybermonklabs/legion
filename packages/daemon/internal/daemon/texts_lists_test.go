package daemon

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/texts"
)

// The check c-daemon-texts-are-facts, third test, at this head: each list of a daemon text names
// whole what its reader acts on and counts the rest. The samples are the golden file's
// (goldenSamples), read through the fields they state (goldenFieldsOf).
//
//   - A catch-up, of a root's architect and of a child's own architect (cybermonklabs/legion#4203)
//     alike, over a tree of 60 issues: the 20 recorded issues that are not done with their fields
//     and the 5 children not yet released whole, which are the 25 that are not done; the 35 done
//     ones by key alone; and the 3 waiting in accepting for this architect's acceptance, in the
//     catch-up's accepting list. The record has no reason (texts.CatchUpRecord): the accepting
//     list is where main's reason named them.
//   - The children-first notice names all 30 open children.
//   - A tick names its held and blocked lists and its 30 silent workers whole.
//   - The CI settlement, the review-stuck reason of a head GitHub will not merge, and the
//     settlement wake of a role the settlement woke (workflow.SettlementWake) each name the 25
//     checks that failed or were cancelled, each by GitHub's word for its run where the settlement
//     carried one, name the 5 that GitHub skipped in a list of their own under the key that says
//     a skipped check ran nothing (dispatch://INFRA-4135's R1), and name `gh pr checks` with the
//     pull request and its repository; the settlement and the wake name the phase. The head also
//     carries 30 commit statuses (cybermonklabs/legion#4299), 3 of them in state failure or
//     error: each text names the 3 with their states and counts the other 27 with the 30 checks
//     that passed, 57 in all. The head has no awaiting-children notice (#4017): that part lands
//     with it.
//   - An update round ask, and an update-stalled notice of that round, name the image by a short
//     digest and each of the 30 settings by its key, with no value.
func TestEveryListNamesWhatNeedsAction(t *testing.T) {
	samples := goldenSamples(t)
	fieldsOf := func(kind texts.Kind, variant string, n int) goldenFields {
		t.Helper()
		for i := range samples {
			s := &samples[i]
			if s.Kind == kind && s.Variant == variant && s.Sample == n {
				entry, _ := texts.Lookup(kind)
				fields, problems := goldenFieldsOf(s, entry)
				for _, problem := range problems {
					t.Errorf("%s: %s", s.id(), problem)
				}
				return fields
			}
		}
		t.Fatalf("no sample %s/%s#%d", kind, variant, n)
		return goldenFields{}
	}
	list := func(values map[string]string, key string) []string {
		n, _ := strconv.Atoi(values[key+"#"])
		items := make([]string, n)
		for i := range n {
			items[i] = values[fmt.Sprintf("%s[%d]", key, i)]
		}
		return items
	}
	keysOf := func(values map[string]string, key, field string) []string {
		n, _ := strconv.Atoi(values[key+"#"])
		keys := make([]string, n)
		for i := range n {
			keys[i] = values[fmt.Sprintf("%s[%d].%s", key, i, field)]
		}
		return keys
	}

	for _, r := range goldenRenderings {
		for _, variant := range []string{"root", "child"} {
			tree := goldenTreeOf(r, variant)
			values := fieldsOf(texts.CatchUp, variant, r.n).values
			issues := keysOf(values, "catch_up.issues", "key")
			if !slices.Equal(issues, tree.notDone) {
				t.Errorf("%s catch-up #%d names the issues %v, want the %d not done %v", variant, r.n, issues, len(tree.notDone), tree.notDone)
			}
			titles := map[string]string{}
			for _, member := range tree.members {
				titles[member.Key] = member.Title
			}
			for i, key := range issues {
				for _, field := range []string{"title", "phase", "status"} {
					if values[fmt.Sprintf("catch_up.issues[%d].%s", i, field)] == "" {
						t.Errorf("%s catch-up #%d names %s with no %s", variant, r.n, key, field)
					}
				}
				if got := values[fmt.Sprintf("catch_up.issues[%d].title", i)]; got != titles[key] {
					t.Errorf("%s catch-up #%d names %s with the title %q, want its whole title", variant, r.n, key, goldenCut(got))
				}
			}
			unreleased := keysOf(values, "catch_up.unreleased", "key")
			if !slices.Equal(unreleased, tree.unreleased) {
				t.Errorf("%s catch-up #%d names the children not released %v, want %v", variant, r.n, unreleased, tree.unreleased)
			}
			for i := range unreleased {
				if values[fmt.Sprintf("catch_up.unreleased[%d].parent", i)] == "" || values[fmt.Sprintf("catch_up.unreleased[%d].status", i)] == "" {
					t.Errorf("%s catch-up #%d names the child %s not whole", variant, r.n, unreleased[i])
				}
			}
			if n := len(issues) + len(unreleased); n != 25 {
				t.Errorf("%s catch-up #%d names %d issues that are not done, want 25", variant, r.n, n)
			}
			done := list(values, "catch_up.done")
			if !slices.Equal(done, tree.done) || len(done) != 35 {
				t.Errorf("%s catch-up #%d names the done issues %v, want the 35 %v by key", variant, r.n, done, tree.done)
			}
			// A done issue is named by its key alone: no field of the catch-up is a field of an
			// item of its done list.
			for path := range values {
				if strings.HasPrefix(path, "catch_up.done[") && strings.Contains(path, "].") {
					t.Errorf("%s catch-up #%d names a done issue with a field, %s, want each by its key alone", variant, r.n, path)
				}
			}
			if accepting := list(values, "catch_up.accepting"); !slices.Equal(accepting, tree.accepting) || len(accepting) != 3 {
				t.Errorf("%s catch-up #%d names %v waiting for acceptance, want the 3 %v", variant, r.n, accepting, tree.accepting)
			}
		}

		wait := goldenChildWait(r)
		var open []string
		for _, child := range wait.Open {
			open = append(open, child.Key)
		}
		values := fieldsOf(texts.ChildrenFirst, "open-children", r.n).values
		if got := keysOf(values, "children", "key"); !slices.Equal(got, open) || len(got) != 30 {
			t.Errorf("children-first #%d names %v, want all 30 open children %v", r.n, got, open)
		}

		held, blocked, silent := goldenTickLists(r)
		values = fieldsOf(texts.Tick, "tick", r.n).values
		goldenWhole(t, fmt.Sprintf("tick #%d held", r.n), values, "held", held, func(i int, h record.HeldIssue) map[string]string {
			return map[string]string{"issue": h.Issue, "reason": string(h.Reason), "heldBy": strings.Join(h.HeldBy, ",")}
		})
		goldenWhole(t, fmt.Sprintf("tick #%d blocked", r.n), values, "blocked", blocked, func(i int, b record.BlockedIssue) map[string]string {
			return map[string]string{"issue": b.Issue, "blockedBy": strings.Join(b.BlockedBy, ",")}
		})
		goldenWhole(t, fmt.Sprintf("tick #%d silent", r.n), values, "silent", silent, func(i int, s record.SilentIssue) map[string]string {
			return map[string]string{"issue": s.Issue, "role": string(s.Role), "silent": s.Silent}
		})
		if len(blocked) != 173 || len(silent) != 30 {
			t.Fatalf("the tick's lists hold %d blocked and %d silent, want 173 and 30", len(blocked), len(silent))
		}

		for _, settled := range []struct {
			kind       texts.Kind
			variant    string
			label      string
			number     int
			phase      bool
			conclusion bool
		}{
			{texts.CISettled, "settled", "settled", 1000 + r.n, true, true},
			{texts.ReviewStuck, "merge-state", "stuck", 800 + r.n, false, true},
			{texts.PodWake, "checks", "wake", 500 + r.n, true, true},
		} {
			checks := goldenChecks(r, settled.label)
			values := fieldsOf(settled.kind, settled.variant, r.n).values
			named := keysOf(values, "not_passed", "check")
			if !slices.Equal(named, checks.notPassed) || len(named) != 25 {
				t.Errorf("%s #%d names the checks %v that did not pass, want the 25 %v", settled.kind, r.n, named, checks.notPassed)
			}
			for i, name := range named {
				want := checks.conclusionOf[name]
				if got := values[fmt.Sprintf("not_passed[%d].conclusion", i)]; got != want {
					t.Errorf("%s #%d names %q %s, want %s", settled.kind, r.n, name, got, want)
				}
			}
			statuses := keysOf(values, "not_passed_statuses", "context")
			if !slices.Equal(statuses, checks.notPassedStatuses) || len(statuses) != 3 {
				t.Errorf("%s #%d names the commit statuses %v that did not pass, want the 3 %v", settled.kind, r.n, statuses, checks.notPassedStatuses)
			}
			for i, context := range statuses {
				want := string(checks.stateOf[context])
				if got := values[fmt.Sprintf("not_passed_statuses[%d].state", i)]; got != want {
					t.Errorf("%s #%d names the commit status %q %s, want %s", settled.kind, r.n, context, got, want)
				}
			}
			if skipped := list(values, "skipped"); !slices.Equal(skipped, checks.skipped) || len(skipped) != 5 {
				t.Errorf("%s #%d names the checks %v skipped, want the 5 %v", settled.kind, r.n, skipped, checks.skipped)
			}
			if values["each_skipped_check"] != texts.RanNothing {
				t.Errorf("%s #%d says of each skipped check %q, want %q once beside the list", settled.kind, r.n, values["each_skipped_check"], texts.RanNothing)
			}
			if values["passed"] != "57" || checks.passed != 30 || checks.statusesPassed != 27 {
				t.Errorf("%s #%d counts %s that passed, want the 30 checks and the 27 commit statuses, 57", settled.kind, r.n, values["passed"])
			}
			if want := fmt.Sprintf("gh pr checks %d --repo %s", settled.number, r.repo); values["list_passed"] != want {
				t.Errorf("%s #%d lists them with %q, want %q", settled.kind, r.n, values["list_passed"], want)
			}
			if settled.phase && values["phase"] == "" {
				t.Errorf("%s #%d names no phase", settled.kind, r.n)
			}
		}

		drift := goldenDrift(r)
		var settings []string
		for _, item := range drift[1:] {
			settings = append(settings, item.Key)
		}
		for _, round := range []struct {
			kind    texts.Kind
			variant string
			prefix  string
		}{{texts.UpdateAsk, "drift", ""}, {texts.UpdateStalled, "drift", "update."}} {
			values := fieldsOf(round.kind, round.variant, r.n).values
			images := list(values, round.prefix+"images")
			if len(images) != 1 || images[0] != texts.ShortDigest(drift[0].Image) || len(images[0]) != 12 {
				t.Errorf("%s #%d names the images %v, want the new image by its short digest %s", round.kind, r.n, images, texts.ShortDigest(drift[0].Image))
			}
			if got := list(values, round.prefix+"settings"); !slices.Equal(got, settings) || len(got) != 30 {
				t.Errorf("%s #%d names the settings %v, want the 30 keys %v", round.kind, r.n, got, settings)
			}
			for path, value := range values {
				if strings.Contains(value, drift[0].Image) {
					t.Errorf("%s #%d names the image's whole reference at %s", round.kind, r.n, path)
				}
			}
		}
	}
}

// goldenWhole fails unless values names list at key whole: every item, each field of it as want
// gives it.
func goldenWhole[T any](t *testing.T, what string, values map[string]string, key string, list []T, want func(int, T) map[string]string) {
	t.Helper()
	if got := values[key+"#"]; got != strconv.Itoa(len(list)) {
		t.Errorf("%s names %s items, want all %d", what, got, len(list))
		return
	}
	for i, item := range list {
		for field, value := range want(i, item) {
			path := fmt.Sprintf("%s[%d].%s", key, i, field)
			got, ok := values[path]
			if field == "heldBy" || field == "blockedBy" {
				got = strings.Join(goldenListAt(values, path), ",")
				ok = true
			}
			if !ok || got != value {
				t.Errorf("%s: %s = %q, want %q", what, path, got, value)
			}
		}
	}
}

// goldenListAt is the list values holds at path.
func goldenListAt(values map[string]string, path string) []string {
	n, _ := strconv.Atoi(values[path+"#"])
	items := make([]string, n)
	for i := range n {
		items[i] = values[fmt.Sprintf("%s[%d]", path, i)]
	}
	return items
}
