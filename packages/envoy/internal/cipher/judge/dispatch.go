package judge

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"

	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/urljoin"
	"github.com/sjawhar/envoy/redirects"
)

// ErrNoIssue is Dispatch answering 404 for the key a request's reason named: the key has an
// issue key's form, as record.IssueKeyInReason reads one, and no issue carries it. The judge
// shows the model that no such issue exists rather than handing the request over, so a run tells
// "no such issue" from "Dispatch could not be read" and records the one it met.
var ErrNoIssue = errors.New("dispatch holds no issue with this key")

// ErrNoAsk is Dispatch answering 404 for the ask a request's reason cited: the reference has an
// ask reference's form, as record.AskRefInReason reads one, and no ask carries that id. The judge
// shows the model that no such ask exists, as ErrNoIssue is for the issue.
var ErrNoAsk = errors.New("dispatch holds no ask with this id")

// maxDispatchAnswer bounds what one answer is read of, as the secret minters bound an upstream
// answer (secrets.mintResponseLimit). An issue read is a few KiB and a spec document a few tens
// of them; a megabyte is past anything either route sends, and an answer past it fails the read
// rather than being held in memory.
const maxDispatchAnswer = 1 << 20

// Dispatch is the broker's read of Dispatch, and it is a read alone. Issue makes two GETs, Ask
// one and Assignee one, and this type has no other method, no other verb and no body to send:
// nothing here writes an ask, a comment, a label or a status, and nothing reads a person's own
// data. Every route they read is authAny (internal/dispatch/api/routes_table.go), and the
// projected ServiceAccount token its account reads authenticates as a service actor holding no
// scope, which reaches an issue, its documents and the asks on it, and nothing that belongs to a
// person.
type Dispatch struct {
	// baseURL is the root Dispatch's /api/v1 paths hang off.
	baseURL string
	// account is the broker's own projected ServiceAccount token for Dispatch, read at call
	// time rather than once at boot: the kubelet rewrites such a file in place, and a reader
	// that caches for ciphertoken.AccountTokenCacheTTL picks a rotation up within 30 seconds,
	// so a rotated token reaches the judge with no restart. It is the interface rather than
	// the reader so a test can stand in for it; cmd/cipherd wires
	// ciphertoken.NewAccountToken("BROKER_JUDGE_DISPATCH_TOKEN_FILE", the file that setting names).
	account ciphertoken.Account
	// http is the client every call is made with.
	http *http.Client
}

