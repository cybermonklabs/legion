// packages/envoy/internal/cipher/judge/model_words_test.go
// The model's own words where the judge keeps them - a run that ended on a fault, whose error and
// raw answer the request.judged row records, and a well-formed answer's reason, which reaches the
// row, the requesting agent's refusal and the verdict's log line - when whatever answers in the
// model's place echoes the bearer the judge presented to it.
package judge

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/agentloop"
	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// judgeBearer is the bearer the judge presents to its model in these rows: made up, and holding
// two of the gate's markers (eyJhbGci, the start of every JWS, as the identity token a deployment
// presents begins, and -RDX-), so the output a row captures names a marker wherever it holds it.
const judgeBearer = "eyJhbGciOiJSUzI1NiJ9.judge-RDX-bearer.c2lnbmF0dXJlLWp1ZGdlcw"

// judgeRun is a run of 12 of judgeBearer's characters, past the 8 at which redact.Tokens takes a
// run wherever it stands (redact.TokenPiece).
const judgeRun = "hbGciOiJSUzI1"

// judgeEcho is words that echo a URL holding a password, the bearer whole and a run of it, as
// whatever answers in the model's place can write them. The rule for remote text reads them as
// clean, so what keeps the bearer out is the run's own record of what it sent, and what keeps the
// password out of a message is the rule.
const judgeEcho = "the issuer at " + judgePasswordURL + ", then echo " + judgeBearer + ", a run " + judgeRun

// judgePasswordURL is a URL whose user information holds the probe's user name and password.
const judgePasswordURL = "https://" + redacttest.User + ":" + redacttest.Password + "@issuer.example/path"

// wordsOptions judges with the Dispatch stand-in d and the machine r, under the rules that give
// the judge testSecret, against the model endpoint at model, which is presented judgeBearer.
func wordsOptions(t *testing.T, d *dispatchStandIn, model string, r *fakeRequests) Options {
	t.Helper()
	return Options{
		Rules:    testRules(),
		Dispatch: Dispatch{BaseURL: d.server.URL, Account: ciphertoken.NewAccountToken(tokenFile(t, "projected-token")), HTTP: d.server.Client()},
		Model: Model{ID: testModel, Endpoint: agentloop.Endpoint{
			Kind: agentloop.OpenAIChat, BaseURL: model,
			Token: func(context.Context) (string, error) { return judgeBearer, nil },
		}},
		Requests: r,
	}
}

// echoingModel is a model endpoint speaking Chat Completions that answers every call with body,
// an event stream, and records the Authorization of the first call it got.
type echoingModel struct {
	server *httptest.Server
	got    chan string
}

func newEchoingModel(t *testing.T, body string) *echoingModel {
	t.Helper()
	m := &echoingModel{got: make(chan string, 1)}
	m.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case m.got <- r.Header.Get("Authorization"):
		default:
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(m.server.Close)
	return m
}

// presentedBearer fails t unless the model's first call carried judgeBearer, so the row's echo is
// of the bearer the judge sent it.
func (m *echoingModel) presentedBearer(t *testing.T) {
	t.Helper()
	select {
	case got := <-m.got:
		if got != "Bearer "+judgeBearer {
			t.Fatalf("the model was presented %q, want the bearer the row echoes", got)
		}
	default:
		t.Fatal("the model was never asked, so its echo is of nothing the judge sent")
	}
}

// refusalStream is the event stream Chat Completions sends for a reply the model refused, the
// refusal in the choice's delta, as OpenAI streams one.
func refusalStream(refusal string) string {
	var out strings.Builder
	chunk := func(choices []any, usage any) {
		body := map[string]any{"id": "chatcmpl-standin", "object": "chat.completion.chunk", "created": 1, "model": testModel, "choices": choices}
		if usage != nil {
			body["usage"] = usage
		}
		encoded, err := json.Marshal(body)
		if err != nil {
			panic(err)
		}
		fmt.Fprintf(&out, "data: %s\n\n", encoded)
	}
	choice := func(delta map[string]any, finish any) []any {
		return []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}
	}
	chunk(choice(map[string]any{"role": "assistant", "content": ""}, nil), nil)
	chunk(choice(map[string]any{"refusal": refusal}, nil), nil)
	chunk(choice(map[string]any{}, "stop"), nil)
	chunk([]any{}, map[string]any{"prompt_tokens": 20, "completion_tokens": 4, "total_tokens": 24})
	out.WriteString("data: [DONE]\n\n")
	return out.String()
}

