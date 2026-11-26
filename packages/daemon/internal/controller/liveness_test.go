package controller

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/testport"
)

var probeNow = time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)

// listener is the Envoy listener's role lookup as the probe reaches it: it answers every request
// with status and body, and records the path and bearer it was asked with.
type listener struct {
	status        int
	body          string
	path, bearer  string
	authorization bool
}

func (l *listener) serve(t *testing.T) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		l.path = r.URL.Path
		l.bearer = r.Header.Get("Authorization")
		_, l.authorization = r.Header["Authorization"]
		w.WriteHeader(l.status)
		_, _ = io.WriteString(w, l.body)
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// holder is the listener's live-holder answer (roleGetHandler, packages/envoy/cmd/listener/api.go):
// last_seen in unix milliseconds.
func holder(session string, lastSeen time.Time) string {
	return `{"role":"legion-demo-controller","holder":"` + session +
		`","title":"controller","dir":"/x","machine_id":"m","capabilities":[],"last_seen":` + strconv.FormatInt(lastSeen.UnixMilli(), 10) + `}`
}

// lapsed is the listener's 404 for a role whose holder is past the live window, in state
// (writeRoleHolderError, packages/envoy/cmd/listener/api.go).
func lapsed(state string) string {
	return `{"error":"role legion-demo-controller holder ses_ctl lapsed; claim retained","reason":"holder_lapsed","holder":"ses_ctl","holder_state":"` +
		state + `","last_seen":` + strconv.FormatInt(probeNow.Add(-10*time.Minute).UnixMilli(), 10) + `,"claim_released":false}`
}

func newTestProber(url, token string, log *bytes.Buffer) *Prober {
	return NewProber(ProberOptions{
		EnvoyURL: url, EnvoyToken: token, Project: "demo", Log: slog.New(slog.NewTextHandler(log, nil)),
	})
}

// cutAnswer serves status with half of body under a Content-Length for the whole of it, then
// closes the connection, as a reset mid-body reads to the client: the headers, part of the body,
// then an unexpected end.
func cutAnswer(t *testing.T, status int, body string) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, out, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack the connection: %v", err)
			return
		}
		defer conn.Close()
		fmt.Fprintf(out, "HTTP/1.1 %d %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s",
			status, http.StatusText(status), len(body), body[:len(body)/2])
		_ = out.Flush()
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// Live while the listener names the registered session as the controller role's holder, which it
// does only inside its own live window; the lookup is the project's controller token, with the
// listener's bearer.
func TestProbeIsLiveWhileTheSessionHoldsTheRole(t *testing.T) {
	var log bytes.Buffer
	l := &listener{status: http.StatusOK, body: holder("ses_ctl", probeNow.Add(-4*time.Minute))}
	url := l.serve(t)
	if got := newTestProber(url, "envoy-bearer", &log).Probe(context.Background(), "ses_ctl"); got != Live {
		t.Fatalf("Probe = %s, want live; log %s", got, log.String())
	}
	if l.path != "/v1/roles/legion-demo-controller" || l.bearer != "Bearer envoy-bearer" {
		t.Fatalf("lookup = %s with %q, want the controller token with the listener's bearer", l.path, l.bearer)
	}
}

func TestProbeSendsNoBearerWithoutAToken(t *testing.T) {
	var log bytes.Buffer
	l := &listener{status: http.StatusOK, body: holder("ses_ctl", probeNow)}
	url := l.serve(t)
	if got := newTestProber(url, "", &log).Probe(context.Background(), "ses_ctl"); got != Live {
		t.Fatalf("Probe = %s, want live", got)
	}
	if l.authorization {
		t.Fatalf("the lookup carried Authorization %q with no token configured", l.bearer)
	}
}

