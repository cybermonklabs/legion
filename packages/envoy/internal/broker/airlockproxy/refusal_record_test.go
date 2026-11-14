// packages/envoy/internal/broker/airlockproxy/refusal_record_test.go
//
// The airlock's half of the refusal record (internal/broker/api's refusal_record_test.go is the
// API port's half): a refusal driven through each place the proxy port refuses a call, on the
// real proxy of the rig, reading the one record refuse writes under answer.RefusalMessage, the
// answer id the answer carries in its X-Broker-Answer-Id header and at the end of its text body,
// and the answer's count on broker_answers_total. Then the answers that are no refusal: each
// carries its id, writes no record, and is counted.
package airlockproxy

import (
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/airlock/airlocktest"
	"github.com/sjawhar/envoy/internal/broker/answer"
	"github.com/sjawhar/envoy/internal/broker/requests"
	"github.com/sjawhar/envoy/internal/metrics"
)

// The places the proxy port refuses a call, each one no other row drives: the seven of the
// outer server and the 407 among them, and the refusals of a call to a bound host that bound.go,
// azure.go and aws.go hand to refuse.
const (
	siteNotAProxyRequest = "proxy.go serveProxy: a request that is no proxy request (400)"
	siteCredential       = "proxy.go refuseCredential: a credential that opens nothing (407)"
	siteGrantRefused     = "proxy.go serveProxy: a grant the broker refuses (403)"
	siteConnectTarget    = "proxy.go serveConnect: a CONNECT target that is no host and port (400)"
	siteConnectAddress   = "proxy.go serveConnect: an address the proxy never dials (403)"
	sitePlainBound       = "proxy.go servePlain: plain http to a bound host (403)"
	sitePlainAddress     = "proxy.go servePlain: an address the proxy never dials (403)"
	siteBoundConnection  = "bound.go serveBound: a grant that ended on an open connection"
	siteBoundValues      = "bound.go serveBound: a grant the release checks refuse"
	siteBoundDecision    = "bound.go serveBound: a request the decision refuses"
	siteAWSSignature     = "bound.go serveBound: an AWS call the airlock will not sign"
	siteAzureAssertion   = "azure.go exchangeToken: a token request that is not this grant's"
)

// refusalSites is every site above, in the order the table names them.
var refusalSites = []string{
	siteNotAProxyRequest, siteCredential, siteGrantRefused, siteConnectTarget, siteConnectAddress,
	sitePlainBound, sitePlainAddress, siteBoundConnection, siteBoundValues, siteBoundDecision,
	siteAWSSignature, siteAzureAssertion,
}

// answerIDPattern is an answer id: 16 random bytes, in hex (answer.NewID).
var answerIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// proxyAnswer is one answer of the proxy port as its client read it.
type proxyAnswer struct {
	status int
	header http.Header
	body   string
}

func (a proxyAnswer) id() string { return a.header.Get(answer.Header) }

// answeredBy reads what the rig's helpers answer.
func answeredBy(response *http.Response, body string) proxyAnswer {
	return proxyAnswer{status: response.StatusCode, header: response.Header, body: body}
}

// connectAnswer is the answer to one CONNECT of target with the header lines given.
func connectAnswer(t *testing.T, r *rig, target string, headers ...string) proxyAnswer {
	t.Helper()
	response, _, _ := r.connectRaw(target, headers...)
	return answeredBy(response, bodyOf(t, response))
}

// answersCounted is registry's broker_answers_total sample naming labels, 0 when it has none.
func answersCounted(t *testing.T, registry *metrics.Registry, labels string) int {
	t.Helper()
	w := httptest.NewRecorder()
	registry.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	for line := range strings.Lines(w.Body.String()) {
		if value, ok := strings.CutPrefix(strings.TrimSuffix(line, "\n"), "broker_answers_total{"+labels+"} "); ok {
			n, err := strconv.Atoi(value)
			if err != nil {
				t.Fatalf("broker_answers_total{%s} is no count: %q", labels, value)
			}
			return n
		}
	}
	return 0
}

