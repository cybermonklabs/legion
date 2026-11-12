package api

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// answeringUnderTest is an Answering serving a loopback listener, with the base URL a caller
// reaches it on. The server stands down when the test ends.
type answeringUnderTest struct {
	*Answering
	base   string
	client *http.Client
}

func newAnsweringUnderTest(t *testing.T) *answeringUnderTest {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen on loopback: %v", err)
	}
	a := NewAnswering(listener, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() {
		a.Stop()
		if err := a.Wait(); err != nil {
			t.Errorf("the serve goroutine failed: %v", err)
		}
	})
	return &answeringUnderTest{
		Answering: a,
		base:      "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)),
		client:    &http.Client{Timeout: 10 * time.Second},
	}
}

// do sends one request and answers its status and body.
func (a *answeringUnderTest) do(t *testing.T, method, path string, body any) (int, []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode the request body: %v", err)
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(context.Background(), method, a.base+path, reader)
	if err != nil {
		t.Fatalf("build the request: %v", err)
	}
	response, err := a.client.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read the answer to %s %s: %v", method, path, err)
	}
	return response.StatusCode, answer
}

// A registration that arrives while the daemon starts is answered (dispatch://INFRA-2883 R5), and
// so is everything else: the start handler routes nothing, because a caller that meets no answer
// at all is the failure this exists to end, whichever call it made.
func TestTheStartHandlerAnswersEveryRouteWhileTheDaemonStarts(t *testing.T) {
	a := newAnsweringUnderTest(t)
	for _, route := range []struct {
		method, path string
		body         any
	}{
		{http.MethodPost, "/legion/v1/claims/register", claim.RegisterRequest{
			BootToken: "boot-token", SessionID: "ses_1", OmpSessionFile: "/sessions/ses_1.jsonl", AgentID: "ses_1",
		}},
		{http.MethodPost, "/legion/v1/claims/ready", claim.ReadyRequest{
			ClaimToken: "legion-issue-1-implementer", SessionID: "ses_1", Secret: "secret", Generation: 1,
		}},
		{http.MethodPost, "/legion/v1/claims/exit", map[string]string{"claimToken": "legion-issue-1-implementer"}},
		{http.MethodGet, "/healthz", nil},
		{http.MethodGet, "/legion/v1/state", nil},
		{http.MethodPost, "/legion/v1/operator/claims", map[string]string{"tree": "LEGION-1", "issue": "LEGION-1", "role": "architect"}},
		{http.MethodGet, "/legion/v1/operator/claims", nil},
		{http.MethodDelete, "/legion/v1/claims/register", nil},
		{http.MethodGet, "/", nil},
		{http.MethodGet, "/no/such/route", nil},
	} {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			status, body := a.do(t, route.method, route.path, route.body)
			if status != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want %d while the daemon starts; body %s", status, http.StatusServiceUnavailable, body)
			}
			var answer map[string]string
			if err := json.Unmarshal(body, &answer); err != nil {
				t.Fatalf("decode %s: %v", body, err)
			}
			if answer["error"] != startRefusal {
				t.Fatalf("error = %q, want the start's refusal %q", answer["error"], startRefusal)
			}
		})
	}
}

// The start's answer is the shape every caller already retries: the uncoded 503 the daemon's
// stopping refusal writes, with a sentence that names the start and says the caller may ask
// again. A code would be a new thing for a plugin to parse; the shape is held here against the
// stopping refusal the real routes give, so the two cannot drift apart.
func TestTheStartAnswerIsAnUncoded503AndNamesTheStart(t *testing.T) {
	a := newAnsweringUnderTest(t)
	status, body := a.do(t, http.MethodPost, "/legion/v1/claims/register", claim.RegisterRequest{
		BootToken: "boot-token", SessionID: "ses_1", OmpSessionFile: "/sessions/ses_1.jsonl", AgentID: "ses_1",
	})
	if status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d; body %s", status, http.StatusServiceUnavailable, body)
	}
	var answer map[string]any
	if err := json.Unmarshal(body, &answer); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	if len(answer) != 1 {
		t.Fatalf("the answer is %s, want the error alone and no code", body)
	}
	sentence, _ := answer["error"].(string)
	for _, want := range []string{"the daemon is starting", "ask again"} {
		if !bytes.Contains([]byte(sentence), []byte(want)) {
			t.Errorf("the sentence %q does not say %q", sentence, want)
		}
	}

	stoppingStatus, stoppingBody := stoppingRefusal(t)
	if stoppingStatus != status {
		t.Errorf("the stopping refusal is %d and the start's %d; want one status for both", stoppingStatus, status)
	}
	var stopping map[string]any
	if err := json.Unmarshal(stoppingBody, &stopping); err != nil {
		t.Fatalf("decode the stopping refusal %s: %v", stoppingBody, err)
	}
	if len(stopping) != len(answer) || stopping["error"] == nil {
		t.Errorf("the stopping refusal is %s and the start's %s; want the same shape", stoppingBody, body)
	}
}

