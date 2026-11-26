package ghbranch

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
	"github.com/sjawhar/legion/daemon/internal/leakguard"
	"github.com/sjawhar/legion/daemon/internal/leakguard/leakguardtest"
)

// serveRepo is a fake GitHub over g's real repository, which records the path of each request and
// fails the test on any request g does not answer.
func serveRepo(t *testing.T, g *githubtest.GitRepo) (api string, paths func() []string) {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	api = githubtest.Server(t, githubtest.Spec{
		Record: func(r *http.Request) {
			mu.Lock()
			defer mu.Unlock()
			seen = append(seen, r.Method+" "+r.URL.Path)
		},
		Handle: func(w http.ResponseWriter, r *http.Request) {
			if !g.Handler(w, r) {
				githubtest.Unexpected(t, w, r)
			}
		},
	})
	return api, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(seen)
	}
}

// Merge reads GitHub's three answers to a merge of one branch into another: a merge commit (201)
// is a join, a head the base holds already (204) is nothing to join, and a conflict (409) is no
// error but its own answer, with the base left where it was. A head GitHub does not have is an
// error that names it.
func TestMergeReadsTheThreeAnswers(t *testing.T) {
	g := githubtest.GitRepository(t, "acme", "widgets", "main")
	api, _ := serveRepo(t, g)
	repo := ghrepo.MustParse("acme/widgets")
	ctx := context.Background()
	g.Commit(t, "a", map[string]string{"a.txt": "a\n", "x.txt": "from a\n"}, "a")
	g.Commit(t, "c", map[string]string{"c.txt": "c\n"}, "c")
	g.Commit(t, "x", map[string]string{"x.txt": "from x\n"}, "x")

	if got, err := Merge(ctx, widgets(api), repo, "a", "c", "ACME-1: join the work of task c", nil); err != nil || got != MergeJoined {
		t.Fatalf("Merge(c into a) = %v, %v; want MergeJoined", got, err)
	}
	joined := g.Head(t, "a")
	if got, err := Merge(ctx, widgets(api), repo, "a", "c", "ACME-1: join the work of task c", nil); err != nil || got != MergeNothing {
		t.Fatalf("Merge(c into a) again = %v, %v; want MergeNothing", got, err)
	}
	if got, err := Merge(ctx, widgets(api), repo, "a", "x", "ACME-1: join the work of task x", nil); err != nil || got != MergeConflict {
		t.Fatalf("Merge(x into a) = %v, %v; want MergeConflict and no error", got, err)
	}
	if head := g.Head(t, "a"); head != joined {
		t.Fatalf("a stands at %s after a merge of nothing and a conflict, want %s", head, joined)
	}
	if _, err := Merge(ctx, widgets(api), repo, "a", "legion/ACME-1.gone", "ACME-1: join the work of task gone", nil); err == nil || !strings.Contains(err.Error(), "legion/ACME-1.gone") {
		t.Fatalf("Merge of a head GitHub lacks = %v, want an error naming it", err)
	}
}

// A merge's commit message is published on GitHub as the ref name of a create is, so a message
// the rules the caller hands Merge match is refused before anything is sent: with the fixture's
// rules (leakguardtest), a message naming the fixture's invented host asks GitHub for nothing, and
// the refusal shows the message the rule matched.
func TestMergeRefusesAMessageTheRulesMatch(t *testing.T) {
	rules, err := leakguard.LoadRules([]byte(leakguardtest.Rules))
	if err != nil {
		t.Fatalf("compile the fixture rules: %v", err)
	}
	g := githubtest.GitRepository(t, "acme", "widgets", "main")
	api, paths := serveRepo(t, g)
	g.Commit(t, "c", map[string]string{"c.txt": "c\n"}, "c")
	message := "join the work on " + leakguardtest.RulesHost

	_, err = Merge(context.Background(), widgets(api), ghrepo.MustParse("acme/widgets"), "main", "c", message, rules)

	var refused *leakguard.Refusal
	if !errors.As(err, &refused) {
		t.Fatalf("Merge with a message naming the fixture's host = %v, want a leak guard refusal", err)
	}
	if want := []string{leakguardtest.ReportLine("the merge's commit message", leakguardtest.RulesHostClass, leakguardtest.RulesHost, 1, message)}; !slices.Equal(refused.Lines, want) {
		t.Errorf("the report is %q, want %q", refused.Lines, want)
	}
	if sent := paths(); len(sent) != 0 {
		t.Errorf("GitHub was asked for %v, want nothing", sent)
	}
}
