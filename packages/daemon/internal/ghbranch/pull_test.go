package ghbranch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
	"github.com/sjawhar/legion/daemon/internal/leakguard"
	"github.com/sjawhar/legion/daemon/internal/leakguard/leakguardtest"
)

// OpenPullRequest opens a pull request of a branch against a base and answers its number; a second
// open of the same branch, which GitHub refuses with 422 because a pull request of it is open,
// answers the number of the one that is open, and opens none.
func TestOpenPullRequestAnswersTheOneThatIsOpen(t *testing.T) {
	g := githubtest.GitRepository(t, "acme", "widgets", "main")
	api, _ := serveRepo(t, g)
	repo := ghrepo.MustParse("acme/widgets")
	g.Commit(t, "legion/ACME-1.e", map[string]string{"e.txt": "e\n"}, "e")
	ctx := context.Background()

	first, err := OpenPullRequest(ctx, widgets(api), repo, "legion/ACME-1.e", "main", "ACME-1: e", "Dispatch: ACME-1", nil)
	if err != nil || first != 1 {
		t.Fatalf("OpenPullRequest = %d, %v; want pull request 1", first, err)
	}
	second, err := OpenPullRequest(ctx, widgets(api), repo, "legion/ACME-1.e", "main", "ACME-1: e", "Dispatch: ACME-1", nil)
	if err != nil || second != first {
		t.Fatalf("OpenPullRequest again = %d, %v; want the open one, %d", second, err, first)
	}
	if pulls := g.PullRequests(); len(pulls) != 1 || pulls[0].Head != "legion/ACME-1.e" || pulls[0].Base != "main" ||
		pulls[0].Title != "ACME-1: e" || pulls[0].Body != "Dispatch: ACME-1" {
		t.Fatalf("the pull requests GitHub holds = %+v, want the one of legion/ACME-1.e against main", pulls)
	}
}

// Every read that has only a branch takes each open pull request on it
// (dispatch://INFRA-5910/spec#b-d1: "A push, which carries a branch and no number, applies to each
// open pull request on that branch, and so does each read that has only a branch"). This is
// ghbranch's part of the inventory of those reads, row b11 of the plan's decision 20; the
// workflow's (b1 to b5), the daemon's (b6 to b9) and the API's (b10) hold a test of this name in
// their own packages. OpenPullRequest reads the open pull requests of its head by the head alone
// once GitHub refuses the open with 422, "A pull request already exists", which GitHub answers
// for a head that has an open pull request onto the base asked for. legion/ACME-1 holds two open
// pull requests: #1 onto main and #2, opened after it, onto legion/ACME-1-ci-base, as
// INFRA-1276's branch in the deployment repository held #3654 onto main and the draft #3955 onto
// legion/INFRA-1276-ci-base. Both are read, and neither takes the place of the other: an open onto
// main answers #1, and an open onto legion/ACME-1-ci-base answers #2, the open pull request on
// that head whose base is the base the open asked for, in whichever order GitHub lists them:
// newest first, its order when the list names none, and oldest first.
//
// The fault it catches is the fallback taking the first open pull request GitHub lists on the head
// whatever its base (githubrest.Client.OpenPullRequestOn): listed newest first, the open onto main
// answers #2, the draft onto the other base, and its caller records that as the pull request it
// opened.
func TestEachReadByBranchTakesEveryOpenPullRequestOnIt(t *testing.T) {
	t.Parallel()
	const head, otherBase = "legion/ACME-1", "legion/ACME-1-ci-base"
	pull := func(number int, base string) string {
		return fmt.Sprintf(`{"number":%d,"state":"open","base":{"ref":%q},"head":{"ref":%q,"sha":"e","repo":{"full_name":"acme/widgets"}}}`, number, base, head)
	}
	for _, order := range []struct {
		name   string
		listed []string
	}{
		{"newest first", []string{pull(2, otherBase), pull(1, "main")}},
		{"oldest first", []string{pull(1, "main"), pull(2, otherBase)}},
	} {
		t.Run("b11 the open pull request of the head onto the base asked for, listed "+order.name, func(t *testing.T) {
			api := githubtest.Server(t, githubtest.Spec{Token: "installation-token", Handle: func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/widgets/pulls":
					w.WriteHeader(http.StatusUnprocessableEntity)
					fmt.Fprintf(w, `{"message":"Validation Failed","errors":[{"resource":"PullRequest","code":"custom","message":"A pull request already exists for acme:%s."}]}`, head)
				case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/pulls":
					if query := r.URL.Query(); query.Get("head") != "acme:"+head || query.Get("state") != "open" {
						t.Errorf("listed pull requests by %q, want the open ones on acme:%s", r.URL.RawQuery, head)
					}
					fmt.Fprint(w, "["+strings.Join(order.listed, ",")+"]")
				default:
					githubtest.Unexpected(t, w, r)
				}
			}})
			for _, ask := range []struct {
				base string
				want int
			}{{"main", 1}, {otherBase, 2}} {
				got, err := OpenPullRequest(context.Background(), widgets(api), ghrepo.MustParse("acme/widgets"), head, ask.base, "ACME-1: e", "Dispatch: ACME-1", nil)
				if err != nil || got != ask.want {
					t.Errorf("OpenPullRequest of %s onto %s = %d, %v; want #%d, the open pull request of %s onto %s: GitHub lists #1 onto main and #2 onto %s %s",
						head, ask.base, got, err, ask.want, head, ask.base, otherBase, order.name)
				}
			}
		})
	}
}