// answersCountedReach waits for registry's sample naming labels to reach want: an answer is
// counted once its handler returns, which for a streamed answer is after the client has read
// it. It fails the row rather than hanging when the count never comes.
func answersCountedReach(t *testing.T, registry *metrics.Registry, labels string, want int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		got := answersCounted(t, registry, labels)
		if got >= want {
			if got != want {
				t.Errorf("broker_answers_total{%s} = %d; want %d", labels, got, want)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("broker_answers_total{%s} = %d; want %d", labels, got, want)
		}
		time.Sleep(time.Millisecond)
	}
}

// linesNaming is every line of lines that names the answer id.
func linesNaming(lines []logLine, id string) []logLine {
	var naming []logLine
	for _, line := range lines {
		if line.fields["answer_id"] == id {
			naming = append(naming, line)
		}
	}
	return naming
}

// boundCaller is the caller a line names for the rig's grant.
const boundCaller = testGrantID + "@" + testEnrollID

// TestEveryRefusalOfTheAirlockWritesOneRecord drives a refusal through each place the proxy port
// refuses a call, the 407 among them, and reads what each leaves: exactly one record under
// answer.RefusalMessage at WARN, naming the code, the reason, the caller (class airlock, and the
// grant once the proxy password proved one), the secret the call was handled for once one was
// chosen, the host, and the answer id, which the answer carries in its X-Broker-Answer-Id header
// and at the end of its text body; no call line beside it; and the refusal counted. No record
// holds a value or a placeholder.
func TestEveryRefusalOfTheAirlockWritesOneRecord(t *testing.T) {
	registry := metrics.New()
	answers := answer.NewAnswers(registry)
	plain := func(t *testing.T) *rig { return newRig(t, Options{Answers: answers}) }
	refused := fmt.Errorf("%w: AUTO_TOKEN", requests.ErrSecretNotInStore)
	chain := fmt.Errorf("%w: record 9f2c: stored body does not reproduce its id", requests.ErrGrantChainInvalid)

	drove := map[string]bool{}
	for _, row := range []struct {
		site, name string
		rig        func(t *testing.T) *rig
		drive      func(t *testing.T, r *rig) proxyAnswer
		status     int
		code       string
		reason     string
		caller     string
		secrets    []string
		host       string
		path       string
		// challenge is the Proxy-Authenticate the answer carries, "" for none.
		challenge string
	}{
		{
			site: siteNotAProxyRequest, name: "an ordinary GET of the port", rig: plain,
			drive: func(t *testing.T, r *rig) proxyAnswer {
				response, err := (&http.Client{Timeout: 10 * time.Second}).Get("http://" + r.addr + "/healthz")
				if err != nil {
					t.Fatalf("GET the proxy port: %v", err)
				}
				return answeredBy(response, bodyOf(t, response))
			},
			status: http.StatusBadRequest, code: codeNotAProxyRequest,
			reason: "not a proxy request: this port takes CONNECT and absolute-URL proxy requests alone",
			host:   "127.0.0.1", path: "/healthz",
		},
		{
			site: siteCredential, name: "a CONNECT that carries no credential", rig: plain,
			drive: func(t *testing.T, r *rig) proxyAnswer {
				return connectAnswer(t, r, net.JoinHostPort(tunnelHost, "443"))
			},
			status: http.StatusProxyAuthRequired, code: codeProxyCredential,
			reason: "this port takes a Proxy-Authorization: Basic credential whose password is a placeholder of a live grant, and this request carries none",
			host:   tunnelHost, challenge: `Basic realm="airlock"`,
		},
		{
			site: siteCredential, name: "a CONNECT whose grant has ended", rig: plain,
			drive: func(t *testing.T, r *rig) proxyAnswer {
				r.grants.refuseConnection(workerPlaceholder, requests.ErrGrantNotLive)
				return connectAnswer(t, r, net.JoinHostPort(boundHost, "443"), basic(workerPlaceholder))
			},
			status: http.StatusProxyAuthRequired, code: codeGrantNotLive, reason: requests.ErrGrantNotLive.Error(),
			host: boundHost, challenge: `Basic realm="airlock"`,
		},
		{
			site: siteGrantRefused, name: "a CONNECT whose grant the broker refuses", rig: plain,
			drive: func(t *testing.T, r *rig) proxyAnswer {
				r.grants.refuseConnection(workerPlaceholder, refused)
				return connectAnswer(t, r, net.JoinHostPort(boundHost, "443"), basic(workerPlaceholder))
			},
			status: http.StatusForbidden, code: codeSecretNotInStore, reason: refused.Error(), host: boundHost,
		},
		{
			site: siteConnectTarget, name: "a CONNECT of a bracketed address with no port", rig: plain,
			drive: func(t *testing.T, r *rig) proxyAnswer {
				return connectAnswer(t, r, "[::1]", basic(workerPlaceholder))
			},
			status: http.StatusBadRequest, code: codeConnectTarget, reason: "[::1] is no host and port",
			caller: boundCaller, host: "[::1]",
		},
		{
			site: siteConnectAddress, name: "a CONNECT of a loopback address", rig: plain,
			drive: func(t *testing.T, r *rig) proxyAnswer {
				return connectAnswer(t, r, "127.0.0.1:443", basic(workerPlaceholder))
			},
			status: http.StatusForbidden, code: codeAddressRefused,
			reason: "127.0.0.1 resolves to 127.0.0.1, a loopback IPv4 address, which this proxy never dials",
			caller: boundCaller, host: "127.0.0.1",
		},
		{
			site: sitePlainBound, name: "a plain-http call to the bound host", rig: plain,
			drive: func(t *testing.T, r *rig) proxyAnswer {
				return answeredBy(r.get(r.client(workerPlaceholder), "http://"+boundHost+"/v1/things", "Authorization", "Bearer "+workerPlaceholder))
			},
			status: http.StatusForbidden, code: codePlainHTTP,
			reason: "refusing plain http to " + boundHost + ", a host behind the airlock: a secret of this grant may reach it only inside TLS, so ask for it over https",
			caller: boundCaller, host: boundHost, path: "/v1/things",
		},
		{
			site: sitePlainAddress, name: "a plain-http call to a private address", rig: plain,
			drive: func(t *testing.T, r *rig) proxyAnswer {
				return answeredBy(r.get(r.client(workerPlaceholder), "http://10.1.2.3/v1/things"))
			},
			status: http.StatusForbidden, code: codeAddressRefused,
			reason: "10.1.2.3 resolves to 10.1.2.3, a private IPv4 address, which this proxy never dials",
			caller: boundCaller, host: "10.1.2.3", path: "/v1/things",
		},
		{
			site: siteBoundConnection, name: "the second call of a connection whose grant ended after the first", rig: plain,
			drive: func(t *testing.T, r *rig) proxyAnswer {
				client := r.client(workerPlaceholder)
				if first, body := r.get(client, "https://"+boundHost+"/v1/things", "Authorization", "Bearer "+workerPlaceholder); first.StatusCode != http.StatusOK {
					t.Fatalf("the first call answered %d %q; want 200", first.StatusCode, body)
				}
				r.grants.refuseConnection(workerPlaceholder, requests.ErrGrantNotLive)
				return answeredBy(r.get(client, "https://"+boundHost+"/v1/things", "Authorization", "Bearer "+workerPlaceholder))
			},
			status: http.StatusProxyAuthRequired, code: codeGrantNotLive, reason: requests.ErrGrantNotLive.Error(),
			caller: boundCaller, host: boundHost, path: "/v1/things",
		},
		{
			site: siteBoundValues, name: "a grant whose approval chain no longer verifies", rig: plain,
			drive: func(t *testing.T, r *rig) proxyAnswer {
				r.grants.refuseValues(chain)
				return answeredBy(r.get(r.client(workerPlaceholder), "https://"+boundHost+"/v1/things", "Authorization", "Bearer "+workerPlaceholder))
			},
			status: http.StatusForbidden, code: codeGrantChain, reason: chain.Error(),
			caller: boundCaller, secrets: []string{workerName}, host: boundHost, path: "/v1/things",
		},
		{
			site: siteBoundDecision, name: "a path with a dot segment", rig: plain,
			drive: func(t *testing.T, r *rig) proxyAnswer {
				return answeredBy(r.get(r.client(workerPlaceholder), "https://"+boundHost+"/v1/../admin/things", "Authorization", "Bearer "+workerPlaceholder))
			},
			status: http.StatusForbidden, code: codeNotAllowed, reason: "path has a dot segment",
			caller: boundCaller, secrets: []string{workerName}, host: boundHost, path: "/v1/../admin/things",
		},
		{
			site: siteAWSSignature, name: "a call signed with Signature Version 4a",
			rig: func(t *testing.T) *rig {
				r := newRig(t, Options{Rules: awsRules(), Answers: answers})
				awsGrant(r, false)
				return r
			},
			drive: func(t *testing.T, r *rig) proxyAnswer {
				request, err := http.NewRequest(http.MethodGet, "https://"+boundHost+"/v1/things", nil)
				if err != nil {
					t.Fatal(err)
				}
				request.Header.Set("Authorization", sigV4aScheme+" Credential="+awsKeyPlaceholder+"/20260101/"+awsRegion+"/s3/aws4_request, SignedHeaders=host;x-amz-date, Signature="+strings.Repeat("0", 96))
				request.Header.Set("X-Amz-Region-Set", "*")
				request.Header.Set("X-Amz-Date", "20260101T000000Z")
				return answeredBy(sendAWS(t, r.client(awsKeyPlaceholder), request))
			},
			status: http.StatusForbidden, code: codeAWSSignature,
			reason: "refusing Signature Version 4a (" + sigV4aScheme + "): the airlock signs again with Signature Version 4 alone",
			caller: boundCaller, secrets: []string{awsName}, host: boundHost, path: "/v1/things",
		},
		{
			site: siteAzureAssertion, name: "a token request carrying an assertion of its own",
			rig: func(t *testing.T) *rig {
				r, _ := newAzureRig(t, Options{Answers: answers})
				return r
			},
			drive: func(t *testing.T, r *rig) proxyAnswer {
				return answeredBy(tokenRequest(t, r, r.client(azureFederatedPlaceholder), "an.assertion.of.its.own"))
			},
			status: http.StatusForbidden, code: codeTokenAssertion,
			reason: "refusing a token request whose client_assertion is not this grant's placeholder",
			caller: boundCaller, secrets: []string{azureName}, host: boundHost, path: azureTokenPath,
		},
	} {
		t.Run(row.site+": "+row.name, func(t *testing.T) {
			r := row.rig(t)
			labels := `outcome="refused",code="` + row.code + `",caller_class="` + answer.CallerAirlock + `"`
			before := answersCounted(t, registry, labels)
			got := row.drive(t, r)
			drove[row.site] = true
			id := got.id()
			if !answerIDPattern.MatchString(id) {
				t.Fatalf("%s: the answer carries %s %q, want an answer id (%d %q)", row.site, answer.Header, id, got.status, got.body)
			}
			if got.status != row.status {
				t.Fatalf("%s: the answer is %d %q, want %d", row.site, got.status, got.body, row.status)
			}
			if want := "airlock: " + row.reason + " (answer " + id + ")\n"; got.body != want {
				t.Errorf("%s: the body is %q, want %q, ending with the answer id", row.site, got.body, want)
			}
			if challenge := got.header.Get("Proxy-Authenticate"); challenge != row.challenge {
				t.Errorf("%s: Proxy-Authenticate = %q, want %q", row.site, challenge, row.challenge)
			}

			records := linesNaming(r.logs.refusals(), id)
			if len(records) != 1 {
				t.Fatalf("%s: the refusal wrote %d records under %q naming its answer id, want 1: %v", row.site, len(records), answer.RefusalMessage, r.logs.refusals())
			}
			record := records[0]
			if record.level != slog.LevelWarn {
				t.Errorf("%s: the record is at %v, want WARN", row.site, record.level)
			}
			secrets := row.secrets
			if secrets == nil {
				secrets = []string{}
			}
			for field, want := range map[string]any{
				"status": int64(row.status), "code": row.code, "reason": row.reason, "caller_class": answer.CallerAirlock,
				"caller": row.caller, "secrets": secrets, "host": row.host, "path": row.path, "route": "",
			} {
				if fmt.Sprint(record.fields[field]) != fmt.Sprint(want) {
					t.Errorf("%s: the record's %s is %v, want %v: %v", row.site, field, record.fields[field], want, record.fields)
				}
			}
			for _, never := range []string{workerValue, workerPlaceholder, azureRealFederated, azureFederatedPlaceholder, awsRealSecret, awsKeyPlaceholder} {
				if strings.Contains(fmt.Sprint(record.fields), never) {
					t.Errorf("%s: the record holds %q: %v", row.site, never, record.fields)
				}
			}
			if calls := linesNaming(r.logs.calls(), id); len(calls) != 0 {
				t.Errorf("%s: the refusal also wrote the call lines %v, want its record alone", row.site, calls)
			}
			if after := answersCounted(t, registry, labels); after != before+1 {
				t.Errorf("%s: broker_answers_total{%s} went from %d to %d, want one more", row.site, labels, before, after)
			}
		})
	}
	var missing []string
	for _, site := range refusalSites {
		if !drove[site] {
			missing = append(missing, site)
		}
	}
	if len(drove) < len(refusalSites) || len(missing) != 0 {
		t.Fatalf("the table drove %d of the proxy port's %d refusal sites; it never drove %v", len(drove), len(refusalSites), missing)
	}
}

