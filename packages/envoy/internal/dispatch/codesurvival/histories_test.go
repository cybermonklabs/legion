package codesurvival

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/codesurvival/codesurvivaltest"
)

// widgetsProject is the project whose default repository the run's tests measure, widgets, and
// whose issues the histories' pull requests name.
const widgetsProject = "ACME"

// d0 is the first day of every history the run's tests build, a UTC midnight.
var d0 = time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)

// day is the UTC midnight n days after d0.
func day(n int) time.Time { return d0.AddDate(0, 0, n) }

// writer builds a history's branch: the lines of each file of its working tree, which it writes
// whole on each commit, and the next pull request's number. Each line names the history's tag,
// its day, its change and its place, so every line of every history is its own and at least 40
// bytes long.
type writer struct {
	h     *codesurvivaltest.History
	tag   string
	files map[string][]string
	pr    int
	// tips is the last commit of each day the writer committed on, by day.
	tips map[int]string
	// states is the files at the end of each day, by day.
	states map[int]map[string][]string
}

func newWriter(t testing.TB) *writer {
	return &writer{h: codesurvivaltest.NewHistory(t), tag: "a", files: map[string][]string{}, pr: 1, tips: map[int]string{}, states: map[int]map[string][]string{}}
}

// add appends n lines to path, each its own.
func (w *writer) add(path string, n int, at time.Time, what string) {
	for i := range n {
		w.files[path] = append(w.files[path], fmt.Sprintf("%s %s %s line %d of %s, kept as written", w.tag, at.Format("2006-01-02T15:04"), what, i, path))
	}
	w.write(path)
}

// drop removes the first n lines of path, the oldest ones.
func (w *writer) drop(path string, n int) {
	n = min(n, len(w.files[path]))
	w.files[path] = w.files[path][n:]
	w.write(path)
}

// dropLast removes the last n lines of path, the newest ones.
func (w *writer) dropLast(path string, n int) {
	n = min(n, len(w.files[path]))
	w.files[path] = w.files[path][:len(w.files[path])-n]
	w.write(path)
}

func (w *writer) write(path string) {
	w.h.Repo.Write(path, strings.Join(w.files[path], "\n")+"\n")
}

// pull is the next pull request: the agents' (head branch agent/<n>, opened by agent-app[bot]) for
// an even number and another author's (feature/<n>) for an odd one, naming the issue ACME-<1..3>
// in its title, except every fourth, which names none.
func (w *writer) pull() codesurvivaltest.Pull {
	number := w.pr
	w.pr++
	pull := codesurvivaltest.Pull{Number: number, HeadRef: fmt.Sprintf("feature/%d", number), Author: "someone", Title: fmt.Sprintf("Change %d", number)}
	if number%2 == 0 {
		pull.HeadRef, pull.Author = fmt.Sprintf("agent/%d", number), "agent-app[bot]"
	}
	if number%4 != 0 {
		pull.Title = fmt.Sprintf("%s-%d: change %d", widgetsProject, number%3+1, number)
	}
	return pull
}

// change lands one pull request at at: six lines added to one file and, on every third, the two
// oldest lines of another file removed.
func (w *writer) change(n, j int, at time.Time) string {
	path := fmt.Sprintf("src/file%d.txt", (n+j)%5)
	w.add(path, 6, at, fmt.Sprintf("change %d.%d", n, j))
	if (n+j)%3 == 1 {
		if other := fmt.Sprintf("src/file%d.txt", (n+j+2)%5); len(w.files[other]) >= 4 {
			w.drop(other, 2)
		}
	}
	return w.h.Land(w.pull(), at)
}

// days lands count(n) changes on each day n of [from, to), from 10:00, five minutes apart, and the
// extras each day's hook adds before them.
func (w *writer) days(from, to int, count func(n int) int, hook func(n int)) {
	for n := from; n < to; n++ {
		if hook != nil {
			hook(n)
		}
		for j := range count(n) {
			w.tips[n] = w.change(n, j, day(n).Add(10*time.Hour+time.Duration(j)*5*time.Minute))
		}
		w.states[n] = cloneFiles(w.files)
	}
}

// restart makes the branch main the commit that ended day n of a history whose last commit of each
// day is tips and whose files at the end of each day are states, its files as they were then, for
// a force-push that writes the days after it again under tag.
func (w *writer) restart(n int, tag string, tips map[int]string, states map[int]map[string][]string) {
	w.h.Reset(tips[n])
	w.files = cloneFiles(states[n])
	w.tag = tag
}

// sixty is the histories of the 60-day branch (k2, k3, k7) and its force-pushes, built once for the
// tests that read them, which share the branches as the double serves them.
type sixty struct {
	// a is the branch: one change a day on each of its 60 days, eight on forcedDay, a merge
	// commit of a side branch of two commits on mergeDay, and a commit that merged no pull
	// request on each of days 2 and 6.
	a *codesurvivaltest.Branch
	// b is a force-pushed: forcedDay's eight commits replaced by five that remove lines of earlier
	// days, and every day after it written again.
	b *codesurvivaltest.Branch
	// c is a with only the commit of tailDay, a day after the measured days, and the days after
	// it written again.
	c *codesurvivaltest.Branch
}