// A pull request's title and body are published on GitHub as a created ref's name is, so a text
// the rules the caller hands OpenPullRequest match is refused before anything is sent: with the
// fixture's rules (leakguardtest), a body naming the fixture's invented host asks GitHub for
// nothing, and the refusal shows the body the rule matched.
func TestOpenPullRequestRefusesATextTheRulesMatch(t *testing.T) {
	rules, err := leakguard.LoadRules([]byte(leakguardtest.Rules))
	if err != nil {
		t.Fatalf("compile the fixture rules: %v", err)
	}
	g := githubtest.GitRepository(t, "acme", "widgets", "main")
	api, paths := serveRepo(t, g)
	g.Commit(t, "legion/ACME-1.e", map[string]string{"e.txt": "e\n"}, "e")
	body := "Built on " + leakguardtest.RulesHost

	_, err = OpenPullRequest(context.Background(), widgets(api), ghrepo.MustParse("acme/widgets"), "legion/ACME-1.e", "main", "ACME-1: e", body, rules)

	var refused *leakguard.Refusal
	if !errors.As(err, &refused) {
		t.Fatalf("OpenPullRequest with a body naming the fixture's host = %v, want a leak guard refusal", err)
	}
	if want := []string{leakguardtest.ReportLine("the pull request's body", leakguardtest.RulesHostClass, leakguardtest.RulesHost, 1, body)}; !slices.Equal(refused.Lines, want) {
		t.Errorf("the report is %q, want %q", refused.Lines, want)
	}
	if sent := paths(); len(sent) != 0 {
		t.Errorf("GitHub was asked for %v, want nothing", sent)
	}
}

// implementLogin is the implement App's bot login, the login a close by Legion names.
const implementLogin = "legion-implementer[bot]"

// mergedComment is the comment each close of these tests writes.
const mergedComment = "Merged in #9."

// pullComment is one comment on the pull request: its author's login and its body.
type pullComment struct {
	author, body string
}

// refusal is an answer the fake gives a call in place of its own: the status, the body and the
// headers, given for the first times calls, or for every call where times is 0.
type refusal struct {
	status int
	body   string
	header map[string]string
	times  int
}

// closingPull is a GitHub of one pull request, #7 of acme/widgets, as a close reads and writes it:
// its state, whether it merged, the login GitHub names as its closer (nil for a closed_by of null),
// its comments with their authors, and the refusals it is told to give, keyed by method and path.
// A close it takes names implementLogin its closer, and a comment it takes has implementLogin its
// author, as GitHub names the App whose token made each write. It records each call.
type closingPull struct {
	mu       sync.Mutex
	state    string
	merged   bool
	closedBy *string
	comments []pullComment
	refuse   map[string]*refusal
	calls    []string
}

// serve answers the pull request's calls under acme/widgets's REST base, and fails the test on any
// other.
func (g *closingPull) serve(t *testing.T) githubrest.Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		path := strings.TrimPrefix(r.URL.Path, "/repos/acme/widgets")
		call := r.Method + " " + path
		g.calls = append(g.calls, call)
		if refused := g.refuse[call]; refused != nil && refused.times >= 0 {
			if refused.times == 1 {
				refused.times = -1
			} else if refused.times > 1 {
				refused.times--
			}
			for name, value := range refused.header {
				w.Header().Set(name, value)
			}
			w.WriteHeader(refused.status)
			_, _ = fmt.Fprint(w, refused.body)
			return
		}
		switch call {
		case "GET /pulls/7":
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 7, "state": g.state, "merged": g.merged})
		case "PATCH /pulls/7":
			var body struct {
				State string `json:"state"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.State != "closed" {
				t.Errorf("PATCH /pulls/7 with %+v, %v; want the state closed", body, err)
			}
			g.state, g.closedBy = "closed", new(implementLogin)
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 7, "state": g.state})
		case "GET /issues/7":
			issue := map[string]any{"number": 7, "state": g.state, "closed_by": nil}
			if g.closedBy != nil {
				issue["closed_by"] = map[string]string{"login": *g.closedBy}
			}
			_ = json.NewEncoder(w).Encode(issue)
		case "GET /issues/7/comments":
			listed := []map[string]any{}
			for i, comment := range g.comments {
				listed = append(listed, map[string]any{"id": i + 1, "body": comment.body, "user": map[string]string{"login": comment.author}})
			}
			_ = json.NewEncoder(w).Encode(listed)
		case "POST /issues/7/comments":
			var body struct {
				Body string `json:"body"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("POST /issues/7/comments: %v", err)
			}
			g.comments = append(g.comments, pullComment{author: implementLogin, body: body.Body})
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": len(g.comments)})
		default:
			t.Errorf("unexpected %s", call)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return widgets(server.URL)
}

