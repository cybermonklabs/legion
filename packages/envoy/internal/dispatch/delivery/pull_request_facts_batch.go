package delivery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/githubreads"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/logging"
)

// One GraphQL query reads the facts of at most FactsBatchSize pull requests of one installation,
// from any of its repositories (readFactsBatch). A query of 20 pull requests costs one point of
// the installation's GraphQL budget: GitHub adds the requests of each connection, divides by 100
// and rounds, and the least cost is 1. An answer of 50 pull requests of one busy repository passed
// the client's 1 MiB, so a batch holds 20, and an answer above the limit splits the batch.

// FactsBatchSize is the most pull requests that one query of readFactsBatch reads.
const FactsBatchSize = 20

// FactsQueriesPerPass is the most GraphQL requests that one pass sends for each installation to
// read facts. A split of an answer above the client's limit counts as a request too.
const FactsQueriesPerPass = 25

// PullRequestRef is one pull request that a batched read asks GitHub about. Owner and Repo are the
// canonical name of its repository (refs.CanonicalRepo). StoredUnder is the name of its stored
// results row where that is another name, and else the canonical name. The writer of the answer
// drops the row under StoredUnder (upsertHeadUnderCanonicalName).
type PullRequestRef struct {
	Owner, Repo string
	Number      int
	StoredUnder string
}

// batchAnswer is what readFactsBatch learned about one PullRequestRef.
//
// Head is the head row that GitHub's answer makes, with its texts, under the canonical name: the
// row the caller writes (upsertHeadUnderCanonicalName). It carries the place and the time of the
// query that answered it. Absent says that GitHub answered the pull request null with NOT_FOUND
// on its own alias path, and that its repository resolved. Stored says that the pull request was
// read and stored by the refresh (RefreshPullRequestHead), so Head is nil and the caller writes
// nothing.
type batchAnswer struct {
	Head           *PullRequestHead
	Absent, Stored bool
}

// factsHeadFields is what both batched queries read of each pull request for its head row and its
// texts (factsNode, payload).
const factsHeadFields = `number title body url state merged mergedAt updatedAt headRefName headRefOid baseRefName
  headRepository { nameWithOwner } mergeCommit { oid }`

// factsFields is what the query reads of each pull request: the fields of its head row and its
// texts, and its first 100 commit messages with the count of its commits.
const factsFields = `fragment pullRequestFacts on PullRequest {
  ` + factsHeadFields + `
  commits(first: 100) { totalCount nodes { commit { message } } }
}
`

// commitPullsFields is what the query of many commits' pull requests reads of each pull request
// (commitPullsQuery): factsFields without the commit messages. factsNode.payload reads none of
// them, and the read stores each pull request through storePullRequestFacts, which reads its
// commit messages itself, so a page of them in this answer only makes it larger and splits a
// batch for its size sooner. It keeps factsFields' fragment name, so both answers decode into
// factsNode alike.
const commitPullsFields = `fragment pullRequestFacts on PullRequest {
  ` + factsHeadFields + `
}
`

// factsPage is the number of commit messages that the query reads of each pull request, and the
// size of each REST page of commits.
const factsPage = 100

// readFactsBatch reads the facts of refs from GitHub, with token. refs are at most FactsBatchSize
// pull requests of one installation, from any of its repositories, and token is that
// installation's. It sends one GraphQL query, which has one alias r<i> for each repository and,
// inside it, one alias p<n> for each number. It writes no row: the caller writes each answer that
// holds a Head (upsertHeadUnderCanonicalName).
//
// The query takes one place in the order of every read of GitHub (startRead), just before it goes
// out. Each Head that its answer makes carries that place and its time, where GitHub answered an
// updatedAt, as headRow does for a REST read. So the trigger pull_request_heads_older_read orders
// a batched read with every other read.
//
// Each answer holds the texts as every writer stores them (storePullRequestFacts). A fork's head
// has its messages read and empty. A pull request of more than 100 commits pages the rest of its
// messages over REST with token, from the second page (fetchCommitMessagesWithToken). That read
// keeps the place of its query. Where that read fails, the answer holds the messages not read, and
// the answer is not known until a later read stores them. Each Head is stamped checked at now,
// because the reconcile's read comes again, as RefreshPullRequestHead stamps each read.
//
// An answer above the client's limit (*githubapp.ResponseTooLargeError) splits the refs into two
// halves, and each half is asked again with a place of its own. One pull request whose answer is
// still above the limit is read by RefreshPullRequestHead with ref.StoredUnder, which stores it.
// Its answer is Stored. A refusal of that read leaves the ref out of the answers.
//
// A ref is out of the answers where GitHub gave nothing for it: its repository answered null, or
// it answered null with no NOT_FOUND on its own path. GitHub answers a repository null alike when
// the token does not read it and when it does not exist, so that is no proof of absence.
//
// An error ends the read: a failed query, an answer that does not decode, a rate limit of the
// query or of a REST read, a place not taken, or a failed refresh. The answers of the requests
// before it stay in the map that comes back with the error.
func (a *Attributor) readFactsBatch(
	ctx context.Context, pool *store.Pool, client *githubapp.Client,
	installations *passInstallations, token string, refs []PullRequestRef, now time.Time,
) (map[PullRequestRef]batchAnswer, error) {
	return a.readFactsBatchWithin(ctx, pool, client, installations, token, refs, now, nil)
}

