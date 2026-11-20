// packages/envoy/cmd/cipher/secret_gcp_order_test.go
//
// R1's order of the Google credential sources a store command takes, run through the real
// sources rather than the package's seams: a stub gcloud alone on PATH with a gcloud
// configuration directory of its own (useGcloudConfigurations), service-account key files whose
// token endpoint is a local server (writeServiceAccountFile, startGoogleTokenServer), and a fake
// metadata server GCE_METADATA_HOST points at (startMetadataServer). Each test reads the bearer
// token the fake Secret Manager saw, so it reads which source signed the call, not what the
// command says about it.
package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"

	"github.com/sjawhar/envoy/internal/testport"
)

// startGoogleTokenServer answers Google's JWT-bearer grant, as oauth2.googleapis.com/token does
// for a service-account key, with the token not-a-secret-for-<the assertion's iss>, so a call read back
// names the key that signed it. It answers its URL, which writeServiceAccountFile writes as the
// key's token_uri.
func startGoogleTokenServer(t *testing.T) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil || r.PostForm.Get("grant_type") != "urn:ietf:params:oauth:grant-type:jwt-bearer" {
			http.Error(w, "not a JWT-bearer grant", http.StatusBadRequest)
			return
		}
		parts := strings.Split(r.PostForm.Get("assertion"), ".")
		if len(parts) != 3 {
			http.Error(w, "the assertion is not a JWT", http.StatusBadRequest)
			return
		}
		payload, err := base64.RawURLEncoding.DecodeString(parts[1])
		var claims struct {
			Iss string `json:"iss"`
		}
		if err != nil || json.Unmarshal(payload, &claims) != nil || claims.Iss == "" {
			http.Error(w, "the assertion names no iss", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "not-a-secret-for-" + claims.Iss, "expires_in": 3600, "token_type": "Bearer"})
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// writeServiceAccountFile writes a service_account key file for email at path, with an RSA key
// made here and tokenURI as the endpoint its tokens come from.
func writeServiceAccountFile(t *testing.T, path, email, tokenURI string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(map[string]string{
		"type":           "service_account",
		"project_id":     "acme-deployment",
		"private_key_id": "test-key",
		"private_key":    string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
		"client_email":   email,
		"client_id":      "100000000000000000001",
		"token_uri":      tokenURI,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// metadataServer is a fake Compute Engine metadata server: it answers the instance's default
// service account's access token (metadata-not-a-secret), its email, its identity token and the
// project id, each with the Metadata-Flavor: Google header the real one sends, and counts the
// requests to each path.
type metadataServer struct {
	mu       sync.Mutex
	requests map[string]int
}

// metadataTokenPath is where the metadata server hands out the default service account's access
// token.
const metadataTokenPath = "/computeMetadata/v1/instance/service-accounts/default/token"

// startMetadataServer serves a metadataServer for email and points GCE_METADATA_HOST at it for the
// rest of t.
func startMetadataServer(t *testing.T, email string) *metadataServer {
	t.Helper()
	m := &metadataServer{requests: map[string]int{}}
	answers := map[string]string{
		metadataTokenPath: `{"access_token":"metadata-not-a-secret","expires_in":3600,"token_type":"Bearer"}`,
		"/computeMetadata/v1/instance/service-accounts/default/email":    email,
		"/computeMetadata/v1/instance/service-accounts/default/identity": "metadata-identity-token",
		"/computeMetadata/v1/project/project-id":                         "acme-deployment",
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		m.requests[r.URL.Path]++
		m.mu.Unlock()
		w.Header().Set("Metadata-Flavor", "Google")
		answer, ok := answers[r.URL.Path]
		if !ok || r.Header.Get("Metadata-Flavor") != "Google" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(answer))
	}))
	t.Cleanup(server.Close)
	t.Setenv("GCE_METADATA_HOST", strings.TrimPrefix(server.URL, "http://"))
	return m
}

// tokenRequests is how many times the default service account's access token was asked for.
func (m *metadataServer) tokenRequests() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.requests[metadataTokenPath]
}

