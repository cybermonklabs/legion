package natscallout

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/cipher/config"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/testnats"
)

// The restart storm (TestACalloutStormAfterAServerRestartAdmitsEveryConnection) runs its hub and
// its returning service each in a process of its own: this test binary run again as one of the two
// helper tests below (testnats.Process). Each helper checks its own variable first and skips
// without it, so a run of the package's tests runs neither as a helper.
const (
	stormHubHelper     = "TestHelperProcessStormHub"
	stormServiceHelper = "TestHelperProcessStormService"
	// stormServiceEnv names, in the storm service's process, the directory it reads its keys, its
	// sessions and the hub's URL from, and writes its decisions and its reporter's log to.
	stormServiceEnv = "NATSCALLOUT_STORM_SERVICE_DIR"
)

// TestHelperProcessStormHub is no test of its own: it serves the restart storm's hub in the
// process testnats.StartCalloutHubProcess starts, and skips everywhere else.
func TestHelperProcessStormHub(t *testing.T) { testnats.ServeHubProcess(t) }

// stormServiceFiles is what the storm's service process reads from its directory.
type stormServiceFiles struct {
	URL      string
	Keys     testnats.CalloutKeys
	Sessions []stormServiceSession
}

// stormServiceSession is one session as the service's introspection answers it.
type stormServiceSession struct {
	Token  string
	Answer oauthwire.Introspection
}

// stormService is the restart storm's returning service, back, in a process of its own
// (TestHelperProcessStormService). It decides as the service does, introspecting each of sessions
// as introspector does, and keeps a line for each introspection and for each request it decides
// with no error, with the nonce and how long the decision took, and its reporter's log, which
// decisions reads once it has stopped.
type stormService struct {
	dir     string
	process *testnats.Process
}

// startStormService starts back on the hub at url with keys' seeds and returns once its
// subscription has reached the hub, as startService does.
func startStormService(t *testing.T, url string, keys testnats.CalloutKeys, sessions []*session) *stormService {
	t.Helper()
	files := stormServiceFiles{URL: url, Keys: keys, Sessions: make([]stormServiceSession, len(sessions))}
	for index, s := range sessions {
		files.Sessions[index] = stormServiceSession{Token: s.token, Answer: s.answer}
	}
	body, err := json.Marshal(files)
	require.NoError(t, err)
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "service.json"), body, 0o600))
	process, _ := testnats.StartProcess(t, stormServiceHelper, stormServiceEnv+"="+dir)
	return &stormService{dir: dir, process: process}
}

// stormDecisions is what back did, read once it has stopped: the nonce of each request it decided
// with no error, how many tokens it introspected, its slowest decision and its reporter's log.
type stormDecisions struct {
	decided      *decidedNonces
	introspected int64
	slowest      time.Duration
	log          string
}

// decisions stops back, which writes its reporter's last line, and reads what it did.
func (s *stormService) decisions(t *testing.T) stormDecisions {
	t.Helper()
	s.process.Stop(t)
	body, err := os.ReadFile(filepath.Join(s.dir, "decisions"))
	require.NoError(t, err, "read the storm service's decisions")
	read := stormDecisions{decided: newDecidedNonces()}
	for line := range strings.Lines(string(body)) {
		fields := strings.Fields(line)
		switch {
		case len(fields) == 1 && fields[0] == "introspected":
			read.introspected++
		case len(fields) == 3 && fields[0] == "decided":
			took, err := strconv.ParseInt(fields[2], 10, 64)
			require.NoError(t, err, "a decision the storm service wrote: %q", line)
			read.decided.add(fields[1])
			read.slowest = max(read.slowest, time.Duration(took))
		default:
			require.Failf(t, "a line the storm service wrote", "%q", line)
		}
	}
	log, err := os.ReadFile(filepath.Join(s.dir, "log"))
	require.NoError(t, err, "read the storm service's log")
	read.log = string(log)
	return read
}

// TestHelperProcessStormService is no test of its own: it runs the restart storm's returning
// service in the process startStormService starts, until that process's stdin closes, and skips
// everywhere else.
func TestHelperProcessStormService(t *testing.T) {
	dir := os.Getenv(stormServiceEnv)
	if dir == "" {
		t.Skip("runs the storm's service only in the process startStormService starts")
	}
	body, err := os.ReadFile(filepath.Join(dir, "service.json"))
	require.NoError(t, err)
	var files stormServiceFiles
	require.NoError(t, json.Unmarshal(body, &files))
	answers := make(map[string]oauthwire.Introspection, len(files.Sessions))
	for _, s := range files.Sessions {
		answers[s.Token] = s.Answer
	}
	decisions, err := os.Create(filepath.Join(dir, "decisions"))
	require.NoError(t, err)
	log, err := os.Create(filepath.Join(dir, "log"))
	require.NoError(t, err)
	var mu sync.Mutex
	lines := bufio.NewWriter(decisions)
	write := func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		fmt.Fprintf(lines, format, args...)
	}
	deps := Deps{
		Introspect: func(_ context.Context, token string) (oauthwire.Introspection, error) {
			write("introspected\n")
			return answers[token], nil
		},
		Log: slog.New(slog.NewTextHandler(log, nil)),
	}
	decide := func(ctx context.Context, deps Deps, services []config.NATSService, req Request) (Decision, error) {
		began := time.Now()
		decision, err := Decide(ctx, deps, services, req)
		if err == nil {
			write("decided %s %d\n", req.Nonce, int64(time.Since(began)))
		}
		return decision, err
	}
	service := startService(t, files.URL, files.Keys, deps, decide)
	fmt.Println(testnats.ProcessReady + "the storm's service is subscribed")
	testnats.AwaitStop()
	service.Close()
	mu.Lock()
	defer mu.Unlock()
	require.NoError(t, lines.Flush())
	require.NoError(t, decisions.Close())
	require.NoError(t, log.Close())
}
