package replay

//ci:reads docs/site/src/content/docs/legion/replay.md TestEverySignalOfR11HasOneDefinition reads replayPage.

import (
	"maps"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// t30 replaces this empty test declaration with the registry in signals.go. At and Next
// must refer to the implementation functions, not to names standing in for those functions.
var signalDefinitions []struct {
	Name      string
	At, Next  any
	EventOnly bool
	Inputs    []string
	Source    string
}

func TestEverySignalOfR11HasOneDefinition(t *testing.T) {
	inventory := []struct {
		name      string
		inputs    []string
		source    string
		eventOnly bool
	}{
		{"idle", []string{"log.jsonl"}, "replay", false},
		{"queue", []string{"queue/<owner>__<repo>.jsonl"}, "merge queue model", false},
		{"provider", []string{"signals/provider.jsonl"}, "tokens model", false},
		{"kills", []string{"launcher/log.jsonl", "signals/memory.jsonl"}, "memory model", false},
		{"approvals", []string{"events/<KEY>.jsonl", "settings/events.jsonl", "window.json#defaultAssignee"}, "replay", true},
	}
	page, err := os.ReadFile(replayPage)
	if err != nil {
		t.Error(err)
	}
	visible := visibleText(string(page))
	isFunc := func(fn any) bool {
		v := reflect.ValueOf(fn)
		return v.Kind() == reflect.Func && !v.IsNil()
	}
	byName := map[string]int{}
	for _, def := range signalDefinitions {
		byName[def.Name]++
	}
	for _, want := range inventory {
		t.Run(want.name, func(t *testing.T) {
			count := byName[want.name]
			if count > 1 {
				t.Errorf("%s has %d definitions, want one", want.name, count)
			}
			var at, next, eventOnly bool
			var inputs []string
			var source string
			for _, def := range signalDefinitions {
				if def.Name == want.name {
					at, next, eventOnly = isFunc(def.At), isFunc(def.Next), def.EventOnly
					inputs, source = def.Inputs, def.Source
					break
				}
			}
			if !at {
				t.Errorf("no definition of %s at a moment", want.name)
			}
			if want.eventOnly {
				if !eventOnly || next {
					t.Errorf("%s must declare no Next crossing because it changes only at events", want.name)
				}
			} else if !next || eventOnly {
				t.Errorf("%s has no Next crossing function", want.name)
			}
			for _, input := range want.inputs {
				if !slices.Contains(inputs, input) {
					t.Errorf("%s does not name window input %s", want.name, input)
				}
			}
			if source != want.source {
				t.Errorf("%s names source %q, want %q", want.name, source, want.source)
			}
			heading := regexp.MustCompile("(?mi)^#{2,6} Signal `?" + want.name + "`?[ \\t]*$")
			matches := heading.FindAllStringIndex(visible, -1)
			if len(matches) != 1 {
				t.Errorf("%s has %d visible Signal %s sections, want one", replayPage, len(matches), want.name)
			} else if body := strings.TrimSpace(visible[matches[0][1]:]); body == "" || strings.HasPrefix(body, "#") {
				t.Errorf("%s has an empty Signal %s section", replayPage, want.name)
			}
		})
		delete(byName, want.name)
	}
	for _, name := range slices.Sorted(maps.Keys(byName)) {
		t.Errorf("signal %q is not in the R11 inventory", name)
	}
}