// NewDispatch is the broker's read of the Dispatch at baseURL, presenting the token account reads
// on every call, made with client; a nil client is http.DefaultClient. It keeps a copy of either
// that follows no redirect (redirects.Refused), made once here.
func NewDispatch(baseURL string, account ciphertoken.Account, client *http.Client) Dispatch {
	return Dispatch{baseURL: baseURL, account: account, http: redirects.Refused(cmp.Or(client, http.DefaultClient))}
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
// Both calls carry the token its account reads, which is asked for before the first of them: a
// deployment whose token cannot be read makes no call at all, and a token file that holds
// nothing is that same refusal and never an empty bearer for Dispatch to turn down.
func (d Dispatch) Issue(ctx context.Context, key string) (Issue, error) {
	token, err := d.account.Read()
	if err != nil {
		return Issue{}, err
	}
	// Only these three fields of the issue read are decoded. The route answers the whole
	// issue with its artifacts, children and open asks (api.issueDetail), and the judge reads
	// none of that, nor the assignee, which Assignee decodes on its own so the shape of one
	// cannot break the other's read.
	var row struct {
		Key    string `json:"key"`
		Title  string `json:"title"`
		Status string `json:"status"`
	}
	if err := d.readIssue(ctx, token, key, &row); err != nil {
		return Issue{}, err
	}
	issue := Issue{Key: row.Key, Title: row.Title, Status: row.Status}

	var doc struct {
		Markdown string `json:"markdown"`
	}
	switch status, err := d.get(ctx, token, &doc, append(issueRoute(key), "artifacts/spec/text")...); {
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

// Assignee is the person the issue key names is assigned to, "" for an issue nobody is: the
// broker lists a Legion pod's request for a shared secret in that person's Inbox, the pod's claim
// naming the issue (requests.Machine.Assignees). It reads the issue alone, one GET under the
// token its account reads, decodes its assignee and nothing else, and answers ErrNoIssue and every
// other failure as Issue does. A null assignee decodes as "".
func (d Dispatch) Assignee(ctx context.Context, key string) (string, error) {
	token, err := d.account.Read()
	if err != nil {
		return "", err
	}
	var row struct {
		Assignee string `json:"assignee"`
	}
	if err := d.readIssue(ctx, token, key, &row); err != nil {
		return "", err
	}
	return row.Assignee, nil
}

// issueRoute is the route of the issue key names, as get joins it onto the Dispatch's path.
func issueRoute(key string) []string {
	return []string{"api/v1/issues", url.PathEscape(key)}
}

// readIssue makes the one issue GET Issue and Assignee share, decoding into what each of them
// reads of it: a 404 is ErrNoIssue and every other answer that is not 200 an error.
func (d Dispatch) readIssue(ctx context.Context, token, key string, into any) error {
	switch status, err := d.get(ctx, token, into, issueRoute(key)...); {
	case err != nil:
		return err
	case status == http.StatusNotFound:
		return fmt.Errorf("%w: %s", ErrNoIssue, key)
	case status != http.StatusOK:
		return fmt.Errorf("dispatch answered %d for issue %s", status, key)
	}
	return nil
}

// Ask is what the judge reads of one ask: which issue it belongs to, whether it is answered,
// and the whole of the exchange a person decided — the question, the options it offered, and
// the answer's own author, options and text. The model is shown the question with the answer,
// since an answer's meaning is in the question it answers.
type Ask struct {
	// ID, IssueKey and State are the ask as Dispatch shows it. State is one of open,
	// answered and resolved (model.SearchAsk.State); the model reads it with the rest, and
	// a key's guidance says which it needs.
	ID, IssueKey, State string
	// Question is the ask's text, Options the labels it offered, AnsweredBy the login of
	// the person who answered, Selected the option labels they chose and Text what they
	// wrote. The last three are empty for an ask nobody has answered.
	Question   string
	Options    []string
	AnsweredBy string
	Selected   []string
	Text       string
}

// Ask reads one ask. A 404 is ErrNoAsk — an id Dispatch holds no ask for, which the model is
// shown as not found — and every other answer that is not 200 is an error, which hands the
// request to its person.
// It carries the token its account reads, asked for before the call, so a deployment whose token
// cannot be read makes no call at all.
func (d Dispatch) Ask(ctx context.Context, id string) (Ask, error) {
	token, err := d.account.Read()
	if err != nil {
		return Ask{}, err
	}
	// Only these fields of the ask are decoded. The route answers the ask with its replies,
	// its edits, its followers and where its anchor stands (api.askRead), and the judge reads
	// none of that: an answer is the person's decision, and a thread under it is not.
	var row struct {
		Ask struct {
			ID       string  `json:"id"`
			IssueKey *string `json:"issue_key"`
			State    string  `json:"state"`
			Question string  `json:"question"`
			Options  []struct {
				Label string `json:"label"`
			} `json:"options"`
			Answer *struct {
				User     string   `json:"user"`
				Selected []string `json:"selected"`
				Text     *string  `json:"text"`
			} `json:"answer"`
		} `json:"ask"`
	}
	switch status, err := d.get(ctx, token, &row, "api/v1/asks", url.PathEscape(id)); {
	case err != nil:
		return Ask{}, err
	case status == http.StatusNotFound:
		return Ask{}, fmt.Errorf("%w: %s", ErrNoAsk, id)
	case status != http.StatusOK:
		return Ask{}, fmt.Errorf("dispatch answered %d for ask %s", status, id)
	}
	ask := Ask{ID: row.Ask.ID, State: row.Ask.State, Question: row.Ask.Question}
	if row.Ask.IssueKey != nil {
		ask.IssueKey = *row.Ask.IssueKey
	}
	for _, option := range row.Ask.Options {
		ask.Options = append(ask.Options, option.Label)
	}
	if answer := row.Ask.Answer; answer != nil {
		ask.AnsweredBy, ask.Selected = answer.User, answer.Selected
		if answer.Text != nil {
			ask.Text = *answer.Text
		}
	}
	return ask, nil
}

// get makes one authorized GET of route, its elements joined as path.Join joins them, onto the path
// of the Dispatch's URL (urljoin.Join) with its query kept and its fragment, which no request sends,
// dropped: a route appended to the URL's text would sit inside a query or a fragment, and Dispatch
// would be asked another path. On 200 alone it decodes as much of the answer as into names. It
// hands back whatever status came, so each caller decides for itself which of them is a failure; a
// call that was never answered, and a 200 whose body is past maxDispatchAnswer or is not the JSON
// into expects, is an error. A Dispatch URL that does not parse or names no host, or whose joined
// URL does not parse again (url.URL.String can write text url.Parse refuses, such as an IPv6 zone
// escape), is refused in the judge's own words: url.Parse's error, which http.NewRequest's carries,
// quotes the URL whole, user information and query included. BROKER_JUDGE_DISPATCH_URL is held to
// parse before the broker starts (config.Load); a Dispatch built directly is not.
func (d Dispatch) get(ctx context.Context, token string, into any, route ...string) (int, error) {
	endpoint, err := urljoin.Join(d.baseURL, path.Join(route...))
	if err != nil {
		return 0, errors.New("the judge's Dispatch read could not be built: the Dispatch URL does not parse")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return 0, errors.New("the judge's Dispatch read could not be built: the URL it joined does not parse")
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/json")
	answer, err := d.http.Do(request)
	if err != nil {
		return 0, fmt.Errorf("dispatch did not answer: %w", redact.Class(err))
	}
	defer answer.Body.Close()
	if answer.StatusCode != http.StatusOK {
		// A refusal's body says nothing a run acts on. It is drained to the same bound as data
		// no one reads, so the connection goes back to the pool.
		_, _ = io.Copy(io.Discard, redact.StreamAnswer(answer, maxDispatchAnswer))
		return answer.StatusCode, nil
	}
	read, err := redact.ReadAnswer(answer, maxDispatchAnswer)
	if err != nil {
		return answer.StatusCode, fmt.Errorf("dispatch's answer could not be read: %w", err)
	}
	if err := read.Decode(into); err != nil {
		return answer.StatusCode, fmt.Errorf("dispatch's answer could not be read: %w", err)
	}
	return answer.StatusCode, nil
}
