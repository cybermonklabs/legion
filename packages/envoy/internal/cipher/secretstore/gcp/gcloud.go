// The GCP writer's gcloud: finding it on PATH, the logins and settings it reads, the account it
// chooses, and the tokens it lends, every run through the RunChild the CLI passes (runGcloud), so
// a call that reaches its deadline, or an interrupt, ends gcloud with its whole process group.

package gcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"golang.org/x/oauth2"

	"github.com/sjawhar/envoy/internal/cipher/secretstore"
)

// gcloudTokenLife is how long this command takes an access token `gcloud auth print-access-token`
// prints to last, counted from the moment it starts gcloud: gcloud prints the token alone, never
// its expiry, and refreshes a token that would expire within 300 seconds before it prints one
// (_CREDENTIALS_EXPIRY_WINDOW in the Cloud SDK's googlecloudsdk/core/credentials/store.py), so
// every token it prints lasts at least this long. oauth2.ReuseTokenSource asks gcloud again once
// it has passed, and gcloud answers the token it holds until that one nears its own expiry.
const gcloudTokenLife = 5 * time.Minute

// gcloudCommand is the gcloud a source runs: the path of the first gcloud on PATH, and run, the
// CLI's tracker of the children its forms start (secretstore.RunChild), which runs it. gcloud
// reads the configuration and account to use from the environment this command passes it
// (gcloudEnv: CLOUDSDK_ACTIVE_CONFIG_NAME and CLOUDSDK_CORE_ACCOUNT, as the caller set them), never
// a flag of this command's.
type gcloudCommand struct {
	path string
	run  secretstore.RunChild
}

// findGcloud is the first gcloud on PATH (Hooks.FindGcloud), run through run.
func findGcloud(run secretstore.RunChild) (gcloudCommand, error) {
	path, err := hooks.FindGcloud()
	if err != nil {
		return gcloudCommand{}, err
	}
	return gcloudCommand{path: path, run: run}, nil
}

// lookPathGcloud is the path of the first gcloud on PATH. It is Hooks.FindGcloud unless a test
// swaps it.
func lookPathGcloud() (string, error) {
	path, err := exec.LookPath("gcloud")
	if err != nil {
		return "", errors.New("no gcloud is on PATH: install the Google Cloud CLI, then sign in once with `gcloud auth login`")
	}
	return path, nil
}

// gcloudLogins is every login `gcloud auth list` names, and the one it marks active (its
// core/account property, under gcloud's configuration): the first of a gcloud source's three
// calls, which decides whether the source is present. It runs under secretstore.CallDeadline, as the
// token's refresh does (gcloudToken.Token), so a gcloud that never answers stops the command at
// that deadline rather than holding it. A list gcloud cannot make is an error.
func gcloudLogins(ctx context.Context, gcloud gcloudCommand) (accounts []string, active string, err error) {
	ctx, cancel := secretstore.WithCallDeadline(ctx)
	defer cancel()
	out, err := runGcloud(ctx, gcloud, "auth", "list", "--format=json")
	if err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return nil, "", fmt.Errorf("`gcloud auth list` did not answer within %s", secretstore.CallDeadlineText())
	}
	if err != nil {
		return nil, "", fmt.Errorf("`gcloud auth list` failed: %w", err)
	}
	var listed []struct {
		Account string `json:"account"`
		Status  string `json:"status"`
	}
	if err := json.Unmarshal(out, &listed); err != nil {
		return nil, "", fmt.Errorf("`gcloud auth list --format=json` answered something other than a list of accounts: %w", err)
	}
	for _, entry := range listed {
		accounts = append(accounts, entry.Account)
		if entry.Status == "ACTIVE" {
			active = entry.Account
		}
	}
	return accounts, active, nil
}

// gcloudSettings are the auth properties of gcloud's effective configuration that change whose
// token `gcloud auth print-access-token` prints - impersonation, an access token file and a
// credential file override - as `gcloud config list --format=json` shows them, environment
// overrides included: the second of a gcloud source's three calls. gcloud applies them itself, so
// they only name whom the token acts as (principal). The account a chosen configuration names is
// read from the configuration itself (gcloudConfigurationAccount), never from here, where gcloud
// on a Compute Engine VM fills a missing one in with the machine's.
type gcloudSettings struct {
	Auth struct {
		ImpersonateServiceAccount string `json:"impersonate_service_account"`
		AccessTokenFile           string `json:"access_token_file"`
		CredentialFileOverride    string `json:"credential_file_override"`
	} `json:"auth"`
}

