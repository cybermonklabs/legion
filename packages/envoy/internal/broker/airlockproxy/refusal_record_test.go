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
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/airlock"
	"github.com/sjawhar/envoy/internal/broker/airlock/airlocktest"
	"github.com/sjawhar/envoy/internal/broker/answer"
	"github.com/sjawhar/envoy/internal/broker/logmask"
	"github.com/sjawhar/envoy/internal/broker/requests"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/logtest"
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
	siteProtocolSwitch   = "bound.go serveBound: a CONNECT on a connection the proxy decrypted"
	siteBoundValues      = "bound.go serveBound: a grant the release checks refuse"
	siteBoundDecision    = "bound.go serveBound: a request the decision refuses"
	siteAWSSignature     = "bound.go serveBound: an AWS call the airlock will not sign"
	siteBoundCut         = "bound.go serveBound: a call whose grant ended before its service answered"
	siteFederatedSwap    = "bound.go serveBound: the federated token outside an exchange"
	siteAzureAssertion   = "azure.go exchangeToken: a token request that is not this grant's"
	siteAzureBound       = "azure.go exchangeToken: a token request as an application the document does not name"
)

// refusalSites is every site above, in the order the table names them.
var refusalSites = []string{
	siteNotAProxyRequest, siteCredential, siteGrantRefused, siteConnectTarget, siteConnectAddress,
	sitePlainBound, sitePlainAddress, siteBoundConnection, siteProtocolSwitch, siteBoundValues,
	siteBoundDecision, siteAWSSignature, siteBoundCut, siteFederatedSwap, siteAzureAssertion,
	siteAzureBound,
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
// chosen and how many that is (secrets_count, and never secrets_omitted), the host, and the
// answer id, which the answer carries in its X-Broker-Answer-Id header and at the end of its text
// body; no call line beside it; and the refusal counted. No record holds a value or a placeholder.
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
			site: siteProtocolSwitch, name: "a CONNECT written inside the decrypted connection", rig: plain,
			drive: func(t *testing.T, r *rig) proxyAnswer {
				_, conn, _ := r.connectRaw(net.JoinHostPort(boundHost, "443"), basic(workerPlaceholder))
				decrypted := tls.Client(conn, &tls.Config{RootCAs: r.trust, ServerName: boundHost})
				t.Cleanup(func() { decrypted.Close() })
				if _, err := decrypted.Write([]byte("CONNECT " + boundHost + ":443 HTTP/1.1\r\nHost: " + boundHost + ":443\r\n\r\n")); err != nil {
					t.Fatalf("write the CONNECT: %v", err)
				}
				response, err := http.ReadResponse(bufio.NewReader(decrypted), &http.Request{Method: http.MethodConnect})
				if err != nil {
					t.Fatalf("read the answer to the CONNECT: %v", err)
				}
				return answeredBy(response, bodyOf(t, response))
			},
			status: http.StatusForbidden, code: codeProtocolSwitch,
			reason: "refusing a CONNECT inside a connection the proxy decrypted: it would open a tunnel to the service whose bytes the proxy does not read",
			caller: boundCaller, host: boundHost, path: "",
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
			site: siteBoundCut, name: "a long poll whose grant ended while its service had not answered",
			rig: func(t *testing.T) *rig { return newRig(t, Options{Answers: answers, Watch: watchFast}) },
			drive: func(t *testing.T, r *rig) proxyAnswer {
				r.answerWith(func(_ http.ResponseWriter, request *http.Request, _ airlocktest.Request) bool {
					<-request.Context().Done()
					return true
				})
				// The grant ends once the call has reached the service, which is when the
				// call is waiting on its answer.
				go func() {
					deadline := time.Now().Add(cutWait)
					for len(r.requests()) == 0 && time.Now().Before(deadline) {
						time.Sleep(time.Millisecond)
					}
					r.grants.refuseConnection(workerPlaceholder, requests.ErrGrantNotLive)
				}()
				return answeredBy(r.get(r.client(workerPlaceholder), "https://"+boundHost+"/v1/poll", "Authorization", "Bearer "+workerPlaceholder))
			},
			status: http.StatusProxyAuthRequired, code: codeGrantNotLive, reason: requests.ErrGrantNotLive.Error(),
			caller: boundCaller, secrets: []string{workerName}, host: boundHost, path: "/v1/poll",
		},
		{
			site: siteFederatedSwap, name: "a GET carrying the federated token's placeholder",
			rig: func(t *testing.T) *rig {
				r, _ := newAzureRig(t, Options{Answers: answers})
				return r
			},
			drive: func(t *testing.T, r *rig) proxyAnswer {
				return answeredBy(r.get(r.client(azureFederatedPlaceholder), "https://"+boundHost+"/v1/subscriptions", "Authorization", "Bearer "+azureFederatedPlaceholder))
			},
			status: http.StatusForbidden, code: codeFederatedSwap,
			reason: "refusing to swap " + azureName + "'s federated token into a request that is no token request: that token leaves the broker only in an exchange at the token authority",
			caller: boundCaller, secrets: []string{azureName}, host: boundHost, path: "/v1/subscriptions",
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
		{
			site: siteAzureBound, name: "a token request naming another client_id",
			rig: func(t *testing.T) *rig {
				r, _ := newAzureRig(t, Options{Answers: answers})
				return r
			},
			drive: func(t *testing.T, r *rig) proxyAnswer {
				form := tokenForm(azureFederatedPlaceholder)
				form.Set("client_id", azureOtherClientID)
				return answeredBy(tokenRequestAt(t, r, r.client(azureFederatedPlaceholder), azureTokenPath, form))
			},
			status: http.StatusForbidden, code: codeTokenApplication,
			reason: "refusing an exchange of " + azureName + "'s federated token whose client_id is not the application " + azureName + "'s secret names",
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
				"caller": row.caller, "secrets": secrets, "secrets_count": len(secrets), "host": row.host, "path": row.path, "route": "",
			} {
				if fmt.Sprint(record.fields[field]) != fmt.Sprint(want) {
					t.Errorf("%s: the record's %s is %v, want %v: %v", row.site, field, record.fields[field], want, record.fields)
				}
			}
			if omitted, carried := record.fields["secrets_omitted"]; carried {
				t.Errorf("%s: the record carries secrets_omitted %v for a call that named one secret at most: %v", row.site, omitted, record.fields)
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

// TestAnAirlockRefusalLineStaysUnderOneLogEntryWhateverHostTheCallerNames: a refusal's line on
// the proxy port is the one record of that refusal, so it must fit one Cloud Logging entry,
// 262,144 bytes, which Cloud Logging refuses past and GKE drops a jsonPayload entry past. Its
// host field is cut to 256 bytes, and so must be every other field that holds the caller's host:
// the reason of a CONNECT target that is no host and port, and of a name that does not resolve,
// on a CONNECT and on a plain proxy request, names the host, and net/http hands the handler an
// authority of up to a mebibyte. Each row writes its line through the broker's own JSON handler
// (logging.NewCloudHandler), so its size is the entry's.
//
// The named revert: put the caller's whole host into the reason (r.Host and checkedAddresses'
// error, as at 49bc87bc); each row's line is then about 300 KB and this test fails.
func TestAnAirlockRefusalLineStaysUnderOneLogEntryWhateverHostTheCallerNames(t *testing.T) {
	const entryLimit = 256 << 10
	long := strings.Repeat("a", 300000)
	driven := 0
	for _, row := range []struct {
		name   string
		status int
		send   func(r *rig) *http.Response
	}{
		{"a CONNECT target that does not resolve", http.StatusForbidden, func(r *rig) *http.Response {
			answer, _, _ := r.connectRaw(long+":443", basic(workerPlaceholder))
			return answer
		}},
		{"a CONNECT target that is no host and port", http.StatusBadRequest, func(r *rig) *http.Response {
			answer, _, _ := r.connectRaw(long+"]", basic(workerPlaceholder))
			return answer
		}},
		{"a plain proxy request to a host that does not resolve", http.StatusForbidden, func(r *rig) *http.Response {
			answer, _ := r.get(r.client(workerPlaceholder), "http://"+long+"/")
			return answer
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			driven++
			r := newRig(t, Options{})
			response := row.send(r)
			id := response.Header.Get(answer.Header)
			if response.StatusCode != row.status || id == "" {
				t.Fatalf("the answer is %d with answer id %q, want %d with one", response.StatusCode, id, row.status)
			}
			var lines []string
			for _, record := range r.logs.Records() {
				if record.Message != answer.RefusalMessage {
					continue
				}
				named := false
				record.Attrs(func(a slog.Attr) bool {
					named = named || (a.Key == "answer_id" && a.Value.String() == id)
					return true
				})
				if !named {
					continue
				}
				out := &logtest.Buffer{}
				if err := logging.NewCloudHandler(out).Handle(context.Background(), record); err != nil {
					t.Fatalf("write the refusal's line: %v", err)
				}
				lines = append(lines, out.String())
			}
			if len(lines) != 1 {
				t.Fatalf("the refusal wrote %d lines naming its answer id, want 1", len(lines))
			}
			if size := len(lines[0]); size >= entryLimit {
				t.Fatalf("the refusal's line is %d bytes, want under the %d bytes of one Cloud Logging entry", size, entryLimit)
			}
		})
	}
	if driven != 3 {
		t.Fatalf("the table drove %d rows, want 3", driven)
	}
}

// TestEveryLineOfTheAirlockCutsEachFieldTheCallerWrote: a refusal's line, a call's line, a joined
// connection's line, a cut call's line, an upstream failure's line and net/http's own line on the
// proxy port each fit one Cloud Logging entry, 262,144 bytes, whatever the caller sends, because
// each text field the caller wrote is cut to its first 256 bytes where it enters the line
// (answer.ClipFields; ca-refusal-record, Verification v19). Each row sends, through one field the
// caller chose, text that writes that line at about 300 KB uncut, on the real proxy of the rig:
//
//   - of a refusal's line, the method, the host, the path and the X-Correlation-Id of a request
//     that is no proxy request, and the reason in both its forms that quote the host - a CONNECT
//     target that is no host and port (r.Host), and a name that does not resolve
//     (checkedAddresses' error), on a CONNECT and on a plain proxy request;
//   - of a call's line (callFields), the method of a call whose grant read did not happen and the
//     path of a call the proxy carried;
//   - of a joined connection's line (watchCarried, through callFields), the method of an upgrade
//     whose entry allows any method (Allow nil) and the path of one under the allowed prefix,
//     each joined and then closed by a grant that stopped carrying it;
//   - of a cut call's line (watchCarried, through callFields), the method and the path of a call
//     whose answer streams, under the same two entries, each cut by a grant that stopped
//     carrying it;
//   - of a tunnel's line (logTunnel, through dialFields), the host of a CONNECT whose name
//     resolves to an address the proxy may dial and a service answers on, so the tunnel opens;
//   - of a failed dial's line (logDialFailure, through dialFields), the host and the reason of a
//     CONNECT to a name that resolves nowhere, the reason being the proxy's own words around that
//     host;
//   - of the upstream failure's line (badGateway), the path and the host of a plain proxy request
//     whose service cannot be reached, and the error of one whose name answers the check and
//     nothing to the transport's own lookup, which quotes that host;
//   - of net/http's own line (slogWriter, the ErrorLog of both servers), the error of a TLS
//     handshake that failed on a bound connection, whose ClientHello offers only protocols the
//     proxy does not speak, which crypto/tls's error quotes whole.
//
// The cut is for the caller's text, never for the server's own. One row more panics a handler of
// the inner server (serveBound, through the clock the rig hands the proxy), which net/http
// recovers and logs with the stack of the goroutine that panicked. That line is the one record of
// where the panic happened, so it holds the first line of net/http's text as its error, cut as
// every other error is, and the stack after it whole in its stack field, which must name the
// handler that panicked; net/http bounds the stack it writes, so the line still fits the entry.
// No other row's line holds a stack.
//
// Each row writes its line through the broker's own JSON handler (logging.NewCloudHandler), reads
// its length, and reads the field back as the first 256 bytes of the text it held, which for a
// reason or an error is the proxy's words around the host, for net/http's error its words around
// the client's address and the protocols it offered, and for the panic's error, which is shorter,
// its whole first line: the client's address and the value the clock panicked with. The table
// records which field of which line it drove, and fails when it drove fewer than all of them.
//
// The named reverts: let one field pass answer.ClipFields uncut; that field's row writes a line
// of about 300 KB and fails. Or pass net/http's whole text through answer.ClipFields as its error
// with no stack field, as slogWriter did before Verification v19; the panic row then fails, since
// the first 256 bytes of that text end inside the runtime's frames, before any of the handler's.
func TestEveryLineOfTheAirlockCutsEachFieldTheCallerWrote(t *testing.T) {
	const entryLimit = 256 << 10
	// The caller's text: not one byte repeated, so its first 256 bytes are no other 256 of it.
	sent := strings.Repeat("abcdefghij", 30000)
	// public is an address the proxy may dial that no stand-in service answers on port 80, so a
	// plain proxy request to a name holding it reaches badGateway.
	public := netip.MustParseAddr("93.184.216.37")
	// notAProxyRequest is a request to the proxy port that is no proxy request, changed by edit,
	// which the port refuses before it reads any credential.
	notAProxyRequest := func(t *testing.T, r *rig, edit func(*http.Request)) *http.Response {
		t.Helper()
		request, err := http.NewRequest(http.MethodGet, "http://"+r.addr+"/healthz", nil)
		if err != nil {
			t.Fatal(err)
		}
		edit(request)
		response, err := (&http.Client{Timeout: 20 * time.Second}).Do(request)
		if err != nil {
			t.Fatalf("a request to the proxy port: %v", err)
		}
		bodyOf(t, response)
		return response
	}
	// bound is one call to the bound host through the proxy, as a command's client makes it.
	bound := func(r *rig, method, path string) *http.Response {
		response, _ := r.do(r.client(workerPlaceholder), method, "https://"+boundHost+path, "", "Authorization", "Bearer "+workerPlaceholder)
		return response
	}
	connect := func(target string) func(t *testing.T, r *rig) *http.Response {
		return func(_ *testing.T, r *rig) *http.Response {
			response, _, _ := r.connectRaw(target, basic(workerPlaceholder))
			return response
		}
	}
	// join upgrades to the service at that method and path, joins the two connections, and then
	// ends the grant behind them, which is what makes the watcher write the join's own line
	// (TestAJoinedConnectionIsClosedWhenItsGrantStopsCarryingIt drives the same close).
	join := func(method, path string) func(t *testing.T, r *rig) *http.Response {
		return func(t *testing.T, r *rig) *http.Response {
			_, conn, reader, upgraded := joinedThroughPath(t, r, method, path)
			r.grants.refuseConnection(workerPlaceholder, requests.ErrGrantNotLive)
			waitClosed(t, conn, reader)
			return upgraded
		}
	}
	// cut streams the service's answer to a call at that method and path, and then ends the grant
	// behind it, which is what makes the watcher write the cut's own line
	// (TestACarriedStreamEndsWhenItsGrantStopsCarryingIt drives the same cut).
	cut := func(method, path string) func(t *testing.T, r *rig) *http.Response {
		return func(t *testing.T, r *rig) *http.Response {
			streamStandIn(r)
			request, err := http.NewRequest(method, "https://"+boundHost+path, nil)
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Authorization", "Bearer "+workerPlaceholder)
			response, err := r.client(workerPlaceholder).Do(request)
			if err != nil {
				t.Fatalf("the streamed call through the proxy: %v", err)
			}
			defer response.Body.Close()
			reader := bufio.NewReader(response.Body)
			if _, err := reader.ReadString('\n'); err != nil {
				t.Fatalf("the stream's first line: %v", err)
			}
			r.grants.refuseConnection(workerPlaceholder, requests.ErrGrantNotLive)
			_, _ = io.Copy(io.Discard, reader)
			return response
		}
	}
	// anyMethod is the rules file of the join's method row: an entry with no allow list, which
	// the airlock reads as any method and path (airlock.SecretRule.Allow), so the upgrade the row
	// sends is carried rather than refused and its method reaches the join's line.
	anyMethod := airlock.Rules{Secrets: []airlock.SecretRule{{
		Name: workerName, Hosts: []string{boundHost}, Headers: []string{"authorization"},
	}}}
	// protocols is an ALPN list without http/1.1 that crypto/tls's client still sends: 240 names
	// of 255 bytes, the most one name holds, which keeps the ClientHello under the 65,536 bytes
	// crypto/tls's server reads of one handshake message. Each byte is one %q writes as four
	// (\x00) and the JSON handler as five, so net/http's line quoting the list is about 300 KB
	// uncut.
	protocols := make([]string, 240)
	for i := range protocols {
		protocols[i] = strings.Repeat("\x00", 255)
	}
	// handshakeError is what net/http's line holds before the cut. It names the client's own
	// address, which only the row's dial picks, so the row's send writes it.
	var handshakeError string
	// panicked is what the panic row's clock panics with, and panicError the first line of
	// net/http's text for it, which names the client's address as handshakeError does.
	const panicked = "the clock the panic row hands the proxy panicked"
	var panicError string
	// fields is every field of each line that holds the caller's text, each of which a row drives.
	fields := map[string][]string{
		answer.RefusalMessage: {"method", "host", "path", "correlation_id", "reason"},
		callMessage:           {"method", "path"},
		joinMessage:           {"method", "path"},
		cutMessage:            {"method", "path"},
		tunnelMessage:         {"host"},
		dialFailedMessage:     {"host", "reason"},
		upstreamMessage:       {"path", "host", "error"},
		netHTTPMessage:        {"error"},
	}
	drove := map[string]bool{}
	driven := 0
	for _, row := range []struct {
		name string
		// message is the line's, field the field the caller's text goes into, and held what that
		// field holds before the cut.
		message, field, held string
		// heldBy, where set, answers held once send has run, for a field whose text names what
		// only the row's own dial picks.
		heldBy func() string
		// frame, where set, is the function the line's stack must name: the handler that
		// panicked. A row that sets none writes a line with no stack.
		frame  string
		status int
		// options is the rig's; the zero value is the plain rig every other row drives.
		options Options
		send    func(t *testing.T, r *rig) *http.Response
	}{
		{name: "a refusal's method", message: answer.RefusalMessage, field: "method", held: sent, status: http.StatusBadRequest,
			send: func(t *testing.T, r *rig) *http.Response {
				return notAProxyRequest(t, r, func(request *http.Request) { request.Method = sent })
			}},
		{name: "a refusal's host", message: answer.RefusalMessage, field: "host", held: sent, status: http.StatusBadRequest,
			send: func(t *testing.T, r *rig) *http.Response {
				return notAProxyRequest(t, r, func(request *http.Request) { request.Host = sent })
			}},
		{name: "a refusal's path", message: answer.RefusalMessage, field: "path", held: "/" + sent, status: http.StatusBadRequest,
			send: func(t *testing.T, r *rig) *http.Response {
				return notAProxyRequest(t, r, func(request *http.Request) { request.URL.Path = "/" + sent })
			}},
		{name: "a refusal's X-Correlation-Id", message: answer.RefusalMessage, field: "correlation_id", held: sent, status: http.StatusBadRequest,
			send: func(t *testing.T, r *rig) *http.Response {
				return notAProxyRequest(t, r, func(request *http.Request) { request.Header.Set("X-Correlation-Id", sent) })
			}},
		{name: "a refusal's reason: a CONNECT target that is no host and port", message: answer.RefusalMessage, field: "reason",
			held: sent + "] is no host and port", status: http.StatusBadRequest, send: connect(sent + "]")},
		{name: "a refusal's reason: a CONNECT target that does not resolve", message: answer.RefusalMessage, field: "reason",
			held: "cannot resolve " + sent + ": no such host", status: http.StatusForbidden, send: connect(sent + ":443")},
		{name: "a refusal's reason: a plain proxy request to a host that does not resolve", message: answer.RefusalMessage, field: "reason",
			held: "cannot resolve " + sent + ": no such host", status: http.StatusForbidden,
			send: func(_ *testing.T, r *rig) *http.Response {
				response, _ := r.get(r.client(workerPlaceholder), "http://"+sent+"/")
				return response
			}},
		{name: "a call's method", message: callMessage, field: "method", held: sent, status: http.StatusServiceUnavailable,
			send: func(_ *testing.T, r *rig) *http.Response {
				r.grants.refuseValues(databaseDown)
				return bound(r, sent, "/v1/things")
			}},
		{name: "a call's path", message: callMessage, field: "path", held: "/v1/" + sent, status: http.StatusOK,
			send: func(_ *testing.T, r *rig) *http.Response {
				return bound(r, http.MethodGet, "/v1/"+sent)
			}},
		{name: "a joined connection's method", message: joinMessage, field: "method", held: sent, status: http.StatusSwitchingProtocols,
			options: Options{Watch: watchFast, Rules: anyMethod}, send: join(sent, upgradePath)},
		{name: "a joined connection's path", message: joinMessage, field: "path", held: "/v1/" + sent, status: http.StatusSwitchingProtocols,
			options: Options{Watch: watchFast}, send: join(http.MethodGet, "/v1/"+sent)},
		{name: "a cut call's method", message: cutMessage, field: "method", held: sent, status: http.StatusOK,
			options: Options{Watch: watchFast, Rules: anyMethod}, send: cut(sent, "/v1/stream")},
		{name: "a cut call's path", message: cutMessage, field: "path", held: "/v1/" + sent, status: http.StatusOK,
			options: Options{Watch: watchFast}, send: cut(http.MethodGet, "/v1/"+sent)},
		{name: "a tunnel's host: a CONNECT to a name the proxy may dial that a service answers", message: tunnelMessage, field: "host",
			held: sent, status: http.StatusOK,
			send: func(_ *testing.T, r *rig) *http.Response {
				// The name resolves to a public address wired to the echo service, so the
				// CONNECT opens a tunnel and the proxy writes that tunnel's one line.
				r.addresses[sent] = []netip.Addr{public}
				r.dialTo[net.JoinHostPort(public.String(), "443")] = r.dialTo[net.JoinHostPort(echoHost, "443")]
				response, _, _ := r.connectRaw(sent+":443", basic(workerPlaceholder))
				return response
			}},
		{name: "a failed dial's host: a CONNECT to a name that resolves nowhere", message: dialFailedMessage, field: "host",
			held: sent, status: http.StatusForbidden, send: connect(sent + ":443")},
		{name: "a failed dial's reason: a CONNECT to a name that resolves nowhere", message: dialFailedMessage, field: "reason",
			held: "cannot resolve " + sent + ": no such host", status: http.StatusForbidden, send: connect(sent + ":443")},
		{name: "an upstream failure's path: a plain proxy request whose service cannot be reached", message: upstreamMessage, field: "path",
			held: "/" + sent, status: http.StatusBadGateway,
			send: func(_ *testing.T, r *rig) *http.Response {
				// The rig dials nothing on port 80, so the request is a 502 of badGateway's.
				response, _ := r.get(r.client(workerPlaceholder), "http://"+echoHost+"/"+sent)
				return response
			}},
		{name: "an upstream failure's host: a plain proxy request to a name that resolves and answers nothing", message: upstreamMessage, field: "host",
			held: sent, status: http.StatusBadGateway,
			send: func(_ *testing.T, r *rig) *http.Response {
				// The name resolves to an address the proxy may dial and no service answers on.
				r.addresses[sent] = []netip.Addr{public}
				response, _ := r.get(r.client(workerPlaceholder), "http://"+sent+"/")
				return response
			}},
		{name: "an upstream failure's error: a name that answers the check and nothing the transport's own lookup", message: upstreamMessage, field: "error",
			held: sent + " resolves to no address", status: http.StatusBadGateway,
			send: func(_ *testing.T, r *rig) *http.Response {
				// One answer for servePlain's check and none for the transport's lookup, whose
				// error names the host the caller asked for.
				r.answers[sent] = [][]netip.Addr{{public}, {}}
				response, _ := r.get(r.client(workerPlaceholder), "http://"+sent+"/")
				return response
			}},
		{name: "Go's HTTP server's error: a TLS handshake on a bound connection whose ClientHello offers no protocol the proxy speaks", message: netHTTPMessage, field: "error",
			// The error quotes each protocol the client offered, which the line writes as
			// logmask.PeerBytesShown (logmask.HidePeerBytes), one for each: still past the cut.
			heldBy: func() string { return logmask.HidePeerBytes(handshakeError) }, status: http.StatusOK,
			send: func(t *testing.T, r *rig) *http.Response {
				response, conn, _ := r.connectRaw(boundHost+":443", basic(workerPlaceholder))
				// net/http names the client by the address its CONNECT came from, this end of the
				// connection, and crypto/tls quotes every protocol the client offered.
				handshakeError = fmt.Sprintf("http: TLS handshake error from %s: tls: client requested unsupported application protocols (%q)",
					conn.LocalAddr(), protocols)
				client := tls.Client(conn, &tls.Config{ServerName: boundHost, RootCAs: r.trust, NextProtos: protocols})
				if err := client.Handshake(); err == nil {
					t.Fatal("the proxy finished a handshake whose ClientHello offered no protocol it speaks; want it refused")
				}
				return response
			}},
		{name: "Go's HTTP server's error: a handler of the inner server that panics, whose stack the line keeps whole", message: netHTTPMessage, field: "error",
			heldBy: func() string { return panicError }, frame: "github.com/sjawhar/envoy/internal/broker/airlockproxy.(*Server).serveBound(",
			status: http.StatusOK, options: Options{Now: func() time.Time { panic(panicked) }},
			send: func(t *testing.T, r *rig) *http.Response {
				response, conn, _ := r.connectRaw(boundHost+":443", basic(workerPlaceholder))
				// net/http names the client as the handshake row's line does, and the value the
				// handler panicked with beside it.
				panicError = fmt.Sprintf("http: panic serving %s: %s", conn.LocalAddr(), panicked)
				client := tls.Client(conn, &tls.Config{ServerName: boundHost, RootCAs: r.trust})
				// A call to the bound host: serveBound reads the grant, chooses the name the call is
				// for (choosePair) and asks for the grant's values, whose first read of the clock panics.
				call := "GET /v1/things HTTP/1.1\r\nHost: " + boundHost + "\r\nAuthorization: Bearer " + workerPlaceholder + "\r\n\r\n"
				if _, err := client.Write([]byte(call)); err != nil {
					t.Fatalf("write the call on the bound connection: %v", err)
				}
				if answered, err := http.ReadResponse(bufio.NewReader(client), nil); err == nil {
					t.Fatalf("the call whose handler panicked was answered %d; want the connection closed with no answer", answered.StatusCode)
				}
				return response
			}},
	} {
		t.Run(row.name, func(t *testing.T) {
			driven++
			drove[row.message+" "+row.field] = true
			r := newRig(t, row.options)
			response := row.send(t, r)
			id := response.Header.Get(answer.Header)
			if response.StatusCode != row.status || id == "" {
				t.Fatalf("the answer is %d with answer id %q, want %d with one", response.StatusCode, id, row.status)
			}
			// A call's line is written once its answer has been copied to the client, so it
			// can come a moment after the client has read the answer, and net/http's once the
			// handshake has failed or the handler has panicked. The upstream failure's line and
			// net/http's name no answer id, so each is read by its message alone: each row has a
			// rig of its own.
			var lines []string
			for deadline := time.Now().Add(10 * time.Second); len(lines) == 0 && time.Now().Before(deadline); time.Sleep(time.Millisecond) {
				for _, record := range r.logs.Records() {
					named, carries := false, false
					record.Attrs(func(a slog.Attr) bool {
						carries = carries || a.Key == "answer_id"
						named = named || (a.Key == "answer_id" && a.Value.String() == id)
						return true
					})
					if record.Message != row.message || (carries && !named) {
						continue
					}
					out := &logtest.Buffer{}
					if err := logging.NewCloudHandler(out).Handle(context.Background(), record); err != nil {
						t.Fatalf("write the line: %v", err)
					}
					lines = append(lines, out.String())
				}
			}
			if len(lines) != 1 {
				t.Fatalf("the answer wrote %d lines under %q naming its answer id, or none, want 1", len(lines), row.message)
			}
			if size := len(lines[0]); size >= entryLimit {
				t.Fatalf("the line is %d bytes, want under the %d bytes of one Cloud Logging entry", size, entryLimit)
			}
			var written map[string]any
			if err := json.Unmarshal([]byte(lines[0]), &written); err != nil {
				t.Fatalf("the line is not one JSON object: %v", err)
			}
			// The stack first, so that a line that kept no stack fails on the frame it lost
			// rather than on the error that holds the stack's first bytes in its place.
			stack, _ := written["stack"].(string)
			_, stacked := written["stack"]
			switch {
			case row.frame == "" && stacked:
				t.Fatalf("the line has a stack of %d bytes, want none: no handler panicked", len(stack))
			case row.frame != "" && !strings.Contains(stack, row.frame):
				t.Fatalf("the line names no frame %s, the handler that panicked: its stack is %d bytes and its error %q",
					row.frame, len(stack), written["error"])
			}
			held := row.held
			if row.heldBy != nil {
				held = row.heldBy()
			}
			want := held[:min(len(held), 256)]
			if kept, _ := written[row.field].(string); kept != want {
				t.Fatalf("the line's %s is %d bytes, want the first %d of the %d it held: %.80q", row.field, len(kept), len(want), len(held), kept)
			}
		})
	}
	var missing []string
	for message, named := range fields {
		for _, field := range named {
			if !drove[message+" "+field] {
				missing = append(missing, message+" "+field)
			}
		}
	}
	if driven != 21 || len(missing) != 0 {
		t.Fatalf("the table drove %d rows, want 21; it never drove %v", driven, missing)
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