// readFactsBatchWithin is readFactsBatch, each of whose GraphQL requests, splits included, takes
// one request of budget first. A request budget does not cover is not sent: the read ends with
// errFactsBudgetSpent and the answers of the requests before it. A nil budget sends every request.
func (a *Attributor) readFactsBatchWithin(
	ctx context.Context, pool *store.Pool, client *githubapp.Client,
	installations *passInstallations, token string, refs []PullRequestRef, now time.Time, budget *factsBudget,
) (map[PullRequestRef]batchAnswer, error) {
	if len(refs) > FactsBatchSize {
		return nil, fmt.Errorf("a batched read of facts takes at most %d pull requests, not %d", FactsBatchSize, len(refs))
	}
	answers := make(map[PullRequestRef]batchAnswer, len(refs))
	batch := factsRead{attributor: a, pool: pool, client: client, installations: installations, token: token, now: now, budget: budget, answers: answers}
	return answers, batch.query(ctx, refs)
}

// errFactsBudgetSpent ends a batched read whose installation has sent every facts query a pass
// may send (FactsQueriesPerPass).
var errFactsBudgetSpent = errors.New("the pass has sent every query of facts it may send for this installation")

// factsBudget is the GraphQL requests one installation may still send in one pass to read facts:
// FactsQueriesPerPass at the start, shared by the open arm and the facts line
// (passInstallations.factsBudget).
type factsBudget struct {
	mu   sync.Mutex
	left int
}

// take takes one request of the budget, and answers false where none is left.
func (b *factsBudget) take() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.left <= 0 {
		return false
	}
	b.left--
	return true
}

// remaining is the requests the budget still covers.
func (b *factsBudget) remaining() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.left
}

// factsBudget is the budget of the installation scope (githubapp.Client.TokenScope) in this pass,
// made at FactsQueriesPerPass where the pass has none yet. A nil passInstallations, a caller with
// no pass, gets a new budget at each call.
func (p *passInstallations) factsBudget(scope string) *factsBudget {
	if p == nil {
		return &factsBudget{left: FactsQueriesPerPass}
	}
	budget, _ := p.budgets.LoadOrStore(scope, &factsBudget{left: FactsQueriesPerPass})
	return budget.(*factsBudget)
}

// factsRead is one call of readFactsBatch: what each of its requests needs, and the answers it
// collects.
type factsRead struct {
	attributor    *Attributor
	pool          *store.Pool
	client        *githubapp.Client
	installations *passInstallations
	token         string
	now           time.Time
	budget        *factsBudget
	answers       map[PullRequestRef]batchAnswer
}

// factsKey is one pull request of a query: its canonical repository and its number. Two refs of
// one pull request share one alias and one answer.
type factsKey struct {
	owner, repo string
	number      int
}

// factsRepository is one repository alias of a query, and the numbers it asks for.
type factsRepository struct {
	alias, owner, repo string
	numbers            []int
}

// query asks GitHub about refs in one request, and splits refs where the answer is above the
// client's limit.
func (f *factsRead) query(ctx context.Context, refs []PullRequestRef) error {
	if len(refs) == 0 {
		return nil
	}
	if f.budget != nil && !f.budget.take() {
		return errFactsBudgetSpent
	}
	text, variables, repositories := factsQuery(refs)
	read, err := startRead(ctx, f.pool)
	if err != nil {
		return err
	}
	body, status, _, err := f.client.GraphQL(ctx, f.token, text, variables)
	var tooLarge *githubapp.ResponseTooLargeError
	switch {
	case errors.As(err, &tooLarge) && len(refs) == 1:
		return f.refresh(ctx, refs[0])
	case errors.As(err, &tooLarge):
		half := len(refs) / 2
		if err := f.query(ctx, refs[:half]); err != nil {
			return err
		}
		return f.query(ctx, refs[half:])
	case err != nil:
		return fmt.Errorf("query the facts of %d pull requests: %w", len(refs), err)
	case status != http.StatusOK:
		return fmt.Errorf("query the facts of %d pull requests: status %d: %s", len(refs), status, githubapp.ShownBody(body))
	}
	learned, err := f.decode(ctx, body, repositories, read)
	for _, ref := range refs {
		if answer, found := learned[factsKey{ref.Owner, ref.Repo, ref.Number}]; found {
			f.answers[ref] = answer
		}
	}
	return err
}

