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
// and logStatus answers every such read with that HTTP status instead (403 is the Actions read a
// deployment's App may never have been granted). logFill answers the read with that many bytes
// before the log, which a log past githubrest.JobLogBound is made of, and logDelay holds the
// answer open for that long, which a read past githubrest.JobLogTimeout waits out. logReads
// counts the job logs read.
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
	logStatus                      int
	logFill                        int
	logDelay                       time.Duration
	logReads                       int
	mutations                      []string
}

// updatedAt is the fake pull request's default last change on GitHub, baseBranch and baseOid are
// the branch it merges into and the commit that branch is at, and absent is how the fake records
// an input the mutation it serves does not pass.
const (
	updatedAt  = "2026-10-16T05:00:00Z"
	baseBranch = "main"
	baseOid    = "a2679f2e"
	absent     = "<absent>"
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

// jobLogsPath is where the fake answers a job's log, the Actions jobs route under the REST base
// of acme/widgets, and jobLogFiller is the line a log is padded with to pass a byte bound.
const (
	jobLogsPath  = "/repos/acme/widgets/actions/jobs/"
	jobLogFiller = "a line of a log that says nothing about what failed\n"
)

// serveJobLog answers the log of the Actions job job names ("{id}/logs") as GitHub does: the
// status the fake refuses every log read with, else the log it holds for that job, written after
// logFill bytes of filler and logDelay of waiting, which is how a log past githubrest.JobLogBound
// and a read past githubrest.JobLogTimeout are made. A job the fake holds no log for is GitHub's
// 404, as a check run no Actions job backs would answer.
func (g *github) serveJobLog(t *testing.T, w http.ResponseWriter, r *http.Request, job string) {
	t.Helper()
	id, err := strconv.ParseInt(strings.TrimSuffix(job, "/logs"), 10, 64)
	if r.Method != http.MethodGet || !strings.HasSuffix(job, "/logs") || err != nil {
		t.Errorf("unexpected job log request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if g.logStatus != 0 {
		http.Error(w, `{"message":"Resource not accessible by integration"}`, g.logStatus)
		return
	}
	log, held := g.jobLogs[id]
	if !held {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		return
	}
	if g.logDelay > 0 {
		select {
		case <-time.After(g.logDelay):
		case <-r.Context().Done():
			return
		}
	}
	for written := 0; written < g.logFill; written += len(jobLogFiller) {
		if _, err := io.WriteString(w, jobLogFiller); err != nil {
			return
		}
	}
	_, _ = io.WriteString(w, log)
}

func (g *github) serve(t *testing.T) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		// The log of a failing job is the one read this package makes outside GraphQL: GET
		// /actions/jobs/{id}/logs under the repository's REST base (githubrest.RepositoryAPI).
		if job, found := strings.CutPrefix(r.URL.Path, jobLogsPath); found {
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

// clients are the two clients the merge path calls the fake with: GitHub's GraphQL route, which
// the pull request is read and changed on, and the REST base of acme/widgets, which a failing
// job's log is read under. One server answers both, as GitHub's one API root does.
func clients(t *testing.T, g *github) (githubrest.Client, githubrest.Client) {
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
			client, logs := clients(t, g)
			got, err := Submit(context.Background(), client, logs, ghrepo.MustParse("acme/widgets"), 42, "head", false, keeping(tc.keep))
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

// failedRun is the merge_group run whose checks failed on the commit the merge queue built for #42.
const failedRun = "https://github.com/acme/widgets/actions/runs/7"

// unexplained is a hypothetical queue removal reason, one GitHub gives no other meaning: GitHub's
// own are merged, failed_checks, manual and merge_conflict, and this stands for any it adds.
const unexplained = "timed_out"

// enqueued and enabled are GitHub's timeline records of the pull request entering the merge queue
// and of its auto-merge being enabled.
const (
	enqueued = `{"__typename":"AddedToMergeQueueEvent","createdAt":"2026-10-16T05:00:00Z"}`
	enabled  = `{"__typename":"AutoMergeEnabledEvent","createdAt":"2026-10-16T05:00:00Z"}`
)

// pagedChecks is a merge queue commit whose checks GitHub lists over two pages: the first, cut at
// 100, all passed, and its integration failure is on the second, secondChecksPage, which moreChecks
// reads after the cursor after-100.
var pagedChecks = func() string {
	var passing []string
	for i := range 100 {
		passing = append(passing, fmt.Sprintf(`{"__typename":"CheckRun","name":"check-%d","status":"COMPLETED","conclusion":"SUCCESS","detailsUrl":"%s/job/%d","checkSuite":{"workflowRun":{"url":"%s"}}}`, i, failedRun, i, failedRun))
	}
	return `{"oid":"group-sha","statusCheckRollup":{"contexts":{"pageInfo":{"hasNextPage":true,"endCursor":"after-100"},"nodes":[` + strings.Join(passing, ",") + `]}}}`
}()

const secondChecksPage = `{"pageInfo":{"hasNextPage":false,"endCursor":"after-101"},"nodes":[` +
	`{"__typename":"CheckRun","name":"integration","status":"COMPLETED","conclusion":"FAILURE","detailsUrl":"` + failedRun + `/job/200","checkSuite":{"workflowRun":{"url":"` + failedRun + `"}}}]}`

// dequeued is GitHub's record of the pull request taken out of the merge queue by actor for reason,
// with commit, the commit the queue built for it, or null.
func dequeued(reason, actor, commit string) string {
	return fmt.Sprintf(`{"__typename":"RemovedFromMergeQueueEvent","id":"RFMQE_9","createdAt":"2026-10-16T05:10:00Z","reason":%q,"actor":{"login":%q},"beforeCommit":%s}`, reason, actor, commit)
}

// disableReasons is GitHub's sentence for each auto-merge reason code these tests use, as its
// timeline answers them.
var disableReasons = map[string]string{
	"manually_disabled":    "Manually disabled by user",
	"merge_commit_blocked": "Merge commits are not allowed on this repository",
	"closed":               "Pull request was closed",
}

// disabled is GitHub's record of the pull request's auto-merge turned off for reasonCode, with its
// sentence, by actor, or naming no actor when actor is empty.
func disabled(reasonCode, actor string) string {
	reason, ok := disableReasons[reasonCode]
	if !ok {
		panic("no sentence of GitHub's for auto-merge reason code " + reasonCode)
	}
	login := "null"
	if actor != "" {
		login = fmt.Sprintf(`{"login":%q}`, actor)
	}
	return fmt.Sprintf(`{"__typename":"AutoMergeDisabledEvent","id":"AMDE_9","createdAt":"2026-10-16T05:10:00Z","reason":%q,"reasonCode":%q,"actor":%s}`, reason, reasonCode, login)
}

// Once the row has seen READY's head armed or queued (armed), an open pull request at that head with
// nothing armed is a removal someone made, which Submit reads from the pull request's timeline and
// never answers with a second submission (LEGION-800). The merge queue taking it out because the
// merge failed - GitHub's reason failed_checks, or a reason GitHub gives no other meaning with a run
// that failed on the commit the queue built - is MergeFailed, naming each failing check and its run
// and not one that passed. Anyone else's removal since the last arm (a person's dequeue, a person
// turning auto-merge off) is Removed, naming who, GitHub's reason and whether it was the queue,
// and the queue finding a conflict is a ConflictRemoval instead, no Removed at all, since what
// that removal means is the merge state's to say and the daemon's to decide (INFRA-1442).
// The queue's removal for the merge itself waits for GitHub to show the
// merge, routinely, and a timeline with no removal since the arm waits on GitHub too, but not
// routinely. Only removals that restart the submission submit again: the App submitting's own
// (GraphQL's viewer: the daemon's disarm landing late), and auto-merge a close turned off, which the
// reopen at READY's head undid. Only a queue removal for failed_checks, or for a reason GitHub gives
// no other meaning, reads its commit's checks past the first page: the merge, a person's dequeue and
// a conflict mean what they say, so they read none, and a page GitHub fails to answer never turns
// the merge's wait into a failure.
func TestSubmitReadsARemovalItDidNotMakeInsteadOfSubmittingAgain(t *testing.T) {
	red := `{"oid":"group-sha","statusCheckRollup":{"contexts":{"nodes":[` +
		`{"__typename":"CheckRun","name":"lint","status":"COMPLETED","conclusion":"SUCCESS","detailsUrl":"` + failedRun + `/job/1","checkSuite":{"workflowRun":{"url":"` + failedRun + `"}}},` +
		`{"__typename":"CheckRun","name":"e2e","status":"IN_PROGRESS","conclusion":null,"detailsUrl":"` + failedRun + `/job/3","checkSuite":{"workflowRun":{"url":"` + failedRun + `"}}},` +
		`{"__typename":"CheckRun","name":"integration","status":"COMPLETED","conclusion":"TIMED_OUT","detailsUrl":"` + failedRun + `/job/2","checkSuite":{"workflowRun":{"url":"` + failedRun + `"}}},` +
		`{"__typename":"StatusContext","context":"freeze","state":"SUCCESS","targetUrl":"https://example.com/freeze"}]}}}`
	green := `{"oid":"group-sha","statusCheckRollup":{"contexts":{"nodes":[{"__typename":"CheckRun","name":"lint","status":"COMPLETED","conclusion":"SUCCESS","detailsUrl":"` + failedRun + `/job/1","checkSuite":{"workflowRun":{"url":"` + failedRun + `"}}}]}}}`
	// cancelled is the queue's commit after a person dequeues it while its merge_group runs are in
	// flight: they end cancelled, which is no failed merge.
	cancelled := `{"oid":"group-sha","statusCheckRollup":{"contexts":{"nodes":[` +
		`{"__typename":"CheckRun","name":"lint","status":"COMPLETED","conclusion":"SUCCESS","detailsUrl":"` + failedRun + `/job/1","checkSuite":{"workflowRun":{"url":"` + failedRun + `"}}},` +
		`{"__typename":"CheckRun","name":"integration","status":"COMPLETED","conclusion":"CANCELLED","detailsUrl":"` + failedRun + `/job/2","checkSuite":{"workflowRun":{"url":"` + failedRun + `"}}},` +
		`{"__typename":"CheckRun","name":"e2e","status":"COMPLETED","conclusion":"SKIPPED","detailsUrl":"` + failedRun + `/job/3","checkSuite":{"workflowRun":{"url":"` + failedRun + `"}}}]}}}`
	// needsAction is the queue's commit with a run that ended needing action, which Legion's rule for
	// a check run counts as failed (classify.RunResult).
	needsAction := `{"oid":"group-sha","statusCheckRollup":{"contexts":{"nodes":[` +
		`{"__typename":"CheckRun","name":"lint","status":"COMPLETED","conclusion":"SUCCESS","detailsUrl":"` + failedRun + `/job/1","checkSuite":{"workflowRun":{"url":"` + failedRun + `"}}},` +
		`{"__typename":"CheckRun","name":"integration","status":"COMPLETED","conclusion":"ACTION_REQUIRED","detailsUrl":"` + failedRun + `/job/2","checkSuite":{"workflowRun":{"url":"` + failedRun + `"}}}]}}}`
	for _, tc := range []struct {
		name     string
		queue    bool
		timeline []string
		// failPages fails every read of a further page of the queue commit's checks, and pages is how
		// many such reads Submit makes.
		failPages bool
		pages     int
		// failed names what MergeFailed must name; removed what Removed must; conflict what the
		// queue's ConflictRemoval must, which is no Removed and ends no merge row here; wait the
		// wait, merging (GitHub's own merge, routine) or disappeared (not routine); resubmit is a
		// second submission.
		failed, removed, conflict []string
		wait                      string
		resubmit                  bool
	}{
		{name: "a person dequeues it while its merge group runs", queue: true, timeline: []string{enqueued, dequeued("manual", "a-maintainer", cancelled)},
			removed: []string{"a-maintainer", "merge queue", "manual"}},
		{name: "the merge queue removes it for an unexplained reason on a merge group that only cancelled", queue: true, timeline: []string{enqueued, dequeued(unexplained, "github-merge-queue", cancelled)},
			removed: []string{"github-merge-queue", "merge queue", unexplained}},
		{name: "the merge queue removes it for an unexplained reason on a merge group whose run needs action", queue: true, timeline: []string{enqueued, dequeued(unexplained, "github-merge-queue", needsAction)},
			failed: []string{unexplained, "integration (action_required, " + failedRun + ")"}},
		{name: "the merge queue removes it for failed_checks, its failure past the first 100 checks", queue: true, timeline: []string{enqueued, dequeued("failed_checks", "github-merge-queue", pagedChecks)},
			pages: 1, failed: []string{"failed_checks", "integration (failure, " + failedRun + ")"}},
		{name: "the merge queue removes it for an unexplained reason, its failure past the first 100 checks", queue: true, timeline: []string{enqueued, dequeued(unexplained, "github-merge-queue", pagedChecks)},
			pages: 1, failed: []string{unexplained, "integration (failure, " + failedRun + ")"}},
		{name: "a person dequeues it from a merge group of more than 100 checks", queue: true, timeline: []string{enqueued, dequeued("manual", "a-maintainer", pagedChecks)},
			failPages: true, removed: []string{"a-maintainer", "merge queue", "manual"}},
		{name: "the merge queue finds a conflict in a merge group of more than 100 checks", queue: true, timeline: []string{enqueued, dequeued("merge_conflict", "github-merge-queue", pagedChecks)},
			failPages: true, conflict: []string{"#42", "head", "merge_conflict", "CLEAN"}},
		{name: "the merge queue takes it out to merge a merge group of more than 100 checks", queue: true, timeline: []string{enqueued, dequeued("merged", "github-merge-queue", pagedChecks)},
			failPages: true, wait: "merging"},
		{name: "the merge queue removes it for failed_checks", queue: true, timeline: []string{enqueued, dequeued("failed_checks", "github-merge-queue", red)},
			failed: []string{"#42", "head", "failed_checks", "group-sha", "integration (timed_out, " + failedRun + ")"}},
		{name: "the merge queue removes it for an unexplained reason on a red merge group", queue: true, timeline: []string{enqueued, dequeued(unexplained, "github-merge-queue", red)},
			failed: []string{unexplained, "integration (timed_out, " + failedRun + ")"}},
		{name: "the merge queue removes it for failed_checks naming no commit", queue: true, timeline: []string{enqueued, dequeued("failed_checks", "github-merge-queue", "null")},
			failed: []string{"failed_checks", "named no commit"}},
		{name: "the merge queue removes it for failed_checks with no failing run read", queue: true, timeline: []string{enqueued, dequeued("failed_checks", "github-merge-queue", green)},
			failed: []string{"failed_checks", "no failing run", "group-sha"}},
		{name: "a person dequeues it", queue: true, timeline: []string{enqueued, dequeued("manual", "a-maintainer", "null")},
			removed: []string{"a-maintainer", "merge queue", "manual", "#42", "head"}},
		{name: "the merge queue finds a conflict", queue: true, timeline: []string{enqueued, dequeued("merge_conflict", "github-merge-queue", "null")},
			conflict: []string{"#42", "head", "merge_conflict", "CLEAN"}},
		{name: "a person turns its auto-merge off", timeline: []string{enabled, disabled("manually_disabled", "a-maintainer")},
			removed: []string{"a-maintainer turned off the auto-merge", "manually_disabled: Manually disabled by user"}},
		{name: "auto-merge is turned off with GitHub naming no actor", timeline: []string{enabled, disabled("manually_disabled", "")},
			removed: []string{"someone turned off the auto-merge", "manually_disabled: Manually disabled by user"}},
		{name: "GitHub turns its auto-merge off, naming the App that enabled it", timeline: []string{enabled, disabled("merge_commit_blocked", "legion-implementer")},
			removed: []string{"GitHub turned off the auto-merge", "naming legion-implementer, the App that submitted it", "merge_commit_blocked: Merge commits are not allowed on this repository"}},
		{name: "the daemon's own disarm lands late", timeline: []string{enabled, disabled("manually_disabled", "legion-implementer")}, resubmit: true},
		{name: "the daemon's own disarm, then a person's dequeue", queue: true, timeline: []string{enqueued, disabled("manually_disabled", "legion-implementer"), dequeued("manual", "a-maintainer", "null")},
			removed: []string{"a-maintainer", "merge queue", "manual"}},
		{name: "a person's removal before the last arm, then the daemon's own", timeline: []string{enabled, disabled("manually_disabled", "a-maintainer"), enabled, disabled("manually_disabled", "legion-implementer")}, resubmit: true},
		{name: "a close a reopen at READY's head undid", timeline: []string{enabled, disabled("closed", "a-maintainer")}, resubmit: true},
		{name: "the merge queue takes it out to merge it", queue: true, timeline: []string{enqueued, dequeued("merged", "github-merge-queue", green)}, wait: "merging"},
		{name: "the timeline names no removal since the arm", queue: true, timeline: []string{dequeued("failed_checks", "github-merge-queue", red), enqueued}, wait: "disappeared"},
		{name: "the timeline names no arm or removal at all", wait: "disappeared"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A merge-queue base GitHub reports mergeable, or a repository that allows auto-merge
			// whose required checks still run: a second submission would be an enable either way.
			mergeState := "BLOCKED"
			if tc.queue {
				mergeState = "CLEAN"
			}
			g := &github{head: "head", mergeState: mergeState, queueRequired: tc.queue, autoMergeOK: true, timeline: tc.timeline,
				contextPages: map[string]string{"after-100": secondChecksPage}, failPages: tc.failPages}
			client, logs := clients(t, g)
			_, err := Submit(context.Background(), client, logs, ghrepo.MustParse("acme/widgets"), 42, "head", true, keeping("head"))
			var failed *MergeFailed
			var removed *Removed
			var conflict *ConflictRemoval
			var merging *Merging
			var disappeared *Disappeared
			var routine interface{ Routine() bool }
			var armed *Armed
			switch {
			case tc.failed != nil:
				if !errors.As(err, &failed) || errors.Is(err, wait.ErrWaiting) {
					t.Fatalf("Submit = %v (%T), want MergeFailed", err, err)
				}
				if failed.Event != "RFMQE_9" {
					t.Fatalf("MergeFailed event %q, want GitHub's id for the removal", failed.Event)
				}
				for _, want := range tc.failed {
					if !strings.Contains(err.Error(), want) {
						t.Fatalf("MergeFailed %q, want it to name %q", err, want)
					}
				}
				for _, passed := range []string{"lint", "e2e", "freeze"} {
					if strings.Contains(err.Error(), passed) {
						t.Fatalf("MergeFailed %q names %s, which did not fail", err, passed)
					}
				}
			case tc.removed != nil:
				if !errors.As(err, &removed) || errors.Is(err, wait.ErrWaiting) || removed.Queue != tc.queue {
					t.Fatalf("Submit = %v (%T), want Removed from the merge queue %t", err, err, tc.queue)
				}
				for _, want := range tc.removed {
					if !strings.Contains(err.Error(), want) {
						t.Fatalf("Removed %q, want it to name %q", err, want)
					}
				}
				if (removed.Detail == "") != removed.Queue {
					t.Fatalf("Removed %q with Detail %q, want GitHub's sentence in Detail for an auto-merge disable and none for a queue removal", err, removed.Detail)
				}
			case tc.conflict != nil:
				if !errors.As(err, &conflict) || errors.As(err, &removed) || errors.Is(err, wait.ErrWaiting) {
					t.Fatalf("Submit = %v (%T), want a ConflictRemoval that is no Removed: the daemon decides that removal", err, err)
				}
				for _, want := range tc.conflict {
					if !strings.Contains(err.Error(), want) {
						t.Fatalf("ConflictRemoval %q, want it to name %q", err, want)
					}
				}
				ended := conflict.Ended()
				if !ended.Queue || ended.Actor != "github-merge-queue" || ended.Reason != "merge_conflict" || ended.Head != "head" || ended.Number != 42 {
					t.Fatalf("ConflictRemoval ended as %+v, want the merge queue's removal of #42 at head for merge_conflict", *ended)
				}
			case tc.wait == "merging":
				if !errors.As(err, &merging) || !errors.Is(err, wait.ErrWaiting) || !merging.Routine() {
					t.Fatalf("Submit = %v (%T), want a routine Merging wait", err, err)
				}
			case tc.wait == "disappeared":
				if !errors.As(err, &disappeared) || !errors.Is(err, wait.ErrWaiting) || errors.As(err, &routine) && routine.Routine() {
					t.Fatalf("Submit = %v (%T), want a Disappeared wait that is not routine", err, err)
				}
			case tc.resubmit:
				if !errors.As(err, &armed) {
					t.Fatalf("Submit = %v (%T), want READY's head submitted again and an Armed wait", err, err)
				}
			}
			g.mu.Lock()
			defer g.mu.Unlock()
			var want []string
			if tc.resubmit {
				want = []string{submitted("enable")}
			}
			if !slices.Equal(g.mutations, want) {
				t.Fatalf("mutations %v, want %v", g.mutations, want)
			}
			if g.pageReads != tc.pages {
				t.Fatalf("reads of the queue commit's further checks %d, want %d", g.pageReads, tc.pages)
			}
		})
	}
}

// A removal of the arm or merge-queue entry at a head Submit still finds open and unarmed is read
// from the timeline, and every reason GitHub can give it has an error of its own. The merge
// queue's conflict (merge_conflict) is the one whose error the reason alone does not decide: it is
// a ConflictRemoval carrying the merge state GitHub answered for the pull request once the entry
// was gone, since a head that conflicts with the base itself reads DIRTY while a head GitHub
// merges cleanly conflicted with an entry ahead of it in the queue, and the daemon's row decides
// between them (INFRA-1442). It is deliberately no Removed, the error that ends the merge row by
// itself. Every other reason answers exactly as it did before: a failed merge (failed_checks, or
// a reason GitHub gives no other meaning with a run that failed on the commit the queue built) is
// MergeFailed, the merge itself is a wait for GitHub to show it, a person's dequeue is Removed,
// and the two removals that restart the submission - the App's own disarm and auto-merge a close
// turned off - submit the head again.
func TestARemovalAtAnUnchangedHeadIsReadByItsReasonAndMergeState(t *testing.T) {
	// failing is the commit the merge queue built, with a run that failed on it.
	const failing = `{"oid":"group-sha","statusCheckRollup":{"contexts":{"nodes":[` +
		`{"__typename":"CheckRun","name":"integration","status":"COMPLETED","conclusion":"FAILURE","detailsUrl":"` + failedRun + `/job/2","checkSuite":{"workflowRun":{"url":"` + failedRun + `"}}}]}}}`
	for _, tc := range []struct {
		name string
		// queue says the base requires a merge queue, and mergeState is what GitHub answers for
		// the pull request in the read that found nothing armed.
		queue      bool
		mergeState string
		timeline   []string
		// want is the error the removal gives: conflict, failed, removed, merging or resubmit.
		want string
		// names is what that error must say.
		names []string
	}{
		{name: "a conflict GitHub still merges the head through", queue: true, mergeState: "CLEAN",
			timeline: []string{enqueued, dequeued("merge_conflict", "github-merge-queue", "null")},
			want:     "conflict", names: []string{"#42", "head", "merge_conflict", "CLEAN"}},
		{name: "a conflict with the base itself", queue: true, mergeState: "DIRTY",
			timeline: []string{enqueued, dequeued("merge_conflict", "github-merge-queue", "null")},
			want:     "conflict", names: []string{"#42", "head", "merge_conflict", "DIRTY"}},
		{name: "a conflict whose merge state GitHub has not computed", queue: true, mergeState: "UNKNOWN",
			timeline: []string{enqueued, dequeued("merge_conflict", "github-merge-queue", "null")},
			want:     "conflict", names: []string{"#42", "head", "merge_conflict", "UNKNOWN"}},
		{name: "a merge the queue failed", queue: true, mergeState: "CLEAN",
			timeline: []string{enqueued, dequeued("failed_checks", "github-merge-queue", failing)},
			want:     "failed", names: []string{"failed_checks", "group-sha", "integration (failure, " + failedRun + ")"}},
		{name: "a reason GitHub gives no other meaning with a run that failed on the queue's commit", queue: true, mergeState: "CLEAN",
			timeline: []string{enqueued, dequeued(unexplained, "github-merge-queue", failing)},
			want:     "failed", names: []string{unexplained, "integration (failure, " + failedRun + ")"}},
		{name: "the merge itself", queue: true, mergeState: "CLEAN",
			timeline: []string{enqueued, dequeued("merged", "github-merge-queue", "null")}, want: "merging"},
		{name: "a person's dequeue", queue: true, mergeState: "CLEAN",
			timeline: []string{enqueued, dequeued("manual", "a-maintainer", "null")},
			want:     "removed", names: []string{"a-maintainer", "merge queue", "manual"}},
		{name: "the App's own disarm", mergeState: "BLOCKED",
			timeline: []string{enabled, disabled("manually_disabled", "legion-implementer")}, want: "resubmit"},
		{name: "auto-merge a close turned off", mergeState: "BLOCKED",
			timeline: []string{enabled, disabled("closed", "a-maintainer")}, want: "resubmit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := &github{head: "head", mergeState: tc.mergeState, queueRequired: tc.queue, autoMergeOK: true, timeline: tc.timeline}
			client, logs := clients(t, g)
			_, err := Submit(context.Background(), client, logs, ghrepo.MustParse("acme/widgets"), 42, "head", true, keeping("head"))
			var conflict *ConflictRemoval
			var failed *MergeFailed
			var removed *Removed
			var merging *Merging
			var armed *Armed
			switch tc.want {
			case "conflict":
				if !errors.As(err, &conflict) || errors.As(err, &removed) || errors.Is(err, wait.ErrWaiting) {
					t.Fatalf("Submit = %v (%T), want a ConflictRemoval that is no Removed and no wait", err, err)
				}
				if conflict.MergeState != tc.mergeState || !conflict.Ended().Queue || conflict.Ended().Reason != "merge_conflict" {
					t.Fatalf("ConflictRemoval %+v ending as %+v, want the queue's merge_conflict removal at merge state %s", *conflict, *conflict.Ended(), tc.mergeState)
				}
			case "failed":
				if !errors.As(err, &failed) || errors.Is(err, wait.ErrWaiting) {
					t.Fatalf("Submit = %v (%T), want MergeFailed", err, err)
				}
			case "removed":
				if !errors.As(err, &removed) || errors.As(err, &conflict) || errors.Is(err, wait.ErrWaiting) {
					t.Fatalf("Submit = %v (%T), want Removed", err, err)
				}
			case "merging":
				if !errors.As(err, &merging) || !errors.Is(err, wait.ErrWaiting) {
					t.Fatalf("Submit = %v (%T), want a Merging wait", err, err)
				}
			case "resubmit":
				if !errors.As(err, &armed) {
					t.Fatalf("Submit = %v (%T), want the head submitted again and an Armed wait", err, err)
				}
			}
			for _, want := range tc.names {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("Submit = %q, want it to name %q", err, want)
				}
			}
			g.mu.Lock()
			defer g.mu.Unlock()
			var want []string
			if tc.want == "resubmit" {
				want = []string{submitted("enable")}
			}
			if !slices.Equal(g.mutations, want) {
				t.Fatalf("mutations %v, want %v: only a removal that restarts the submission submits again", g.mutations, want)
			}
		})
	}
}

// A queue removal whose checks GitHub fails to answer past their first page is a failure the row
// retries, never a judgment of the page it read: failed_checks judged on that page alone would say
// GitHub reported no failing run, and an unexplained reason would read as a removal by someone. The
// retry, with the page answered, finds the failure on it.
func TestSubmitRetriesAQueueRemovalWhoseChecksGitHubFailsToRead(t *testing.T) {
	for _, reason := range []string{"failed_checks", unexplained} {
		t.Run(reason, func(t *testing.T) {
			g := &github{head: "head", mergeState: "CLEAN", queueRequired: true, autoMergeOK: true,
				timeline:     []string{enqueued, dequeued(reason, "github-merge-queue", pagedChecks)},
				contextPages: map[string]string{"after-100": secondChecksPage}, failPages: true}
			client, logs := clients(t, g)
			_, err := Submit(context.Background(), client, logs, ghrepo.MustParse("acme/widgets"), 42, "head", true, keeping("head"))
			var failed *MergeFailed
			var removed *Removed
			var refused *Refused
			if err == nil || errors.Is(err, wait.ErrWaiting) || errors.As(err, &failed) || errors.As(err, &removed) || errors.As(err, &refused) ||
				strings.Contains(err.Error(), "no failing run") || !strings.Contains(err.Error(), "after-100") {
				t.Fatalf("Submit with the queue commit's second page of checks unanswered = %v (%T), want a failure to retry naming the page", err, err)
			}
			g.mu.Lock()
			g.failPages = false
			g.mu.Unlock()
			_, err = Submit(context.Background(), client, logs, ghrepo.MustParse("acme/widgets"), 42, "head", true, keeping("head"))
			if !errors.As(err, &failed) || !strings.Contains(err.Error(), "integration (failure, "+failedRun+")") {
				t.Fatalf("the retry once GitHub answers the page = %v (%T), want MergeFailed naming the failure on it", err, err)
			}
			g.mu.Lock()
			defer g.mu.Unlock()
			if len(g.mutations) != 0 || g.pageReads != 2 {
				t.Fatalf("mutations %v and page reads %d, want none and one each run", g.mutations, g.pageReads)
			}
		})
	}
}

// failingJob is the Actions job the integration check run of a merge group ran as, the id GitHub
// answers as that check run's databaseId and the id its log is read by. The number is the one
// measured on run 42397923554 of cybermonklabs/legion.
const failingJob = 128479266994

// failedGroupWithJob is the commit the merge queue built, with one check run that passed and one,
// integration, that failed as Actions job failingJob.
const failedGroupWithJob = `{"oid":"group-sha","statusCheckRollup":{"contexts":{"nodes":[` +
	`{"__typename":"CheckRun","name":"lint","status":"COMPLETED","conclusion":"SUCCESS","detailsUrl":"` + failedRun + `/job/1","databaseId":1,"checkSuite":{"workflowRun":{"url":"` + failedRun + `"}}},` +
	`{"__typename":"CheckRun","name":"integration","status":"COMPLETED","conclusion":"FAILURE","detailsUrl":"` + failedRun + `/job/2","databaseId":128479266994,"checkSuite":{"workflowRun":{"url":"` + failedRun + `"}}}]}}}`

// goLog is the end of a Go test binary's log, where it prints a line for each test that failed,
// its subtests among them.
const goLog = `=== RUN   TestTheQueueTakesTheEntryOut
    removal_test.go:42: want the entry taken out, got it queued
--- FAIL: TestTheQueueTakesTheEntryOut (0.01s)
--- FAIL: TestTheNoticeNamesTheRun/a_run_that_failed (0.00s)
FAIL
exit status 1
FAIL	github.com/sjawhar/legion/daemon/internal/ghautomerge	0.241s
`

// browserLog is the end of a browser runner's log: a progress row of the kind it prints as it
// goes, then the summary block it ends with, which is measured verbatim from job 128479266994's
// log of run 42397923554, bytes 426,973 to the end.
const browserLog = `  ✘ 1463 [firefox] › e2e/deep-links.e2e.ts:442:1 › a deep link to a step (3.1s)

  1 failed
    [chromium] › e2e/gated-step-approvals.e2e.ts:386:1 › a gate with no step for the primary document, and the longest awaiting label, fits every width @phone
  37 skipped
  55 did not run
  1193 passed (15.4m)
`

// stamped is log as GitHub's own job-log route carries it, which is what the daemon reads: every
// line prefixed with the UTC timestamp GitHub writes before it and one space. Measured on job
// 128479266994 of run 42397923554 of cybermonklabs/legion, the job browserLog is quoted from:
// its summary block's count line reads "2026-11-07T18:04:24.8502586Z   1 failed" and its failing
// row "2026-11-07T18:04:24.8503263Z     [chromium] › e2e/gated-step-approvals.e2e.ts:386:1 › …",
// so no line of a real job log begins with the indentation a summary block prints (INFRA-1442).
func stamped(log string) string {
	lines := strings.Split(strings.TrimSuffix(log, "\n"), "\n")
	for i, line := range lines {
		lines[i] = fmt.Sprintf("2026-11-07T18:04:%02d.8502586Z %s", i%60, line)
	}
	return strings.Join(lines, "\n") + "\n"
}

// The runs a removal names say what failed in each of them: the daemon reads the end of each
// failing job's log and names the tests it states failed with the first line stating one, so the
// tree the merge queue ejected reads the cause instead of fetching and grepping a merge-group log
// (INFRA-1442). Go's "--- FAIL:" lines and a browser runner's summary block are the two shapes
// read; the "✘" rows that runner prints as it goes are not, since they stand in the middle of a
// stream this read only reaches the end of. A log the daemon could not read says so and which way
// it could not — the end states no test name, the body passed githubrest.JobLogBound, the read
// passed githubrest.JobLogTimeout, the fetch failed, or GitHub refused the read because this
// deployment's App was never granted GitHub's optional Actions read — and keeps the run link the
// notice carries today. No read of a log holds a removal up or fails the row: every case here is
// still the removal it was. The log of a check that passed is never read, and the names reach the
// merge queue's conflict removal too, which the daemon posts on the issue.
func TestTheFailingRunsNameWhatEachJobsLogSaysFailed(t *testing.T) {
	for _, tc := range []struct {
		name string
		// log is what the fake answers for the failing job, status the HTTP status it refuses
		// every log read with instead, fill how many bytes it writes before the log, and delay
		// how long it holds the answer open.
		log    string
		status int
		fill   int
		delay  time.Duration
		// conflict reads the removal as the queue's conflict, which the daemon submits once
		// more, rather than as the failed merge every other case is.
		conflict bool
		// wants is what the failing run's line must say, and absent what it must not.
		wants, absent []string
	}{
		{name: "a Go log naming the tests that failed", log: goLog,
			wants: []string{"integration (failure, " + failedRun + ")",
				"its log names TestTheQueueTakesTheEntryOut, TestTheNoticeNamesTheRun/a_run_that_failed as failing",
				`its first failure line "--- FAIL: TestTheQueueTakesTheEntryOut (0.01s)"`},
			absent: []string{"could not read", "removal_test.go"}},
		// Go's shape off that same route, measured on job 128558893568, whose first failure
		// line reads "2026-11-07T23:22:24.4790233Z --- FAIL: …": the names are the names, and
		// the first failure line is the line a reader would quote, with no timestamp in front.
		{name: "a Go log as GitHub's job log carries it", log: stamped(goLog),
			wants: []string{"integration (failure, " + failedRun + ")",
				"its log names TestTheQueueTakesTheEntryOut, TestTheNoticeNamesTheRun/a_run_that_failed as failing",
				`its first failure line "--- FAIL: TestTheQueueTakesTheEntryOut (0.01s)"`},
			absent: []string{"could not read", "removal_test.go", "2026-11-07"}},
		{name: "a browser runner's summary block", log: browserLog,
			wants: []string{"its log names e2e/gated-step-approvals.e2e.ts:386:1 › a gate with no step for the primary document, and the longest awaiting label, fits every width @phone as failing",
				`its first failure line "[chromium] › e2e/gated-step-approvals.e2e.ts:386:1 › a gate`},
			absent: []string{"could not read", "deep-links", "1463", "37 skipped"}},
		{name: "a browser runner's summary block as GitHub's job log carries it", log: stamped(browserLog),
			wants:  []string{"its log names e2e/gated-step-approvals.e2e.ts:386:1 › a gate with no step for the primary document, and the longest awaiting label, fits every width @phone as failing"},
			absent: []string{"could not read", "deep-links", "1463", "37 skipped"}},
		{name: "a log stating no test name", log: "Process completed with exit code 1.\n",
			wants:  []string{"integration (failure, " + failedRun + ")", "could not read its failing tests", "the end of its log states no test name"},
			absent: []string{"as failing"}},
		{name: "a fetch that failed", status: http.StatusBadGateway,
			wants: []string{"could not read its failing tests", "the fetch of its log failed", "502"}},
		{name: "a fetch refused for want of the Actions read", status: http.StatusForbidden,
			wants: []string{"could not read its failing tests", "GitHub refused the log",
				"never granted GitHub's optional Actions read"}},
		{name: "a log past the byte bound", log: goLog, fill: githubrest.JobLogBound + 1,
			wants:  []string{"could not read its failing tests", "passed the 16 MiB this read takes"},
			absent: []string{"as failing"}},
		{name: "a read past the time bound", log: goLog, delay: githubrest.JobLogTimeout + 2*time.Second,
			wants:  []string{"could not read its failing tests", "the read passed its 10s bound"},
			absent: []string{"as failing"}},
		{name: "a conflict removal whose commit carries a failing run", log: goLog, conflict: true,
			wants: []string{"integration (failure, " + failedRun + ")", "its log names TestTheQueueTakesTheEntryOut"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reason := "failed_checks"
			if tc.conflict {
				reason = "merge_conflict"
			}
			g := &github{head: "head", mergeState: "CLEAN", queueRequired: true, autoMergeOK: true,
				timeline:  []string{enqueued, dequeued(reason, "github-merge-queue", failedGroupWithJob)},
				jobLogs:   map[int64]string{failingJob: tc.log},
				logStatus: tc.status, logFill: tc.fill, logDelay: tc.delay}
			client, logs := clients(t, g)
			_, err := Submit(context.Background(), client, logs, ghrepo.MustParse("acme/widgets"), 42, "head", true, keeping("head"))
			var runs []string
			var failed *MergeFailed
			var conflict *ConflictRemoval
			switch {
			case tc.conflict:
				if !errors.As(err, &conflict) {
					t.Fatalf("Submit = %v (%T), want the queue's ConflictRemoval", err, err)
				}
				runs = conflict.Removal.Runs
			default:
				if !errors.As(err, &failed) {
					t.Fatalf("Submit = %v (%T), want MergeFailed: no read of a log fails the row", err, err)
				}
				runs = failed.Runs
			}
			if len(runs) != 1 {
				t.Fatalf("the removal names %v, want the one run that failed", runs)
			}
			for _, want := range tc.wants {
				if !strings.Contains(runs[0], want) {
					t.Fatalf("the failing run's line %q, want it to say %q", runs[0], want)
				}
			}
			for _, absent := range tc.absent {
				if strings.Contains(runs[0], absent) {
					t.Fatalf("the failing run's line %q says %q, want it left out", runs[0], absent)
				}
			}
			if !strings.Contains(err.Error(), runs[0]) {
				t.Fatalf("the removal %q, want it to carry the failing run's line %q", err, runs[0])
			}
			g.mu.Lock()
			defer g.mu.Unlock()
			if g.logReads != 1 {
				t.Fatalf("job logs read %d, want the one failing job's: a check that passed has no failure to name", g.logReads)
			}
		})
	}
}

// A row that has seen READY's head armed reads no timeline while the arm or entry stands: it waits
// on it (Armed) as before.
func TestSubmitWaitsOnAStandingArmWithoutReadingTheTimeline(t *testing.T) {
	g := &github{head: "head", mergeState: "CLEAN", queueRequired: true, queued: true}
	client, logs := clients(t, g)
	_, err := Submit(context.Background(), client, logs, ghrepo.MustParse("acme/widgets"), 42, "head", true, keeping("head"))
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
func TestAnArmedWaitCarriesTheMergeQueueEntryAndBaseOfItsRead(t *testing.T) {
	for _, tc := range []struct {
		name string
		g    *github
		// seen is the row having seen READY's head armed or queued before this run.
		seen bool
		want Armed
	}{
		{name: "a pull request the merge queue has an entry for", seen: true,
			g:    &github{head: "head", mergeState: "BLOCKED", queueRequired: true, queued: true, queueState: "AWAITING_CHECKS", queueBase: "queue-base", baseName: "release", baseTip: "release-tip"},
			want: Armed{QueueState: "AWAITING_CHECKS", QueueBase: "queue-base", BaseName: "release", BaseTip: "release-tip"}},
		{name: "an arm GitHub has just accepted",
			g:    &github{head: "head", mergeState: "BLOCKED", autoMergeOK: true},
			want: Armed{BaseName: baseBranch, BaseTip: baseOid}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, logs := clients(t, tc.g)
			_, err := Submit(context.Background(), client, logs, ghrepo.MustParse("acme/widgets"), 42, "head", tc.seen, keeping("head"))
			var armed *Armed
			if !errors.As(err, &armed) {
				t.Fatalf("Submit = %v, want an Armed wait", err)
			}
			want := tc.want
			want.Number, want.Repo, want.Head = 42, ghrepo.MustParse("acme/widgets"), "head"
			if *armed != want {
				t.Fatalf("Armed = %+v, want %+v", *armed, want)
			}
		})
	}
}

// An entry GitHub's merge queue cannot merge is a wait like any other entry (Armed), and the wait
// carries what the one read found of it: the queue's UNMERGEABLE state, the commit the queue built
// the entry on, and the branch the pull request merges into with that branch's tip. Submit sends
// no mutation at all for it — it neither submits the head again nor takes the entry out of the
// queue — whatever GitHub answers for the pull request's own merge state, which says nothing about
// an entry: what the daemon makes of the state is its row's (the daemon's queueConflict).
func TestAnUnmergeableEntryIsAWaitCarryingTheQueuesOwnState(t *testing.T) {
	for _, tc := range []struct {
		name string
		g    *github
		want Armed
	}{
		{name: "built on the base branch's tip",
			g:    &github{head: "head", mergeState: "UNKNOWN", queueRequired: true, queued: true, queueState: "UNMERGEABLE", queueBase: baseOid},
			want: Armed{QueueState: "UNMERGEABLE", QueueBase: baseOid, BaseName: baseBranch, BaseTip: baseOid}},
		{name: "built on an entry queued ahead of it",
			g:    &github{head: "head", mergeState: "UNKNOWN", queueRequired: true, queued: true, queueState: "UNMERGEABLE", queueBase: "entry-ahead"},
			want: Armed{QueueState: "UNMERGEABLE", QueueBase: "entry-ahead", BaseName: baseBranch, BaseTip: baseOid}},
		{name: "on another base branch, while GitHub reports the pull request clean",
			g: &github{head: "head", mergeState: "CLEAN", queueRequired: true, queued: true, queueState: "UNMERGEABLE", queueBase: "release-tip",
				baseName: "release", baseTip: "release-tip"},
			want: Armed{QueueState: "UNMERGEABLE", QueueBase: "release-tip", BaseName: "release", BaseTip: "release-tip"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, logs := clients(t, tc.g)
			_, err := Submit(context.Background(), client, logs, ghrepo.MustParse("acme/widgets"), 42, "head", true, keeping("head"))
			var armed *Armed
			if !errors.As(err, &armed) || !errors.Is(err, wait.ErrWaiting) {
				t.Fatalf("Submit on an unmergeable entry = %v, want an Armed wait", err)
			}
			want := tc.want
			want.Number, want.Repo, want.Head = 42, ghrepo.MustParse("acme/widgets"), "head"
			if *armed != want {
				t.Fatalf("Armed = %+v, want %+v", *armed, want)
			}
			tc.g.mu.Lock()
			defer tc.g.mu.Unlock()
			if len(tc.g.mutations) != 0 {
				t.Fatalf("mutations %v, want none for an entry the queue holds", tc.g.mutations)
			}
		})
	}
}

// A Lagging submission carries GitHub's last change to the pull request from the same read, and it
// is stalled only once that change has stood longer than LagBound, on GitHub's own clock: a
// disagreement GitHub made a moment ago is a webhook still on its way.
func TestALaggingSubmissionStallsOnlyOnGitHubsOwnClock(t *testing.T) {
	g := &github{state: "CLOSED", head: "head", mergeState: "UNKNOWN"}
	client, logs := clients(t, g)
	_, err := Submit(context.Background(), client, logs, ghrepo.MustParse("acme/widgets"), 42, "head", false, keeping("head"))
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
	client, logs := clients(t, g)
	_, err := Submit(context.Background(), client, logs, ghrepo.MustParse("acme/widgets"), 42, "head", false, keeping("head"))
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
			client, logs := clients(t, g)
			_, err := Submit(context.Background(), client, logs, ghrepo.MustParse("acme/widgets"), 42, "head", false, keeping("head"))
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
			client, logs := clients(t, tc.g)
			_, err := Submit(context.Background(), client, logs, ghrepo.MustParse("acme/widgets"), 42, "head", false, keeping("head"))
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
	client, logs := clients(t, g)
	_, err := Submit(context.Background(), client, logs, ghrepo.MustParse("acme/widgets"), 42, "head", false, keeping("head"))
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