// useGcloudConfigurations gives the rest of t a gcloud configuration directory of its own: HOME
// a test directory and CLOUDSDK_CONFIG its .config/gcloud, holding configurations/config_<name>
// with core/account configs[name] for each configuration (none where that is empty), and
// active_config naming active (written only when active is not empty). It clears every variable
// that chooses a Google credential, and puts a stub gcloud alone on PATH that holds a login for
// each configuration's account. The stub answers `auth list` (every login, the effective account
// marked ACTIVE), `config list` (core.account) and `auth print-access-token … ACCOUNT`
// (gcloud-not-a-secret-for-ACCOUNT, or a refresh failure while STUB_GCLOUD_REFUSE_TOKEN is set),
// where the configuration is the --configuration= its command line names, else
// CLOUDSDK_ACTIVE_CONFIG_NAME, else active_config, and CLOUDSDK_CORE_ACCOUNT, where set, replaces
// that configuration's account; any other command fails. Where the configuration names no
// account, the effective account is the active configuration's, as real gcloud on a Compute
// Engine VM answers one with the metadata server's service account (its core/account callback):
// measured on a developer's Compute Engine VM on 2026-11-15, `gcloud config list
// --configuration=person` for a configuration whose file is `[core]` alone named the VM's
// account. It answers the configuration directory.
func useGcloudConfigurations(t *testing.T, active string, configs map[string]string) string {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, ".config", "gcloud")
	if err := os.MkdirAll(filepath.Join(dir, "configurations"), 0o700); err != nil {
		t.Fatal(err)
	}
	var accounts, cases []string
	for _, name := range slices.Sorted(maps.Keys(configs)) {
		account, file := configs[name], "[core]\n"
		if account != "" {
			file += "account = " + account + "\n"
			if !slices.Contains(accounts, shellWord(account)) {
				accounts = append(accounts, shellWord(account))
			}
		}
		if err := os.WriteFile(filepath.Join(dir, "configurations", "config_"+name), []byte(file), 0o600); err != nil {
			t.Fatal(err)
		}
		cases = append(cases, shellWord(name)+") account="+shellWord(account)+" ;;")
	}
	if active != "" {
		if err := os.WriteFile(filepath.Join(dir, "active_config"), []byte(active), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	bin := t.TempDir()
	script := strings.Join([]string{
		"#!/bin/sh",
		`config=`,
		`for word; do case "$word" in --configuration=*) config=${word#--configuration=} ;; esac; done`,
		`[ -n "$config" ] || config=$CLOUDSDK_ACTIVE_CONFIG_NAME`,
		`[ -n "$config" ] || { [ -f ` + shellWord(filepath.Join(dir, "active_config")) + ` ] && read -r config <` + shellWord(filepath.Join(dir, "active_config")) + `; }`,
		`account=`,
		`case "$config" in ` + strings.Join(cases, " ") + ` *) ;; esac`,
		`[ -n "$account" ] || case "` + active + `" in ` + strings.Join(cases, " ") + ` *) ;; esac`,
		`[ -n "$CLOUDSDK_CORE_ACCOUNT" ] && account=$CLOUDSDK_CORE_ACCOUNT`,
		`case "$1 $2" in`,
		`"auth list") printf '['; sep=; for held in ` + strings.Join(accounts, " ") + `; do status=; [ "$held" = "$account" ] && status=ACTIVE; printf '%s{"account":"%s","status":"%s"}' "$sep" "$held" "$status"; sep=,; done; printf ']\n' ;;`,
		`"config list") printf '{"core":{"account":"%s"}}\n' "$account" ;;`,
		`"auth print-access-token") [ -z "$STUB_GCLOUD_REFUSE_TOKEN" ] || { echo 'ERROR: (gcloud.auth.print-access-token) Reauthentication failed.' >&2; exit 1; }; last=; for word; do case "$word" in --*) ;; *) last=$word ;; esac; done; printf 'gcloud-not-a-secret-for-%s\n' "$last" ;;`,
		`*) printf 'stub gcloud: unexpected command: %s\n' "$*" >&2; exit 2 ;;`,
		"esac",
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(bin, "gcloud"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("CLOUDSDK_CONFIG", dir)
	t.Setenv("PATH", bin)
	for _, name := range []string{"CLOUDSDK_AUTH_ACCESS_TOKEN", "GOOGLE_APPLICATION_CREDENTIALS", "CLOUDSDK_ACTIVE_CONFIG_NAME", "CLOUDSDK_CORE_ACCOUNT"} {
		t.Setenv(name, "")
	}
	return dir
}

// orderConfigurations are the two gcloud configurations of a Google VM a person also works on:
// the machine's own, box, active, and the person's, person.
var orderConfigurations = map[string]string{"box": "box@example.iam.gserviceaccount.com", "person": "grace@example.com"}

// orderSetup is one source of R1 made present, on top of the rows before it (useOrderSources).
type orderSetup struct {
	wellKnown, chosenConfig, chosenAccount, credentialsFile, lentToken string
}

// useOrderSources makes present each source setup names, writing the key files with tokenURI as
// their token endpoint: the well-known credentials file in dir, a credentials file
// GOOGLE_APPLICATION_CREDENTIALS names, CLOUDSDK_ACTIVE_CONFIG_NAME, CLOUDSDK_CORE_ACCOUNT and a
// lent access token.
func useOrderSources(t *testing.T, dir, tokenURI string, setup orderSetup) {
	t.Helper()
	if setup.wellKnown != "" {
		writeServiceAccountFile(t, filepath.Join(dir, "application_default_credentials.json"), setup.wellKnown, tokenURI)
	}
	if setup.chosenConfig != "" {
		t.Setenv("CLOUDSDK_ACTIVE_CONFIG_NAME", setup.chosenConfig)
	}
	if setup.chosenAccount != "" {
		t.Setenv("CLOUDSDK_CORE_ACCOUNT", setup.chosenAccount)
	}
	if setup.credentialsFile != "" {
		path := filepath.Join(t.TempDir(), "credentials.json")
		writeServiceAccountFile(t, path, setup.credentialsFile, tokenURI)
		t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", path)
	}
	if setup.lentToken != "" {
		t.Setenv("CLOUDSDK_AUTH_ACCESS_TOKEN", setup.lentToken)
	}
}

// TestGCPSecretCredentialOrder: each source of R1's order wins over every source after it, a
// person's chosen gcloud login over the machine's, on a Google VM whose active gcloud
// configuration is the machine's own (box) and whose person's login is in another (person).
// Each row adds one source to the row before it and reads the bearer token Secret Manager saw.
func TestGCPSecretCredentialOrder(t *testing.T) {
	const (
		wk           = "wk@example.iam.gserviceaccount.com"
		gac          = "gac@example.iam.gserviceaccount.com"
		boxSA        = "box@example.iam.gserviceaccount.com"
		person       = "grace@example.com"
		lent         = "lent-token"
		personConfig = "person"
	)
	for _, row := range []struct {
		name   string
		setup  orderSetup
		args   []string
		bearer string
	}{
		{"1 metadata alone", orderSetup{}, nil, "metadata-not-a-secret"},
		{"2 active gcloud over metadata", orderSetup{}, nil, "gcloud-not-a-secret-for-" + boxSA},
		{"3 well-known file over active gcloud", orderSetup{wellKnown: wk}, nil, "not-a-secret-for-" + wk},
		{"4 chosen configuration over well-known file", orderSetup{wellKnown: wk, chosenConfig: personConfig}, nil, "gcloud-not-a-secret-for-" + person},
		{"5 chosen account over well-known file", orderSetup{wellKnown: wk, chosenAccount: person}, nil, "gcloud-not-a-secret-for-" + person},
		{"6 credentials file over chosen configuration", orderSetup{wellKnown: wk, chosenConfig: personConfig, credentialsFile: gac}, nil, "not-a-secret-for-" + gac},
		{"7 lent token over credentials file", orderSetup{wellKnown: wk, chosenConfig: personConfig, credentialsFile: gac, lentToken: lent}, nil, lent},
	} {
		t.Run(row.name, func(t *testing.T) {
			fake := newFakeSecretManager()
			startFakeSecretManager(t, fake)
			holdScratchTest(fake)
			startMetadataServer(t, "machine@example.iam.gserviceaccount.com")
			var dir string
			if strings.HasPrefix(row.name, "1 ") {
				dir = useGcloudConfigurations(t, "", nil)
				t.Setenv("PATH", t.TempDir())
			} else {
				dir = useGcloudConfigurations(t, "box", orderConfigurations)
			}
			useOrderSources(t, dir, startGoogleTokenServer(t), row.setup)
			startSecretBrokerWith(t, gcpTestSettings(), gcpServedBy(fake, gcpTestSettings()))
			_, stderr, code := runSecret(append([]string{"show", "SCRATCH_TEST"}, row.args...)...)
			if code != 0 {
				t.Fatalf("exit %d, stderr %q; want 0", code, stderr)
			}
			if got, calls := fake.calledUnder(); got != "Bearer "+row.bearer || calls == 0 {
				t.Fatalf("Secret Manager calls ran under %q (%d calls), want only Bearer %s", got, calls, row.bearer)
			}
		})
	}
}

// TestGCPSecretNoSourceListsEverySource: with no source present - no gcloud on PATH, no
// credentials file, no variable, and nothing answering at the metadata server's address - the
// command stops within 2 s, before any Secret Manager call, with one refusal that names each of
// the six sources (the third by both its variables).
func TestGCPSecretNoSourceListsEverySource(t *testing.T) {
	fake := newFakeSecretManager()
	startFakeSecretManager(t, fake)
	dir := useGcloudConfigurations(t, "", nil)
	t.Setenv("PATH", t.TempDir())
	// A port the test holds and nothing listens on, so no other process can take it and answer
	// as a metadata server (testport.Refused).
	t.Setenv("GCE_METADATA_HOST", testport.Refused(t))
	startSecretBrokerWith(t, gcpTestSettings(), alwaysServed)
	started := time.Now()
	_, stderr, code := runSecret("show", "SCRATCH_TEST")
	took := time.Since(started)
	if code != 1 || took > 2*time.Second {
		t.Fatalf("exit %d after %s, stderr %q; want 1 within 2s", code, took, stderr)
	}
	for _, source := range []string{
		"CLOUDSDK_AUTH_ACCESS_TOKEN", "GOOGLE_APPLICATION_CREDENTIALS",
		"CLOUDSDK_ACTIVE_CONFIG_NAME", "CLOUDSDK_CORE_ACCOUNT",
		filepath.Join(dir, "application_default_credentials.json"), "gcloud", "metadata server",
	} {
		if !strings.Contains(stderr, source) {
			t.Fatalf("stderr %q does not name %s", stderr, source)
		}
	}
	if _, calls := fake.calledUnder(); calls != 0 {
		t.Fatalf("Secret Manager saw %d calls, want none", calls)
	}
}

// TestGCPSecretPresentButBrokenSourceStops: a source that is present but fails stops the command
// with its own error, and never falls through to a later source; a store refusal under the first
// source is never retried under another. The metadata server is there in every row, so a fall
// through would show as a call under it.
func TestGCPSecretPresentButBrokenSourceStops(t *testing.T) {
	for _, row := range []struct {
		name     string
		use      func(t *testing.T, dir, tokenURI string, fake *fakeSecretManager)
		args     []string
		code     int
		says     []string
		onlyCall string
	}{
		{
			name: "core account not held",
			use: func(t *testing.T, _, _ string, _ *fakeSecretManager) {
				t.Setenv("CLOUDSDK_CORE_ACCOUNT", "nobody@example.com")
			},
			code: 1, says: []string{"CLOUDSDK_CORE_ACCOUNT names nobody@example.com"},
		},
		{
			name: "credentials file does not parse",
			use: func(t *testing.T, _, _ string, _ *fakeSecretManager) {
				path := filepath.Join(t.TempDir(), "credentials.json")
				if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
					t.Fatal(err)
				}
				t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", path)
			},
			code: 1, says: []string{"GOOGLE_APPLICATION_CREDENTIALS"},
		},
		{
			name: "well-known file does not parse",
			use: func(t *testing.T, dir, _ string, _ *fakeSecretManager) {
				if err := os.WriteFile(filepath.Join(dir, "application_default_credentials.json"), []byte("not json"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			code: 1, says: []string{"application_default_credentials.json"},
		},
		{
			name: "refused under the first source",
			use: func(t *testing.T, dir, tokenURI string, fake *fakeSecretManager) {
				useOrderSources(t, dir, tokenURI, orderSetup{wellKnown: "wk@example.iam.gserviceaccount.com", chosenConfig: "person", credentialsFile: "gac@example.iam.gserviceaccount.com", lentToken: "lent-token"})
				fake.mu.Lock()
				fake.refuseMethods = map[string]codes.Code{"GetSecret": codes.PermissionDenied}
				fake.mu.Unlock()
			},
			code: 1, says: []string{"PermissionDenied"}, onlyCall: "Bearer lent-token",
		},
		{
			name: "chosen configuration names no account",
			use: func(t *testing.T, _, _ string, _ *fakeSecretManager) {
				useGcloudConfigurations(t, "box", map[string]string{"box": "box@example.iam.gserviceaccount.com", "person": ""})
				t.Setenv("CLOUDSDK_ACTIVE_CONFIG_NAME", "person")
			},
			code: 1, says: []string{"the gcloud configuration person", "names no account"},
		},
		{
			name: "missing configuration",
			use: func(t *testing.T, _, _ string, _ *fakeSecretManager) {
				t.Setenv("CLOUDSDK_ACTIVE_CONFIG_NAME", "missing")
			},
			code: 1, says: []string{"the gcloud configuration missing (CLOUDSDK_ACTIVE_CONFIG_NAME): read the gcloud configuration missing"},
		},
		{
			name: "active gcloud refresh fails",
			use: func(t *testing.T, _, _ string, _ *fakeSecretManager) {
				t.Setenv("STUB_GCLOUD_REFUSE_TOKEN", "1")
			},
			code: 1, says: []string{"the active gcloud account (gcloud configuration box)", "`gcloud auth print-access-token` failed for box@example.iam.gserviceaccount.com", "Reauthentication failed"},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			fake := newFakeSecretManager()
			startFakeSecretManager(t, fake)
			holdScratchTest(fake)
			metadata := startMetadataServer(t, "machine@example.iam.gserviceaccount.com")
			dir := useGcloudConfigurations(t, "box", orderConfigurations)
			row.use(t, dir, startGoogleTokenServer(t), fake)
			startSecretBrokerWith(t, gcpTestSettings(), gcpServedBy(fake, gcpTestSettings()))
			_, stderr, code := runSecret(append([]string{"show", "SCRATCH_TEST"}, row.args...)...)
			if code != row.code {
				t.Fatalf("exit %d, stderr %q; want %d", code, stderr, row.code)
			}
			for _, said := range row.says {
				if !strings.Contains(stderr, said) {
					t.Fatalf("stderr %q does not say %q", stderr, said)
				}
			}
			got, calls := fake.calledUnder()
			switch {
			case row.onlyCall == "" && calls != 0:
				t.Fatalf("Secret Manager saw %d calls under %q, want none", calls, got)
			case row.onlyCall != "" && (got != row.onlyCall || calls == 0):
				t.Fatalf("Secret Manager calls ran under %q (%d calls), want only %s", got, calls, row.onlyCall)
			}
			if n := metadata.tokenRequests(); n != 0 {
				t.Fatalf("the metadata server was asked for %d access tokens, want none", n)
			}
		})
	}
}

// metadataAnswer is how startPartialMetadataServer answers one path.
type metadataAnswer int

const (
	// answerHeld holds the request open until the test ends.
	answerHeld metadataAnswer = iota
	// answerGoogle is the real server's answer, with Metadata-Flavor: Google.
	answerGoogle
	// answerGoogleFault is HTTP 500 with Metadata-Flavor: Google: a Google server that is present
	// and fails.
	answerGoogleFault
	// answerNotGoogle is the real server's answer without Metadata-Flavor: Google, as a resolver
	// that sends every name to one web server would bring it.
	answerNotGoogle
)

// startPartialMetadataServer serves, at the address GCE_METADATA_HOST names for the rest of t, a
// metadata server that answers the default service account's access token path (with
// metadata-not-a-secret) as token says and its email path (with
// machine@example.iam.gserviceaccount.com) as email says, and holds every other request open
// until the test ends.
func startPartialMetadataServer(t *testing.T, token, email metadataAnswer) *metadataServer {
	t.Helper()
	m := &metadataServer{requests: map[string]int{}}
	held := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		m.requests[r.URL.Path]++
		m.mu.Unlock()
		answer, body := answerHeld, ""
		switch r.URL.Path {
		case metadataTokenPath:
			answer, body = token, `{"access_token":"metadata-not-a-secret","expires_in":3600,"token_type":"Bearer"}`
		case "/computeMetadata/v1/instance/service-accounts/default/email":
			answer, body = email, "machine@example.iam.gserviceaccount.com"
		}
		switch answer {
		case answerHeld:
			select {
			case <-held:
			case <-r.Context().Done():
			}
			return
		case answerGoogle, answerGoogleFault:
			w.Header().Set("Metadata-Flavor", "Google")
		}
		if answer == answerGoogleFault {
			http.Error(w, "the test fails it", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(held) })
	t.Setenv("GCE_METADATA_HOST", strings.TrimPrefix(server.URL, "http://"))
	return m
}

// TestGCPSecretMetadataSourceIsPresentWhenItYieldsAToken: with no other source, the metadata
// server is present exactly when it yields a token (R2), through the real token and email reads:
// a token with no answer to the email read signs in, naming the machine's identity generically; a
// server that answers neither, and an answer that is not Google's, are no metadata server, so the
// command lists every source; a Google server that answers the token read with an error is
// present and broken, so that error stops the command.
func TestGCPSecretMetadataSourceIsPresentWhenItYieldsAToken(t *testing.T) {
	for _, row := range []struct {
		name         string
		token, email metadataAnswer
		code         int
		within       time.Duration
		says         []string
		saysNot      string
		onlyCall     string
	}{
		{"mints a token, never answers the email read", answerGoogle, answerHeld, 0, 5 * time.Second,
			[]string{"using the metadata server: this machine's or this pod's identity\n"}, "", "Bearer metadata-not-a-secret"},
		{"answers neither", answerHeld, answerHeld, 1, 15 * time.Second,
			[]string{"found no Google credential", "the metadata server: no metadata server"}, "ask the metadata server for this machine's access token", ""},
		{"answers, but not as Google", answerNotGoogle, answerNotGoogle, 1, 5 * time.Second,
			[]string{"found no Google credential", "not a Google metadata server"}, "ask the metadata server for this machine's access token", ""},
		{"answers the token read with an error", answerGoogleFault, answerGoogle, 1, 5 * time.Second,
			[]string{"ask the metadata server for this machine's access token", "answered HTTP 500"}, "found no Google credential", ""},
	} {
		t.Run(row.name, func(t *testing.T) {
			fake := newFakeSecretManager()
			startFakeSecretManager(t, fake)
			holdScratchTest(fake)
			useGcloudConfigurations(t, "", nil)
			t.Setenv("PATH", t.TempDir())
			startPartialMetadataServer(t, row.token, row.email)
			startSecretBrokerWith(t, gcpTestSettings(), gcpServedBy(fake, gcpTestSettings()))
			started := time.Now()
			_, stderr, code := runSecret("show", "SCRATCH_TEST")
			took := time.Since(started)
			if code != row.code || took > row.within {
				t.Fatalf("exit %d after %s, stderr %q; want %d within %s", code, took, stderr, row.code, row.within)
			}
			for _, said := range row.says {
				if !strings.Contains(stderr, said) {
					t.Fatalf("stderr %q does not say %q", stderr, said)
				}
			}
			if row.saysNot != "" && strings.Contains(stderr, row.saysNot) {
				t.Fatalf("stderr %q says %q", stderr, row.saysNot)
			}
			got, calls := fake.calledUnder()
			switch {
			case row.onlyCall == "" && calls != 0:
				t.Fatalf("Secret Manager saw %d calls under %q, want none", calls, got)
			case row.onlyCall != "" && (got != row.onlyCall || calls == 0):
				t.Fatalf("Secret Manager calls ran under %q (%d calls), want only %s", got, calls, row.onlyCall)
			}
		})
	}
}
