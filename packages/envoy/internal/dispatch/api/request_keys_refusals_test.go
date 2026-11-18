package api

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
)

// updateKeyedRefusals rewrites keyedRefusalsFile from the tree under test's own answers. It is run
// once, on the main commit before the keyed layer answered a caller it could not resolve itself
// (b1c42d1d7d's parent, #4350), so the file holds what every client read before that change.
var updateKeyedRefusals = flag.Bool("update-keyed-refusals", false,
	"write "+keyedRefusalsFile+" from this tree's answers rather than compare them with it")

// keyedRefusalsFile is every keyed route's answer to each caller keyedRefusalCallers names, as
// main answered them at 958173da0e, the parent of #4350's b1c42d1d7d.
const keyedRefusalsFile = "testdata/keyed_refusals.json"

// keyedRefusal is one answer as a client reads it: its status, the headers a refusal sets, and
// its body.
type keyedRefusal struct {
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    string            `json:"body"`
}

// refusalHeaders are the headers a refusal of a caller, or of a body, sets: the scope a 403 names,
// the wait a 429 asks for, and the key echo, which says whether the keyed layer judged the key.
var refusalHeaders = []string{"WWW-Authenticate", "Retry-After", contracts.IdempotencyKeyHeader}

// storeDownIdentity is a cookie identity whose session store does not answer: CookieIdentity
// hands the store's own error on, which no handler names and every one answers 500 INTERNAL.
type storeDownIdentity struct{}

func (storeDownIdentity) Login(*http.Request) (string, error) {
	return "", errors.New("read the session generation: the test's session store is down")
}

// keyedRefusalCaller is one way a keyed write is refused before it writes: a caller that does not
// resolve, or a body past its route's limit.
type keyedRefusalCaller struct {
	name    string
	options testServerOptions
	// credential is the caller's headers; nil is no Authorization and no sign-in at all.
	credential http.Header
	// oversized sends a body one byte past the route's limit (routeBody); undeclared leaves its
	// length for the server to find as it reads.
	oversized, undeclared bool
}

var keyedRefusalCallers = []keyedRefusalCaller{
	{name: "no Authorization"},
	{name: "a bad token", credential: withBearer("dsp_nobody-issued-this")},
	{name: "the sign-in issuer down", options: testServerOptions{identity: signInDownIdentity{}}},
	{name: "the service-account issuer down", options: testServerOptions{oidc: issuerDownVerifier{}}, credential: withBearer("header.payload.signature")},
	{name: "the store down", options: testServerOptions{identity: storeDownIdentity{}}},
	{name: "a declared body past the route's limit", credential: asUser("alice"), oversized: true},
	{name: "an undeclared body past the route's limit", credential: asUser("alice"), oversized: true, undeclared: true},
}

// R9 of dispatch://INFRA-3862/spec: a deployment behaves exactly as it did. Every POST and PATCH
// route, sent a keyed write by each caller keyedRefusalCallers names, answers what main answered
// before #4350 moved the refusal of a caller the keyed layer cannot resolve into that layer: the
// same status, refusal headers and body, recorded in keyedRefusalsFile from that commit.
func TestEveryKeyedRefusalReadsAsItDidBeforeTheLayerAnsweredIt(t *testing.T) {
	got := map[string]keyedRefusal{}
	for _, caller := range keyedRefusalCallers {
		handler, _, _ := newTestServer(t, caller.options)
		for index, route := range (&server{}).routes() {
			if !keyedMethod(route.Method) {
				continue
			}
			body := routeProbeBody(route)
			if caller.oversized {
				limit, _ := routeBody(route.Method, route.Pattern)
				body = `"` + strings.Repeat("x", int(limit)-1) + `"`
			}
			request := httptest.NewRequest(route.Method, routeProbePath(route.Pattern), strings.NewReader(body))
			if caller.undeclared {
				request.ContentLength = -1
			}
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set(contracts.IdempotencyKeyHeader, fmt.Sprintf("refusal-%d", index))
			maps.Copy(request.Header, caller.credential)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			answer := keyedRefusal{Status: response.Code, Body: response.Body.String()}
			for _, name := range refusalHeaders {
				if value := response.Header().Get(name); value != "" {
					if answer.Headers == nil {
						answer.Headers = map[string]string{}
					}
					answer.Headers[name] = value
				}
			}
			got[route.Method+" "+route.Pattern+" | "+caller.name] = answer
		}
	}
	if len(got) == 0 {
		t.Fatal("no keyed route was sent anything, so nothing was compared")
	}
	if *updateKeyedRefusals {
		data, err := json.MarshalIndent(got, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(keyedRefusalsFile), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(keyedRefusalsFile, append(data, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %d answers to %s", len(got), keyedRefusalsFile)
		return
	}
	data, err := os.ReadFile(keyedRefusalsFile)
	if err != nil {
		t.Fatalf("read the recorded answers: %v", err)
	}
	var want map[string]keyedRefusal
	if err := json.Unmarshal(data, &want); err != nil {
		t.Fatalf("decode %s: %v", keyedRefusalsFile, err)
	}
	for _, row := range slices.Sorted(maps.Keys(got)) {
		recorded, ok := want[row]
		if !ok {
			t.Errorf("%s: answered status=%d body=%.200s, and %s records no answer for it", row, got[row].Status, got[row].Body, keyedRefusalsFile)
			continue
		}
		if answer := got[row]; answer.Status != recorded.Status || answer.Body != recorded.Body || !maps.Equal(answer.Headers, recorded.Headers) {
			t.Errorf("%s: answered status=%d headers=%v body=%.200s; before #4350 it answered status=%d headers=%v body=%.200s",
				row, answer.Status, answer.Headers, answer.Body, recorded.Status, recorded.Headers, recorded.Body)
		}
	}
	for _, row := range slices.Sorted(maps.Keys(want)) {
		if _, ok := got[row]; !ok {
			t.Errorf("%s: recorded in %s, and no keyed route of this tree answered it", row, keyedRefusalsFile)
		}
	}
	t.Logf("compared %d answers with %s", len(got), keyedRefusalsFile)
}
