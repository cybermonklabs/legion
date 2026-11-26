// packages/envoy/cmd/dev-proxy-stands/github_test.go
//
// The GitHub stand as this command serves it: --installation-permissions, what each installation
// of the App holds, and the wiring of the stand-in package's one GitHub (proxyrulestest.GitHubStand,
// whose own tests hold its routes) at the plain github stand and the github-tls stand. Each row
// runs the real compiled binary, as the local stack's check scripts do.
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/proxyrules/proxyrulestest"
	"github.com/sjawhar/envoy/internal/testwait"
	"github.com/sjawhar/envoy/testexec"
)

// TestTheInstallationPermissionsFlagSetsWhatAnInstallationHolds: --installation-permissions
// gives one owner's installation the permissions it names, which the installations listing
// names and the token route holds a request to, and an owner it names nothing for holds nothing;
// an owner it does not name keeps the stand's own map.
func TestTheInstallationPermissionsFlagSetsWhatAnInstallationHolds(t *testing.T) {
	out := startStands(t, "--dir", t.TempDir(),
		"--installation", "acme-org=7", "--installation", "ada=8", "--installation", "Ada=9",
		"--installation-permissions", "acme-org=contents:read,administration:read",
		"--installation-permissions", "ada=")
	github := out.Stands[standGitHub].URL

	held := installationsHeld(t, github)
	if want := map[string]string{"contents": "read", "administration": "read"}; !maps.Equal(held["acme-org"], want) {
		t.Errorf("acme-org's installation holds %v; want %v, what the flag named", held["acme-org"], want)
	}
	if len(held["ada"]) != 0 {
		t.Errorf("ada's installation holds %v; want nothing, since the flag named nothing for it", held["ada"])
	}
	if held["Ada"]["contents"] != "write" {
		t.Errorf("Ada's installation holds %v; want the stand's own map, contents at write among it, since the flag names nothing of Ada's", held["Ada"])
	}

	for _, c := range []struct {
		name         string
		installation int
		asked        string
		want         int
	}{
		{"a permission the installation holds", 7, `{"permissions":{"contents":"read"}}`, http.StatusCreated},
		{"a level past the one it holds", 7, `{"permissions":{"contents":"write"}}`, http.StatusUnprocessableEntity},
		{"a permission it does not hold", 7, `{"permissions":{"issues":"read"}}`, http.StatusUnprocessableEntity},
		{"anything of an installation that holds nothing", 8, `{"permissions":{"metadata":"read"}}`, http.StatusUnprocessableEntity},
		{"the stand's own map, where the flag names nothing", 9, `{"permissions":{"contents":"write"}}`, http.StatusCreated},
	} {
		if status, body := mintAt(t, github, c.installation, c.asked); status != c.want {
			t.Errorf("%s: the token request answered %d %s; want %d", c.name, status, body, c.want)
		}
	}
}

// TestTheInstallationPermissionsFlagRefusesWhatItCannotRead: each malformed value, an owner
// named twice, and an owner no --installation installs the App on are usage errors, exit 2, each
// naming what it refuses, before any stand is served, within refusalLimit of when the kernel made
// the command's process. The limit counts from that start, not from the call, so a loaded machine
// that starts the binary late kills no run that would have refused on its own; a run still going
// with 30 s left of the test binary's deadline is killed too.
func TestTheInstallationPermissionsFlagRefusesWhatItCannotRead(t *testing.T) {
	binary := buildStands(t)
	for _, c := range []struct {
		name string
		args []string
		want string
	}{
		{"no owner", []string{"--installation-permissions", "=contents:read"}, "names no owner"},
		{"no level", []string{"--installation-permissions", "acme-org=contents"}, `"contents" names no level`},
		{"a level GitHub gives no permission", []string{"--installation-permissions", "acme-org=contents:owner"}, `the level "owner" is none of read, write and admin`},
		{"a permission no App permission is named", []string{"--installation-permissions", "acme-org=Contents:read"}, `"Contents" is no permission's name`},
		{"a permission named twice", []string{"--installation-permissions", "acme-org=contents:read,contents:write"}, "names contents twice"},
		{"an owner named twice", []string{"--installation-permissions", "acme-org=contents:read", "--installation-permissions", "acme-org=issues:read"}, "names acme-org twice"},
		{"an owner the App is not installed on", []string{"--installation-permissions", "other-org=contents:read"}, "other-org, on which no --installation installs the App"},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), testwait.Remaining(t, 30*time.Second))
			defer cancel()
			cmd := exec.CommandContext(ctx, binary, append([]string{"--dir", t.TempDir()}, c.args...)...)
			var output bytes.Buffer
			cmd.Stdout, cmd.Stderr = &output, &output
			overran, err := runFromItsStart(t, cmd, refusalLimit)
			if overran {
				t.Fatalf("the command did not exit within %s of when the kernel made its process and was killed; want exit %d\n%s", refusalLimit, exitUsageError, output.Bytes())
			}
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != exitUsageError {
				t.Fatalf("the command exited %v; want exit %d\n%s", err, exitUsageError, output.Bytes())
			}
			if !strings.Contains(output.String(), c.want) {
				t.Errorf("the command said %q; want it to name %q", output.Bytes(), c.want)
			}
		})
	}
}