// sent is each call the fake answered, in order.
func (g *closingPull) sent() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.calls)
}

// written is each comment the pull request holds, in order.
func (g *closingPull) written() []pullComment {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.comments)
}

// closeSeven closes #7 with mergedComment as the implement App.
func closeSeven(github githubrest.Client) (Closed, error) {
	return ClosePullRequest(context.Background(), github, ghrepo.MustParse("acme/widgets"), 7, mergedComment, implementLogin, nil)
}

// A pull request a person closed before the row ran is left as that person left it: the close
// reads who closed it, finds another login than the implement App's, and sends no close, reads no
// comment and writes none, answering that login.
//
// The fault it catches is Legion's comment, which says Legion closed the pull request, written on
// one a person closed (its control: the comment step reached with no read of the closer, under
// which the comment is written).
func TestAPersonsCloseBeforeTheRowGetsNoComment(t *testing.T) {
	t.Parallel()
	pull := &closingPull{state: "closed", closedBy: new("a-person")}
	closed, err := closeSeven(pull.serve(t))
	if err != nil || closed != (Closed{By: "a-person"}) {
		t.Errorf("ClosePullRequest of #7, closed by a-person = %+v, %v; want Closed{By: a-person}", closed, err)
	}
	if want := []string{"GET /pulls/7", "GET /issues/7"}; !slices.Equal(pull.sent(), want) {
		t.Errorf("GitHub was sent %q, want %q", pull.sent(), want)
	}
	if comments := pull.written(); len(comments) != 0 {
		t.Fatalf("#7 holds the comments %+v, want none", comments)
	}
}

// A try that closed the pull request and lost its comment to GitHub's 502 errs, and the next try
// of the row writes the comment once: it sends no second close, reads the closer, the implement
// App, reads the comments, finds none of the App's, and writes it. The pull request is then closed
// with exactly one comment, the App's, of that body.
//
// The fault it catches is a close whose comment a retry never writes (its control: the retry
// skipping the comment where the pull request is closed already, under which it holds none).
func TestAnEarlierTrysCloseWithNoCommentGetsOne(t *testing.T) {
	t.Parallel()
	pull := &closingPull{state: "open", refuse: map[string]*refusal{"POST /issues/7/comments": {status: http.StatusBadGateway, body: "bad gateway", times: 1}}}
	github := pull.serve(t)
	if closed, err := closeSeven(github); err == nil || !closed.Now {
		t.Fatalf("the first close of #7, its comment answered 502 = %+v, %v; want the close made and an error", closed, err)
	}
	closed, err := closeSeven(github)
	if err != nil || closed != (Closed{By: implementLogin}) {
		t.Fatalf("the second close of #7 = %+v, %v; want Closed{By: %s}", closed, err, implementLogin)
	}
	if patches := slices.DeleteFunc(pull.sent(), func(call string) bool { return call != "PATCH /pulls/7" }); len(patches) != 1 {
		t.Fatalf("GitHub was sent %d closes, want one", len(patches))
	}
	if want := []pullComment{{author: implementLogin, body: mergedComment}}; pull.state != "closed" || !slices.Equal(pull.written(), want) {
		t.Fatalf("#7 is %s with the comments %+v, want closed with %+v", pull.state, pull.written(), want)
	}
}