// refresh reads one pull request whose answer alone is above the client's limit, over REST, and
// stores it (RefreshPullRequestHead). It passes ref.StoredUnder, so the write drops the row under
// an old name too.
func (f *factsRead) refresh(ctx context.Context, ref PullRequestRef) error {
	_, refused, err := f.attributor.RefreshPullRequestHead(ctx, f.pool, f.client, f.installations, ref.StoredUnder, ref.Number, f.now)
	if err != nil {
		return fmt.Errorf("read %s/%s#%d, whose facts are above the limit of one answer: %w", ref.Owner, ref.Repo, ref.Number, err)
	}
	if refused == nil {
		f.answers[ref] = batchAnswer{Stored: true}
	}
	return nil
}

// factsQuery is the text and the variables of the query of refs, and its repository aliases in
// their order. The owner and the name of each repository go in variables. Each number goes in the
// text, since GraphQL's Int holds every number that a pull request can have.
func factsQuery(refs []PullRequestRef) (string, map[string]any, []factsRepository) {
	var repositories []factsRepository
	for _, ref := range refs {
		at := slices.IndexFunc(repositories, func(r factsRepository) bool { return r.owner == ref.Owner && r.repo == ref.Repo })
		if at < 0 {
			at = len(repositories)
			repositories = append(repositories, factsRepository{alias: "r" + strconv.Itoa(at), owner: ref.Owner, repo: ref.Repo})
		}
		if !slices.Contains(repositories[at].numbers, ref.Number) {
			repositories[at].numbers = append(repositories[at].numbers, ref.Number)
		}
	}
	variables := make(map[string]any, 2*len(repositories))
	var text strings.Builder
	text.WriteString("query(")
	for i, repository := range repositories {
		if i > 0 {
			text.WriteString(", ")
		}
		fmt.Fprintf(&text, "$o%d: String!, $n%d: String!", i, i)
		variables["o"+strconv.Itoa(i)], variables["n"+strconv.Itoa(i)] = repository.owner, repository.repo
	}
	text.WriteString(") {\n")
	for i, repository := range repositories {
		fmt.Fprintf(&text, "  %s: repository(owner: $o%d, name: $n%d) {\n", repository.alias, i, i)
		for _, number := range repository.numbers {
			fmt.Fprintf(&text, "    p%d: pullRequest(number: %d) { ...pullRequestFacts }\n", number, number)
		}
		text.WriteString("  }\n")
	}
	text.WriteString("}\n")
	text.WriteString(factsFields)
	return text.String(), variables, repositories
}

// factsNode is one pull request as the query answers it.
type factsNode struct {
	Number         int        `json:"number"`
	Title          string     `json:"title"`
	Body           string     `json:"body"`
	URL            string     `json:"url"`
	State          string     `json:"state"`
	Merged         bool       `json:"merged"`
	MergedAt       *time.Time `json:"mergedAt"`
	UpdatedAt      *time.Time `json:"updatedAt"`
	HeadRefName    string     `json:"headRefName"`
	HeadRefOid     string     `json:"headRefOid"`
	BaseRefName    string     `json:"baseRefName"`
	HeadRepository *struct {
		NameWithOwner string `json:"nameWithOwner"`
	} `json:"headRepository"`
	MergeCommit *struct {
		Oid string `json:"oid"`
	} `json:"mergeCommit"`
	Commits struct {
		TotalCount int `json:"totalCount"`
		Nodes      []struct {
			Commit struct {
				Message string `json:"message"`
			} `json:"commit"`
		} `json:"nodes"`
	} `json:"commits"`
}

// payload is the node in the shape of GitHub's REST answer (pullRequestPayload), with the fields
// that the query reads. GraphQL's state MERGED is REST's closed with merged true.
func (n factsNode) payload() pullRequestPayload {
	var p pullRequestPayload
	p.Number, p.Title, p.Body, p.HTMLURL = n.Number, n.Title, n.Body, n.URL
	p.State = "open"
	if n.State != "OPEN" {
		p.State = "closed"
	}
	p.Merged, p.MergedAt, p.UpdatedAt = n.Merged || n.State == "MERGED", n.MergedAt, n.UpdatedAt
	p.Head.Ref, p.Head.SHA, p.Base.Ref = n.HeadRefName, n.HeadRefOid, n.BaseRefName
	if n.HeadRepository != nil {
		p.Head.Repo = &struct {
			FullName string `json:"full_name"`
		}{FullName: n.HeadRepository.NameWithOwner}
	}
	if n.MergeCommit != nil {
		p.MergeCommitSHA = &n.MergeCommit.Oid
	}
	return p
}

