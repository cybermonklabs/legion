package gcptest

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	secretmanager "cloud.google.com/go/secretmanager/apiv1"
	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/sjawhar/envoy/internal/cipher/secretstore"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/gcp"
	"github.com/sjawhar/envoy/internal/googleidentity"
	"github.com/sjawhar/envoy/testexec"
)

// The settings a GCP broker of the writer's tests answers: a project number, the agent-secrets key
// and the prefix in that project.
const (
	SettingsProject = "100000000001"
	SettingsKMSKey  = "projects/acme-deployment/locations/global/keyRings/agent-secrets/cryptoKeys/agent-secrets"
	SettingsPrefix  = "projects/100000000001/secrets/agent-secrets-"
)

// Settings are a GCP broker's GET /v1/settings as the writer reads them (secretstore.Settings),
// with SettingsProject, SettingsKMSKey and SettingsPrefix, each field printing as it is.
func Settings() secretstore.Settings {
	return secretstore.Settings{
		Store: gcp.Name, Prefix: SettingsPrefix,
		Fields: map[string]string{"gcp_project": SettingsProject, "kms_key_name": SettingsKMSKey},
	}
}

// UseFake serves f over a real gRPC server on loopback for the rest of t, and points the GCP
// writer's client dial (gcp.Hooks.ClientDial) at it. The fake takes no authentication, but each
// call carries the bearer token of the credentials it was dialed with (bearerOf), which the fake
// records (Intercept), so a test sees which sign-in a call ran under.
func UseFake(t *testing.T, f *FakeSecretManager) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server := grpc.NewServer(grpc.UnaryInterceptor(f.Intercept))
	secretmanagerpb.RegisterSecretManagerServiceServer(server, f)
	go server.Serve(lis)
	t.Cleanup(server.Stop)

	UseDialTo(t, lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
}

// UseDial leaves the rest of t with the GCP writer's client dial (gcp.Hooks.ClientDial) as dial,
// and puts the one in place back after: the one way a test points the Secret Manager client at a
// stand-in.
func UseDial(t *testing.T, dial func(ctx context.Context, creds *google.Credentials, interceptor grpc.UnaryClientInterceptor) (*secretmanager.Client, error)) {
	t.Helper()
	t.Cleanup(gcp.SwapHooks(gcp.Hooks{ClientDial: dial}))
}

// UseDialTo leaves the rest of t with the GCP writer building its client on a connection to
// target dialed with opts (UseDial). A client built on a connection dialed here ignores the
// client's own dial options, so the interceptor that words every failure is installed on the
// connection itself, as the real dial installs it, and each call carries the bearer token of the
// credentials it signs in with (bearerOf).
func UseDialTo(t *testing.T, target string, opts ...grpc.DialOption) {
	t.Helper()
	UseDial(t, func(ctx context.Context, creds *google.Credentials, interceptor grpc.UnaryClientInterceptor) (*secretmanager.Client, error) {
		dial := append(slices.Clip(opts), grpc.WithChainUnaryInterceptor(interceptor))
		if creds != nil && creds.TokenSource != nil {
			dial = append(dial, grpc.WithPerRPCCredentials(bearerOf{creds.TokenSource}))
		}
		conn, err := grpc.NewClient(target, dial...)
		if err != nil {
			return nil, err
		}
		return secretmanager.NewClient(ctx, option.WithGRPCConn(conn))
	})
}

// bearerOf hands each call the access token source answers, as the real client does; the fake
// listens without TLS, so it asks for none.
type bearerOf struct{ source oauth2.TokenSource }

func (b bearerOf) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	token, err := b.source.Token()
	if err != nil {
		return nil, err
	}
	return map[string]string{"authorization": "Bearer " + token.AccessToken}, nil
}

func (bearerOf) RequireTransportSecurity() bool { return false }

