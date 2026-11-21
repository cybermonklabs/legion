package daemon

import (
	"bytes"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/nats-io/nkeys"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/testnats"
	"github.com/sjawhar/legion/daemon/internal/testnkeys"
)

// lookup is os.LookupEnv over exactly values, whatever the shell running the test set.
func lookup(values map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		value, ok := values[name]
		return value, ok
	}
}

// The workflow's own NATS connection authenticates as the seed the daemon resolved from
// nats_nkey_seed_file — the seed every pane receives — so a daemon boots against a server whose one
// user is that seed's nkey, and without it the same server refuses the daemon's boot.
func TestTheWorkflowConnectsAsTheNatsSeedTheDaemonResolved(t *testing.T) {
	quickAppMints(t)
	seed, public := testnkeys.User(t)
	url := testnats.StartNkeyAuthorized(t, public)
	user, err := nkeys.FromSeed([]byte(seed))
	if err != nil {
		t.Fatal(err)
	}
	conn, err := nats.Connect(url, nats.Name(t.Name()), nats.Nkey(public, user.Sign), nats.Timeout(5*time.Second))
	if err != nil {
		t.Fatalf("connect as the seed's user: %v", err)
	}
	t.Cleanup(conn.Close)
	js, err := jetstream.New(conn)
	if err != nil {
		t.Fatalf("open JetStream: %v", err)
	}
	if _, err := js.CreateStream(t.Context(), jetstream.StreamConfig{Name: "ENVOY_NOTIFICATIONS", Subjects: []string{"notifications.>"}}); err != nil {
		t.Fatalf("create notification stream: %v", err)
	}

	t.Run("with the seed", func(t *testing.T) {
		cfg := workflowConfig(t, url)
		cfg.NatsNkeySeedFile = testnkeys.SeedFile(t, seed+"\n")
		tokens, _ := appTokens(t, func(w http.ResponseWriter, _ *http.Request, _ string, _ int) { installed(w) })
		if err := bootsOrExits(t, cfg, tokens, 60*time.Second); err != nil {
			t.Fatalf("boot with the seed = %v, want the daemon serving", err)
		}
	})
	t.Run("without one", func(t *testing.T) {
		cfg := workflowConfig(t, url)
		tokens, _ := appTokens(t, func(w http.ResponseWriter, _ *http.Request, _ string, _ int) { installed(w) })
		err := bootsOrExits(t, cfg, tokens, 60*time.Second)
		if err == nil || !strings.Contains(strings.ToLower(err.Error()), "authorization violation") {
			t.Fatalf("boot without a seed = %v, want the server's authorization violation", err)
		}
	})
}