// Gone: nobody claimed the role, its holder reached the gone horizon, another session holds it, or
// a listener of an earlier release named no state with its 404 — that listener's lapsed holder is
// no holder at all.
func TestProbeIsGone(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		body     string
		wantLogs []string
	}{
		{name: "nobody claimed the role", status: http.StatusNotFound, body: `{"error":"no holder for role legion-demo-controller","reason":"unclaimed"}`,
			wantLogs: []string{"unclaimed"}},
		{name: "the holder is gone", status: http.StatusNotFound, body: lapsed("gone"), wantLogs: []string{"holder_lapsed", "gone"}},
		{name: "an older listener names no state", status: http.StatusNotFound, body: `{"error":"no live holder for role legion-demo-controller"}`},
		{name: "a 404 whose body is not JSON", status: http.StatusNotFound, body: "<html>"},
		{name: "another session holds the role", status: http.StatusOK, body: holder("ses_other", probeNow),
			wantLogs: []string{"ses_other", "ses_ctl"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var log bytes.Buffer
			l := &listener{status: tc.status, body: tc.body}
			url := l.serve(t)
			if got := newTestProber(url, "envoy-bearer", &log).Probe(context.Background(), "ses_ctl"); got != Gone {
				t.Fatalf("Probe = %s, want gone; log %s", got, log.String())
			}
			for _, want := range tc.wantLogs {
				if !strings.Contains(log.String(), want) {
					t.Errorf("log %q does not name %q", log.String(), want)
				}
			}
		})
	}
}

// A holder the listener calls unresponsive is past its live window and inside its gone horizon: it
// still holds the role and answers for itself once its heartbeats land, so the probe says Unknown
// and the keeper leaves the controller alone rather than replacing a session that is working
// (dispatch://INFRA-392).
func TestProbeIsUnknownForAnUnresponsiveHolder(t *testing.T) {
	var log bytes.Buffer
	l := &listener{status: http.StatusNotFound, body: lapsed("unresponsive")}
	url := l.serve(t)
	if got := newTestProber(url, "envoy-bearer", &log).Probe(context.Background(), "ses_ctl"); got != Unknown {
		t.Fatalf("Probe = %s, want unknown; log %s", got, log.String())
	}
	if !strings.Contains(log.String(), "unresponsive, not gone") {
		t.Errorf("log %q does not say the holder is unresponsive rather than gone", log.String())
	}
}

// A 404 whose body could not be read to its end is no death verdict (review round 19 of #4220): it
// may have named an unresponsive holder, which is not gone. The control (the pull request records
// the run): the probe reading the cut body as an empty refusal answers gone.
func TestProbeIsUnknownForA404ItCouldNotRead(t *testing.T) {
	var log bytes.Buffer
	url := cutAnswer(t, http.StatusNotFound, lapsed("unresponsive"))
	if got := newTestProber(url, "envoy-bearer", &log).Probe(context.Background(), "ses_ctl"); got != Unknown {
		t.Fatalf("Probe = %s, want unknown: the 404's body was cut off, and it named an unresponsive holder; log %s", got, log.String())
	}
	if !strings.Contains(log.String(), "could not be read to its end") {
		t.Errorf("log %q does not say the 404 could not be read", log.String())
	}
}