// refusalLimit bounds a run that refuses its flags, from when the kernel made its process to its
// exit: the command reads its flags before it serves any stand.
const refusalLimit = 30 * time.Second

// runFromItsStart starts cmd and waits for it, and kills it once it has run bound from when the
// kernel made its process, read right after Start (testexec.ProcStart), not from this call. Each of
// the two times it compares, that start and the time since boot it reads to set the kill, can read
// up to a tick early, so the kill waits two ticks more: no run is killed before it has run bound.
// overran says the bound ended the run: the kill had been sent, and the run's own exit status says
// a SIGKILL ended it, so a run that exited before the kill reached it did not overrun. A start the
// kernel dates more than two ticks before the call is no start at all, and the row says it cannot
// read when its run started, as testexec.StartedAfter does, rather than judge it.
func runFromItsStart(t *testing.T, cmd *exec.Cmd, bound time.Duration) (overran bool, err error) {
	t.Helper()
	called := testexec.SinceBoot(t)
	if err := cmd.Start(); err != nil {
		return false, err
	}
	started, err := testexec.ProcStart(cmd.Process.Pid)
	if err == nil && started < called-2*testexec.ClockTick {
		err = fmt.Errorf("the kernel made it %s before the call, which no start can be: this machine's /proc/uptime and its processes' start times are not one clock", called-started)
	}
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("read when the kernel made the process of %s: %v", cmd.Path, err)
	}
	var sent atomic.Bool
	kill := time.AfterFunc(started+bound+2*testexec.ClockTick-testexec.SinceBoot(t), func() {
		sent.Store(true)
		_ = cmd.Process.Kill()
	})
	err = cmd.Wait()
	kill.Stop()
	if !sent.Load() || cmd.ProcessState == nil {
		return false, err
	}
	status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
	return ok && status.Signaled() && status.Signal() == syscall.SIGKILL, err
}

