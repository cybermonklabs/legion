package agentloop

import (
	"context"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/testport"
)

// TestRetryableBoundsTheWaitTheEndpointNames: the one retry of a model call reads the wait the
// endpoint names by the one rule every reader of a server's wait in this module keeps
// (internal/serverwait). A Retry-After of 9999999999 seconds and an X-RateLimit-Reset of
// math.MaxInt64 with no requests left are past what a Duration holds and read as one hour, which
// a run bounded under an hour, such as the judge's 20 s modelBound, has no time for, so the call
// is not made again (this test's run has two minutes): converted as it came, the count read as a
// negative wait, which any run had time for, and the call was made again at once. A negative
// Retry-After reads as no wait, an ordinary value of each header as itself, and an answer naming
// none waits defaultRetryWait. The test counts the rows it drove and fails under the number the
// table must hold.
func TestRetryableBoundsTheWaitTheEndpointNames(t *testing.T) {
	const rowsToDrive = 6
	now := time.Now()
	exhausted := func(reset string) http.Header {
		return http.Header{"X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Reset": {reset}}
	}
	rows := []struct {
		name     string
		status   int
		header   http.Header
		min, max time.Duration
		// again is whether a run with two minutes left waits the wait and makes the call again.
		again bool
	}{
		{"a 429 asking 9999999999 seconds", http.StatusTooManyRequests, http.Header{"Retry-After": {"9999999999"}}, time.Hour, time.Hour, false},
		{"a 429 whose X-RateLimit-Reset is math.MaxInt64 with no requests left", http.StatusTooManyRequests, exhausted(strconv.FormatInt(math.MaxInt64, 10)), time.Hour, time.Hour, false},
		{"a 429 asking a negative wait", http.StatusTooManyRequests, http.Header{"Retry-After": {"-30"}}, 0, 0, true},
		{"a 429 asking 30 seconds", http.StatusTooManyRequests, http.Header{"Retry-After": {"30"}}, 30 * time.Second, 30 * time.Second, true},
		// The reset is whole seconds, and retryable reads it against its own clock a moment
		// later, so the wait is up to two seconds short of the 90 it names.
		{"a 503 whose X-RateLimit-Reset is 90 seconds out with no requests left", http.StatusServiceUnavailable, exhausted(strconv.FormatInt(now.Add(90*time.Second).Unix(), 10)), 88 * time.Second, 90 * time.Second, true},
		{"a 503 naming no wait", http.StatusServiceUnavailable, http.Header{}, defaultRetryWait, defaultRetryWait, true},
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	driven := 0
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			driven++
			wait, retry := retryable(&http.Response{StatusCode: row.status, Header: row.header}, nil)
			if !retry || wait < row.min || wait > row.max {
				t.Errorf("retryable = %s (retry %t), want %s to %s and a retry", wait, retry, row.min, row.max)
			}
			if again := waitable(ctx, wait); again != row.again {
				t.Errorf("a run with two minutes left makes the call again after %s: %t, want %t", wait, again, row.again)
			}
		})
	}
	if driven < rowsToDrive {
		t.Fatalf("the table drove %d rows, want %d", driven, rowsToDrive)
	}
}

// TestA429IsLoggedOnceForTheAttemptItAnswered: a 429 the model service answers is logged at WARN
// for the attempt it answered, once, with the deployment the request names, the attempt, the wait
// before the retry, here the Retry-After of 1 the service gave, and that the call is made again;
// the retry's answer is the call's. The test runs alone, not in parallel, so the default logger it
// reads holds no other test's lines.
func TestA429IsLoggedOnceForTheAttemptItAnswered(t *testing.T) {
	logs := &logtest.Recorder{}
	logtest.SetDefault(t, logs)
	limited := reply{
		status: http.StatusTooManyRequests, headers: map[string]string{"Retry-After": "1"},
		body: []byte(`{"error":{"code":"429","message":"Requests to the deployment have exceeded its rate limit."}}`),
	}
	s := newStandIn(t, limited, chatStream("stop", chatUsage{Prompt: 2, Completion: 3}, "The note says plum."))
	f := newFixture(t, s, OpenAIChat, 1)
	f.req.Model.ID = "chat-deployment-standin"
	result, err := Run(context.Background(), f.req)
	if err != nil || result.Fault != "" || result.Text != "The note says plum." {
		t.Fatalf("after a 429: result %+v, %v; want the retry's answer", result, err)
	}
	if n := len(s.attempts()); n != 2 {
		t.Errorf("a 429 then an answer took %d requests, want 2", n)
	}
	var warned []map[string]any
	for _, record := range logs.Records() {
		if record.Level != slog.LevelWarn {
			continue
		}
		attrs := map[string]any{"msg": record.Message}
		record.Attrs(func(attr slog.Attr) bool {
			attrs[attr.Key] = attr.Value.Any()
			return true
		})
		warned = append(warned, attrs)
	}
	want := []map[string]any{{
		"msg":        "agentloop: the model service answered 429, its rate limit",
		"deployment": "chat-deployment-standin", "attempt": int64(1), "wait": time.Second, "retry": true,
	}}
	if !reflect.DeepEqual(warned, want) {
		t.Errorf("the run logged the warnings %v, want the one line of its 429 %v", warned, want)
	}
}

