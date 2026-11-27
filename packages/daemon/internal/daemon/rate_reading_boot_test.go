package daemon

import (
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/sjawhar/envoy/githubrate"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/testlog"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// A daemon that mints through its own token manager, as every production daemon does, reads that
// manager's GitHub at the same root as its workflow's reads, and logs a rate reading of each
// installation of each App once its workflow runs: here the implement App lists acme and globex,
// and the review App acme. The reading's own calls go through the process's REST client and count
// as rate_reading. Each App's first mint reads its bot user with the token it minted, a call the
// token manager makes with its own client, and that call counts too, under the caller kind of the
// mint that made it: the reading's own mint, or another loop's when that loop minted first. A
// fresh daemon reads no repository of its records at boot (prepareAtStart runs once the API
// serves, from the records, and this daemon holds none), so the count of each acme installation
// is the bot-user read and the reading's listing and workflows read, and globex's, whose App read
// its bot user on acme, the reading's two.
func TestADaemonBootLogsARateReadingOfEachInstallationWithItsOwnCalls(t *testing.T) {
	// The boot puts the process's REST client on its counter's transport; the next test of the
	// binary starts from the default.
	t.Cleanup(func() { githubrest.SetTransport(nil) })
	// Each core counter resets an hour ahead: a boot on a loaded machine that outlasted a nearer
	// reset would find the counts of its own calls gone with it, as GitHub's counter is.
	github := rateStandInFor(t, map[string][]rateInstallation{
		"1": {{id: "77", owner: "acme", repository: "acme/widgets", coreAhead: time.Hour}, {id: "79", owner: "globex", repository: "globex/gizmos", coreAhead: time.Hour}},
		"2": {{id: "87", owner: "acme", repository: "acme/widgets", coreAhead: time.Hour}},
	})
	cfg := workflowConfig(t, workflowNATS(t))
	cfg.GitHubApps = config.GitHubApps{
		Implement: config.GitHubApp{AppID: "1", PrivateKey: github.key},
		Review:    config.GitHubApp{AppID: "2", PrivateKey: github.key},
	}
	var logs testlog.Buffer
	if err := bootsOrExitsOn(t, cfg, nil, github.url, 60*time.Second, slog.New(slog.NewJSONHandler(&logs, nil)), quickAppMints()); err != nil {
		t.Fatalf("boot = %v, want the daemon serving with a token manager of its own", err)
	}

	var lines []rateLine
	testwait.Eventually(t, "a rate reading line for each of the three installations", func() bool {
		lines = rateLines(t, logs.String())
		return len(lines) >= 3
	})
	want := map[string]struct {
		app, owner    string
		installations int
	}{"77": {"1", "acme", 2}, "79": {"1", "globex", 2}, "87": {"2", "acme", 1}}
	for _, line := range lines[:3] {
		installation, listed := want[line.InstallationID]
		if !listed || line.AppID != installation.app || line.Owner != installation.owner || line.Installations != installation.installations ||
			line.Project != cfg.Project || line.Error != "" {
			t.Errorf("the first pass wrote %+v, want one line for each of installations 77 and 79 of App 1 and 87 of App 2, read, of project %s", line, cfg.Project)
			continue
		}
		delete(want, line.InstallationID)
		own := 0
		for _, calls := range line.CoreOwn {
			own += calls
		}
		wantOwn := map[string]int{"77": 3, "79": 2, "87": 3}[line.InstallationID]
		if line.CoreOwn["rate_reading"] < 2 || own != wantOwn {
			t.Errorf("installation %s's core_own = %v, want %d calls: the reading's listing and workflows read as rate_reading, and on acme its App's bot-user read, made by the token manager's own client", line.InstallationID, line.CoreOwn, wantOwn)
		}
	}
	if len(want) != 0 {
		t.Errorf("the first pass wrote no line for installations %v:\n%s", want, logs.String())
	}
}

// A workflow that fails after taking the process's transport releases it, so a later boot or
// another test never inherits the failed workflow's counter. The initial nondefault transport is
// the control: a refusal before workflow construction cannot satisfy this check accidentally.
func TestARefusedWorkflowBootReleasesItsRateTransport(t *testing.T) {
	counter := githubrate.NewCounter()
	githubrest.SetTransport(counter.Transport(nil))
	t.Cleanup(func() { githubrest.SetTransport(nil) })
	if githubrest.HTTPClient() == http.DefaultClient {
		t.Fatal("the control did not install a nondefault transport")
	}
	cfg := workflowConfig(t, workflowNATS(t))
	cfg.GitHubApps = config.GitHubApps{
		Implement: config.GitHubApp{AppID: "1", PrivateKey: "not a private key"},
		Review:    config.GitHubApp{AppID: "2", PrivateKey: "not a private key"},
	}
	if err := bootsOrExitsOn(t, cfg, nil, "http://github.invalid", 60*time.Second, quietLogger(), quickAppMints()); err == nil {
		t.Fatal("the invalid App key did not refuse workflow boot")
	}
	if githubrest.HTTPClient() != http.DefaultClient {
		t.Error("the refused workflow left its counting transport installed")
	}
}

// A normal workflow stop releases only the transport it owns. A supplied token source owns no
// counter, so stopping that workflow cannot reset a transport installed by another caller.
func TestWorkflowStopReleasesOnlyItsOwnedRateTransport(t *testing.T) {
	t.Cleanup(func() { githubrest.SetTransport(nil) })
	for _, owns := range []bool{false, true} {
		githubrest.SetTransport(githubrate.NewCounter().Transport(nil))
		workflow := &workflowRuntime{ownsTransport: owns}
		workflow.stop()
		if reset := githubrest.HTTPClient() == http.DefaultClient; reset != owns {
			t.Errorf("stop with owned counter %t reset the transport: %t", owns, reset)
		}
	}
}