// The workflow's connection authenticates as the daemon's own seed when it has one, over the pane
// seed every pane receives, and logs that user once, by public key and whether it is the pane user:
// a server whose one user is the daemon seed's admits a daemon configured with both seeds, and
// refuses one configured with the pane seed alone, which it then connects as.
func TestTheWorkflowConnectsAsTheDaemonSeedOverThePaneSeed(t *testing.T) {
	quickAppMints(t)
	daemonSeed, daemonUser := testnkeys.User(t)
	paneSeed, paneUser := testnkeys.User(t)
	url := testnats.StartNkeyAuthorized(t, daemonUser)
	user, err := nkeys.FromSeed([]byte(daemonSeed))
	if err != nil {
		t.Fatal(err)
	}
	conn, err := nats.Connect(url, nats.Name(t.Name()), nats.Nkey(daemonUser, user.Sign), nats.Timeout(5*time.Second))
	if err != nil {
		t.Fatalf("connect as the daemon seed's user: %v", err)
	}
	t.Cleanup(conn.Close)
	js, err := jetstream.New(conn)
	if err != nil {
		t.Fatalf("open JetStream: %v", err)
	}
	if _, err := js.CreateStream(t.Context(), jetstream.StreamConfig{Name: "ENVOY_NOTIFICATIONS", Subjects: []string{"notifications.>"}}); err != nil {
		t.Fatalf("create notification stream: %v", err)
	}

	t.Run("with both seeds", func(t *testing.T) {
		cfg := workflowConfig(t, url)
		cfg.NatsNkeySeedFile = testnkeys.SeedFile(t, paneSeed+"\n")
		cfg.NatsDaemonNkeySeedFile = testnkeys.SeedFile(t, daemonSeed+"\n")
		var out syncBuffer
		tokens, _ := appTokens(t, func(w http.ResponseWriter, _ *http.Request, _ string, _ int) { installed(w) })
		if err := bootsOrExitsLogging(t, cfg, tokens, 60*time.Second, slog.New(slog.NewTextHandler(&out, nil))); err != nil {
			t.Fatalf("boot with both seeds = %v, want the daemon serving", err)
		}
		logged := out.String()
		want := `level=INFO msg="legion daemon connects to NATS" user=` + daemonUser + ` paneUser=false seed=daemon`
		if strings.Count(logged, "legion daemon connects to NATS") != 1 || !strings.Contains(logged, want) {
			t.Errorf("log =\n%s\nwant exactly one line containing %s", logged, want)
		}
		for _, seed := range []string{daemonSeed, paneSeed} {
			if strings.Contains(logged, seed) {
				t.Errorf("log carries a seed:\n%s", logged)
			}
		}
	})
	t.Run("with the pane seed alone", func(t *testing.T) {
		cfg := workflowConfig(t, url)
		cfg.NatsNkeySeedFile = testnkeys.SeedFile(t, paneSeed+"\n")
		var out syncBuffer
		tokens, _ := appTokens(t, func(w http.ResponseWriter, _ *http.Request, _ string, _ int) { installed(w) })
		err := bootsOrExitsLogging(t, cfg, tokens, 60*time.Second, slog.New(slog.NewTextHandler(&out, nil)))
		if err == nil || !strings.Contains(strings.ToLower(err.Error()), "authorization violation") {
			t.Fatalf("boot with the pane seed alone = %v, want the server's authorization violation", err)
		}
		want := `level=INFO msg="legion daemon connects to NATS" user=` + paneUser + ` paneUser=true seed=pane`
		if !strings.Contains(out.String(), want) {
			t.Errorf("log =\n%s\nwant a line containing %s", out.String(), want)
		}
	})
}

// chooseNATSConnection picks the daemon's platform identity token, else the daemon seed, else the
// pane seed, else none, and its one line says which user that is and whether it is the pane user,
// the daemon seed naming the pane user included: for the token, the account it names.
func TestChooseNATSConnectionNamesTheUserTheDaemonConnectsAs(t *testing.T) {
	daemonSeed, daemonUser := testnkeys.User(t)
	paneSeed, paneUser := testnkeys.User(t)
	const account = "system:serviceaccount:legion-pub:legion-daemon"
	tokenFile := filepath.Join(t.TempDir(), "nats-token")
	if err := os.WriteFile(tokenFile, []byte(testnkeys.ServiceToken(account, time.Now().Add(time.Hour))), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, token, daemon, pane, paneUser, seed, line string
	}{
		{"both", "", daemonSeed, paneSeed, paneUser, daemonSeed, "user=" + daemonUser + " paneUser=false seed=daemon"},
		{"the daemon seed alone", "", daemonSeed, "", "", daemonSeed, "user=" + daemonUser + " paneUser=false seed=daemon"},
		{"a daemon seed that is the pane seed", "", paneSeed, paneSeed, paneUser, paneSeed, "user=" + paneUser + " paneUser=true seed=daemon"},
		{"the pane seed alone", "", "", paneSeed, paneUser, paneSeed, "user=" + paneUser + " paneUser=true seed=pane"},
		{"neither", "", "", "", "", "", `user="" paneUser=false seed=none`},
		{"the service token", tokenFile, "", "", "", "", "user=" + account + " paneUser=false seed=service-token"},
		{"the service token beside the pane seed", tokenFile, "", paneSeed, paneUser, "", "user=" + account + " paneUser=false seed=service-token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			nc, err := chooseNATSConnection(tc.token, tc.daemon, tc.pane, tc.paneUser)
			// Named, never printed: a failure message is no place for a seed.
			name := map[string]string{daemonSeed: "the daemon seed", paneSeed: "the pane seed", "": "none"}
			if err != nil || nc.seed != tc.seed || nc.tokenFile != tc.token {
				t.Fatalf("chooseNATSConnection = %s and token %q, %v; want %s and token %q", name[nc.seed], nc.tokenFile, err, name[tc.seed], tc.token)
			}
			var out bytes.Buffer
			nc.log(slog.New(slog.NewTextHandler(&out, nil)))
			want := `level=INFO msg="legion daemon connects to NATS" ` + tc.line + "\n"
			if !strings.HasSuffix(out.String(), want) || strings.Count(out.String(), "\n") != 1 {
				t.Errorf("log = %q, want one line ending %q", out.String(), want)
			}
		})
	}
	t.Run("the service token beside a daemon seed is refused naming both", func(t *testing.T) {
		_, err := chooseNATSConnection(tokenFile, daemonSeed, paneSeed, paneUser)
		if err == nil || !strings.Contains(err.Error(), "nats_daemon_service_token_file and a daemon seed (nats_daemon_nkey_seed_file") {
			t.Fatalf("chooseNATSConnection = %v, want the refusal naming both", err)
		}
		if strings.Contains(err.Error(), daemonSeed) {
			t.Fatal("the refusal carries the seed")
		}
	})
	t.Run("an unreadable service token refuses boot naming the key", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "absent")
		_, err := chooseNATSConnection(missing, "", "", "")
		if err == nil || !strings.Contains(err.Error(), "nats_daemon_service_token_file names "+missing) {
			t.Fatalf("chooseNATSConnection = %v, want the refusal naming the key and the path", err)
		}
	})
}