// GcloudSettingsDeadline bounds readGcloudSettings. The read only names whom a login's token acts
// as, which R2 makes best effort, so a gcloud that has not answered by then leaves the principal
// unknown (unreadPrincipal) and the command goes on; each gcloud run starts a Python interpreter,
// which on a loaded machine takes seconds, so the bound leaves room for that. Tests lengthen it,
// as they shorten secretstore.CallDeadline, and the CLI's own tests time against it.
var GcloudSettingsDeadline = 5 * time.Second

// readGcloudSettings runs `gcloud config list` under GcloudSettingsDeadline. Its error is never
// the command's (signInWithGcloud).
func readGcloudSettings(ctx context.Context, gcloud gcloudCommand) (gcloudSettings, error) {
	var settings gcloudSettings
	ctx, cancel := context.WithTimeout(ctx, GcloudSettingsDeadline)
	defer cancel()
	out, err := runGcloud(ctx, gcloud, "config", "list", "--format=json")
	if err != nil {
		return settings, fmt.Errorf("`gcloud config list` failed: %w", err)
	}
	if err := json.Unmarshal(out, &settings); err != nil {
		return settings, fmt.Errorf("`gcloud config list --format=json` answered something other than gcloud's properties: %w", err)
	}
	return settings, nil
}

// principal is who account's gcloud token acts as, by settings: account, or account impersonating
// the service account gcloud is set to impersonate, or principalUnknown where an access token
// file or a credential file override replaces account's own token.
func (settings gcloudSettings) principal(account string) string {
	switch {
	case settings.Auth.AccessTokenFile != "" || settings.Auth.CredentialFileOverride != "":
		return principalUnknown
	case settings.Auth.ImpersonateServiceAccount != "":
		return account + " impersonating " + settings.Auth.ImpersonateServiceAccount
	}
	return account
}

// chooseGcloudAccount picks, of accounts, the login gcloud itself would use, whatever kind of
// account it is, as `gcloud auth list` names it (an email, or a principal:// identifier for a
// workload or workforce identity federation login): the one gcloud's own CLOUDSDK_CORE_ACCOUNT
// names; else, where the caller chose a configuration (chosen, by CLOUDSDK_ACTIVE_CONFIG_NAME),
// the account that configuration itself names (configured,
// gcloudConfigurationAccount); else the one gcloud marks active, else the only one. An account or
// a configuration the caller chose is the caller's choice, so where it names no login gcloud holds
// the command is refused, naming it and each login gcloud holds, rather than run as another login -
// a configuration that names no account at all included, for which gcloud on a Compute Engine VM
// would lend the machine's own login. It also refuses no login at all, and several of which none
// is active, naming each.
func chooseGcloudAccount(chosen string, accounts []string, active, configured string) (string, error) {
	held := "none"
	if len(accounts) > 0 {
		held = strings.Join(accounts, ", ")
	}
	if named := os.Getenv("CLOUDSDK_CORE_ACCOUNT"); named != "" {
		if !slices.Contains(accounts, named) {
			// The account is not printed: an account gcloud holds no login for is the caller's
			// text, and a variable meant for an account is where a pasted credential lands.
			return "", fmt.Errorf("CLOUDSDK_CORE_ACCOUNT names an account gcloud holds no login for (not printed; it holds %s): sign in as that account with `gcloud auth login`, or name another login gcloud holds", held)
		}
		return named, nil
	}
	if chosen != "" {
		switch {
		case configured == "":
			return "", fmt.Errorf("it names no account, so no login is chosen (gcloud holds logins for %s): sign in to it with `gcloud auth login --configuration=%s`, or name a login gcloud holds with `gcloud config set account ACCOUNT --configuration=%s`", held, chosen, chosen)
		case !slices.Contains(accounts, configured):
			return "", fmt.Errorf("it names %s, and gcloud holds no login for it (it holds %s): sign in to it with `gcloud auth login --configuration=%s`", configured, held, chosen)
		}
		return configured, nil
	}
	switch {
	case active != "":
		return active, nil
	case len(accounts) == 1:
		return accounts[0], nil
	case len(accounts) == 0:
		return "", errors.New("gcloud holds no login: sign in once with `gcloud auth login`")
	}
	return "", fmt.Errorf("gcloud holds several logins (%s) and none is active: run the command again with CLOUDSDK_CORE_ACCOUNT set to the one to use", strings.Join(accounts, ", "))
}

// gcloudToken is the oauth2.TokenSource of one gcloud login: each Token runs
// `gcloud auth print-access-token` for its account and takes the token to last gcloudTokenLife.
// No error carries the token: a failure names the account and gcloud's exit status, never what
// gcloud wrote on its standard error, which runGcloud discards.
type gcloudToken struct {
	ctx     context.Context
	gcloud  gcloudCommand
	account string
}