// Unknown, never a death verdict: the listener unreachable, a refusal other than 404, or a body
// the probe cannot read.
func TestProbeIsUnknown(t *testing.T) {
	for _, tc := range []struct {
		name, token string
		status      int
		body        string
		wantLog     string
	}{
		{name: "a server error", token: "envoy-bearer", status: http.StatusInternalServerError, body: `{"error":"kv"}`, wantLog: "500"},
		{name: "an unauthorized lookup with no bearer", status: http.StatusUnauthorized, body: `{"error":"unauthorized"}`, wantLog: "no bearer token sent"},
		{name: "a body that is not JSON", token: "envoy-bearer", status: http.StatusOK, body: "<html>", wantLog: "unreadable"},
		{name: "a body with no holder", token: "envoy-bearer", status: http.StatusOK, body: `{"last_seen":1}`, wantLog: "unreadable"},
		{name: "a last_seen that is not an integer", token: "envoy-bearer", status: http.StatusOK, body: `{"holder":"ses_ctl","last_seen":"now"}`, wantLog: "unreadable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var log bytes.Buffer
			l := &listener{status: tc.status, body: tc.body}
			url := l.serve(t)
			if got := newTestProber(url, tc.token, &log).Probe(context.Background(), "ses_ctl"); got != Unknown {
				t.Fatalf("Probe = %s, want unknown", got)
			}
			if !strings.Contains(log.String(), tc.wantLog) {
				t.Errorf("log %q does not say %q", log.String(), tc.wantLog)
			}
		})
	}
	t.Run("the listener is unreachable", func(t *testing.T) {
		var log bytes.Buffer
		// A port the test holds and nothing listens on: every dial to it is refused.
		url := "http://" + testport.Refused(t)
		if got := newTestProber(url, "envoy-bearer", &log).Probe(context.Background(), "ses_ctl"); got != Unknown {
			t.Fatalf("Probe = %s, want unknown", got)
		}
		if !strings.Contains(log.String(), "unreachable") {
			t.Errorf("log %q does not say the listener was unreachable", log.String())
		}
	})
}

// Every line the probe logs names the lookup without envoy_url's userinfo, where a deployment
// writes the listener's credential: a made-up token written as the user, and a made-up password
// holding a "'", which a text rule stops reading at. The lookup is named as redact.URL shows such a
// URL, its scheme and the mark of a URL with credentials, then its path; the unreachable one also
// logs the HTTP client's error, without its quote of the URL. The probe requests the lookup without
// the userinfo and sends it as Basic auth, as the HTTP client sends a URL's own, when no bearer
// token is configured. A password holding an "@" and no host after it (`http://u:p@<tail>`) sends
// the lookup to its tail, which neither the url field nor the resolver's error names.
func TestProbeLogsNoCredentialOfTheEnvoyURL(t *testing.T) {
	const userToken, quotedUserinfo = "madeUpEnvoyToken0123", "legion:madeUpEnvoy'Password0123"
	withUser := func(base, user string) string { return strings.Replace(base, "://", "://"+user+"@", 1) }
	l := &listener{status: http.StatusInternalServerError, body: `{"error":"kv"}`}
	served := l.serve(t)
	unreached := httptest.NewServer(http.NotFoundHandler())
	gone := unreached.URL
	unreached.Close()
	for name, base := range map[string]string{"refused": served, "unreachable": gone} {
		for _, user := range []string{userToken, quotedUserinfo} {
			var log bytes.Buffer
			if got := newTestProber(withUser(base, user), "envoy-bearer", &log).Probe(context.Background(), "ses_ctl"); got != Unknown {
				t.Fatalf("%s: Probe = %s, want unknown", name, got)
			}
			if !strings.Contains(log.String(), `url="http://[a URL with credentials]/v1/roles/`) {
				t.Errorf("%s: the log %q does not name the lookup by the mark of a URL with credentials", name, log.String())
			}
			// Both made-up credentials begin with this, so it catches either.
			if strings.Contains(log.String(), "madeUpEnvoy") {
				t.Errorf("%s: the log names the credential: %s", name, log.String())
			}
		}
	}
	var tailLog bytes.Buffer
	tailURL := "http://legion:p" + "@madeUpEnvoyTail0123.invalid"
	if got := newTestProber(tailURL, "envoy-bearer", &tailLog).Probe(context.Background(), "ses_ctl"); got != Unknown {
		t.Fatalf("a password holding an @: Probe = %s, want unknown", got)
	}
	if strings.Contains(tailLog.String(), "madeUpEnvoyTail") || !strings.Contains(tailLog.String(), "unreachable") {
		t.Errorf("a password holding an @: the log %q names the password's tail, or not the unreachable listener", tailLog.String())
	}
	// A tail that is not ASCII is dialed by its ASCII form (`xn--…`, its ASCII letters kept and
	// lower-cased), which the resolver's error names: the error is not printed.
	var asciiLog bytes.Buffer
	asciiURL := "http://legion:p" + "@sswmadeuptail0123\u00f6rd.invalid"
	if got := newTestProber(asciiURL, "envoy-bearer", &asciiLog).Probe(context.Background(), "ses_ctl"); got != Unknown {
		t.Fatalf("a password holding an @ and a tail that is not ASCII: Probe = %s, want unknown", got)
	}
	if lower := strings.ToLower(asciiLog.String()); strings.Contains(lower, "madeuptail") || strings.Contains(lower, "xn--") {
		t.Errorf("a password holding an @ and a tail that is not ASCII: the log %q names the password's tail", asciiLog.String())
	}
	// A password whose part after its "@" ends in ":<digits>" leaves them where the port goes, and
	// the dialer's refusal of an out-of-range port quotes them: the error is not printed.
	var portLog bytes.Buffer
	portURL := "http://legion:p" + "@madeuptail0123.invalid:98765432"
	if got := newTestProber(portURL, "envoy-bearer", &portLog).Probe(context.Background(), "ses_ctl"); got != Unknown {
		t.Fatalf("a password ending in digits read as the port: Probe = %s, want unknown", got)
	}
	if strings.Contains(portLog.String(), "98765432") || strings.Contains(portLog.String(), "madeuptail") {
		t.Errorf("a password ending in digits read as the port: the log %q names part of the password", portLog.String())
	}
	var log bytes.Buffer
	newTestProber(withUser(served, quotedUserinfo), "", &log).Probe(context.Background(), "ses_ctl")
	if user, password, ok := (&http.Request{Header: http.Header{"Authorization": {l.bearer}}}).BasicAuth(); !ok || user+":"+password != quotedUserinfo {
		t.Errorf("with no bearer token the lookup carried Authorization %q, want envoy_url's userinfo as Basic auth", l.bearer)
	}
}

