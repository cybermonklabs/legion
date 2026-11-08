package ghautomerge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/wait"
)

// github is GitHub's GraphQL API for acme/widgets#42 as Disarm and Submit read and change it: what
// its read cannot find ("pullRequest", "repository", or empty when it finds the pull request), the
// pull request's state, head and merge state, whether its base requires a merge queue, whether the
// repository allows auto-merge, whether its auto-merge is enabled and whether it has a merge-queue
// entry, that entry's state and the commit the queue built it on (queueState, queueBase: QUEUED at
// the base branch's tip when empty), the branch the pull request merges into and that branch's tip
// (baseName, baseTip: baseBranch at baseTip when empty), its timeline of arms and removals as
// GraphQL's viewer (viewer, the implement App's bot when empty) reads it, and every mutation sent.
// withdrawing says the test drives Disarm, the one path in this tree that takes an entry out of
// the merge queue: the fake fails a dequeuePullRequest from anywhere else, since a submission
// that dequeued an entry would rebuild every entry queued behind it.
// requeue is GitHub enqueueing again a pull request it dequeues while its auto-merge is still
// enabled; failDisable answers the disable with an error, stuckArm answers it as done while
// leaving auto-merge enabled, refuseSubmit answers an enable or merge with GitHub's refusal,
// refuseType is that GraphQL error's type, movedOnRefuse is the head a push moves the pull request
// to as GitHub refuses it, and submitStatus answers an enable or merge with that HTTP status
// instead. updated is the pull request's updatedAt as its JSON value, GitHub's last change to it:
// updatedAt by default, `null` for an answer that carries none.
// limitAfterRefusal answers every read after a refused enable or merge with GitHub's RATE_LIMITED
// and a Retry-After of 180 seconds. timelineReads counts the timeline's reads. contextPages is each
// further page of the merge queue commit's checks by the cursor it follows, pageReads counts their
// reads, and failPages answers every one with GitHub's server error.
// jobLogs is the log of each Actions job by its id, as GET /actions/jobs/{id}/logs answers it,
// and logReads counts the job logs read: what one such log says failed, and each way a read of
// one can end, is read of jobFailures itself (failingtests_test.go).
type github struct {
	mu                             sync.Mutex
	notFound                       string
	state                          string
	head                           string
	mergeState                     string
	updated                        string
	queueRequired, autoMergeOK     bool
	armed, queued                  bool
	queueState, queueBase          string
	baseName, baseTip              string
	withdrawing                    bool
	requeue, failDisable, stuckArm bool
	refuseSubmit, refuseType       string
	movedOnRefuse                  string
	submitStatus                   int
	limitAfterRefusal, refused     bool
	viewer                         string
	timeline                       []string
	timelineReads, pageReads       int
	contextPages                   map[string]string
	failPages                      bool
	jobLogs                        map[int64]string
	logReads                       int
	mutations                      []string
}

// updatedAt is the fake pull request's default last change on GitHub, baseBranch and baseOid are
// the branch it merges into and the commit that branch is at, and absent is how the fake records
// an input the mutation it serves does not pass. queueBuiltAhead is the baseCommit an entry
// behind another carries: the commit GitHub's merge queue built for the entry ahead, whose
// parent is the base branch's tip, and which is no pull request's head — the fixture is the one
// measured 2026-11-08 00:43:57Z on cybermonklabs/legion, where entry 2 (#2859) carried
// b66b78d6d9f3, the queue's build for entry 1 (#2234, head 7c4463b8ac8b) on main's tip
// 1f46e1cc76f4, and no pull request had b66b78d6d9f3 as its head.
const (
	updatedAt       = "2026-10-16T05:00:00Z"
	baseBranch      = "main"
	baseOid         = "a2679f2e"
	queueBuiltAhead = "b66b78d6d9f3"
	absent          = "<absent>"
)

// submitted is the enable or merge the fake records for #42 at head. Both inputs of the squash
// commit's message are absent: the mutations pass neither, so each repository's own squash
// settings decide the message (why is on the enable and merge mutations in ghautomerge.go).
func submitted(verb string) string {
	return fmt.Sprintf("%s PR_42 head SQUASH headline=%q body=%q", verb, absent, absent)
}

// operationHeader is the start of a GraphQL document's operation signature: the operation's name
// and the "(" that opens the variables it declares. variableName is every $name a document names,
// in that signature or in the selection set after it. fieldArguments is one innermost argument
// list a field passes, and word is every word of a selection set, each field it selects among
// them once the argument lists are gone.
var (
	operationHeader = regexp.MustCompile(`^\s*(?:query|mutation)\s+(\w+)\s*\(`)
	variableName    = regexp.MustCompile(`\$\w+`)
	fieldArguments  = regexp.MustCompile(`\([^()]*\)`)
	word            = regexp.MustCompile(`\w+`)
)

// operationBody takes document apart: the operation's name ("the operation" where it names none),
// the variables its signature declares, and the selection set after that signature.
func operationBody(document string) (string, []string, string) {
	header := operationHeader.FindStringSubmatchIndex(document)
	if header == nil {
		return "the operation", nil, document
	}
	name := document[header[2]:header[3]]
	depth := 0
	for i := header[1] - 1; i < len(document); i++ {
		switch document[i] {
		case '(':
			depth++
		case ')':
			depth--
		}
		if depth == 0 {
			return name, variableName.FindAllString(document[header[1]:i], -1), document[i+1:]
		}
	}
	return name, nil, document
}

// refusedVariables is GitHub's validation message for the first variable document declares and
// never uses, or uses and never declares, and "" when every variable it names is both. GraphQL's
// validator refuses either document before GitHub reads an input of it (its "All Variables Used"
// and "All Variable Uses Defined" rules, sections 5.8.4 and 5.8.3), so a mutation that loses an
// input while its declaration stands merges nothing.
func refusedVariables(document string) string {
	name, declared, body := operationBody(document)
	used := variableName.FindAllString(body, -1)
	for _, variable := range declared {
		if !slices.Contains(used, variable) {
			return fmt.Sprintf("Variable %s is declared by %s but not used", variable, name)
		}
	}
	for _, variable := range used {
		if !slices.Contains(declared, variable) {
			return fmt.Sprintf("Variable %s is not defined by %s", variable, name)
		}
	}
	return ""
}