// UseIdentity makes the GCP writer's credential sources answer kind/email for the rest of t, and
// its userinfo read email, with no network call (gcp.SwapHooks). kind "" is the metadata
// server's identity: it makes the metadata token hook yield a token and the metadata principal
// hook answer email, which signs in only where no source before the metadata server is present (a
// test that also puts a stub gcloud holding a login on PATH runs under that login).
// "authorized_user" is a person's own Application Default Credentials, where `gcloud auth
// application-default login` writes them: the well-known credentials file. Any other kind is a
// machine's credentials file GOOGLE_APPLICATION_CREDENTIALS names, of that "type"
// ("service_account" for a key file, whose client_email is email, "external_account" for a
// workload identity federation credential such as a dev box's, which impersonates email). It
// writes the file and makes the credentials file hook answer credentials carrying its JSON.
// Either way it clears CLOUDSDK_AUTH_ACCESS_TOKEN, which would outrank them, and the variables a
// developer's own shell could set to choose a sign-in (GOOGLE_APPLICATION_CREDENTIALS,
// CLOUDSDK_CORE_ACCOUNT), which the commands a form prints carry, and points HOME and
// CLOUDSDK_CONFIG at an empty directory, so no credentials file or gcloud configuration of the
// developer's own is read; a test that calls UseGcloud, which does that again, calls it before this.
func UseIdentity(t *testing.T, kind, email string) {
	t.Helper()
	t.Setenv("CLOUDSDK_AUTH_ACCESS_TOKEN", "")
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "")
	t.Setenv("CLOUDSDK_CORE_ACCOUNT", "")
	UseEmptyGcloudConfigDir(t)
	token := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "fake"})
	if kind == "" {
		t.Cleanup(gcp.SwapHooks(gcp.Hooks{
			Userinfo: UserinfoOf(email),
			MetadataToken: func(context.Context) (*google.Credentials, error) {
				return &google.Credentials{TokenSource: token}, nil
			},
			MetadataPrincipal: func(context.Context) string { return email },
		}))
		return
	}
	var data string
	switch kind {
	case "service_account":
		data = `{"type":"service_account","client_email":"` + email + `"}`
	case "external_account":
		data = `{"type":"external_account","service_account_impersonation_url":"https://iamcredentials.googleapis.com/v1/projects/-/serviceAccounts/` + email + `:generateAccessToken"}`
	default:
		data = `{"type":"` + kind + `"}`
	}
	path := filepath.Join(t.TempDir(), "credentials.json")
	if kind == "authorized_user" {
		path = gcp.WellKnownCredentialsFile()
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if kind != "authorized_user" {
		t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", path)
	}
	t.Cleanup(gcp.SwapHooks(gcp.Hooks{
		Userinfo: UserinfoOf(email),
		CredentialsFile: func(_ context.Context, named string) (*google.Credentials, error) {
			if named != path {
				return nil, fmt.Errorf("the test wrote no credentials file at %s", named)
			}
			return &google.Credentials{JSON: []byte(data), TokenSource: token}, nil
		},
	}))
}

// UseEmptyGcloudConfigDir points HOME and CLOUDSDK_CONFIG at an empty directory for the rest of
// t, so no well-known credentials file and no gcloud configuration of the developer's own is
// read, and clears CLOUDSDK_ACTIVE_CONFIG_NAME.
func UseEmptyGcloudConfigDir(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLOUDSDK_CONFIG", filepath.Join(home, ".config", "gcloud"))
	t.Setenv("CLOUDSDK_ACTIVE_CONFIG_NAME", "")
}

// UseNoADC leaves the rest of t with none of the credentials Application Default Credentials
// would find: no credentials file GOOGLE_APPLICATION_CREDENTIALS names, no well-known file (HOME
// and CLOUDSDK_CONFIG an empty directory), and no metadata server (the metadata token hook
// answers googleidentity.ErrNoMetadataServer, as it does off Google Cloud); and it clears
// CLOUDSDK_AUTH_ACCESS_TOKEN.
func UseNoADC(t *testing.T) {
	t.Helper()
	t.Setenv("CLOUDSDK_AUTH_ACCESS_TOKEN", "")
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "")
	UseEmptyGcloudConfigDir(t)
	t.Cleanup(gcp.SwapHooks(gcp.Hooks{
		MetadataToken: func(context.Context) (*google.Credentials, error) {
			return nil, fmt.Errorf("%w: the test has none", googleidentity.ErrNoMetadataServer)
		},
		MetadataPrincipal: func(context.Context) string { return "" },
		Userinfo:          UserinfoOf(""),
	}))
}

// UseLentAccessToken lends token in CLOUDSDK_AUTH_ACCESS_TOKEN for the rest of t, and makes
// userinfo answer email for it.
func UseLentAccessToken(t *testing.T, token, email string) {
	t.Helper()
	t.Cleanup(gcp.SwapHooks(gcp.Hooks{Userinfo: UserinfoOf(email)}))
	t.Setenv("CLOUDSDK_AUTH_ACCESS_TOKEN", token)
}

