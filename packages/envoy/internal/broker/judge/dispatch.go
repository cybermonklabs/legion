package judge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/sjawhar/envoy/internal/brokertoken"
)

// ErrNoIssue is Dispatch answering 404 for the key a request's reason named: the key has an
// issue key's form, as record.IssueKeyInReason reads one, and no issue carries it. It is the
// judge's fifth check failing rather than the read failing, so a run tells "no such issue" from
// "Dispatch could not be read" and records the one it met.
var ErrNoIssue = errors.New("dispatch holds no issue with this key")

// maxDispatchAnswer bounds what one answer is read of, as the secret minters bound an upstream
// answer (secrets.mintResponseLimit). An issue read is a few KiB and a spec document a few tens
// of them; a megabyte is past anything either route sends, and an answer past it fails the read
// rather than being held in memory.
const maxDispatchAnswer = 1 << 20

// Dispatch is the broker's read of Dispatch, and it is a read alone. Issue makes two GETs and
// this type has no other method, no other verb and no body to send: nothing here writes an ask,
// a comment, a label or a status, and nothing reads a person's own data. Both routes are authAny
// (internal/dispatch/api/routes_table.go), and the projected ServiceAccount token Account reads
// authenticates as a service actor holding no scope, which reaches an issue and its documents
// and nothing that belongs to a person.
type Dispatch struct {
	// BaseURL is the root Dispatch's /api/v1 paths hang off.
	BaseURL string
	// Account is the broker's own projected ServiceAccount token for Dispatch, read at call
	// time rather than once at boot: the kubelet rewrites such a file in place, and a reader
	// that caches for brokertoken.AccountTokenCacheTTL picks a rotation up within 30 seconds,
	// so a rotated token reaches the judge with no restart. It is the interface rather than
	// the reader so a test can stand in for it; cmd/broker wires
	// brokertoken.NewAccountToken(BROKER_JUDGE_DISPATCH_TOKEN_FILE).
	Account brokertoken.Account
	// HTTP is the client both calls are made with; a nil client is http.DefaultClient.
	HTTP *http.Client
}

// Issue is what the judge reads of one issue: enough to tell whether the reason's issue is open
// and what it is about.
type Issue struct {
	// Key, Title and Status are the issue as Dispatch shows it.
	Key, Title, Status string
	// Spec is the whole markdown of the issue's spec document, and HasSpec whether Dispatch
	// holds one at all. An issue with no spec document is judged on its key, title and
	// status, and the data object in front of the model says the spec is absent (D14).
	Spec    string
	HasSpec bool
}

// Issue reads one issue and its spec document. The issue itself must be there: a 404 is
// ErrNoIssue and every other answer that is not 200 is an error, since a judge that cannot tell
// whether the issue is open decides nothing. The spec document need not be: a 404, and an answer
// that is no document, leave HasSpec false and are no error at all.
//
// Both calls carry the token Account reads, which is asked for before the first of them: a
// deployment whose token cannot be read makes no call at all, and a token file that holds
// nothing is that same refusal and never an empty bearer for Dispatch to turn down.
func (d Dispatch) Issue(ctx context.Context, key string) (Issue, error) {
	token, err := d.Account.Read()
	if err != nil {
		return Issue{}, err
	}
	base := strings.TrimSuffix(d.BaseURL, "/") + "/api/v1/issues/" + url.PathEscape(key)
	// Only these three fields of the issue read are decoded. The route answers the whole
	// issue with its artifacts, children and open asks (api.issueDetail), and the judge
	// reads none of that.
	var row struct {
		Key    string `json:"key"`
		Title  string `json:"title"`
		Status string `json:"status"`
	}
	switch status, err := d.get(ctx, base, token, &row); {
	case err != nil:
		return Issue{}, err
	case status == http.StatusNotFound:
		return Issue{}, fmt.Errorf("%w: %s", ErrNoIssue, key)
	case status != http.StatusOK:
		return Issue{}, fmt.Errorf("dispatch answered %d for issue %s", status, key)
	}
	issue := Issue{Key: row.Key, Title: row.Title, Status: row.Status}

	var doc struct {
		Markdown string `json:"markdown"`
	}
	switch status, err := d.get(ctx, base+"/artifacts/spec/text", token, &doc); {
	case err != nil:
		return Issue{}, err
	case status == http.StatusOK:
		issue.Spec, issue.HasSpec = doc.Markdown, true
	case status == http.StatusNotFound, status == http.StatusBadRequest:
		// The issue holds no document under that name (404), or what it holds is no
		// document at all or names more than one (400, the route's NOT_DOCUMENT and
		// ARTIFACT_REFERENCE_AMBIGUOUS). Either way there is no markdown to show the
		// model, and an issue with no spec is still an issue the judge decides on.
	default:
		return Issue{}, fmt.Errorf("dispatch answered %d for the spec of issue %s", status, key)
	}
	return issue, nil
}

// get makes one authorized GET and, on 200 alone, decodes as much of the answer as into names.
// It hands back whatever status came, so each caller decides for itself which of them is a
// failure; a call that was never answered, and a 200 whose body is past maxDispatchAnswer or is
// not the JSON into expects, is an error.
func (d Dispatch) get(ctx context.Context, endpoint, token string, into any) (int, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return 0, fmt.Errorf("the judge's Dispatch read could not be built: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/json")
	client := d.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	answer, err := client.Do(request)
	if err != nil {
		return 0, fmt.Errorf("dispatch did not answer: %w", err)
	}
	defer answer.Body.Close()
	if answer.StatusCode != http.StatusOK {
		// A refusal's body says nothing a run acts on. It is drained to the same bound
		// rather than read, so the connection goes back to the pool.
		_, _ = io.Copy(io.Discard, io.LimitReader(answer.Body, maxDispatchAnswer))
		return answer.StatusCode, nil
	}
	if err := json.NewDecoder(io.LimitReader(answer.Body, maxDispatchAnswer)).Decode(into); err != nil {
		return answer.StatusCode, fmt.Errorf("dispatch's answer could not be read: %w", err)
	}
	return answer.StatusCode, nil
}