// TestTheGitHubStandsServeOneStandInOverPlainAndTLS: a token minted at the plain github stand,
// where the broker's minter mints, is spent at the github-tls stand, over TLS under the stands' own
// authority, where a command behind the proxy spends it: one stand-in serves both. What the token
// holds there is what --installation-permissions let the installation hold, and the commit the
// report names is the one a status is written for.
func TestTheGitHubStandsServeOneStandInOverPlainAndTLS(t *testing.T) {
	out := startStands(t, "--dir", t.TempDir(), "--installation", "acme-org=7",
		"--installation-permissions", "acme-org=metadata:read,statuses:write")
	plain := out.Stands[standGitHub].URL
	if out.GitHubCommitSHA != proxyrulestest.GitHubCommitSHA {
		t.Fatalf("the report names the commit %q; want the one the stand holds, %s", out.GitHubCommitSHA, proxyrulestest.GitHubCommitSHA)
	}
	mint := func(asked string) string {
		t.Helper()
		status, body := mintAt(t, plain, 7, asked)
		if status != http.StatusCreated {
			t.Fatalf("the token request %s answered %d %s; want 201", asked, status, body)
		}
		var answer struct {
			Token string `json:"token"`
		}
		if err := json.Unmarshal([]byte(body), &answer); err != nil || answer.Token == "" {
			t.Fatalf("the token request answered no token: %v %s", err, body)
		}
		return answer.Token
	}
	// statuses at write is past the stand's own map, so this mint is the flag's.
	statusWriter := mint(`{"repositories":["proofs"],"permissions":{"statuses":"write"}}`)
	reader := mint(`{"repositories":["proofs"],"permissions":{"metadata":"read"}}`)

	tlsStand := out.Stands[standGitHubTLS]
	if !strings.HasPrefix(tlsStand.URL, "https://") || tlsStand.CertFile == "" {
		t.Fatalf("the github-tls stand is %+v; want an https stand with a certificate of its own", tlsStand)
	}
	client := trustingTheStands(t, out.CACertFile)
	commit := "/repos/acme-org/proofs/statuses/" + out.GitHubCommitSHA
	for _, c := range []struct {
		name, token, method, path string
		want                      int
	}{
		{"a commit status, to the token holding statuses at write", statusWriter, http.MethodPost, commit, http.StatusCreated},
		{"a commit status, to a token without it", reader, http.MethodPost, commit, http.StatusForbidden},
		{"the repository read of the minted token", reader, http.MethodGet, "/repos/acme-org/proofs", http.StatusOK},
	} {
		if status, body := callGitHub(t, client, tlsStand.URL, c.method, c.path, c.token); status != c.want {
			t.Errorf("%s: %s %s answered %d %s; want %d", c.name, c.method, c.path, status, body, c.want)
		}
	}
	if records := recordsOf(t, tlsStand.Records); len(records) != 3 {
		t.Errorf("the github-tls stand recorded %d requests; want the 3 this row made there", len(records))
	}
}

// installationsHeld reads the GitHub stand's installations listing as what each owner's
// installation holds, by owner.
func installationsHeld(t *testing.T, url string) map[string]map[string]string {
	t.Helper()
	status, body := callGitHub(t, http.DefaultClient, url, http.MethodGet, "/app/installations", "")
	if status != http.StatusOK {
		t.Fatalf("GET /app/installations answered %d %s; want 200", status, body)
	}
	var listing []struct {
		Account struct {
			Login string `json:"login"`
		} `json:"account"`
		Permissions map[string]string `json:"permissions"`
	}
	if err := json.Unmarshal([]byte(body), &listing); err != nil {
		t.Fatalf("GET /app/installations answered no listing: %v %s", err, body)
	}
	held := map[string]map[string]string{}
	for _, installation := range listing {
		held[installation.Account.Login] = installation.Permissions
	}
	return held
}

// mintAt makes one access_tokens request of one installation, the call the broker's minter
// makes, and answers its status and body.
func mintAt(t *testing.T, url string, installation int, asked string) (int, string) {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		url+"/app/installations/"+strconv.Itoa(installation)+"/access_tokens", strings.NewReader(asked))
	if err != nil {
		t.Fatal(err)
	}
	return send(t, http.DefaultClient, request)
}

// callGitHub makes one call of the GitHub stand with token as its bearer, none for "", and
// answers its status and body.
func callGitHub(t *testing.T, client *http.Client, url, method, path, token string) (int, string) {
	t.Helper()
	var body io.Reader
	if method == http.MethodPost {
		body = strings.NewReader(`{"state":"success","context":"stand-in"}`)
	}
	request, err := http.NewRequestWithContext(t.Context(), method, url+path, body)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	return send(t, client, request)
}

// send makes one request and answers its status and its body, trimmed.
func send(t *testing.T, client *http.Client, request *http.Request) (int, string) {
	t.Helper()
	bounded := *client
	bounded.Timeout = 10 * time.Second
	answer, err := bounded.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", request.Method, request.URL, err)
	}
	defer answer.Body.Close()
	read, err := io.ReadAll(answer.Body)
	if err != nil {
		t.Fatalf("read the answer to %s %s: %v", request.Method, request.URL, err)
	}
	return answer.StatusCode, strings.TrimSpace(string(read))
}

// trustingTheStands is a client that trusts the stands' authority alone, as a command behind the
// proxy trusts the certificate the proxy presents for a bound host.
func trustingTheStands(t *testing.T, caFile string) *http.Client {
	t.Helper()
	pem, err := os.ReadFile(caFile)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem) {
		t.Fatalf("%s holds no PEM certificate", caFile)
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}}
}
