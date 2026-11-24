package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/ciphertest"
	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/requests"
	"github.com/sjawhar/envoy/internal/cipher/secrets"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/aws"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/aws/awstest"
	"github.com/sjawhar/envoy/internal/cipherclient"
	"github.com/sjawhar/envoy/internal/logtest"
)

// TestSecretRetagOfAValueItCannotReadIsRefusedByTheBrokerUntilOneDecodes is the case the broker's
// own value check is for: a person who may retag a secret and may not read its value retags a
// stored secret holding a value with no app_id to github-app. The retag cannot check the value (its
// GetSecretValue is refused AccessDeniedException), so it writes the tags and says on stderr that
// the value was not checked. The broker (ciphertest's, over the same awstest.Local, reading with
// its own access) reads and decodes the value at the reread the retag asks for, before any mint,
// and refuses the secret as value-malformed: one RefusedMessage line and one
// broker_unservable_secrets series for the secret. The broker serves a github-app secret only
// through a profile, so APP_KEY is served by the profile of its own name, on the one account the
// retag names, and a reread of that name answers the profile (policy.Current.RefreshOne): the
// retag exits 1 on its refusal, profile-source-missing, whose source the broker refuses, and the
// profile has a series of its own. A reload writes no further line and reads no value. Once
// `secret set` stores a value the decoder reads, the reread it asks for writes one
// ServableMessage line, both series are gone, and a release of the secret mints.
func TestSecretRetagOfAValueItCannotReadIsRefusedByTheBrokerUntilOneDecodes(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (t.Setenv at secret_value_refused_test.go:76); it signals the test process or changes its signal handling (ciphertest.NewRig, api.Register, api.routes, (*server).listLauncherCredentials, (*server).machineLoginsOwnOnly, projectaccess.Restricted, (*Client).MemberKind, (*Client).askKind, (*Client).readKind, (*Client).get, (Reader).Get, (*GoogleAccount).Read, (*GoogleAccount).read, (*GoogleAccount).mint, (*GoogleAccount).generateIDToken, (*GoogleAccount).accessToken, (gcloudToken).Token, main.runGcloud, main.runRecorded, main.watchSignalsWhileGcloudRuns, signal.Notify at secret_gcp.go:558); it captures the process's default logger, whose records are every test's that logs meanwhile (logtest.SetDefault, logtest.Swap, slog.SetDefault at logtest.go:36); it sets a package variable the package's other tests read (main.useAWS, awsClients set at secret_test.go:375).
	recorder := &logtest.Recorder{}
	logtest.SetDefault(t, recorder)
	var tokensAsked atomic.Int32
	github := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/app/installations":
			_ = json.NewEncoder(w).Encode([]map[string]any{{"id": 7, "account": map[string]string{"login": "acme-org"}, "permissions": map[string]string{"contents": "write", "metadata": "read"}}})
		case r.Method == http.MethodPost && r.URL.Path == "/app/installations/7/access_tokens":
			tokensAsked.Add(1)
			// The token holds what its request asked, as GitHub's does.
			var asked struct {
				Permissions map[string]string `json:"permissions"`
			}
			_ = json.NewDecoder(r.Body).Decode(&asked)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"token": "ghs-value-7", "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
				"permissions": asked.Permissions, "repository_selection": "all",
			})
		default:
			t.Errorf("unexpected request to GitHub: %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(github.Close)

	profile := `{"name": "APP_KEY", "github": {"accounts": ["acme-org"], "permissions": {"metadata": "read"}}}`
	rig, local := awsRig(t, ciphertest.WithProfiles(policytest.ProfilesFile(profile)), ciphertest.WithMinters(map[string]requests.Minter{
		policy.TypeGitHubApp: secrets.GitHubApp{Base: github.URL, HTTP: github.Client()}.Mint,
	}))
	noAppID := appValue(t, map[string]string{"private_key": appKeyPEM(t)})
	local.Put(awstest.Secret("APP_KEY", rig.Operator, policy.TierAgent, noAppID))
	// The person's own sign-in reads through valueReads, which refuses every GetSecretValue as
	// IAM refuses a role that holds no read; the broker reads local itself.
	signedIn := &valueReads{Local: local}
	awsTypeStore{signedIn}.refuseValueReads(true)
	useAWS(t, signedIn, testAccount, adaSignIn)
	t.Setenv("AGENT_SECRETS_URL", rig.URL)
	useRigSession(t, rig)

	id := policytest.ID("APP_KEY")
	lines := func(message string) int {
		t.Helper()
		n := 0
		for _, record := range recorder.Records() {
			if record.Message != message {
				continue
			}
			record.Attrs(func(attr slog.Attr) bool {
				if attr.Key == "name" && attr.Value.String() == id {
					n++
				}
				return true
			})
		}
		return n
	}
	scrape := func(metric string) string {
		t.Helper()
		answer := httptest.NewRecorder()
		rig.Registry.Handler().ServeHTTP(answer, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		var samples strings.Builder
		for line := range strings.Lines(answer.Body.String()) {
			if strings.HasPrefix(line, metric+"{") {
				samples.WriteString(line)
			}
		}
		return samples.String()
	}
	valueReadsCounted := func() string {
		t.Helper()
		for line := range strings.Lines(scrape("broker_policy_store_reads_total")) {
			if count, ok := strings.CutPrefix(line, `broker_policy_store_reads_total{read="value"} `); ok {
				return strings.TrimSpace(count)
			}
		}
		return "0"
	}
	// The secret's own refusal and its profile's, keyed as the profile's RefusedMessage line names
	// it (profile:APP_KEY), each under the secret's owner.
	refusedSeries := `broker_unservable_secrets{secret="` + id + `",reason="` + policy.ReasonValueMalformed + `",owner="` + rig.Operator + `"} 1` + "\n" +
		`broker_unservable_secrets{secret="profile:APP_KEY",reason="` + policy.ReasonProfileSourceMissing + `",owner="` + rig.Operator + `"} 1` + "\n"

	// The retag: tags written, the value unchecked, and the broker's refusal of the profile its
	// reread answers.
	stdout, stderr, code := runSecret("retag", "APP_KEY", "--type", policy.TypeGitHubApp, "--github-owners", "acme-org")
	if code != 1 {
		t.Fatalf("retag: exit %d, stdout %q, stderr %q; want 1, the broker refusing the secret it reread", code, stdout, stderr)
	}
	if tags := aws.TagMap(describe(t, local, "APP_KEY").Tags); tags[policy.TagType] != policy.TypeGitHubApp || tags[policy.TagGitHubOwners] != "acme-org" {
		t.Fatalf("tags after the retag = %v; want github-app for acme-org, written though the value was not checked", tags)
	}
	if !strings.Contains(stderr, "the tags are written, but APP_KEY's value was not checked as a github-app secret's") || !strings.Contains(stderr, "secretsmanager:GetSecretValue") {
		t.Fatalf("retag stderr %q; want its line saying the value was not checked and naming the read it needs", stderr)
	}
	if !strings.Contains(stdout, "(reason="+policy.ReasonProfileSourceMissing+")") || !strings.Contains(stderr, "(reason="+policy.ReasonProfileSourceMissing+")") {
		t.Fatalf("retag stdout %q, stderr %q; want the broker's refusal of the profile naming %s", stdout, stderr, policy.ReasonProfileSourceMissing)
	}
	if signedIn.reads != 1 {
		t.Fatalf("the retag made %d value reads with the person's sign-in; want the one it was refused", signedIn.reads)
	}
	if n := tokensAsked.Load(); n != 0 {
		t.Fatalf("GitHub was asked for %d tokens before any release; the refusal must come before any mint", n)
	}
	if _, served := rig.Policy.Get().Secrets["APP_KEY"]; served {
		t.Fatal("the broker serves APP_KEY after the reread, whose value has no app_id")
	}
	if n := lines(policy.RefusedMessage); n != 1 {
		t.Fatalf("the reread wrote %d %q lines for %s; want 1", n, policy.RefusedMessage, id)
	}
	if got := scrape("broker_unservable_secrets"); got != refusedSeries {
		t.Fatalf("broker_unservable_secrets after the reread reads\n%s\nwant\n%s", got, refusedSeries)
	}
	if got := valueReadsCounted(); got != "1" {
		t.Fatalf("the broker has read %s values; want the one its reread decoded", got)
	}

	// A reload: no further line, and no value read.
	if err := rig.Policy.Refresh(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if n := lines(policy.RefusedMessage); n != 1 {
		t.Fatalf("after a reload there are %d %q lines for %s; want still 1", n, policy.RefusedMessage, id)
	}
	if got := valueReadsCounted(); got != "1" {
		t.Fatalf("after a reload the broker has read %s values; want still 1", got)
	}
	if got := scrape("broker_unservable_secrets"); got != refusedSeries {
		t.Fatalf("broker_unservable_secrets after a reload reads\n%s\nwant\n%s", got, refusedSeries)
	}

	// A value that decodes: the next reread serves the secret again.
	useStdin(t, policytest.TypeValue(policy.TypeGitHubApp))
	stdout, stderr, code = runSecret("set", "APP_KEY")
	if code != 0 || !strings.Contains(stdout, "broker: serving APP_KEY") {
		t.Fatalf("set of a value the decoder reads: exit %d, stdout %q, stderr %q; want 0 with the secret served", code, stdout, stderr)
	}
	if n := lines(policy.ServableMessage); n != 1 {
		t.Fatalf("the set's reread wrote %d %q lines for %s; want 1", n, policy.ServableMessage, id)
	}
	if got := scrape("broker_unservable_secrets"); got != "" {
		t.Fatalf("broker_unservable_secrets after the set reads\n%s\nwant no series", got)
	}
	if got := valueReadsCounted(); got != "2" {
		t.Fatalf("after the set the broker has read %s values; want 2, one for the new version", got)
	}

	// A release mints: a machine of the operator's logs in, enrolls a session, and is granted the
	// agent-tier secret its own person owns at once.
	client := ciphertest.NewClient(t, rig.URL, strings.TrimPrefix(rig.URL, "http://"))
	client.UIToken, client.DPoP = rig.UIToken, cipherclient.NewDPoPTransport
	machineKey := ciphertest.NewSigningKey(t)
	authorization := client.DeviceLogin(t, machineKey, rig.Operator, "value-box")
	credentialID := client.ApproveMachineLogin(t, rig.Operator, authorization.UserCode)
	if _, err := client.DeviceToken(t, machineKey, authorization); err != nil {
		t.Fatalf("exchange the device code: %v", err)
	}
	enrollmentID, sessionKey := client.EnrollSession(t, machineKey, credentialID, "value-box-session")
	grantID := client.RequestAgentSecret(t, sessionKey, enrollmentID, "the stored value decodes now", "APP_KEY")
	if values := client.GrantValues(t, sessionKey, enrollmentID, grantID); !strings.Contains(values["APP_KEY"], "ghs-value-7") {
		t.Fatalf("the release = %v; want APP_KEY minted from the installation's token", values)
	}
	if n := tokensAsked.Load(); n != 1 {
		t.Fatalf("GitHub was asked for %d tokens; want the release's one", n)
	}
}