// An envoy_url with userinfo whose host resolves and whose port is closed: the dial's error names
// the resolved address and the port (`dial tcp 127.0.0.1:<port>: connect: connection refused`), and
// the port can be a password's digits, so the line writes it as the mark, as it writes the host.
// The password is made up.
func TestProbeLogsNoPortOfAnEnvoyURLWithUserinfo(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	var log bytes.Buffer
	envoyURL := "http://legion:" + "madeuphead0123" + "@127.0.0.1:" + port
	if got := newTestProber(envoyURL, "envoy-bearer", &log).Probe(context.Background(), "ses_ctl"); got != Unknown {
		t.Fatalf("Probe = %s, want unknown", got)
	}
	if !strings.Contains(log.String(), "connect: connection refused") {
		t.Fatalf("the log %q holds no refused dial", log.String())
	}
	for _, part := range []string{":" + port, "madeuphead"} {
		if strings.Contains(log.String(), part) {
			t.Errorf("the log %q holds %q", log.String(), part)
		}
	}
}

// The probe follows no redirect: its lookup carries the credential in an explicit Authorization
// header, which Go's client copies to a redirect's target on the same host, so a listener that
// redirected would hand the credential to another path. A redirect is an answer the probe reads as
// Unknown, and the target is never asked. The control is a client that follows redirects, which
// asks the target with the bearer.
func TestProbeFollowsNoRedirect(t *testing.T) {
	var targeted, bearer string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/elsewhere" {
			targeted, bearer = r.URL.Path, r.Header.Get("Authorization")
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	}))
	t.Cleanup(server.Close)
	var log bytes.Buffer
	if got := newTestProber(server.URL, "envoy-bearer", &log).Probe(context.Background(), "ses_ctl"); got != Unknown {
		t.Fatalf("Probe = %s, want unknown", got)
	}
	if targeted != "" || bearer != "" {
		t.Fatalf("the probe followed the redirect to %q with Authorization %q", targeted, bearer)
	}
	if !strings.Contains(log.String(), "status=302") || !strings.Contains(log.String(), "answered the controller role lookup with a redirect, which the probe does not follow") {
		t.Errorf("the log %q does not name the redirect, not followed, and its status", log.String())
	}
}