// gcloudTokenArgs is the `gcloud auth print-access-token` command line for account, less gcloud:
// the third of a gcloud source's three calls. gcloud prints a command's output through its logger,
// whose file handler keeps a copy in its log file (calliope/display.py's printer writes to
// log.out); the format's private attribute writes the token to the console stream alone
// (core/resource/resource_printer_base.py). gcloud's own impersonation, access token file and
// credential file override apply as its settings say, as they do for gcloud itself, and
// gcloudSettings.principal names what they make the token act as.
func gcloudTokenArgs(account string) []string {
	return []string{"auth", "print-access-token", "--format=value[private](token)", account}
}

// Token runs `gcloud auth print-access-token` for the account under a deadline of its own,
// secretstore.CallDeadline: a refresh runs inside a store call, under that call's context only where
// oauth2 passes one, which it does not, so without its own bound a gcloud that never answered would
// hold the call past its deadline.
func (s gcloudToken) Token() (*oauth2.Token, error) {
	started := time.Now()
	ctx, cancel := secretstore.WithCallDeadline(s.ctx)
	defer cancel()
	out, err := runGcloud(ctx, s.gcloud, gcloudTokenArgs(s.account)...)
	if err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return nil, fmt.Errorf("`gcloud auth print-access-token` did not answer within %s for %s", secretstore.CallDeadlineText(), s.account)
	}
	if err != nil {
		return nil, fmt.Errorf("`gcloud auth print-access-token` failed for %s: %w", s.account, err)
	}
	token := strings.TrimSpace(string(out))
	if token == "" || strings.ContainsAny(token, " \t\r\n") {
		return nil, fmt.Errorf("`gcloud auth print-access-token` answered something other than one access token for %s", s.account)
	}
	return &oauth2.Token{AccessToken: token, TokenType: "Bearer", Expiry: started.Add(gcloudTokenLife)}, nil
}

// gcloudEnv is the environment gcloud runs in: this command's, less CLOUDSDK_AUTH_ACCESS_TOKEN,
// which would make gcloud print that token rather than the login's; with prompts
// off, so gcloud fails rather than waits on one, file logging off (core/disable_file_logging:
// log.py's AddLogsDir adds no file handler), so no run leaves a log file behind, and HTTP logging
// off (core/log_http), whatever this command's environment or gcloud's configuration sets: with
// it on, and its token redaction off, a failed refresh writes its request body, the refresh token
// in it, on gcloud's standard error, which runGcloud discards but which a person who runs the same
// gcloud command to read its message sees. A variable outranks the configuration file, and given
// twice takes its last value.
func gcloudEnv() []string {
	dropped := []string{"CLOUDSDK_AUTH_ACCESS_TOKEN"}
	env := slices.DeleteFunc(os.Environ(), func(variable string) bool {
		name, _, _ := strings.Cut(variable, "=")
		return slices.Contains(dropped, name)
	})
	return append(env, "CLOUDSDK_CORE_DISABLE_PROMPTS=1", "CLOUDSDK_CORE_DISABLE_FILE_LOGGING=1", "CLOUDSDK_CORE_LOG_HTTP=false")
}

// runGcloud runs gcloud with args in gcloudEnv and answers what it wrote on its standard output.
// gcloud reads the configuration and account to use from that environment
// (CLOUDSDK_ACTIVE_CONFIG_NAME and CLOUDSDK_CORE_ACCOUNT, as the caller set them), and the account
// to print a token for is named on the command line (gcloudTokenArgs), so no flag of this
// command's picks a configuration.
// gcloud reads /dev/null, never this command's standard input, which can be a secret's value on
// its way in. What it writes on its standard error is discarded, never printed: a refresh the
// token endpoint refuses writes that endpoint's error_description there, which the server writes
// what it likes in, so a failure is its exit status and the advice to run the same gcloud command
// to read gcloud's own message. It runs through gcloud.run, the CLI's one tracker of the children
// the secret forms start: once ctx ends gcloud is killed with its whole process group, not the
// gcloud process alone, since the gcloud on PATH can be a script whose child, gcloud's Python,
// holds the output open; and an end by a signal kills it first.
func runGcloud(ctx context.Context, gcloud gcloudCommand, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, gcloud.path, args...)
	cmd.Env = gcloudEnv()
	var stdout bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, io.Discard
	if err := gcloud.run(cmd); err != nil {
		return nil, fmt.Errorf("%w: run the same gcloud command yourself to see its message", err)
	}
	return stdout.Bytes(), nil
}

// gcloudLoginList is what `gcloud auth list` answered, for a source that read it to learn
// whether gcloud holds any login at all.
type gcloudLoginList struct {
	accounts []string
	active   string
}