// decode reads GitHub's answer to one query of repositories, which took the place read. A rate
// limit of a REST read of the rest of the messages comes back with the answers. After it, no
// further REST read is sent, and each answer that needs one holds its messages not read.
func (f *factsRead) decode(ctx context.Context, body []byte, repositories []factsRepository, read readStart) (map[factsKey]batchAnswer, error) {
	var answer struct {
		Data   map[string]json.RawMessage `json:"data"`
		Errors []struct {
			Type string `json:"type"`
			Path []any  `json:"path"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		return nil, fmt.Errorf("decode the answer to the facts query: %w", err)
	}
	if answer.Data == nil {
		return nil, fmt.Errorf("GitHub answered the facts query with no data: %s", githubapp.ShownBody(body))
	}
	absent := map[[2]string]bool{}
	for _, refusal := range answer.Errors {
		if refusal.Type != "NOT_FOUND" || len(refusal.Path) != 2 {
			continue
		}
		repository, repoOK := refusal.Path[0].(string)
		pull, pullOK := refusal.Path[1].(string)
		if repoOK && pullOK {
			absent[[2]string{repository, pull}] = true
		}
	}
	learned := map[factsKey]batchAnswer{}
	var limited error
	for _, repository := range repositories {
		raw := answer.Data[repository.alias]
		if isJSONNull(raw) {
			continue
		}
		var pulls map[string]json.RawMessage
		if err := json.Unmarshal(raw, &pulls); err != nil {
			return learned, fmt.Errorf("decode the answer of %s/%s to the facts query: %w", repository.owner, repository.repo, err)
		}
		for _, number := range repository.numbers {
			key, alias := factsKey{repository.owner, repository.repo, number}, "p"+strconv.Itoa(number)
			rawPull := pulls[alias]
			if isJSONNull(rawPull) {
				if absent[[2]string{repository.alias, alias}] {
					learned[key] = batchAnswer{Absent: true}
				}
				continue
			}
			var node factsNode
			if err := json.Unmarshal(rawPull, &node); err != nil {
				return learned, fmt.Errorf("decode the answer of %s/%s#%d to the facts query: %w", repository.owner, repository.repo, number, err)
			}
			var facts batchAnswer
			facts, limited = f.answer(ctx, key, node, read, limited)
			learned[key] = facts
		}
	}
	return learned, limited
}

// answer is the batchAnswer of one pull request that the query answered with node. limited is a
// rate limit that an earlier REST read of this answer met, and the rate limit after this one.
func (f *factsRead) answer(ctx context.Context, key factsKey, node factsNode, read readStart, limited error) (batchAnswer, error) {
	canonical := key.owner + "/" + key.repo
	payload := node.payload()
	texts := &PullRequestTexts{HeadRepo: headRepositoryOf(payload), BaseRef: payload.Base.Ref, Title: payload.Title, Body: payload.Body}
	first := make([]string, len(node.Commits.Nodes))
	for i, commit := range node.Commits.Nodes {
		first[i] = commit.Commit.Message
	}
	switch {
	case !headInOwnRepository(canonical, texts.HeadRepo):
		texts.CommitMessages, texts.MessagesRead = []string{}, true
	case node.Commits.TotalCount <= len(first):
		texts.CommitMessages, texts.MessagesRead = first, true
	case limited == nil:
		// The query answers the first page. Where it answers less than a page of a pull request
		// of more commits, the pages do not line up, and the read takes every page again.
		from, kept := 2, first
		if len(first) != factsPage {
			from, kept = 1, []string{}
		}
		rest, _, err := fetchCommitMessagesWithToken(ctx, f.client, func() (string, error) { return f.token, nil }, key.owner, key.repo, key.number, from)
		if _, ok := githubapp.AsRateLimit(err); ok {
			limited = err
		} else if err != nil {
			logging.FromContext(ctx).Debug("dispatch delivery: the commit messages of a pull request are not read, so its answer is not known until a later read",
				"repo", canonical, "number", key.number, "error", err)
			// The attempt that ctx carries notes this failed read (githubreads), as
			// storePullRequestFacts notes its own.
			githubreads.NoteFailuref(ctx, "read the commit messages of %s PR #%d: %w", canonical, key.number, err)
		} else {
			texts.CommitMessages, texts.MessagesRead = append(kept, rest...), true
		}
	}
	head := headRow(canonical, payload, &f.now, read)
	head.Texts = texts
	return batchAnswer{Head: &head}, limited
}

// isJSONNull reports whether raw is JSON's null, or absent.
func isJSONNull(raw json.RawMessage) bool {
	return len(raw) == 0 || bytes.Equal(raw, []byte("null"))
}
