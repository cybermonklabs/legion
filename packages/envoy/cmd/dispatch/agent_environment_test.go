package main

// agent_environment_test.go holds the agent's limits and environment, which envoy-go's untagged
// run keeps: the ceiling, GOMEMLIMIT and slots a container's limit gives the agent, and the
// environment the server starts it with, which holds none of the server's secrets. The tests that
// work runs through the agent are multiprocess tests (agent_harness_test.go says which).

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/sjawhar/envoy/going/agentwire"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp/githubapptest"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// requireTheContainerLimits holds agentLimits to the three cases of section 5: each container
// limit gives the agent's ceiling, its GOMEMLIMIT, the server's limit and the slots.
func requireTheContainerLimits(t *testing.T) {
	for _, c := range []struct {
		name                               string
		container, ceiling, agent, servers int64
	}{
		{"24 GiB", 24 << 30, 4 << 30, 3 << 30, 18 << 30},
		{"8 GiB", 8 << 30, (8 << 30) / 6, 1 << 30, 6 << 30},
		{"no limit", 0, 4 << 30, 3 << 30, 0},
	} {
		got := agentLimits(c.container)
		want := agentMemory{Ceiling: c.ceiling, AgentLimit: c.agent, ServerLimit: c.servers, Slots: 8}
		if got != want {
			t.Errorf("%s: agentLimits = %+v, want %+v", c.name, got, want)
		}
	}
}

// TestTheAgentLimitsOfAContainer: container limits of 24 GiB, 8 GiB and none give the agent a
// ceiling of 4 GiB, L/6 and 4 GiB, a GOMEMLIMIT of 3 GiB, 1 GiB and 3 GiB, the server a limit of
// 18 GiB, 6 GiB and none, and 8 slots (plan settlement 12, the background-runs design's section 5).
func TestTheAgentLimitsOfAContainer(t *testing.T) {
	requireTheContainerLimits(t)
}

// TestTheAgentRunsUnderSchedIdle: lowerPriority puts every thread of the agent under the SCHED_IDLE
// policy, below every thread of the server's, or, where the kernel refuses SCHED_IDLE, sets each
// to nice 10 and says so (check c29 holds the agent's threads at SCHED_IDLE on a real server). The
// agent is this test binary as `agent`, its Go runtime's threads started, holding at the read of
// the secret it waits for on its standard input, which the test never writes.
func TestTheAgentRunsUnderSchedIdle(t *testing.T) {
	child := exec.Command(os.Args[0], "agent")
	child.Env = []string{"DISPATCH_AGENT_SOCKET=" + filepath.Join(t.TempDir(), "agent.sock")}
	stdin, err := child.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = child.Process.Kill()
		_ = child.Wait()
	})
	pid := child.Process.Pid
	deadline := time.Now().Add(10 * time.Second)
	for len(threadsOf(pid)) < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("the agent ran %d threads after 10 s, want its Go runtime's", len(threadsOf(pid)))
		}
		time.Sleep(10 * time.Millisecond)
	}
	lowerPriority(pid)
	threads := threadsOf(pid)
	for _, id := range threads {
		// Each thread's policy and nice value as the kernel answers them for that thread
		// (sched_getattr, getpriority, which answers 20 less the nice value). check c29 reads the
		// same from /proc, which gVisor, where Legion's pods run this test, does not fill in.
		policy, err := threadPolicy(id)
		if err != nil && !lowerPriorityRefused.Load() {
			t.Fatalf("read the policy of the agent's thread %d: %v", id, err)
		}
		priority, err := unix.Getpriority(unix.PRIO_PROCESS, id)
		if err != nil {
			t.Fatalf("read the nice value of the agent's thread %d: %v", id, err)
		}
		switch {
		case !lowerPriorityRefused.Load() && policy != schedIdle:
			t.Errorf("the agent's thread %d runs under policy %d, want SCHED_IDLE (%d)", id, policy, schedIdle)
		case lowerPriorityRefused.Load() && 20-priority != agentNice:
			t.Errorf("the kernel refused SCHED_IDLE, and the agent's thread %d runs at nice %d, want %d", id, 20-priority, agentNice)
		}
	}
	t.Logf("%d threads of the agent lowered; the kernel refused SCHED_IDLE: %t", len(threads), lowerPriorityRefused.Load())
}