// CloudPlatformToken is a lent access token minted for the cloud-platform scope alone, as the
// broker mints a google-login secret's token: it carries no email scope.
const CloudPlatformToken = "lent-cloud-platform-token"

// UseCloudPlatformToken lends CloudPlatformToken in CLOUDSDK_AUTH_ACCESS_TOKEN for the rest of t,
// with no Application Default Credentials and no gcloud, and makes userinfo refuse it as Google's
// endpoint refuses every token without an email scope: 401, naming no account.
func UseCloudPlatformToken(t *testing.T) {
	t.Helper()
	UseNoADC(t)
	UseNoGcloud(t)
	t.Setenv("CLOUDSDK_AUTH_ACCESS_TOKEN", CloudPlatformToken)
	inner := gcp.CurrentHooks().Userinfo
	t.Cleanup(gcp.SwapHooks(gcp.Hooks{Userinfo: func(_ context.Context, creds *google.Credentials) (string, error) {
		token, err := creds.TokenSource.Token()
		if err != nil {
			return "", err
		}
		if token.AccessToken == CloudPlatformToken {
			return "", errors.New("read the Google sign-in's email from " + gcp.UserinfoURL + ": the server answered HTTP 401 UNAUTHENTICATED")
		}
		return inner(context.Background(), creds)
	}}))
}

// userinfoOf answers as Google's userinfo endpoint does, the account a token was issued to: the
// account the stub gcloud lent a token for (StubTokenPrefix), and email for any other token.
func UserinfoOf(email string) func(context.Context, *google.Credentials) (string, error) {
	return func(_ context.Context, creds *google.Credentials) (string, error) {
		token, err := creds.TokenSource.Token()
		if err != nil {
			return "", err
		}
		if account, ok := strings.CutPrefix(token.AccessToken, StubTokenPrefix); ok {
			return account, nil
		}
		return email, nil
	}
}

// StubTokenPrefix starts every access token the stub gcloud lends (UseGcloud): it lends account
// the token StubTokenPrefix + account, so a test reads whose login a call ran under.
const StubTokenPrefix = "stub-gcloud-token-for-"

// GcloudVariables are the variables the stub gcloud records from its environment on every run:
// those that choose the login or the token gcloud lends (an access token, an account, impersonation,
// an access token file, a credential file override), and those that keep it from prompting, from
// writing log files and from printing its HTTP traffic.
var GcloudVariables = []string{
	"CLOUDSDK_AUTH_ACCESS_TOKEN",
	"CLOUDSDK_AUTH_ACCESS_TOKEN_FILE",
	"CLOUDSDK_AUTH_CREDENTIAL_FILE_OVERRIDE",
	"CLOUDSDK_AUTH_IMPERSONATE_SERVICE_ACCOUNT",
	"CLOUDSDK_CORE_ACCOUNT",
	"CLOUDSDK_CORE_DISABLE_FILE_LOGGING",
	"CLOUDSDK_CORE_DISABLE_PROMPTS",
	"CLOUDSDK_CORE_LOG_HTTP",
}

// GcloudStub is a stand-in gcloud alone on PATH (UseGcloud).
type GcloudStub struct{ dir string }