// `legion config check` (CheckStart) names whom each NATS credential boot reads
// authenticates as, as boot logs it, and never the credential: beside the pane seed's user, the
// daemon seed's user by public key, or, for nats_daemon_service_token_file, the account the token
// names. A configuration with no project mapped, run on tmux with LEGION_OMP_PATH naming an
// executable, gets past every other read.
func TestCheckStartNamesWhomTheDaemonsNATSCredentialAuthenticatesAs(t *testing.T) {
	omp, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("LEGION_OMP_PATH", omp)
	paneSeed, paneUser := testnkeys.User(t)
	daemonSeed, daemonUser := testnkeys.User(t)
	const account = "system:serviceaccount:legion-pub:legion-daemon"
	token := testnkeys.ServiceToken(account, time.Now().Add(time.Hour))
	dir := t.TempDir()
	tokenFile, operatorToken := filepath.Join(dir, "nats-token"), filepath.Join(dir, "operator-token")
	for path, contents := range map[string]string{tokenFile: token, operatorToken: testOperatorToken + "\n"} {
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name              string
		tokenFile, daemon string
		want              CheckedNATS
	}{
		{"the service token", tokenFile, "", CheckedNATS{PaneUser: paneUser, DaemonServiceAccount: account}},
		{"the daemon seed", "", daemonSeed, CheckedNATS{PaneUser: paneUser, DaemonUser: daemonUser}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Config{
				Project: "TEST", Runtime: config.Runtime{Name: "tmux"}, OperatorTokenFile: operatorToken,
				NatsNkeySeedFile: testnkeys.SeedFile(t, paneSeed+"\n"), NatsDaemonServiceTokenFile: tc.tokenFile,
			}
			if tc.daemon != "" {
				cfg.NatsDaemonNkeySeedFile = testnkeys.SeedFile(t, tc.daemon+"\n")
			}
			checked, err := CheckStart(cfg, lookup(map[string]string{}))
			if err != nil || checked != tc.want {
				t.Fatalf("CheckStart = %+v, %v; want %+v", checked, err, tc.want)
			}
			if printed := fmt.Sprintf("%+v", checked); strings.Contains(printed, token) || strings.Contains(printed, daemonSeed) || strings.Contains(printed, paneSeed) {
				t.Fatal("CheckStart answered a credential")
			}
		})
	}
}

// The service token's connection waits its reconnects out on the boot's own schedule
// (readinessRetry: one second, doubling, capped at a minute), so a refusal of the auth service is
// waited out alike at boot and while running; a seed's connection, or one with no credential,
// keeps nats.go's own reconnect wait.
func TestTheTokenConnectionReconnectsOnTheBootsSchedule(t *testing.T) {
	apply := func(nc natsConnection) nats.Options {
		options := nats.GetDefaultOptions()
		for _, option := range nc.options(quietLogger()) {
			if err := option(&options); err != nil {
				t.Fatal(err)
			}
		}
		return options
	}
	token := apply(natsConnection{tokenFile: "/var/run/secrets/legion/nats-token", source: natsServiceToken})
	if token.CustomReconnectDelayCB == nil {
		t.Fatal("the service token's connection has no reconnect schedule of its own")
	}
	for attempt, want := range map[int]time.Duration{1: readinessRetry.Initial, 2: 2 * readinessRetry.Initial, 10: readinessRetry.Max} {
		if got := token.CustomReconnectDelayCB(attempt); got != want {
			t.Errorf("reconnect %d waits %s, want %s", attempt, got, want)
		}
	}
	for _, nc := range []natsConnection{{source: natsSeedDaemon}, {source: natsSeedPane}, {source: natsSeedNone}} {
		if apply(nc).CustomReconnectDelayCB != nil {
			t.Errorf("a %s connection waits on the token's schedule", nc.source)
		}
	}
}