// selectedFields is every field name document's selection set names: its words, without the
// operation's signature and without the arguments its fields pass.
func selectedFields(document string) map[string]bool {
	_, _, body := operationBody(document)
	for {
		stripped := fieldArguments.ReplaceAllString(body, "")
		if stripped == body {
			break
		}
		body = stripped
	}
	fields := make(map[string]bool)
	for _, field := range word.FindAllString(body, -1) {
		fields[field] = true
	}
	return fields
}

// unselectedFields names every field answer carries that document does not select, and "" where
// it carries none. GitHub answers the fields a document asks for and no others, so an answer the
// fake writes by hand with a field the document never selected proves nothing about what the
// package decodes of GitHub: it is the fake's answer and the document drifting apart.
func unselectedFields(document, answer string) string {
	var carried struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal([]byte(answer), &carried); err != nil {
		return fmt.Sprintf("it is no JSON at all: %v", err)
	}
	selected := selectedFields(document)
	var unselected []string
	var walk func(value any)
	walk = func(value any) {
		switch value := value.(type) {
		case map[string]any:
			for field, under := range value {
				if !selected[field] {
					unselected = append(unselected, field)
				}
				walk(under)
			}
		case []any:
			for _, under := range value {
				walk(under)
			}
		}
	}
	walk(carried.Data)
	if len(unselected) == 0 {
		return ""
	}
	slices.Sort(unselected)
	name, _, _ := operationBody(document)
	return fmt.Sprintf("it carries %s, which %s does not select", strings.Join(slices.Compact(unselected), ", "), name)
}