// UseGcloud puts a stub gcloud alone on PATH for the rest of t, holding a login for each of
// accounts. It answers `auth list` with them as `gcloud auth list --format=json` lists them, the
// one CLOUDSDK_CORE_ACCOUNT names marked active, as gcloud marks its core/account property, which
// that variable sets - so none is active where it names one gcloud holds no login for - and the
// first one where it is unset. It answers `config list` with that account and the auth properties
// its environment sets, as gcloud shows environment overrides there, and `auth print-access-token
// … <account>` with StubTokenPrefix plus that account (the last word that is not a flag). It
// refuses any other command, and records every command line it runs and, for each run,
// GcloudVariables as its environment holds them. It also clears the variables a developer's own
// shell could set to steer the sign-in, GOOGLE_APPLICATION_CREDENTIALS, CLOUDSDK_CORE_ACCOUNT,
// CLOUDSDK_ACTIVE_CONFIG_NAME and CLOUDSDK_AUTH_ACCESS_TOKEN, and points HOME and CLOUDSDK_CONFIG
// at an empty directory, so no test reaches the developer's own gcloud or credentials.
func UseGcloud(t *testing.T, accounts ...string) GcloudStub {
	t.Helper()
	dir := t.TempDir()
	words := make([]string, len(accounts))
	for i, account := range accounts {
		words[i] = secretstore.ShellWord(account)
	}
	first := ""
	if len(accounts) > 0 {
		first = secretstore.ShellWord(accounts[0])
	}
	environments := secretstore.ShellWord(filepath.Join(dir, "environments"))
	script := strings.Join([]string{
		"#!/bin/sh",
		`printf '%s\n' "$*" >>` + secretstore.ShellWord(filepath.Join(dir, "commands")),
		`for name in ` + strings.Join(GcloudVariables, " ") + `; do eval "value=\${$name-unset}"; printf '%s=%s\n' "$name" "$value"; done >>` + environments,
		`printf '%s\n' -- >>` + environments,
		`case "$1 $2" in`,
		`"auth list") active=${CLOUDSDK_CORE_ACCOUNT:-` + first + `}; sep=; printf '['; for account in ` + strings.Join(words, " ") + `; do status=; [ "$account" = "$active" ] && status=ACTIVE; printf '%s{"account":"%s","status":"%s"}' "$sep" "$account" "$status"; sep=,; done; printf ']\n' ;;`,
		`"config list") printf '{"auth":{"impersonate_service_account":"%s","access_token_file":"%s","credential_file_override":"%s"},"core":{"account":"%s"}}\n' "$CLOUDSDK_AUTH_IMPERSONATE_SERVICE_ACCOUNT" "$CLOUDSDK_AUTH_ACCESS_TOKEN_FILE" "$CLOUDSDK_AUTH_CREDENTIAL_FILE_OVERRIDE" "${CLOUDSDK_CORE_ACCOUNT:-` + first + `}" ;;`,
		`"auth print-access-token") for word; do case "$word" in --*) ;; *) account=$word ;; esac; done; printf '` + StubTokenPrefix + `%s\n' "$account" ;;`,
		`*) printf 'stub gcloud: unexpected command: %s\n' "$*" >&2; exit 2 ;;`,
		"esac",
		"",
	}, "\n")
	if err := testexec.WriteFile(filepath.Join(dir, "gcloud"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "")
	t.Setenv("CLOUDSDK_CORE_ACCOUNT", "")
	t.Setenv("CLOUDSDK_AUTH_ACCESS_TOKEN", "")
	UseEmptyGcloudConfigDir(t)
	return GcloudStub{dir}
}

// Environments is, for each run of the stub in order, GcloudVariables as its environment held
// them, "unset" for one it did not hold.
func (g GcloudStub) Environments(t *testing.T) []map[string]string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(g.dir, "environments"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var runs []map[string]string
	run := map[string]string{}
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		if line == "--" {
			runs, run = append(runs, run), map[string]string{}
			continue
		}
		name, value, _ := strings.Cut(line, "=")
		run[name] = value
	}
	return runs
}

// UseNoGcloud leaves PATH holding no gcloud for the rest of t, and clears the variables UseGcloud
// clears.
func UseNoGcloud(t *testing.T) {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "")
	t.Setenv("CLOUDSDK_CORE_ACCOUNT", "")
	t.Setenv("CLOUDSDK_AUTH_ACCESS_TOKEN", "")
}

// Commands is every command line the stub ran, in order.
func (g GcloudStub) Commands(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(g.dir, "commands"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}

// Lent is each account the stub was asked for an access token of, in order: the last word of
// each `auth print-access-token` command line that is not a flag.
func (g GcloudStub) Lent(t *testing.T) []string {
	t.Helper()
	var accounts []string
	for _, command := range g.Commands(t) {
		words := strings.Fields(command)
		if len(words) <= 2 || words[0] != "auth" || words[1] != "print-access-token" {
			continue
		}
		words = slices.DeleteFunc(words, func(word string) bool { return strings.HasPrefix(word, "--") })
		accounts = append(accounts, words[len(words)-1])
	}
	return accounts
}

// Dir is the directory the stub gcloud is in, alone, which PATH names.
func (g GcloudStub) Dir() string { return g.dir }