// TestTheAirlocksRefusalRecordCutsWhatTheCallerWrote: the method, the host and the path a refusal
// record names are the caller's own text, and the record keeps at most 256 bytes of each, as the
// API port's record keeps of its method (answer.Clip). A request that is no proxy request is
// refused before any credential is read, so anyone who reaches the port writes this record.
func TestTheAirlocksRefusalRecordCutsWhatTheCallerWrote(t *testing.T) {
	r := newRig(t, Options{})
	long := func(c string) string { return strings.Repeat(c, 300) }
	request, err := http.NewRequest(long("M"), "http://"+r.addr+"/"+long("p"), nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Host = long("h")
	response, err := (&http.Client{Timeout: 10 * time.Second}).Do(request)
	if err != nil {
		t.Fatalf("a request to the proxy port: %v", err)
	}
	got := answeredBy(response, bodyOf(t, response))
	if got.status != http.StatusBadRequest {
		t.Fatalf("the answer is %d %q, want 400", got.status, got.body)
	}
	records := linesNaming(r.logs.refusals(), got.id())
	if len(records) != 1 {
		t.Fatalf("the refusal wrote %d records naming its answer id, want 1: %v", len(records), r.logs.refusals())
	}
	for field, want := range map[string]string{"method": long("M")[:256], "host": long("h")[:256], "path": ("/" + long("p"))[:256]} {
		if kept := fmt.Sprint(records[0].fields[field]); kept != want {
			t.Errorf("the record's %s is %d bytes, want the first 256 of the caller's: %q", field, len(kept), kept)
		}
	}
}

// TestEveryAnswerOfTheAirlockThatIsNoRefusalIsCountedAndWritesNoRecord: a call the airlock
// carried writes its one call line, a failure keeps the line its writer writes, and neither
// writes a refusal record; each carries its answer id and is counted, a carried call served
// whatever the service answered, and a failure failed under its code. A token endpoint's 429 the
// proxy forwards is no refusal record either, as a 429 of the API port is none: it keeps its
// call line and is counted rate_limited. A call to the bound host is two answers of the port,
// each counted: the 200 of the CONNECT its client opened, and the call's own on the decrypted
// connection.
func TestEveryAnswerOfTheAirlockThatIsNoRefusalIsCountedAndWritesNoRecord(t *testing.T) {
	registry := metrics.New()
	answers := answer.NewAnswers(registry)
	for _, row := range []struct {
		name string
		// rig is the row's rig; nil is the plain one.
		rig   func(t *testing.T) *rig
		drive func(t *testing.T, r *rig) proxyAnswer
		// status is the answer's; calls is how many call lines name its answer id; counted is
		// how many answers of the row its labels count.
		status  int
		calls   int
		counted int
		labels  string
	}{
		{
			name: "a carried call to the bound host, beside its CONNECT's 200",
			drive: func(t *testing.T, r *rig) proxyAnswer {
				return answeredBy(r.get(r.client(workerPlaceholder), "https://"+boundHost+"/v1/things", "Authorization", "Bearer "+workerPlaceholder))
			},
			status: http.StatusOK, calls: 1, counted: 2, labels: `outcome="served",code="HTTP_200",caller_class="airlock"`,
		},
		{
			name: "a carried call the service refused",
			drive: func(t *testing.T, r *rig) proxyAnswer {
				r.answerWith(func(w http.ResponseWriter, _ *http.Request, _ airlocktest.Request) bool {
					http.NotFound(w, nil)
					return true
				})
				return answeredBy(r.get(r.client(workerPlaceholder), "https://"+boundHost+"/v1/things", "Authorization", "Bearer "+workerPlaceholder))
			},
			status: http.StatusNotFound, calls: 1, counted: 1, labels: `outcome="served",code="HTTP_404",caller_class="airlock"`,
		},
		{
			name: "a tunnel",
			drive: func(t *testing.T, r *rig) proxyAnswer {
				response, conn, _ := r.connectRaw(net.JoinHostPort(tunnelHost, "443"), basic(workerPlaceholder))
				conn.Close()
				return proxyAnswer{status: response.StatusCode, header: response.Header}
			},
			status: http.StatusOK, counted: 1, labels: `outcome="served",code="HTTP_200",caller_class="airlock"`,
		},
		{
			name: "a CONNECT whose grant read did not happen",
			drive: func(t *testing.T, r *rig) proxyAnswer {
				r.grants.refuseConnection(workerPlaceholder, databaseDown)
				return connectAnswer(t, r, net.JoinHostPort(boundHost, "443"), basic(workerPlaceholder))
			},
			status: http.StatusServiceUnavailable, counted: 1, labels: `outcome="failed",code="` + codeGrantUnreadable + `",caller_class="airlock"`,
		},
		{
			name: "a call to the bound host whose grant read did not happen",
			drive: func(t *testing.T, r *rig) proxyAnswer {
				r.grants.refuseValues(databaseDown)
				return answeredBy(r.get(r.client(workerPlaceholder), "https://"+boundHost+"/v1/things", "Authorization", "Bearer "+workerPlaceholder))
			},
			status: http.StatusServiceUnavailable, calls: 1, counted: 1, labels: `outcome="failed",code="` + codeGrantUnreadable + `",caller_class="airlock"`,
		},
		{
			name: "a token endpoint's 429, forwarded",
			rig: func(t *testing.T) *rig {
				r, stand := newAzureRig(t, Options{Answers: answers})
				stand.Refuse(http.StatusTooManyRequests)
				return r
			},
			drive: func(t *testing.T, r *rig) proxyAnswer {
				return answeredBy(tokenRequest(t, r, r.client(azureFederatedPlaceholder), azureFederatedPlaceholder))
			},
			status: http.StatusTooManyRequests, calls: 1, counted: 1, labels: `outcome="rate_limited",code="` + codeExchangeRefused + `",caller_class="airlock"`,
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			var r *rig
			if row.rig != nil {
				r = row.rig(t)
			} else {
				r = newRig(t, Options{Answers: answers})
			}
			before := answersCounted(t, registry, row.labels)
			got := row.drive(t, r)
			id := got.id()
			if !answerIDPattern.MatchString(id) {
				t.Fatalf("the answer carries %s %q, want an answer id (%d %q)", answer.Header, id, got.status, got.body)
			}
			if got.status != row.status {
				t.Fatalf("the answer is %d %q, want %d", got.status, got.body, row.status)
			}
			if strings.Contains(got.body, "(answer "+id+")") {
				t.Errorf("an answer that refused nothing ends its body with its answer id, as a refusal's does: %q", got.body)
			}
			answersCountedReach(t, registry, row.labels, before+row.counted)
			if records := r.logs.refusals(); len(records) != 0 {
				t.Errorf("an answer that refused nothing wrote the refusal records %v", records)
			}
			if row.calls > 0 {
				r.logs.callsAtLeast(t, row.calls)
			}
			if calls := linesNaming(r.logs.calls(), id); len(calls) != row.calls {
				t.Errorf("the answer wrote %d call lines naming its id, want %d: %v", len(calls), row.calls, r.logs.calls())
			}
		})
	}
}