// serveJobLog answers the log of the Actions job job names ("{id}/logs") as GitHub does: the
// log the fake holds for that job, and GitHub's 404 for a job it holds none for, as a check run
// no Actions job backs would answer. What a log says failed, and each way a read of one can end
// without a log, are read of jobFailures itself (failingtests_test.go).
func (g *github) serveJobLog(t *testing.T, w http.ResponseWriter, r *http.Request, job string) {
	t.Helper()
	id, err := strconv.ParseInt(strings.TrimSuffix(job, "/logs"), 10, 64)
	log, held := g.jobLogs[id]
	switch {
	case r.Method != http.MethodGet || !strings.HasSuffix(job, "/logs") || err != nil:
		t.Errorf("unexpected job log request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusBadRequest)
	case !held:
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	default:
		_, _ = io.WriteString(w, log)
	}
}

func (g *github) serve(t *testing.T) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		// The log of a failing job is the one read this package makes outside GraphQL: GET
		// /actions/jobs/{id}/logs under the repository's REST base (githubrest.RepositoryAPI).
		if job, found := strings.CutPrefix(r.URL.Path, jobLogPath); found {
			g.logReads++
			g.serveJobLog(t, w, r, job)
			return
		}
		var request struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if r.Method != http.MethodPost || r.URL.Path != "/graphql" || json.NewDecoder(r.Body).Decode(&request) != nil {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		// GitHub validates the document before it reads anything of it, so a document whose
		// variables and inputs disagree is an answer with errors and no change at all.
		if message := refusedVariables(request.Query); message != "" {
			_, _ = fmt.Fprintf(w, `{"data":null,"errors":[{"message":%q,"type":"VALIDATION"}]}`, message)
			return
		}
		switch {
		case strings.Contains(request.Query, "disablePullRequestAutoMerge"):
			g.mutations = append(g.mutations, fmt.Sprintf("disable %v", request.Variables["id"]))
			if g.failDisable {
				_, _ = w.Write([]byte(`{"data":null,"errors":[{"type":"FORBIDDEN","message":"Resource not accessible by integration"}]}`))
				return
			}
			g.armed = g.armed && g.stuckArm
			_, _ = w.Write([]byte(`{"data":{"disablePullRequestAutoMerge":{"clientMutationId":null}}}`))
		case strings.Contains(request.Query, "dequeuePullRequest"):
			if !g.withdrawing {
				t.Errorf("dequeue of %v, want none: the withdrawal's Disarm is the one path that takes an entry out of the merge queue", request.Variables["id"])
			}
			g.mutations = append(g.mutations, fmt.Sprintf("dequeue %v", request.Variables["id"]))
			g.queued = g.requeue && g.armed
			_, _ = w.Write([]byte(`{"data":{"dequeuePullRequest":{"clientMutationId":null}}}`))
		case strings.Contains(request.Query, "enablePullRequestAutoMerge"), strings.Contains(request.Query, "mergePullRequest"):
			verb := "merge"
			if strings.Contains(request.Query, "enablePullRequestAutoMerge") {
				verb = "enable"
			}
			// GitHub reads only what the mutation's input passes, so a field the mutation leaves out
			// is recorded as absent rather than as the value of the variable it never reads.
			passed := func(field, variable string) any {
				if !strings.Contains(request.Query, field+": $"+variable) {
					return absent
				}
				return request.Variables[variable]
			}
			g.mutations = append(g.mutations, fmt.Sprintf("%s %v %v %v headline=%q body=%q", verb, request.Variables["id"], request.Variables["head"], request.Variables["method"],
				passed("commitHeadline", "headline"), passed("commitBody", "body")))
			if g.submitStatus != 0 {
				http.Error(w, `{"message":"Server Error"}`, g.submitStatus)
				return
			}
			if g.refuseSubmit != "" {
				if g.movedOnRefuse != "" {
					g.head = g.movedOnRefuse
				}
				kind := g.refuseType
				if kind == "" {
					kind = "UNPROCESSABLE"
				}
				g.refused = true
				_, _ = fmt.Fprintf(w, `{"data":null,"errors":[{"type":%q,"message":%q}]}`, kind, g.refuseSubmit)
				return
			}
			if verb == "enable" {
				g.armed = true
			} else {
				g.state = "MERGED"
			}
			_, _ = w.Write([]byte(`{"data":{}}`))
		case strings.Contains(request.Query, "MergeGroupContexts"):
			g.pageReads++
			if g.failPages {
				http.Error(w, `{"message":"Server Error"}`, http.StatusBadGateway)
				return
			}
			page, ok := g.contextPages[fmt.Sprint(request.Variables["after"])]
			if !ok || request.Variables["oid"] != "group-sha" {
				t.Errorf("checks page read of %v, want a page this fake lists", request.Variables)
			}
			_, _ = fmt.Fprintf(w, `{"data":{"repository":{"object":{"oid":"group-sha","statusCheckRollup":{"contexts":%s}}}}}`, page)
		case strings.Contains(request.Query, "timelineItems"):
			g.timelineReads++
			if request.Variables["owner"] != "acme" || request.Variables["name"] != "widgets" || request.Variables["number"] != float64(42) {
				t.Errorf("timeline read of %v, want acme/widgets#42", request.Variables)
			}
			viewer := g.viewer
			if viewer == "" {
				viewer = "legion-implementer[bot]"
			}
			_, _ = fmt.Fprintf(w, `{"data":{"viewer":{"login":%q},"repository":{"pullRequest":{"timelineItems":{"nodes":[%s]}}}}}`, viewer, strings.Join(g.timeline, ","))
		case g.notFound == "pullRequest":
			// GitHub's own answer for a pull request number the repository does not have.
			_, _ = w.Write([]byte(`{"data":{"repository":{"pullRequest":null}},"errors":[{"type":"NOT_FOUND","path":["repository","pullRequest"],"message":"Could not resolve to a PullRequest with the number of 42."}]}`))
		case g.limitAfterRefusal && g.refused:
			w.Header().Set("Retry-After", "180")
			_, _ = w.Write([]byte(`{"data":null,"errors":[{"type":"RATE_LIMITED","message":"API rate limit exceeded"}]}`))
		case g.notFound == "repository":
			// GitHub's answer for a repository the token cannot see.
			_, _ = w.Write([]byte(`{"data":{"repository":null},"errors":[{"type":"NOT_FOUND","path":["repository"],"message":"Could not resolve to a Repository with the name 'acme/widgets'."}]}`))
		default:
			if request.Variables["owner"] != "acme" || request.Variables["name"] != "widgets" || request.Variables["number"] != float64(42) {
				t.Errorf("read of %v, want acme/widgets#42", request.Variables)
			}
			autoMerge, entry := "null", "null"
			if g.armed {
				autoMerge = `{"enabledAt":"2026-10-11T12:00:00Z"}`
			}
			if g.queued {
				queueState, queueBase := g.queueState, g.queueBase
				if queueState == "" {
					queueState = "QUEUED"
				}
				if queueBase == "" {
					queueBase = baseOid
				}
				entry = fmt.Sprintf(`{"enqueuedAt":"2026-10-11T12:05:00Z","state":%q,"baseCommit":{"oid":%q}}`, queueState, queueBase)
			}
			state, mergeState, updated := g.state, g.mergeState, g.updated
			if state == "" {
				state = "OPEN"
			}
			if mergeState == "" {
				mergeState = "BLOCKED"
			}
			if updated == "" {
				updated = strconv.Quote(updatedAt)
			}
			baseName, baseTip := g.baseName, g.baseTip
			if baseName == "" {
				baseName = baseBranch
			}
			if baseTip == "" {
				baseTip = baseOid
			}
			answer := fmt.Sprintf(`{"data":{"repository":{"autoMergeAllowed":%t,"pullRequest":{"id":"PR_42","state":%q,"headRefOid":%q,"mergeStateStatus":%q,"updatedAt":%s,"isMergeQueueEnabled":%t,"autoMergeRequest":%s,"mergeQueueEntry":%s,"baseRef":{"name":%q,"target":{"oid":%q}}}}}}`,
				g.autoMergeOK, state, g.head, mergeState, updated, g.queueRequired, autoMerge, entry, baseName, baseTip)
			// GitHub answers the fields a document selects and no others, so a field this
			// hand-written answer carries that read never selected is a decode no GitHub would
			// ever feed: the answer and the document have drifted apart.
			if drifted := unselectedFields(read, answer); drifted != "" {
				t.Errorf("the fake's answer to the read is one GitHub would never give: %s", drifted)
			}
			_, _ = w.Write([]byte(answer))
		}
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// clients are the two the merge path calls the fake with: GitHub's GraphQL route, which the pull
// request is read and changed on and which deciding a submission is the whole of, and the REST
// base of acme/widgets, which a failing job's log is read under once a removal is decided
// (ReadFailingTests). One server answers both, as GitHub's one API root does. The daemon builds
// the second where it reads, from the first's own token (its readyMerge).
func clients(t *testing.T, g *github) (client, logs githubrest.Client) {
	t.Helper()
	root := g.serve(t)
	return githubrest.Client{Token: "token", API: githubrest.GraphQLAPI(root)},
		githubrest.Client{Token: "token", API: githubrest.RepositoryAPI(root, ghrepo.MustParse("acme/widgets"))}
}

// The fake answers a document that declares a variable it never uses, or uses one it never
// declares, as GitHub's validator does: an error, and no mutation made. Every document this
// package sends passes that check, so each case of this file proves its document valid too.
func TestTheFakeRefusesAGraphQLDocumentGitHubsValidatorWouldRefuse(t *testing.T) {
	for _, document := range []string{read, enable, merge, disable, dequeue, removals, moreChecks} {
		if message := refusedVariables(document); message != "" {
			t.Errorf("a document this package sends is refused: %s", message)
		}
	}
	for _, tc := range []struct{ name, document string }{
		{name: "a variable declared and never used", document: `mutation EnableAutoMerge($id: ID!, $headline: String!) {
  enablePullRequestAutoMerge(input: {pullRequestId: $id}) { clientMutationId }
}`},
		{name: "a variable used and never declared", document: `mutation EnableAutoMerge($id: ID!) {
  enablePullRequestAutoMerge(input: {pullRequestId: $id, commitHeadline: $headline}) { clientMutationId }
}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := &github{}
			client := githubrest.Client{Token: "token", API: githubrest.GraphQLAPI(g.serve(t))}
			err := client.GraphQL(context.Background(), tc.document, map[string]any{"id": "PR_42", "headline": "a headline"}, nil)
			var refusal *githubrest.GraphQLError
			if !errors.As(err, &refusal) {
				t.Fatalf("the fake answered %v, want GitHub's validation error", err)
			}
			g.mu.Lock()
			defer g.mu.Unlock()
			if len(g.mutations) != 0 {
				t.Fatalf("the fake recorded %v, want no mutation for a document GitHub refuses", g.mutations)
			}
		})
	}
}

// The fake answers the read with a hand-written JSON literal, and nothing but this check holds
// that literal to the document the package sends: GitHub answers the fields a document selects
// and no others, so a field the fake answers that read never selects is a decode nothing on
// GitHub feeds. The fake's read branch runs unselectedFields over every answer it writes, so the
// answer and the document cannot drift apart as the read grows.
func TestTheFakeRefusesAnAnswerTheReadDocumentDoesNotSelect(t *testing.T) {
	// answered is the fake's own answer to the read, with extra, a further field of the
	// merge-queue entry, where it is given.
	answered := func(extra string) string {
		return `{"data":{"repository":{"autoMergeAllowed":true,"pullRequest":{"id":"PR_42","state":"OPEN","headRefOid":"head",` +
			`"mergeStateStatus":"BLOCKED","updatedAt":"` + updatedAt + `","isMergeQueueEnabled":true,"autoMergeRequest":null,` +
			`"mergeQueueEntry":{"enqueuedAt":"2026-10-11T12:05:00Z","state":"QUEUED","baseCommit":{"oid":"` + baseOid + `"}` + extra + `},` +
			`"baseRef":{"name":"` + baseBranch + `","target":{"oid":"` + baseOid + `"}}}}}}`
	}
	for _, tc := range []struct{ name, answer, want string }{
		{name: "the answer the fake writes", answer: answered("")},
		{name: "a field of the merge-queue entry the read does not select", answer: answered(`,"position":3`), want: "position"},
		{name: "a field of the pull request the read does not select", answer: `{"data":{"repository":{"pullRequest":{"id":"PR_42","mergeable":"MERGEABLE"}}}}`, want: "mergeable"},
		{name: "a field of the base branch the read does not select", answer: `{"data":{"repository":{"pullRequest":{"baseRef":{"name":"main","prefix":"refs/heads/"}}}}}`, want: "prefix"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			message := unselectedFields(read, tc.answer)
			switch {
			case tc.want == "" && message != "":
				t.Fatalf("the fake's own answer to the read is refused: %s", message)
			case tc.want != "" && !strings.Contains(message, tc.want):
				t.Fatalf("an answer carrying %s is answered %q, want it refused for that field", tc.want, message)
			}
		})
	}
}

// Disarm turns off what would merge the pull request without a person, by the pull request's state
// when it runs: while the pull request's head is the READY head it was told to keep, nothing is
// touched; otherwise auto-merge is disabled where GitHub has it enabled, and then the merge-queue
// entry is removed where it has one, however late GitHub made it. A pull request with nothing
// armed, or one GitHub answers NOT_FOUND for at the pull request, is left alone. A second Disarm
// changes nothing.
func TestDisarmTurnsOffEverythingArmedUnlessTheHeadIsTheKeptREADYHead(t *testing.T) {
	for _, tc := range []struct {
		name          string
		notFound      string
		armed, queued bool
		keep          string
		want          []string
		wantDisarmed  Disarmed
	}{
		{"auto-merge enabled, no READY to keep", "", true, false, "", []string{"disable PR_42"}, Disarmed{AutoMerge: true}},
		{"in the merge queue, no READY to keep", "", false, true, "", []string{"dequeue PR_42"}, Disarmed{Queue: true}},
		// In a merge-queue repository GitHub enqueues an armed pull request only once its checks
		// pass, so an arm's queue entry can be made after the disarm was decided: it goes too.
		{"armed and its queue entry made since", "", true, true, "", []string{"disable PR_42", "dequeue PR_42"}, Disarmed{AutoMerge: true, Queue: true}},
		{"armed for a READY the head moved past", "", true, true, "ready", []string{"disable PR_42", "dequeue PR_42"}, Disarmed{AutoMerge: true, Queue: true}},
		{"armed at the READY head it was told to keep", "", true, true, "head", nil, Disarmed{Kept: true}},
		{"nothing armed", "", false, false, "", nil, Disarmed{}},
		{"a pull request GitHub does not have", "pullRequest", false, false, "", nil, Disarmed{Missing: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := &github{notFound: tc.notFound, head: "head", armed: tc.armed, queued: tc.queued, withdrawing: true}
			client := githubrest.Client{Token: "token", API: githubrest.GraphQLAPI(g.serve(t))}
			got, err := Disarm(context.Background(), client, ghrepo.MustParse("acme/widgets"), 42, keeping(tc.keep))
			if err != nil || got != tc.wantDisarmed {
				t.Fatalf("Disarm = %+v, %v; want %+v", got, err, tc.wantDisarmed)
			}
			if again, err := Disarm(context.Background(), client, ghrepo.MustParse("acme/widgets"), 42, keeping(tc.keep)); err != nil || again.AutoMerge || again.Queue {
				t.Fatalf("a second Disarm = %+v, %v; want nothing left to disarm", again, err)
			}
			g.mu.Lock()
			defer g.mu.Unlock()
			if !slices.Equal(g.mutations, tc.want) {
				t.Fatalf("mutations %v, want %v", g.mutations, tc.want)
			}
		})
	}
}

// GitHub enqueues again a pull request it takes out of the merge queue while its auto-merge is
// still enabled, so Disarm disables auto-merge first: dequeued first, the pull request would be
// back in the queue, still armed, as Disarm reported it disarmed.
func TestDisarmDisablesAutoMergeBeforeItDequeues(t *testing.T) {
	g := &github{head: "head", armed: true, queued: true, requeue: true, withdrawing: true}
	client := githubrest.Client{Token: "token", API: githubrest.GraphQLAPI(g.serve(t))}
	if _, err := Disarm(context.Background(), client, ghrepo.MustParse("acme/widgets"), 42, keeping("")); err != nil {
		t.Fatalf("Disarm = %v", err)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.armed || g.queued {
		t.Fatalf("after Disarm auto-merge enabled %t, queued %t; want neither (mutations %v)", g.armed, g.queued, g.mutations)
	}
}

// A disable GitHub refuses does not leave the queue entry standing: Disarm still takes the pull
// request out of the merge queue, then fails with the refused disable, so the row retries it.
func TestDisarmDequeuesEvenWhenTheDisableFails(t *testing.T) {
	g := &github{head: "head", armed: true, queued: true, failDisable: true, withdrawing: true}
	client := githubrest.Client{Token: "token", API: githubrest.GraphQLAPI(g.serve(t))}
	_, err := Disarm(context.Background(), client, ghrepo.MustParse("acme/widgets"), 42, keeping(""))
	if err == nil || !strings.Contains(err.Error(), "disable the auto-merge") {
		t.Fatalf("Disarm with the disable refused = %v, want the disable's failure", err)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if want := []string{"disable PR_42", "dequeue PR_42"}; !slices.Equal(g.mutations, want) || g.queued {
		t.Fatalf("mutations %v, queued %t; want %v and the entry gone", g.mutations, g.queued, want)
	}
}

// Disarm reads the pull request once more after its mutations, and fails, so the row retries,
// when anything it was to turn off is still armed: a mutation GitHub answered as done is not proof
// the merge is off.
func TestDisarmFailsWhenTheArmIsStillOnAfterItsMutations(t *testing.T) {
	g := &github{head: "head", armed: true, stuckArm: true, withdrawing: true}
	client := githubrest.Client{Token: "token", API: githubrest.GraphQLAPI(g.serve(t))}
	if _, err := Disarm(context.Background(), client, ghrepo.MustParse("acme/widgets"), 42, keeping("")); err == nil || !strings.Contains(err.Error(), "still armed") {
		t.Fatalf("Disarm with the arm still on afterwards = %v, want an error saying it is still armed", err)
	}
}

// A NOT_FOUND anywhere but at the pull request, such as a repository the implement App cannot see,
// is no answer about the pull request: Disarm fails, so its row retries and the failure is logged,
// rather than finishing as though there were nothing to disarm.
func TestDisarmFailsWhenGitHubCannotFindTheRepository(t *testing.T) {
	g := &github{notFound: "repository", withdrawing: true}
	client := githubrest.Client{Token: "token", API: githubrest.GraphQLAPI(g.serve(t))}
	got, err := Disarm(context.Background(), client, ghrepo.MustParse("acme/widgets"), 42, keeping(""))
	if err == nil || got.Missing {
		t.Fatalf("Disarm of a repository GitHub cannot find = %+v, %v; want an error, not Missing", got, err)
	}
}

// Submit asks GitHub to merge the READY head under the repository's own rules, and never another
// head. A merge queue gets the pull request enqueued once GitHub reports it mergeable now, and a
// repository that allows auto-merge gets it armed, each with GitHub's expected-head check; a pull
// request GitHub reports mergeable now is merged at that head. A merge-queue base waits, without a
// mutation, while GitHub reports the pull request anything else (a required check running, BLOCKED;
// GitHub computing, UNKNOWN), since an enable there would arm auto-merge rather than enqueue it; a
// repository that allows neither waits the same way. An arm or a merge-queue entry at READY's head
// is not the end: Submit waits on it (Armed) until GitHub reports the pull request merged at that
// head, which alone ends the submission, as does the record leaving READY (Stale). While the record
// still awaits READY's head, a pull request GitHub shows closed, merged at another head, or at
// another head waits for the webhook that moves the record (Lagging).
func TestSubmitUsesGitHubsAnswerForTheREADYHead(t *testing.T) {
	for _, tc := range []struct {
		name                          string
		state, head, mergeState, keep string
		queueRequired, autoMergeOK    bool
		armed, queued                 bool
		want                          []string
		outcome                       Outcome
		wait                          string
	}{
		{name: "a merge queue enqueues a pull request GitHub reports mergeable and waits for the merge", head: "head", keep: "head", mergeState: "CLEAN", queueRequired: true,
			want: []string{submitted("enable")}, wait: "armed"},
		{name: "a merge queue waits while a required check runs", head: "head", keep: "head", mergeState: "BLOCKED", queueRequired: true, autoMergeOK: true, wait: "unmergeable"},
		{name: "a merge queue waits while GitHub computes", head: "head", keep: "head", mergeState: "UNKNOWN", queueRequired: true, autoMergeOK: true, wait: "unmergeable"},
		{name: "auto-merge allowed arms it and waits for the merge", head: "head", keep: "head", mergeState: "BLOCKED", autoMergeOK: true,
			want: []string{submitted("enable")}, wait: "armed"},
		{name: "a clean pull request merges at once", head: "head", keep: "head", mergeState: "CLEAN",
			want: []string{submitted("merge")}, outcome: Merged},
		{name: "an unstable pull request merges at once", head: "head", keep: "head", mergeState: "UNSTABLE",
			want: []string{submitted("merge")}, outcome: Merged},
		{name: "auto-merge disallowed waits while blocked", head: "head", keep: "head", mergeState: "BLOCKED", wait: "unmergeable"},
		{name: "auto-merge disallowed waits while GitHub computes", head: "head", keep: "head", mergeState: "UNKNOWN", wait: "unmergeable"},
		{name: "auto-merge disallowed waits while behind", head: "head", keep: "head", mergeState: "BEHIND", wait: "unmergeable"},
		{name: "already armed waits for the merge", head: "head", keep: "head", mergeState: "BLOCKED", autoMergeOK: true, armed: true, wait: "armed"},
		{name: "already queued waits for the merge", head: "head", keep: "head", mergeState: "BLOCKED", queueRequired: true, queued: true, wait: "armed"},
		{name: "merged at READY's head", state: "MERGED", head: "head", keep: "head", mergeState: "MERGED", outcome: Already},
		{name: "merged at another head while the record awaits READY", state: "MERGED", head: "moved", keep: "head", mergeState: "MERGED", wait: "lagging"},
		{name: "closed while the record awaits READY", state: "CLOSED", head: "head", keep: "head", mergeState: "CLEAN", wait: "lagging"},
		{name: "closed once the record left READY", state: "CLOSED", head: "head", keep: "", mergeState: "CLEAN", outcome: Stale},
		{name: "moved past READY while the record awaits it", head: "moved", keep: "head", mergeState: "CLEAN", wait: "lagging"},
		{name: "moved past READY once the record left it", head: "moved", keep: "", mergeState: "CLEAN", outcome: Stale},
		{name: "armed once the record left READY", head: "head", keep: "", mergeState: "BLOCKED", autoMergeOK: true, armed: true, outcome: Stale},
		{name: "the record no longer covers READY", head: "head", keep: "", mergeState: "CLEAN", outcome: Stale},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := &github{state: tc.state, head: tc.head, mergeState: tc.mergeState, queueRequired: tc.queueRequired,
				autoMergeOK: tc.autoMergeOK, armed: tc.armed, queued: tc.queued}
			client, _ := clients(t, g)
			got, err := Submit(context.Background(), client, ghrepo.MustParse("acme/widgets"), 42, "head", false, keeping(tc.keep))
			var lagging *Lagging
			var unmergeable *Unmergeable
			var armed *Armed
			waited := map[string]bool{"lagging": errors.As(err, &lagging), "unmergeable": errors.As(err, &unmergeable), "armed": errors.As(err, &armed)}
			if tc.wait != "" {
				if !errors.Is(err, wait.ErrWaiting) || !waited[tc.wait] {
					t.Fatalf("Submit = %q, %v; want a %s wait", got, err, tc.wait)
				}
			} else if err != nil || got != tc.outcome {
				t.Fatalf("Submit = %q, %v; want %q", got, err, tc.outcome)
			}
			if unmergeable != nil && (unmergeable.State != tc.mergeState || unmergeable.Queue != tc.queueRequired) {
				t.Fatalf("Unmergeable wait in %s on a merge-queue base %t, want %s and %t", unmergeable.State, unmergeable.Queue, tc.mergeState, tc.queueRequired)
			}
			if armed != nil && armed.Head != "head" {
				t.Fatalf("Armed wait at %s, want READY's head", armed.Head)
			}
			g.mu.Lock()
			defer g.mu.Unlock()
			if !slices.Equal(g.mutations, tc.want) {
				t.Fatalf("mutations %v, want %v", g.mutations, tc.want)
			}
			if g.timelineReads != 0 {
				t.Fatalf("timeline reads %d by a row that never saw READY's head armed, want none", g.timelineReads)
			}
		})
	}
}

// A row that has seen READY's head armed reads no timeline while the arm or entry stands: it waits
// on it (Armed) as before.
func TestSubmitWaitsOnAStandingArmWithoutReadingTheTimeline(t *testing.T) {
	g := &github{head: "head", mergeState: "CLEAN", queueRequired: true, queued: true}
	client, _ := clients(t, g)
	_, err := Submit(context.Background(), client, ghrepo.MustParse("acme/widgets"), 42, "head", true, keeping("head"))
	var armed *Armed
	if !errors.As(err, &armed) || !armed.Routine() {
		t.Fatalf("Submit while queued = %v, want a routine Armed wait", err)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.timelineReads != 0 || len(g.mutations) != 0 {
		t.Fatalf("timeline reads %d and mutations %v while READY's head stands queued, want none", g.timelineReads, g.mutations)
	}
}

// An Armed wait carries what the read it came from found: the merge-queue entry's state and the
// commit the queue built its merge on, and the branch the pull request merges into with that
// branch's tip. The arm GitHub has just accepted carries no entry, since the read before the
// mutation found none; the next run reads the entry GitHub made of it.
//
// An entry GitHub's merge queue cannot merge is a wait like any other entry, carrying the same
// read's state: the queue's UNMERGEABLE, the commit the queue built the entry on, and the base
// branch with its tip. Submit sends no mutation at all for it — it neither submits the head
// again nor takes the entry out of the queue — whatever GitHub answers for the pull request's
// own merge state, which says nothing about an entry: what the daemon makes of the state is its
// row's (the daemon's queueConflict). The name is the one the issue's check k1 runs
// (`go test ./internal/ghautomerge/ -run TestAnUnmergeableEntry`), this being the table every
// entry state a wait carries is read by.
func TestAnUnmergeableEntryIsAWaitCarryingTheQueuesOwnState(t *testing.T) {
	for _, tc := range []struct {
		name string
		g    *github
		// seen is the row having seen READY's head armed or queued before this run, want the
		// wait Submit must answer with, and mutations what it must have sent.
		seen      bool
		want      Armed
		mutations []string
	}{
		{name: "a pull request the merge queue has an entry for", seen: true,
			g:    &github{head: "head", mergeState: "BLOCKED", queueRequired: true, queued: true, queueState: "AWAITING_CHECKS", queueBase: "queue-base", baseName: "release", baseTip: "release-tip"},
			want: Armed{QueueState: "AWAITING_CHECKS", QueueBase: "queue-base", BaseName: "release", BaseTip: "release-tip"}},
		{name: "an arm GitHub has just accepted",
			g:         &github{head: "head", mergeState: "BLOCKED", autoMergeOK: true},
			want:      Armed{BaseName: baseBranch, BaseTip: baseOid},
			mutations: []string{submitted("enable")}},
		{name: "an entry the queue cannot merge, built on the base branch's tip", seen: true,
			g:    &github{head: "head", mergeState: "UNKNOWN", queueRequired: true, queued: true, queueState: "UNMERGEABLE", queueBase: baseOid},
			want: Armed{QueueState: "UNMERGEABLE", QueueBase: baseOid, BaseName: baseBranch, BaseTip: baseOid}},
		{name: "an entry the queue cannot merge, built on the commit the queue made for the entry ahead, which is no pull request's head", seen: true,
			g:    &github{head: "head", mergeState: "UNKNOWN", queueRequired: true, queued: true, queueState: "UNMERGEABLE", queueBase: queueBuiltAhead},
			want: Armed{QueueState: "UNMERGEABLE", QueueBase: queueBuiltAhead, BaseName: baseBranch, BaseTip: baseOid}},
		{name: "an entry the queue cannot merge on another base branch, while GitHub reports the pull request clean", seen: true,
			g: &github{head: "head", mergeState: "CLEAN", queueRequired: true, queued: true, queueState: "UNMERGEABLE", queueBase: "release-tip",
				baseName: "release", baseTip: "release-tip"},
			want: Armed{QueueState: "UNMERGEABLE", QueueBase: "release-tip", BaseName: "release", BaseTip: "release-tip"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, _ := clients(t, tc.g)
			_, err := Submit(context.Background(), client, ghrepo.MustParse("acme/widgets"), 42, "head", tc.seen, keeping("head"))
			var armed *Armed
			if !errors.As(err, &armed) || !errors.Is(err, wait.ErrWaiting) {
				t.Fatalf("Submit = %v, want an Armed wait", err)
			}
			want := tc.want
			want.Number, want.Repo, want.Head = 42, ghrepo.MustParse("acme/widgets"), "head"
			if *armed != want {
				t.Fatalf("Armed = %+v, want %+v", *armed, want)
			}
			tc.g.mu.Lock()
			defer tc.g.mu.Unlock()
			if !slices.Equal(tc.g.mutations, tc.mutations) {
				t.Fatalf("mutations %v, want %v: an entry the queue holds is submitted no further", tc.g.mutations, tc.mutations)
			}
		})
	}
}

// Which states of GitHub's merge-queue entry are the entry's own head conflicting rather than
// the queue working (Armed.UnmergeableOnBaseTip, which the daemon's merge row decides a conflict
// by): UNMERGEABLE while the queue has built the entry on the tip of the pull request's own base
// branch, where nothing ahead of it in the queue can be what the queue cannot merge, and nothing
// else at all. An entry behind another carries the commit the queue built for the entry ahead,
// which is the tip with that merge on top and so never the tip itself; an entry the queue has
// built nothing for yet carries no commit; and every other state, the ones this daemon does not
// know among them, is the queue still working (INFRA-1442).
func TestAnUnmergeableEntryOnItsBaseBranchesTipIsTheHeadsOwnConflict(t *testing.T) {
	for _, tc := range []struct {
		name                           string
		queueState, queueBase, baseTip string
		want                           bool
	}{
		{name: "unmergeable on the base branch's tip", queueState: "UNMERGEABLE", queueBase: baseOid, baseTip: baseOid, want: true},
		{name: "unmergeable on the commit the queue built for the entry ahead", queueState: "UNMERGEABLE", queueBase: queueBuiltAhead, baseTip: baseOid},
		{name: "unmergeable with no commit the queue has built", queueState: "UNMERGEABLE", baseTip: baseOid},
		{name: "unmergeable with the read naming no base tip", queueState: "UNMERGEABLE", queueBase: baseOid},
		{name: "queued on the base branch's tip", queueState: "QUEUED", queueBase: baseOid, baseTip: baseOid},
		{name: "awaiting its checks on the base branch's tip", queueState: "AWAITING_CHECKS", queueBase: baseOid, baseTip: baseOid},
		{name: "mergeable on the base branch's tip", queueState: "MERGEABLE", queueBase: baseOid, baseTip: baseOid},
		{name: "locked on the base branch's tip", queueState: "LOCKED", queueBase: baseOid, baseTip: baseOid},
		{name: "in a state the daemon does not know", queueState: "BLOCKED_BY_SOMETHING_NEW", queueBase: baseOid, baseTip: baseOid},
		{name: "no entry at all, an auto-merge arm alone", baseTip: baseOid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			armed := Armed{Number: 42, Head: "head", QueueState: tc.queueState, QueueBase: tc.queueBase, BaseTip: tc.baseTip}
			if armed.UnmergeableOnBaseTip() != tc.want {
				t.Fatalf("an entry %q built on %q with %q the base branch's tip reads a conflict %t, want %t",
					tc.queueState, tc.queueBase, tc.baseTip, !tc.want, tc.want)
			}
		})
	}
}

// A Lagging submission carries GitHub's last change to the pull request from the same read, and it
// is stalled only once that change has stood longer than LagBound, on GitHub's own clock: a
// disagreement GitHub made a moment ago is a webhook still on its way.
func TestALaggingSubmissionStallsOnlyOnGitHubsOwnClock(t *testing.T) {
	g := &github{state: "CLOSED", head: "head", mergeState: "UNKNOWN"}
	client, _ := clients(t, g)
	_, err := Submit(context.Background(), client, ghrepo.MustParse("acme/widgets"), 42, "head", false, keeping("head"))
	var lagging *Lagging
	if !errors.As(err, &lagging) {
		t.Fatalf("Submit = %v, want Lagging", err)
	}
	changed, _ := time.Parse(time.RFC3339, updatedAt)
	if !lagging.Since.Equal(changed) {
		t.Fatalf("Lagging since %s, want GitHub's updatedAt %s", lagging.Since, changed)
	}
	if lagging.Stalled(changed.Add(LagBound)) || !lagging.Stalled(changed.Add(LagBound+time.Second)) {
		t.Fatalf("Lagging stalled at the bound or not past it (bound %s from %s)", LagBound, changed)
	}
}

// GitHub always answers a pull request's updatedAt. A read without one is not GitHub's answer, so
// Submit fails it loudly rather than judging a disagreement against no clock at all.
func TestSubmitFailsAReadWithoutUpdatedAt(t *testing.T) {
	g := &github{state: "CLOSED", head: "head", mergeState: "UNKNOWN", updated: "null"}
	client, _ := clients(t, g)
	_, err := Submit(context.Background(), client, ghrepo.MustParse("acme/widgets"), 42, "head", false, keeping("head"))
	var lagging *Lagging
	if err == nil || errors.Is(err, wait.ErrWaiting) || errors.As(err, &lagging) || !strings.Contains(err.Error(), "updatedAt") {
		t.Fatalf("Submit without updatedAt = %v, want a failure naming updatedAt", err)
	}
}

// A wait on GitHub's merge state is routine only while the repository's own rules end it without
// anyone acting for Legion: a required check still running or a required review (BLOCKED), or
// GitHub still computing the state (UNKNOWN). A branch to update (BEHIND), a draft, or a conflict
// (DIRTY) waits on someone, so it is not. The same holds in a repository that allows no auto-merge
// and on a base that requires a merge queue.
func TestOnlyABlockedOrComputingMergeStateIsARoutineWait(t *testing.T) {
	for state, routine := range map[string]bool{"BLOCKED": true, "UNKNOWN": true, "BEHIND": false, "DRAFT": false, "DIRTY": false} {
		for _, queue := range []bool{false, true} {
			g := &github{head: "head", mergeState: state, queueRequired: queue, autoMergeOK: queue}
			client, _ := clients(t, g)
			_, err := Submit(context.Background(), client, ghrepo.MustParse("acme/widgets"), 42, "head", false, keeping("head"))
			var unmergeable *Unmergeable
			if !errors.As(err, &unmergeable) || !errors.Is(err, wait.ErrWaiting) || unmergeable.Routine() != routine || unmergeable.Queue != queue {
				t.Fatalf("Submit while %s on a merge-queue base %t = %v; want an Unmergeable wait, routine %t", state, queue, err, routine)
			}
		}
	}
}

// GitHub's refusal of the merge or the arm is permanent when GitHub still stands where it refused:
// the same open pull request at READY's head, nothing armed, and the same submission chosen, so a
// retry would send the same request and meet the same refusal. Submit returns it as *Refused, with
// GitHub's message. A refusal after which the pull request moved, and an HTTP failure, are
// transient: the row retries and reads GitHub again.
func TestSubmitTellsAPermanentRefusalFromATransientOne(t *testing.T) {
	const squash = "Squash merges are not allowed on this repository."
	for _, tc := range []struct {
		name      string
		g         *github
		permanent bool
	}{
		{"GitHub refuses the merge and stands where it was", &github{head: "head", mergeState: "CLEAN", refuseSubmit: squash}, true},
		{"GitHub refuses the arm and stands where it was", &github{head: "head", mergeState: "BLOCKED", autoMergeOK: true, refuseSubmit: squash}, true},
		{"GitHub refuses the enqueue and stands where it was", &github{head: "head", mergeState: "CLEAN", queueRequired: true, refuseSubmit: squash}, true},
		{"GitHub rejects the merge as invalid", &github{head: "head", mergeState: "CLEAN", refuseSubmit: squash, refuseType: "VALIDATION_FAILED"}, true},
		{"GitHub rate limits the merge", &github{head: "head", mergeState: "CLEAN", refuseSubmit: "API rate limit exceeded", refuseType: "RATE_LIMITED"}, false},
		{"GitHub has an internal merge error", &github{head: "head", mergeState: "CLEAN", refuseSubmit: "Service temporarily unavailable", refuseType: "INTERNAL"}, false},
		{"GitHub refuses the merge as a push moves the head", &github{head: "head", mergeState: "CLEAN", refuseSubmit: "Head branch was modified. Review and try the merge again.", movedOnRefuse: "moved"}, false},
		{"GitHub answers the merge with a server error", &github{head: "head", mergeState: "CLEAN", submitStatus: http.StatusBadGateway}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, _ := clients(t, tc.g)
			_, err := Submit(context.Background(), client, ghrepo.MustParse("acme/widgets"), 42, "head", false, keeping("head"))
			var refused *Refused
			if err == nil || errors.Is(err, wait.ErrWaiting) {
				t.Fatalf("Submit = %v, want GitHub's failure", err)
			}
			if errors.As(err, &refused) != tc.permanent {
				t.Fatalf("Submit = %v (%T); want permanent %t", err, err, tc.permanent)
			}
			if tc.permanent && (!strings.Contains(refused.Message, squash) || !strings.Contains(err.Error(), squash)) {
				t.Fatalf("refusal = %+v, want GitHub's message to include %q", refused, squash)
			}
		})
	}
}

// GitHub refuses the merge, then rate-limits the read that would confirm the refusal. Submit
// returns the read's error, the refusal as its context, so the runner sees GitHub's rate limit, its
// Retry-After and its budget, and the row neither ends nor meets the limit again early.
func TestSubmitKeepsTheRateLimitOfItsConfirmationRead(t *testing.T) {
	const squash = "Squash merges are not allowed on this repository."
	g := &github{head: "head", mergeState: "CLEAN", refuseSubmit: squash, limitAfterRefusal: true}
	client, _ := clients(t, g)
	_, err := Submit(context.Background(), client, ghrepo.MustParse("acme/widgets"), 42, "head", false, keeping("head"))
	var refused *Refused
	if err == nil || errors.As(err, &refused) || errors.Is(err, wait.ErrWaiting) {
		t.Fatalf("Submit = %v, want a failure to retry", err)
	}
	if after, budget := githubrest.RetryAfter(err); after != 180*time.Second || budget != githubrest.BudgetSecondary {
		t.Fatalf("RetryAfter(%v) = %s, %q; want the confirmation read's 180s secondary limit", err, after, budget)
	}
	if !strings.Contains(err.Error(), squash) {
		t.Fatalf("Submit = %v, want the refusal kept as context", err)
	}
}

// keeping is a Keep that answers head.
func keeping(head string) Keep {
	return func(context.Context) (string, error) { return head, nil }
}