// callsAt are the model calls this package makes, a run on each protocol the run fixture speaks
// and a decision, each put to endpoint in place of its stand-in, which hears nothing: each returns
// the fault the call ended on and its detail.
func callsAt(endpoint string) map[string]func(*testing.T) (Fault, string) {
	run := func(kind Kind) func(*testing.T) (Fault, string) {
		return func(t *testing.T) (Fault, string) {
			f := newFixture(t, newStandIn(t), kind, 1)
			f.req.Model.Endpoint.BaseURL = endpoint
			result, err := Run(context.Background(), f.req)
			if err != nil {
				t.Fatal(err)
			}
			return result.Fault, result.Detail
		}
	}
	return map[string]func(*testing.T) (Fault, string){
		"a run on " + string(AnthropicMessages): run(AnthropicMessages),
		"a run on " + string(OpenAIResponses):   run(OpenAIResponses),
		"a run on " + string(OpenAIChat):        run(OpenAIChat),
		"a decision": func(t *testing.T) (Fault, string) {
			req := newDecideFixture(t, newStandIn(t))
			req.Model.Endpoint.BaseURL = endpoint
			result, err := Decide(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			return result.Fault, result.Detail
		},
	}
}

// An endpoint that carries a made-up token as its user name, with nothing listening: each model
// call is a provider error whose detail is the failed call's class alone and holds no part of the
// token, though net/http's error quotes the request URL with only a password masked. The same
// endpoint in front of a server that refuses every call with a 400 holds no part of it either,
// though Anthropic's SDK quotes the request URL whole in its words for the status: there the URL
// prints as redact.NotPrinted, which the Anthropic row requires, so the row reads the SDK's own
// words. Dispatch's boot refuses such a DISPATCH_CHAT_ENDPOINT or DISPATCH_AZURE_CHAT_ENDPOINT
// (and the client one with a query); a model call prints none of it all the same.
func TestAModelCallPrintsNoTokenOfTheEndpointOnAFailedCall(t *testing.T) {
	t.Parallel()
	const token = "madeUpChatToken0123"
	for name, call := range callsAt("http://" + token + "@" + testport.Refused(t) + "/v1") {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fault, detail := call(t)
			if fault != FaultProviderError || detail != "connection refused" {
				t.Fatalf("fault %q with detail %q, want %q with the failed call's class alone", fault, detail, FaultProviderError)
			}
			if strings.Contains(detail, token[:10]) {
				t.Errorf("the detail %q holds part of the token", detail)
			}
		})
	}
	// A stand-in with no reply answers every call 400 (noMoreReplies).
	refusing := newStandIn(t)
	for name, call := range callsAt("http://" + token + "@" + strings.TrimPrefix(refusing.server.URL, "http://") + "/v1") {
		t.Run(name+", refused 400", func(t *testing.T) {
			t.Parallel()
			fault, detail := call(t)
			if fault != FaultProviderError || !strings.Contains(detail, "400") {
				t.Fatalf("fault %q with detail %q, want %q naming the status", fault, detail, FaultProviderError)
			}
			if strings.Contains(detail, token[:10]) {
				t.Errorf("the detail %q holds part of the token", detail)
			}
			if name == "a run on "+string(AnthropicMessages) && !strings.Contains(detail, redact.NotPrinted) {
				t.Errorf("the detail %q does not withhold the request URL Anthropic's SDK quotes", detail)
			}
		})
	}
}

// A clean endpoint's failed model call prints the failure itself, without net/http's quote of the
// URL: a refused dial names the address it dialled, and a TLS failure the certificate's fault,
// which the operator reads to fix it, where the class alone said "TLS failed".
func TestAModelCallPrintsTheFailureOfACleanEndpoint(t *testing.T) {
	t.Parallel()
	refused := testport.Refused(t)
	tlsServer := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(tlsServer.Close)
	for _, row := range []struct{ name, endpoint, want string }{
		{"a refused dial", "http://" + refused + "/v1", "dial tcp " + refused + ": connect: connection refused"},
		{"a TLS failure", tlsServer.URL + "/v1", "tls: failed to verify certificate: x509: "},
	} {
		for name, call := range callsAt(row.endpoint) {
			t.Run(row.name+", "+name, func(t *testing.T) {
				t.Parallel()
				if fault, detail := call(t); fault != FaultProviderError || !strings.HasPrefix(detail, row.want) {
					t.Errorf("fault %q with detail %q at %s, want %q starting %q", fault, detail, row.endpoint, FaultProviderError, row.want)
				}
			})
		}
	}
}
