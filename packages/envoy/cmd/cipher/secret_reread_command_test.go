package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/secrets"
)

// TestTheRereadAnOpenOutcomeNamesReachesTheBroker: a retag whose tag write's answer is lost, and
// whose reread the broker then refuses, prints the reread to ask for once the tags hold what the
// person wants (rereadCommand), and says nothing of the write standing. Run in bash, with an
// environment holding PATH and HOME alone, so no curlrc and no BASH_ENV of the runner's can send it
// elsewhere, that command:
//   - makes one reread, with no proof, where the shell's AGENT_SECRETS_URL holds the broker's
//     address, and prints nothing of the broker's answer, whose reason says the write stands;
//   - makes one where that address ends in slashes, as an address typed by hand may: the command
//     strips them itself (`${u%"${u##*[!/]}"}`), since a doubled slash would be answered with a
//     redirect curl does not follow, and the command would exit 0 having asked nothing;
//   - stops naming the config file, asking nothing, where the shell has no AGENT_SECRETS_URL, as
//     where the CLI read the address from that file;
//   - exits non-zero naming the status where the broker refuses the reread, so a person does not
//     read a refusal as the reread done.
func TestTheRereadAnOpenOutcomeNamesReachesTheBroker(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (main.useAWS, t.Setenv at secret_test.go:372); it sets a package variable the package's other tests read (main.useAWS, awsClients set at secret_test.go:375).
	t.Run("the retag prints the command", func(t *testing.T) {
		local := secrets.NewLocal(policytest.Secret("API_KEY", "ada@example.com", policy.TierHuman, "v1"))
		broker := startSecretBroker(t, servedBy(local))
		broker.failRereads(http.StatusServiceUnavailable)
		useAWS(t, &raceOnTag{secretsAPI: local, loseAnswer: "tag"}, testAccount, adaSignIn)
		_, stderr, code := runSecret("retag", "API_KEY", "--tier", policy.TierAgent)
		printed := regexp.MustCompile("ask for the reread with `([^`]*)`").FindStringSubmatch(stderr)
		if code != 1 || printed == nil || printed[1] != rereadCommand("API_KEY") {
			t.Fatalf("retag: exit %d stderr %q; want 1 and the reread command %q", code, stderr, rereadCommand("API_KEY"))
		}
		for _, claim := range []string{"stands", "after the write"} {
			if strings.Contains(stderr, claim) {
				t.Fatalf("retag: stderr %q says %q of a write whose outcome is unknown", stderr, claim)
			}
		}
	})
	for _, c := range []struct {
		name       string
		noVar      bool
		suffix     string
		refuse     int
		wantReread bool
		wantSaid   string
	}{
		{name: "the address in AGENT_SECRETS_URL", wantReread: true},
		{name: "an address ending in slashes", suffix: "//", wantReread: true},
		{name: "no AGENT_SECRETS_URL in the shell", noVar: true, wantSaid: "~/.config/agent-secrets/config"},
		{name: "a broker refusing the reread", refuse: http.StatusTooManyRequests, wantSaid: "returned error: 429"},
	} {
		t.Run(c.name, func(t *testing.T) {
			local := secrets.NewLocal(policytest.Secret("API_KEY", "ada@example.com", policy.TierHuman, "v1"))
			broker := startSecretBroker(t, servedBy(local))
			broker.failRereads(c.refuse)
			home := t.TempDir()
			env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home}
			if c.noVar {
				// The CLI read the address from the config file, so the shell holds none.
				dir := filepath.Join(home, ".config", "agent-secrets")
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "config"), []byte(brokerURLVar+"="+os.Getenv(brokerURLVar)+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			} else {
				env = append(env, brokerURLVar+"="+os.Getenv(brokerURLVar)+c.suffix)
			}
			cmd := exec.Command("bash", "--noprofile", "--norc", "-c", rereadCommand("API_KEY"))
			cmd.Env = env
			out, err := cmd.CombinedOutput()
			rereads := broker.rereadUnsigned()
			if c.wantReread {
				if err != nil || len(rereads) != 1 || rereads[0] != "API_KEY" || strings.Contains(string(out), "stands") {
					t.Fatalf("the printed command: err %v, output %q, rereads %v; want one reread of API_KEY, and no word of the write standing", err, out, rereads)
				}
				return
			}
			if err == nil || !strings.Contains(string(out), c.wantSaid) || len(rereads) != 0 {
				t.Fatalf("the printed command: err %v, output %q, rereads %v; want it to fail saying %q and record no reread", err, out, rereads, c.wantSaid)
			}
		})
	}
}

// TestTheRereadCommandJoinsTheRouteOntoTheAddress: the printed reread (rereadCommand), run in bash
// with AGENT_SECRETS_URL carrying a path and a query, posts to the address's path with the route
// joined onto it and the address's query kept after the route
// (/broker/v1/secrets/API_KEY/reread?sig=...), where the route appended to the address's text sat
// inside the query (/broker?sig=.../v1/...: the broker was asked /broker and curl failed on its 404)
// (query, query-trailing-slashes); an address's fragment, which no request sends, is dropped
// (fragment). An address with neither is posted where it was (no-query). A stand-in records the
// path and the query each reread reaches it with.
func TestTheRereadCommandJoinsTheRouteOntoTheAddress(t *testing.T) {
	type reached struct{ path, query string }
	asked := make(chan reached, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked <- reached{r.URL.Path, r.URL.RawQuery}
		if r.Method == http.MethodPost && r.URL.Path == "/broker/v1/secrets/API_KEY/reread" {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	for _, c := range []struct{ name, suffix, query string }{
		{"query", "/broker?sig=k3y-7Qz4k", "sig=k3y-7Qz4k"},
		{"query-trailing-slashes", "/broker//?sig=k3y-7Qz4k", "sig=k3y-7Qz4k"},
		{"fragment", "/broker#pane", ""},
		{"no-query", "/broker", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			cmd := exec.Command("bash", "--noprofile", "--norc", "-c", rereadCommand("API_KEY"))
			cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), brokerURLVar + "=" + srv.URL + c.suffix}
			out, err := cmd.CombinedOutput()
			select {
			case got := <-asked:
				if want := (reached{"/broker/v1/secrets/API_KEY/reread", c.query}); got != want {
					t.Errorf("an address ending %q was posted at %+v; want %+v", c.suffix, got, want)
				}
			default:
				t.Fatalf("no reread reached the broker: err %v, output %q", err, out)
			}
			if err != nil {
				t.Errorf("the printed command at an address ending %q: err %v, output %q; want exit 0", c.suffix, err, out)
			}
		})
	}
}