// The workflow's own NATS connection presents the daemon's platform identity token to a hub whose
// connections the NATS auth service admits: the daemon boots against such a hub, presenting the
// token its file holds, and logs the account it names. The control boots the same daemon with no
// token: the stand-in refuses a connection with no credential, and the boot fails naming the
// server's authorization violation.
func TestTheWorkflowConnectsByItsServiceToken(t *testing.T) {
	quickAppMints(t)
	const account = "system:serviceaccount:legion-pub:legion-daemon"
	s := testnats.StartCallout(t, 0, ">")
	if _, err := jetstreamOf(t, s.Connect(t)).CreateStream(t.Context(), jetstream.StreamConfig{Name: "ENVOY_NOTIFICATIONS", Subjects: []string{"notifications.>"}}); err != nil {
		t.Fatalf("create notification stream: %v", err)
	}
	token := testnkeys.ServiceToken(account, time.Now().Add(time.Hour))
	tokenFile := filepath.Join(t.TempDir(), "nats-token")
	if err := os.WriteFile(tokenFile, []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("with the token", func(t *testing.T) {
		cfg := workflowConfig(t, s.URL)
		cfg.NatsDaemonServiceTokenFile = tokenFile
		var out syncBuffer
		tokens, _ := appTokens(t, func(w http.ResponseWriter, _ *http.Request, _ string, _ int) { installed(w) })
		if err := bootsOrExitsLogging(t, cfg, tokens, 60*time.Second, slog.New(slog.NewTextHandler(&out, nil))); err != nil {
			t.Fatalf("boot with the service token = %v, want the daemon serving", err)
		}
		if presented := s.Presented(); len(presented) == 0 || presented[0] != token {
			t.Fatalf("the daemon presented %d tokens, want the one its file holds first", len(presented))
		}
		want := `level=INFO msg="legion daemon connects to NATS" user=` + account + ` paneUser=false seed=service-token`
		if !strings.Contains(out.String(), want) {
			t.Errorf("log =\n%s\nwant a line containing %s", out.String(), want)
		}
		if strings.Contains(out.String(), token) {
			t.Error("the log carries the token")
		}
	})
	t.Run("control: without it", func(t *testing.T) {
		cfg := workflowConfig(t, s.URL)
		tokens, _ := appTokens(t, func(w http.ResponseWriter, _ *http.Request, _ string, _ int) { installed(w) })
		err := bootsOrExits(t, cfg, tokens, 60*time.Second)
		if err == nil || !strings.Contains(strings.ToLower(err.Error()), "authorization violation") {
			t.Fatalf("boot without a credential = %v, want the server's authorization violation", err)
		}
	})
}

// The daemon's token connection waits out a refusal of the NATS auth service at boot, as it does
// while running: the stand-in refuses the token's first three CONNECTs, the daemon's boot logs each
// refusal with its reason and tries again on readinessRetry, and the same process boots once the
// token is admitted, with no exit and no restart; it serves nothing, so it reads as not ready,
// until then. The control is today's boot, which refuses an authorization violation at once: make
// natsConnection.waitsOut natsauth.Unreachable alone, and the boot exits naming the violation.
func TestTheWorkflowWaitsOutARefusalOfItsServiceToken(t *testing.T) {
	quickAppMints(t)
	const account = "system:serviceaccount:legion-pub:legion-daemon"
	s := testnats.StartCallout(t, 3, ">")
	if _, err := jetstreamOf(t, s.Connect(t)).CreateStream(t.Context(), jetstream.StreamConfig{Name: "ENVOY_NOTIFICATIONS", Subjects: []string{"notifications.>"}}); err != nil {
		t.Fatalf("create notification stream: %v", err)
	}
	tokenFile := filepath.Join(t.TempDir(), "nats-token")
	if err := os.WriteFile(tokenFile, []byte(testnkeys.ServiceToken(account, time.Now().Add(time.Hour))), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := workflowConfig(t, s.URL)
	cfg.NatsDaemonServiceTokenFile = tokenFile
	var out syncBuffer
	tokens, _ := appTokens(t, func(w http.ResponseWriter, _ *http.Request, _ string, _ int) { installed(w) })
	// While the token is refused the daemon is not ready: /readyz answers nothing or 503 until the
	// fourth CONNECT, the admitted one, has been presented. The count is read after each answer,
	// so a 200 read with fewer than four presented is a daemon ready before it was admitted.
	readyEarly, probing := make(chan int, 1), make(chan struct{})
	go func() {
		defer close(probing)
		for len(s.Presented()) < 4 {
			if response, err := pollClient.Get("http://127.0.0.1:" + strconv.Itoa(cfg.Port) + "/readyz"); err == nil {
				response.Body.Close()
				if presented := len(s.Presented()); response.StatusCode == http.StatusOK && presented < 4 {
					readyEarly <- presented
					return
				}
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	if err := bootsOrExitsLogging(t, cfg, tokens, 60*time.Second, slog.New(slog.NewTextHandler(&out, nil))); err != nil {
		t.Fatalf("boot whose token the auth service refuses three times = %v, want the daemon serving once it is admitted", err)
	}
	<-probing
	select {
	case presented := <-readyEarly:
		t.Fatalf("/readyz answered 200 with %d CONNECTs presented, before the token was admitted", presented)
	default:
	}
	if got := len(s.Presented()); got < 4 {
		t.Fatalf("the daemon presented its token %d times, want at least four: three refused, then admitted", got)
	}
	if refusals := strings.Count(strings.ToLower(out.String()), "authorization violation"); refusals < 3 {
		t.Errorf("the boot logged %d refusals with their reason, want three:\n%s", refusals, out.String())
	}
}

// jetstreamOf is JetStream over conn.
func jetstreamOf(t *testing.T, conn *nats.Conn) jetstream.JetStream {
	t.Helper()
	js, err := jetstream.New(conn)
	if err != nil {
		t.Fatalf("open JetStream: %v", err)
	}
	return js
}

// The daemon's own seed reaches no launch: with both seeds configured, a root's and a worker's
// launch carry the pane seed as NATS_NKEY_SEED and nothing of the daemon seed, in no secret, no
// variable, and no other field of the spec.
func TestNoLaunchCarriesTheDaemonSeed(t *testing.T) {
	daemonSeed, paneSeed := testnkeys.UserSeed(t), testnkeys.UserSeed(t)
	for _, tc := range []struct {
		name    string
		key     string
		environ []string
	}{
		{"nats_daemon_nkey_seed_file", testnkeys.SeedFile(t, daemonSeed), nil},
		{"NATS_DAEMON_NKEY_SEED_FILE", "", []string{"NATS_DAEMON_NKEY_SEED_FILE=" + testnkeys.SeedFile(t, daemonSeed)}},
		{"NATS_DAEMON_NKEY_SEED", "", []string{"NATS_DAEMON_NKEY_SEED=" + daemonSeed}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig(t)
			cfg.NatsNkeySeedFile = testnkeys.SeedFile(t, paneSeed)
			cfg.NatsDaemonNkeySeedFile = tc.key
			rt := fake.NewRuntime()
			o := fakeRuntime(rt, &built{})
			o.environ = tc.environ
			d := startDaemon(t, cfg, o)

			root := lastLaunch(t, rt, d.spawn(architect()))
			worker := lastLaunch(t, rt, d.spawn(api.SpawnRequest{Tree: "LEGION-1", Issue: "LEGION-2", Role: claim.RoleImplementer, Prompt: "Wait."}))
			for kind, spec := range map[string]runtime.SpawnSpec{"root": root, "worker": worker} {
				if got := spec.Secrets["NATS_NKEY_SEED"]; got != paneSeed {
					t.Errorf("the %s launch's NATS_NKEY_SEED secret is %q, want the pane seed", kind, got)
				}
				if dump := fmt.Sprintf("%#v", spec); strings.Contains(dump, daemonSeed) || strings.Contains(dump, "NATS_DAEMON") {
					t.Errorf("the %s launch carries the daemon seed or its name: %s", kind, dump)
				}
			}
		})
	}
}