// TestTheAgentHoldsNoSecret: the server starts its agent with the chat model's settings and the
// three it sets itself, and nothing of its own environment, its secrets among them; the agent's
// socket is in a directory only the server's user can open, and a call with no secret, or with
// another one, is refused (check c24).
func TestTheAgentHoldsNoSecret(t *testing.T) {
	signIn := oidctest.New(t)
	signIn.EnableCodeFlow(signIn.PublishKey(t, "signing-key"), cookieClientID, cookieClientSecret)
	github := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(github.Close)
	_, pem := githubapptest.Key(t)
	tokenFile := filepath.Join(t.TempDir(), "chat-token")
	const signingKey = "agent-holds-no-secret-signing-key"
	secrets := map[string]string{
		"DISPATCH_SIGNING_KEY":          signingKey,
		"DISPATCH_APP_PEM_B64":          pem,
		"DISPATCH_SIGNIN_CLIENT_SECRET": cookieClientSecret,
		"ENVOY_TOKEN":                   "agent-holds-no-secret-envoy-token",
		"DISPATCH_AGENT_TOKEN":          dispatchTestAgentToken,
	}
	database := storetest.Open(t)
	databaseURL := database.Pool.Config().ConnString()
	secrets["DATABASE_URL"] = databaseURL
	process := startDispatchProcessWith(t, databaseURL, dispatchOptions{
		SigningKey:      signingKey,
		OwnProcessGroup: true,
		Env: map[string]string{
			"DISPATCH_IDENTITY":             "cookie",
			"DISPATCH_SIGNIN_ISSUER":        signIn.URL(),
			"DISPATCH_SIGNIN_CLIENT_ID":     cookieClientID,
			"DISPATCH_SIGNIN_CLIENT_SECRET": cookieClientSecret,
			"DISPATCH_SIGNIN_GROUP":         cookieGroup,
			"DISPATCH_INSECURE_COOKIE":      "1",
			"DISPATCH_APP_CLIENT_ID":        "Iv1.agentholdsnosecret",
			"DISPATCH_APP_PEM_B64":          pem,
			"DISPATCH_GITHUB_API_BASE":      github.URL,
			"ENVOY_TOKEN":                   secrets["ENVOY_TOKEN"],
			"DISPATCH_ANSWERER":             "endpoint",
			"DISPATCH_CHAT_ENDPOINT":        "https://chat.example/v1",
			"DISPATCH_CHAT_TOKEN_FILE":      tokenFile,
			"DISPATCH_CHAT_MODEL":           "a-chat-model",
		},
	})
	server := agentServer{dispatchProcess: process, database: database}
	server.waitHealthy(t)
	pid := server.agentStarted(t, 1, 30*time.Second)
	environ, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
	if err != nil {
		t.Fatalf("read the agent's environment: %v", err)
	}
	variables := map[string]string{}
	for entry := range strings.SplitSeq(strings.TrimRight(string(environ), "\x00"), "\x00") {
		name, value, _ := strings.Cut(entry, "=")
		variables[name] = value
	}
	for name, value := range secrets {
		if _, held := variables[name]; held {
			t.Errorf("the agent's environment holds %s, a secret of the server's", name)
		}
		for held, heldValue := range variables {
			if strings.Contains(heldValue, value) {
				t.Errorf("the agent's %s holds the value of the server's %s", held, name)
			}
		}
	}
	socket := variables["DISPATCH_AGENT_SOCKET"]
	want := map[string]string{
		"DISPATCH_AGENT_SOCKET":    socket,
		"DISPATCH_AGENT_SLOTS":     "8",
		"GOMAXPROCS":               variables["GOMAXPROCS"],
		"GOMEMLIMIT":               variables["GOMEMLIMIT"],
		"DISPATCH_ANSWERER":        "endpoint",
		"DISPATCH_CHAT_ENDPOINT":   "https://chat.example/v1",
		"DISPATCH_CHAT_TOKEN_FILE": tokenFile,
		"DISPATCH_CHAT_MODEL":      "a-chat-model",
	}
	if socket == "" || variables["GOMAXPROCS"] == "" || variables["GOMEMLIMIT"] == "" || !maps.Equal(variables, want) {
		t.Errorf("the agent's environment is %v, want the chat model's settings and the socket, its slots, GOMAXPROCS and GOMEMLIMIT alone", variables)
	}
	if socket == "" {
		t.FailNow()
	}
	info, err := os.Stat(filepath.Dir(socket))
	if err != nil {
		t.Fatalf("stat the agent's socket directory: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o700 {
		t.Errorf("the agent's socket directory %s has mode %#o, want 0700", filepath.Dir(socket), mode)
	}
	for name, secret := range map[string][]byte{
		"no secret":      nil,
		"a wrong secret": bytes.Repeat([]byte{1}, agentwire.SecretSize),
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_, err := agentwire.Client{Path: socket, Secret: secret}.Hello(ctx, agentwire.HelloRequest{Instance: "a caller that is no agent"})
		cancel()
		var refused *agentwire.Error
		if !errors.As(err, &refused) || refused.Code != agentwire.CodeUnauthorized {
			t.Errorf("a hello with %s was answered %v, want %s", name, err, agentwire.CodeUnauthorized)
		}
	}
}
