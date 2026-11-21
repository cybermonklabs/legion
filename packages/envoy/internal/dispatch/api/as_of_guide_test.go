package api

import (
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/asof"
)

// asOfGuideList is the paragraph of cmd/dispatch/README.md ("Reads at a past time") that opens the
// list of the members each route of kind answers can leave out at a cut.
const asOfGuideList = "**The members an answer can leave out at a cut**"

// The guide's list of the members that an answer at a cut can leave out names, for each route of
// kind answers (asOfAnswers), exactly the members asof.AbsentFields holds for that route, and no
// route of another kind. An operator or a client author reads that list to know what an answer at
// a cut may lack, so a member the table gained, lost or moved to another route, and a route that
// joined or left the routes that answer as_of, would make the guide wrong with nothing failing.
//
// Each item of the list names one or more routes in code spans that start with GET, then the
// members in code spans that start with /, or none, and the list ends at the next paragraph that
// is not a list item.
func TestTheGuideNamesTheMembersEachAnswersRouteCanLeaveOut(t *testing.T) {
	guide, err := os.ReadFile(filepath.Join("..", "..", "..", "cmd", "dispatch", "README.md"))
	if err != nil {
		t.Fatalf("read the guide: %v", err)
	}
	listed := asOfGuideMembers(t, string(guide))
	var answers []string
	for name, entry := range readClasses {
		if entry.AsOf == asOfAnswers {
			answers = append(answers, name)
		}
	}
	slices.Sort(answers)
	if got := slices.Sorted(maps.Keys(listed)); !slices.Equal(got, answers) {
		t.Errorf("the guide's list names the routes\n %v\nand the routes that answer as_of are\n %v", got, answers)
	}
	for _, route := range answers {
		var want []string
		for _, field := range asof.AbsentFields[route] {
			want = append(want, field.Pointer)
		}
		slices.Sort(want)
		got := slices.Sorted(slices.Values(listed[route]))
		if !slices.Equal(got, want) {
			t.Errorf("the guide names for %s the members\n %v\nand asof.AbsentFields holds\n %v", route, got, want)
		}
	}
}

// asOfGuideMembers reads the guide's list (asOfGuideList): each route it names, with the members
// its item names. A route named twice fails the test.
func asOfGuideMembers(t *testing.T, guide string) map[string][]string {
	t.Helper()
	_, list, found := strings.Cut(guide, asOfGuideList)
	if !found {
		t.Fatalf("the guide holds no paragraph that starts %s", asOfGuideList)
	}
	codeSpan := regexp.MustCompile("`([^`]+)`")
	listed := map[string][]string{}
	var item []string
	flush := func() {
		var routes, members []string
		for _, text := range item {
			for _, span := range codeSpan.FindAllStringSubmatch(text, -1) {
				switch {
				case strings.HasPrefix(span[1], "GET "):
					routes = append(routes, span[1])
				case strings.HasPrefix(span[1], "/"):
					members = append(members, span[1])
				}
			}
		}
		for _, route := range routes {
			if _, twice := listed[route]; twice {
				t.Errorf("the guide's list names %s twice", route)
			}
			listed[route] = members
		}
		item = nil
	}
	started := false
	for _, line := range strings.Split(list, "\n")[1:] {
		switch {
		case strings.HasPrefix(line, "- "):
			flush()
			started = true
			item = append(item, line)
		case started && strings.HasPrefix(line, "  "):
			item = append(item, line)
		case strings.TrimSpace(line) == "":
		case started:
			flush()
			return listed
		}
	}
	flush()
	return listed
}