// judgedWords is one run of the judge and what it left: the row it wrote (through whichever of the
// three writes), the refusal it gave the agent (empty when it refused nothing), and its one log
// line, as the attributes the JSON handler wrote.
type judgedWords struct {
	row     map[string]any
	refusal string
	logged  map[string]any
}

// judgeWords runs the judge once on pending(), every check passing, against a model that answers
// with body and is presented judgeBearer, with each of adjust applied to its options, and reads
// back what it wrote.
func judgeWords(t *testing.T, body string, adjust ...func(*Options)) judgedWords {
	t.Helper()
	var log logtest.Buffer
	logtest.SetDefault(t, slog.NewJSONHandler(&log, nil))
	model := newEchoingModel(t, body)
	machine := newRequests()
	opts := wordsOptions(t, newDispatch(t, "doing", testSpec), model.server.URL, machine)
	for _, change := range adjust {
		change(&opts)
	}
	New(t.Context(), opts).consider(t.Context(), pending())
	model.presentedBearer(t)
	applied, logged := machine.rows()
	denied, refusals := machine.refused()
	var got judgedWords
	switch {
	case len(applied)+len(logged)+len(denied) != 1:
		t.Fatalf("the run applied %d decisions, logged %d rows and refused %d requests; want one write", len(applied), len(logged), len(denied))
	case len(applied) == 1:
		got.row = applied[0]
	case len(logged) == 1:
		got.row = logged[0]
	default:
		got.row, got.refusal = denied[0], refusals[0]
	}
	line := log.Lines("broker judge: wrote a verdict")
	if err := json.Unmarshal([]byte(line), &got.logged); err != nil {
		t.Fatalf("the run's verdict line is no one JSON line: %v\n%s", err, log.String())
	}
	return got
}

// rowText is the value under key of a row or a log line as it holds it, "" where it holds none.
func rowText(t *testing.T, row map[string]any, key string) string {
	t.Helper()
	value, ok := row[key]
	if !ok {
		return ""
	}
	text, ok := value.(string)
	if !ok {
		t.Fatalf("the %s is %T, want a string", key, value)
	}
	return text
}

// requireNoRunOfTheBearer captures text under capture and fails t, naming capture, where it holds
// a run of judgeBearer (redacttest.Piece). It is the designated assertion for the raw answer,
// which the judge keeps as the answer's data: the bearer the run presented never stands in it,
// while the URL the model wrote, which carries no credential of the run's, stays as it came.
func requireNoRunOfTheBearer(t *testing.T, capture, text string) {
	t.Helper()
	redacttest.Capture(t, capture, text)
	if piece := redacttest.Piece(text, judgeBearer); piece != "" {
		t.Errorf("REDACT-ASSERT %s: the output prints %q, a run of the bearer the judge presented: %q", capture, piece, text)
	}
}

