package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/envoy/listenfd"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/stream"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// heldAtTheRuntime holds a boot where a daemon spends most of its start: after it has taken its
// API listener and long before it serves its routes. The runtime is built between the two
// (openSupervision), so a test that holds the factory there reads what a caller meets while the
// daemon starts, and the returned function lets the boot go on.
func heldAtTheRuntime(o *overrides) func() {
	release := make(chan struct{})
	inner := o.runtime
	o.runtime = func(ctx context.Context, listener *stream.Listener, address string, tokens appauth.Tokens,
		repository func(context.Context, string) (ghrepo.Repository, string, error), st *store.Store, mapped func() []ghrepo.Repository) (runtime.Runtime, error) {
		<-release
		return inner(ctx, listener, address, tokens, repository, st, mapped)
	}
	return sync.OnceFunc(func() { close(release) })
}

// startRefusalOf fails the test unless response is the daemon's start refusing: the uncoded 503
// that names the start and says the caller may ask again.
func startRefusalOf(t *testing.T, status int, body []byte) {
	t.Helper()
	if status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d while the daemon starts; body %s", status, http.StatusServiceUnavailable, body)
	}
	var answer map[string]any
	if err := json.Unmarshal(body, &answer); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	sentence, _ := answer["error"].(string)
	if len(answer) != 1 || !strings.Contains(sentence, "the daemon is starting") || !strings.Contains(sentence, "ask again") {
		t.Fatalf("the answer is %s, want the uncoded 503 naming the daemon's start", body)
	}
}

// A daemon that binds its own listener answers on it from that bind: the image probe, the boot
// probes and the claim restore all run between the bind and the serve, and a registration that
// arrives in that window used to complete its handshake and get no answer at all
// (dispatch://INFRA-2883 R5).
func TestTheStartHandlerServesFromTheMomentTheListenerExists(t *testing.T) {
	cfg := testConfig(t)
	o := fakeRuntime(fake.NewRuntime(), &built{})
	letItBoot := heldAtTheRuntime(&o)
	defer letItBoot()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- run(ctx, cfg, quietLogger(), o) }()

	base := "http://127.0.0.1:" + strconv.Itoa(cfg.Port)
	var status int
	var body []byte
	if !testwait.Holds(30*time.Second, 20*time.Millisecond, func() bool {
		select {
		case err := <-done:
			t.Fatalf("the daemon exited before it answered: %v", err)
		default:
		}
		response, err := pollClient.Get(base + "/healthz")
		if err != nil {
			return false
		}
		defer response.Body.Close()
		status = response.StatusCode
		body, _ = io.ReadAll(response.Body)
		return true
	}) {
		t.Fatalf("the daemon never answered on %s while it started", base)
	}
	startRefusalOf(t, status, body)

	// The same listener, the daemon's own routes once it is ready: the start handler stood aside
	// rather than the socket being closed and taken again.
	letItBoot()
	if !testwait.Holds(30*time.Second, 20*time.Millisecond, func() bool {
		response, err := pollClient.Get(base + "/healthz")
		if err != nil {
			return false
		}
		response.Body.Close()
		return response.StatusCode == http.StatusOK
	}) {
		t.Fatal("the daemon never served its own routes on the listener it bound")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("run: %v", err)
	}
}

// `legion start` hands Run the API already serving the socket it inherited (LEGION_LISTEN_FD),
// whose backlog the kernel has been filling since before this process started. A registration the
// kernel accepted that way is answered while the daemon starts, rather than held open until the
// agent's phase has gone by.
func TestAHandedListenerIsAnsweredWhileTheDaemonStarts(t *testing.T) {
	cfg := testConfig(t)
	file, _ := heldSocket(t)
	// The descriptor `legion start` inherits is a bare number no *os.File owns, which Adopt closes.
	descriptor, err := syscall.Dup(int(file.Fd()))
	_ = file.Close()
	if err != nil {
		t.Fatalf("copy the held socket's descriptor: %v", err)
	}
	socket, err := listenfd.Inspect("LEGION_LISTEN_FD", strconv.Itoa(descriptor))
	if err != nil {
		_ = syscall.Close(descriptor)
		t.Fatalf("inspect the held socket: %v", err)
	}
	inherited, err := listenfd.Adopt(socket)
	if err != nil {
		t.Fatalf("adopt the held socket: %v", err)
	}
	address := socket.Addr().String()
	cfg.Port = socket.Addr().Port
	cfg.DaemonURL = "http://" + address

	// The registration the kernel accepted before anything in this process served the socket: the
	// agent's own POST, written whole and waiting in the connection's buffer.
	registration := `{"bootToken":"boot-token","sessionId":"ses_1","ompSessionFile":"/sessions/ses_1.jsonl","agentId":"ses_1"}`
	conn, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatalf("dial the inherited socket before the daemon serves it: %v", err)
	}
	defer conn.Close()
	request, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		"http://"+address+"/legion/v1/claims/register", strings.NewReader(registration))
	if err != nil {
		t.Fatalf("build the registration: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Close = true
	if err := request.Write(conn); err != nil {
		t.Fatalf("write the registration: %v", err)
	}

	o := fakeRuntime(fake.NewRuntime(), &built{})
	o.answering = api.NewAnswering(inherited, quietLogger())
	letItBoot := heldAtTheRuntime(&o)
	defer letItBoot()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- run(ctx, cfg, quietLogger(), o) }()

	if err := conn.SetReadDeadline(time.Now().Add(30 * time.Second)); err != nil {
		t.Fatalf("bound the read of the answer: %v", err)
	}
	response, err := http.ReadResponse(bufio.NewReader(conn), request)
	if err != nil {
		t.Fatalf("read the answer to the registration: %v", err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatalf("read the answer's body: %v", err)
	}
	startRefusalOf(t, response.StatusCode, body)

	letItBoot()
	if !testwait.Holds(30*time.Second, 20*time.Millisecond, func() bool {
		answer, err := pollClient.Get(cfg.DaemonURL + "/healthz")
		if err != nil {
			return false
		}
		answer.Body.Close()
		return answer.StatusCode == http.StatusOK
	}) {
		t.Fatalf("the daemon never served its own routes on the socket it was handed (%s)", address)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("run: %v", err)
	}
	if thief, err := net.Listen("tcp", address); err != nil {
		t.Errorf("the daemon left %s taken after it stopped: %v", address, err)
	} else {
		_ = thief.Close()
	}
}