// stoppingRefusal is what the daemon's real routes answer an operator once its stop has begun
// (server.stopped): the 503 the start's own refusal is shaped after.
func stoppingRefusal(t *testing.T) (int, []byte) {
	t.Helper()
	stopping, stop := context.WithCancel(context.Background())
	stop()
	a := newAnsweringUnderTest(t)
	a.Routes(NewServer(Options{
		Project: testProject, OperatorToken: testOperatorToken, Stopping: stopping,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}))
	encoded, err := json.Marshal(map[string]string{"tree": "LEGION-1", "issue": "LEGION-1", "role": "architect"})
	if err != nil {
		t.Fatalf("encode the spawn: %v", err)
	}
	request, err := http.NewRequestWithContext(context.Background(), http.MethodPost, a.base+"/legion/v1/operator/claims", bytes.NewReader(encoded))
	if err != nil {
		t.Fatalf("build the spawn: %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+testOperatorToken)
	response, err := a.client.Do(request)
	if err != nil {
		t.Fatalf("spawn against a stopping daemon: %v", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read the stopping refusal: %v", err)
	}
	return response.StatusCode, body
}

// The one server this process serves a listener with bounds how long a client may take to send
// its headers, whichever handler is answering: without it a slow-header client would hold a
// listener slot for as long as it likes. The address is the listener's own — the daemon binds its
// configured bind and port, or serves the socket it inherited, where it takes that listener
// (internal/daemon's run) — and the routes themselves decide no address at all.
func TestTheServerOverOneListenerBoundsASlowHeaderClient(t *testing.T) {
	a := newAnsweringUnderTest(t)
	if a.server.ReadHeaderTimeout == 0 {
		t.Fatal("ReadHeaderTimeout is unset: a slow-header client would hold the daemon's listener")
	}
}

// The daemon ready is the routes replacing the start handler on the listener that has been
// answering all along, and the moment of that swap is the moment this process first served a
// route.
func TestTheRealRoutesReplaceTheStartHandler(t *testing.T) {
	a := newAnsweringUnderTest(t)
	if served, ok := a.FirstServed(); ok {
		t.Fatalf("FirstServed before the routes = %s, true; want no moment yet", served)
	}
	if status, body := a.do(t, http.MethodGet, "/healthz", nil); status != http.StatusServiceUnavailable {
		t.Fatalf("/healthz while the daemon starts = %d; body %s", status, body)
	}

	before := time.Now().UTC()
	a.Routes(NewServer(Options{Project: testProject, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}))
	after := time.Now().UTC()

	status, body := a.do(t, http.MethodGet, "/healthz", nil)
	if status != http.StatusOK {
		t.Fatalf("/healthz once the daemon is ready = %d; body %s", status, body)
	}
	var health map[string]string
	if err := json.Unmarshal(body, &health); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	if health["status"] != "ok" {
		t.Fatalf("/healthz answered %s, want the daemon's own route", body)
	}
	// A route the start handler answered 503 and the real routes answer 404 is the proof that the
	// handler, and not the status, is what changed.
	if status, body := a.do(t, http.MethodGet, "/no/such/route", nil); status != http.StatusNotFound {
		t.Fatalf("an unrouted path once the daemon is ready = %d; body %s, want the routes' own 404", status, body)
	}
	served, ok := a.FirstServed()
	if !ok || served.Before(before) || served.After(after) {
		t.Fatalf("FirstServed = %s, %v; want the moment of the swap, between %s and %s", served, ok, before, after)
	}

	a.Routes(NewServer(Options{Project: testProject, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}))
	if again, _ := a.FirstServed(); !again.Equal(served) {
		t.Errorf("FirstServed after a second swap = %s, want the first serve's %s", again, served)
	}
}

// What the start refused is remembered by the launch it belongs to: a registration by the hash of
// its boot token, which is all it carries, and a ready by the claim and generation it names. A
// launch that asks twice — a plugin that retries — is one entry, and no other launch is one.
func TestTheStartRemembersOneEntryForEachRefusedLaunch(t *testing.T) {
	a := newAnsweringUnderTest(t)
	const bootToken = "boot-token-of-the-refused-launch"
	registration := claim.RegisterRequest{
		BootToken: bootToken, SessionID: "ses_1", OmpSessionFile: "/sessions/ses_1.jsonl", AgentID: "ses_1",
	}
	ready := claim.ReadyRequest{ClaimToken: "legion-issue-1-implementer", SessionID: "ses_1", Secret: "secret", Generation: 4}
	for range 2 {
		if status, body := a.do(t, http.MethodPost, "/legion/v1/claims/register", registration); status != http.StatusServiceUnavailable {
			t.Fatalf("register = %d; body %s", status, body)
		}
		if status, body := a.do(t, http.MethodPost, "/legion/v1/claims/ready", ready); status != http.StatusServiceUnavailable {
			t.Fatalf("ready = %d; body %s", status, body)
		}
	}

	hash := supervise.HashBootToken(bootToken)
	if !a.Refused("legion-issue-1-implementer", 3, hash) {
		t.Error("the registration the start refused is not remembered by its boot token's hash")
	}
	if !a.Refused(ready.ClaimToken, ready.Generation, nil) {
		t.Error("the ready the start refused is not remembered by its claim and generation")
	}
	if a.Refused("legion-issue-2-implementer", 1, supervise.HashBootToken("another-launch")) {
		t.Error("a launch the start never refused is remembered")
	}
	if a.Refused(ready.ClaimToken, ready.Generation+1, nil) {
		t.Error("a later generation of the refused claim is remembered")
	}
	a.mu.Lock()
	held := len(a.refusals)
	a.mu.Unlock()
	if held != 2 {
		t.Fatalf("the memory holds %d entries, want one for each refused launch however often it asked", held)
	}
}

// The memory is bounded: a caller that comes back with a fresh boot token every time cannot grow
// it without end, and at the cap the entry that has waited longest is the one that goes.
func TestTheStartRefusalMemoryDropsTheOldestAtItsCap(t *testing.T) {
	a := newAnsweringUnderTest(t)
	first := supervise.HashBootToken("boot-token-0")
	for i := range startRefusalCap + 1 {
		a.refused(refusedLaunch{hash: hex.EncodeToString(supervise.HashBootToken(fmt.Sprintf("boot-token-%d", i)))})
	}

	a.mu.Lock()
	held, ordered := len(a.refusals), len(a.order)
	a.mu.Unlock()
	if held != startRefusalCap || ordered != startRefusalCap {
		t.Fatalf("the memory holds %d entries in an order of %d, want %d of each", held, ordered, startRefusalCap)
	}
	if a.Refused("", 0, first) {
		t.Error("the oldest entry is still held at the cap")
	}
	if !a.Refused("", 0, supervise.HashBootToken(fmt.Sprintf("boot-token-%d", startRefusalCap))) {
		t.Error("the newest entry was dropped at the cap")
	}
}

// An entry lives no longer than the launch it belongs to: it goes when that generation ends, and
// when a later launch of the claim answers for it.
func TestARefusedLaunchIsForgottenWhenItEndsOrALaterLaunchSupersedesIt(t *testing.T) {
	const token = claim.Token("legion-issue-1-implementer")
	hash := supervise.HashBootToken("boot-token-of-generation-2")

	t.Run("its generation ends", func(t *testing.T) {
		a := newAnsweringUnderTest(t)
		a.refused(refusedLaunch{hash: hex.EncodeToString(hash)})
		a.refused(refusedLaunch{token: token, generation: 2})

		a.Ended(token, 2, hash)

		if a.Refused(token, 2, hash) {
			t.Error("the launch is still remembered once its generation has ended")
		}
		a.mu.Lock()
		held, ordered := len(a.refusals), len(a.order)
		a.mu.Unlock()
		if held != 0 || ordered != 0 {
			t.Fatalf("the memory holds %d entries in an order of %d, want neither", held, ordered)
		}
	})

	t.Run("a later launch supersedes it", func(t *testing.T) {
		a := newAnsweringUnderTest(t)
		a.refused(refusedLaunch{token: token, generation: 1})
		a.refused(refusedLaunch{token: token, generation: 2})
		other := claim.Token("legion-issue-2-implementer")
		a.refused(refusedLaunch{token: other, generation: 1})

		a.Superseded(token, 3)

		if a.Refused(token, 1, nil) || a.Refused(token, 2, nil) {
			t.Error("a launch of the claim before the one it holds now is still remembered")
		}
		if !a.Refused(other, 1, nil) {
			t.Error("another claim's refused launch went with it")
		}
		a.mu.Lock()
		held, ordered := len(a.refusals), len(a.order)
		a.mu.Unlock()
		if held != 1 || ordered != 1 {
			t.Fatalf("the memory holds %d entries in an order of %d, want the other claim's one", held, ordered)
		}
	})
}

// The memory is written at the one site that gives the start's refusal, never inferred from a
// status: the daemon's stopping refusal is the same uncoded 503 and writes nothing, because the
// stop's own relaunch on the next boot answers what it refused.
func TestTheStoppingRefusalRemembersNothing(t *testing.T) {
	stopping, stop := context.WithCancel(context.Background())
	stop()
	a := newAnsweringUnderTest(t)
	a.Routes(NewServer(Options{
		Project: testProject, OperatorToken: testOperatorToken, Stopping: stopping,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}))

	encoded, err := json.Marshal(map[string]string{"tree": "LEGION-1", "issue": "LEGION-1", "role": "architect"})
	if err != nil {
		t.Fatalf("encode the spawn: %v", err)
	}
	request, err := http.NewRequestWithContext(context.Background(), http.MethodPost, a.base+"/legion/v1/operator/claims", bytes.NewReader(encoded))
	if err != nil {
		t.Fatalf("build the spawn: %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+testOperatorToken)
	response, err := a.client.Do(request)
	if err != nil {
		t.Fatalf("spawn against a stopping daemon: %v", err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatalf("read the refusal: %v", err)
	}
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("the spawn against a stopping daemon = %d; body %s", response.StatusCode, body)
	}

	a.mu.Lock()
	held := len(a.refusals)
	a.mu.Unlock()
	if held != 0 {
		t.Fatalf("the memory holds %d entries after a refusal the stop gave, want none", held)
	}
}