// The days of sixty: the merge, the day of eight commits, and the day c rewrites.
const (
	mergeDay  = 5
	forcedDay = 34
	tailDay   = 50
)

var sixtyOnce struct {
	once sync.Once
	set  *sixty
}

// sixtyDays is sixty, built on the first call.
func sixtyDays(t *testing.T) *sixty {
	t.Helper()
	sixtyOnce.once.Do(func() { sixtyOnce.set = buildSixty(t) })
	if sixtyOnce.set == nil {
		t.Fatal("the 60-day histories were not built: see the test that built them")
	}
	return sixtyOnce.set
}

func buildSixty(t *testing.T) *sixty {
	w := newWriter(t)
	count := func(n int) int {
		if n == forcedDay {
			return 8
		}
		return 1
	}
	hook := func(n int) {
		switch n {
		case 2:
			w.add("NOTES.md", 2, day(n).Add(6*time.Hour), "note")
			w.tips[n] = w.h.Push("notes", day(n).Add(6*time.Hour))
		case 6:
			w.drop("src/file4.txt", 1)
			w.tips[n] = w.h.Push("tidy a file", day(n).Add(6*time.Hour))
		}
	}
	w.days(0, mergeDay, count, hook)
	// The merge commit: a side branch of two commits merged with --no-ff, which GitHub's compare
	// route serves from its first parent.
	w.h.Repo.Branch("side")
	w.add("src/side.txt", 4, day(mergeDay).Add(8*time.Hour), "side one")
	w.h.Push("side one", day(mergeDay).Add(8*time.Hour))
	w.add("src/side.txt", 4, day(mergeDay).Add(9*time.Hour), "side two")
	w.h.Push("side two", day(mergeDay).Add(9*time.Hour))
	w.h.Repo.Checkout(codesurvivaltest.BranchName)
	pull := w.pull()
	pull.HeadRef = "side"
	w.tips[mergeDay] = w.h.Merge("side", pull, day(mergeDay).Add(10*time.Hour))
	w.states[mergeDay] = cloneFiles(w.files)
	w.days(mergeDay+1, 60, count, hook)
	set := &sixty{a: w.h.Branch()}
	tips, states := maps.Clone(w.tips), maps.Clone(w.states)

	w.restart(forcedDay-1, "b", tips, states)
	w.days(forcedDay, 60, func(n int) int {
		if n == forcedDay {
			return 0
		}
		return 1
	}, func(n int) {
		if n != forcedDay {
			return
		}
		for j := range 5 {
			at := day(n).Add(11*time.Hour + time.Duration(j)*7*time.Minute)
			path := fmt.Sprintf("src/file%d.txt", j)
			w.drop(path, 3)
			w.add(path, 2, at, fmt.Sprintf("rewrite %d", j))
			w.tips[n] = w.h.Land(w.pull(), at)
		}
	})
	set.b = w.h.Branch()

	w.restart(tailDay-1, "c", tips, states)
	w.days(tailDay, 60, count, nil)
	set.c = w.h.Branch()
	return set
}

func cloneFiles(files map[string][]string) map[string][]string {
	clone := make(map[string][]string, len(files))
	for path, lines := range files {
		clone[path] = slices.Clone(lines)
	}
	return clone
}

// The moments the k3 runs pass at: the first, when exactly days 0 to 27 wait (day 27 waits from
// day 60); one day later; six days after that, when days 28 to 34 wait; and seven more days
// later, when days 35 to 41 wait.
var (
	firstPass = day(60).Add(time.Hour)
	nextDay   = day(61).Add(time.Hour)
	weekLater = day(67).Add(time.Hour)
	twoWeeks  = day(74).Add(time.Hour)
)

// widgetsRig is a rig whose double serves branch as widgets, with widgetsProject's issues and the
// project itself, its default repository widgets, under testGroups.
func widgetsRig(t *testing.T, branch *codesurvivaltest.Branch) *rig {
	t.Helper()
	r := newRig(t)
	r.github.Serve(widgets, branch)
	r.project(widgetsProject, widgets, testGroups)
	for number := 1; number <= 3; number++ {
		r.issue(widgetsProject, number, "")
	}
	return r
}

// commitsOn is the SHA of every commit of branch's chain dated on a day of [from, to), in order.
func commitsOn(branch *codesurvivaltest.Branch, from, to time.Time) []string {
	var shas []string
	for _, commit := range branch.Chain {
		if !commit.CommittedAt.Before(from) && commit.CommittedAt.Before(to) {
			shas = append(shas, commit.SHA)
		}
	}
	return shas
}