// A closer GitHub does not name gets no comment on a guess. The pull request is closed, and its
// read answers 502: the close errs with GitHub's answer, a failure that can pass, so the row runs
// again. Its read answers a closed_by of null: the close answers why it names no closer, and no
// error, since a try again reads the same. Its read answers 404: the same, naming GitHub's answer.
//
// The faults it catches are a comment written on a pull request whose closer no read named (its
// control: a closer that cannot be read taken as the App, under which the comment is written in the
// case of null), and a refusal GitHub would repeat retried for ever (its control: a refused read of
// the closer taken as a failure that can pass, under which the close errs in the case of 404).
func TestAnUnreadableClosedByWritesNoComment(t *testing.T) {
	t.Parallel()
	notFound := `{"message":"Not Found"}`
	for _, tc := range []struct {
		name    string
		refusal *refusal
		want    Closed
		failure string
		canPass bool
	}{
		{name: "the read answers 502", refusal: &refusal{status: http.StatusBadGateway, body: "bad gateway"}, failure: "with 502", canPass: true},
		{name: "closed_by is null", want: Closed{Unread: ClosedByNull}},
		{name: "the read answers 404", refusal: &refusal{status: http.StatusNotFound, body: notFound},
			want: Closed{Unread: "GitHub answered GET /issues/7 with 404: " + notFound}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pull := &closingPull{state: "closed", refuse: map[string]*refusal{}}
			if tc.refusal != nil {
				pull.refuse["GET /issues/7"] = tc.refusal
			}
			closed, err := closeSeven(pull.serve(t))
			if tc.failure != "" {
				if err == nil || !strings.Contains(err.Error(), tc.failure) || RefusedForGood(err) != !tc.canPass {
					t.Fatalf("ClosePullRequest of #7 = %+v, %v; want an error %q that can pass", closed, err, tc.failure)
				}
			} else if err != nil || closed != tc.want {
				t.Errorf("ClosePullRequest of #7 = %+v, %v; want %+v and no error", closed, err, tc.want)
			}
			if comments := pull.written(); len(comments) != 0 {
				t.Fatalf("#7 holds the comments %+v, want none", comments)
			}
		})
	}
}

// A close GitHub refuses for good writes no comment: the close of an open pull request answers 403
// with no rate-limit header, so the call errs, refused for good, and the pull request stays open
// with no comment.
//
// The fault it catches is a comment that says Legion closed a pull request that stays open (its
// control: the comment written before the close, under which #7 holds one).
func TestACloseRefusedForGoodWritesNoComment(t *testing.T) {
	t.Parallel()
	pull := &closingPull{state: "open", refuse: map[string]*refusal{"PATCH /pulls/7": {status: http.StatusForbidden, body: `{"message":"Resource not accessible by integration"}`}}}
	closed, err := closeSeven(pull.serve(t))
	var answer *githubrest.Answer
	if err == nil || !errors.As(err, &answer) || answer.Status != http.StatusForbidden || !RefusedForGood(err) || closed != (Closed{}) {
		t.Fatalf("ClosePullRequest of #7, its close answered 403 = %+v, %v; want GitHub's 403, refused for good", closed, err)
	}
	if comments := pull.written(); pull.state != "open" || len(comments) != 0 {
		t.Fatalf("#7 is %s with the comments %+v, want open with none", pull.state, comments)
	}
}

// A close whose both writes passed already gets nothing more: #7 is closed by the App and holds the
// App's comment of that body, so the call sends no close and no comment and answers the App as its
// closer. Where the only comment of that body is another author's, the App's comment is written
// once. A merged pull request gets no close and no comment.
//
// The faults it catches are a second comment on a retry (its control: the comment written with no
// read of the comments, under which #7 holds two of the App's), and a comment held back by another
// author's of the same words (its control: a read that matches the body alone, under which the App
// writes none).
func TestBothPassedAlreadyGetsNothing(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		pull     *closingPull
		want     Closed
		comments []pullComment
	}{
		{name: "closed by the App with its comment", pull: &closingPull{state: "closed", closedBy: new(implementLogin),
			comments: []pullComment{{author: implementLogin, body: mergedComment}}},
			want: Closed{By: implementLogin}, comments: []pullComment{{author: implementLogin, body: mergedComment}}},
		{name: "closed by the App, its words written by another author", pull: &closingPull{state: "closed", closedBy: new(implementLogin),
			comments: []pullComment{{author: "a-person", body: mergedComment}}},
			want: Closed{By: implementLogin}, comments: []pullComment{{author: "a-person", body: mergedComment}, {author: implementLogin, body: mergedComment}}},
		{name: "merged", pull: &closingPull{state: "closed", merged: true}, want: Closed{Merged: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pull := tc.pull
			closed, err := closeSeven(pull.serve(t))
			if err != nil || closed != tc.want {
				t.Fatalf("ClosePullRequest of #7 = %+v, %v; want %+v", closed, err, tc.want)
			}
			if patches := slices.DeleteFunc(pull.sent(), func(call string) bool { return call != "PATCH /pulls/7" }); len(patches) != 0 {
				t.Fatalf("GitHub was sent %d closes, want none", len(patches))
			}
			if !slices.Equal(pull.written(), tc.comments) {
				t.Fatalf("#7 holds the comments %+v, want %+v", pull.written(), tc.comments)
			}
		})
	}
}