// TestTheJudgeKeepsNoBearerItsModelEchoed: whatever answers in the judge's model's place echoes
// the bearer the judge presented, whole, a run of it, and a URL that holds a password, in each of
// the model's own words the judge keeps. A run that ended on a fault - a refusal (refusal), a last
// reply that is no object of the judge's form (malformed), a reply with a property the form
// forbids, which the SDK's error quotes (forbidden-key) - is handed to its person: its row's error
// and its log line hold no secret, and its raw answer, the answer's data, which keeps the model's
// URL as it came, holds no run of the bearer. A well-formed deny whose reason echoes it (deny) is
// refused: the agent's refusal text, the row's reason and the log line's reason and refusal hold
// no secret. A well-formed grant whose reason echoes it (grant) is approved: the row's reason and
// the log line hold none. A deny (split-deny) and a malformed reply (split-malformed) whose reason
// is the bearer with a zero-width space after every 6 of its characters, which no reason may carry
// and record.Sanitize drops, keep no run of it either: the judge drops those runes before it reads
// the words, so the bearer is whole when the run's credentials are redacted from it. Captures
// judge.model-words/<case>/<place>.
func TestTheJudgeKeepsNoBearerItsModelEchoed(t *testing.T) {
	usage := chatTokens{prompt: 20, completion: 4}
	answer := func(verdict, reason string) string {
		return chatStream(`{"verdict":"`+verdict+`","reason":`+quoted(reason)+`}`, usage)
	}
	for _, c := range []struct {
		name, body string
	}{
		{"refusal", refusalStream(judgeEcho)},
		{"malformed", answer("maybe", judgeEcho)},
		{"forbidden-key", chatStream(`{"verdict":"grant","reason":`+quoted("the issuer at "+judgePasswordURL+", then a run "+judgeRun)+`,`+quoted(judgeBearer)+`:1}`, usage)},
		{"deny", answer(Deny, judgeEcho)},
		{"grant", answer(Grant, judgeEcho)},
		{"split-deny", answer(Deny, "echo "+splitBearer)},
		{"split-malformed", answer("maybe", "echo "+splitBearer)},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := judgeWords(t, c.body)
			capture := "judge.model-words/" + c.name + "/"
			switch strings.TrimPrefix(c.name, "split-") {
			case "deny":
				redacttest.RequireNoPiece(t, capture+"refusal", got.refusal, judgeBearer)
				redacttest.RequireNoPiece(t, capture+"reason", rowText(t, got.row, "reason"), judgeBearer)
				if got.refusal == "" {
					t.Errorf("a deny verdict refused nothing: %v", got.row)
				}
			case "grant":
				redacttest.RequireNoPiece(t, capture+"reason", rowText(t, got.row, "reason"), judgeBearer)
			default:
				redacttest.RequireNoPiece(t, capture+"error", rowText(t, got.row, "error"), judgeBearer)
				requireNoRunOfTheBearer(t, capture+"raw-answer", rowText(t, got.row, "raw_answer"))
				if rowText(t, got.row, "error") == "" {
					t.Errorf("the row of a run that ended on a fault records no error: %v", got.row)
				}
			}
			// The log line carries the raw answer too, which keeps the model's URL as the row
			// does; it holds no run of the bearer, and the rest of the line holds no secret.
			requireNoRunOfTheBearer(t, capture+"log-raw-answer", rowText(t, got.logged, "raw_answer"))
			delete(got.logged, "raw_answer")
			rest, err := json.Marshal(got.logged)
			if err != nil {
				t.Fatal(err)
			}
			redacttest.RequireNoPiece(t, capture+"log", string(rest), judgeBearer)
		})
	}
}

// splitBearer is judgeBearer with a zero-width space (U+200B, which record.ReasonRefuses) after
// every 6 of its characters: no run of 8 of the bearer stands in it, so redacting the bearer's runs
// from it as it came would leave every piece, which dropping the zero-width spaces would then join
// into the whole bearer.
var splitBearer = func() string {
	var split strings.Builder
	for i, r := range judgeBearer {
		if i > 0 && i%6 == 0 {
			split.WriteRune('\u200b')
		}
		split.WriteRune(r)
	}
	return split.String()
}()

// quoted is s as a JSON string.
func quoted(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// TestTheJudgeKeepsTheModelsOrdinaryWords: a reason and a reply that hold no credential the run
// sent - quoting the judge's own system prompt, the request's signed reason from the Data block,
// the words grant and deny, and Azure's api-version, which the endpoint adds to its chat
// completions route, where the public-name table declares it public - are kept as they came: a
// deny's reason in the agent's refusal and the row (deny), and a reply that is no object of the
// judge's form in the row's raw answer and its error (malformed). None of them is a credential of
// the run's, though each is a value its request carried.
func TestTheJudgeKeepsTheModelsOrdinaryWords(t *testing.T) {
	const reason = "Follow each key's guidance: Running the broker's own tests is no reason to grant or deny at api-version 2024-10-21."
	azure := func(o *Options) {
		o.Model.Endpoint.BaseURL += "/openai/v1"
		o.Model.Endpoint.Query = map[string][]string{"api-version": {"2024-10-21"}}
	}
	usage := chatTokens{prompt: 20, completion: 4}

	t.Run("deny", func(t *testing.T) {
		got := judgeWords(t, chatStream(`{"verdict":"deny","reason":`+quoted(reason)+`}`, usage), azure)
		if text := rowText(t, got.row, "reason"); text != reason {
			t.Errorf("the row's reason is %q, want the model's reason as it came, %q", text, reason)
		}
		if !strings.Contains(got.refusal, reason) {
			t.Errorf("the agent's refusal is %q, want the model's reason in it as it came", got.refusal)
		}
	})
	t.Run("malformed", func(t *testing.T) {
		reply := `{"verdict":"maybe","reason":` + quoted(reason) + `}`
		got := judgeWords(t, chatStream(reply, usage), azure)
		if text := rowText(t, got.row, "raw_answer"); text != reply {
			t.Errorf("the row's raw answer is %q, want the reply as it came, %q", text, reply)
		}
		if text := rowText(t, got.row, "error"); !strings.Contains(text, reason) {
			t.Errorf("the row's error is %q, want the reply's reason in it as it came", text)
		}
	})
}
