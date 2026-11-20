// packages/envoy/cmd/agent-secrets/secret_gcp.go
//
// The GCP side of the secret forms: the Secret Manager calls they make, under the Google credential
// of the first source this command finds present, in the order newGCPBackend gives (gcp_source.go):
// CLOUDSDK_AUTH_ACCESS_TOKEN, GOOGLE_APPLICATION_CREDENTIALS, CLOUDSDK_ACTIVE_CONFIG_NAME
// or CLOUDSDK_CORE_ACCOUNT, the well-known credentials file, the active gcloud account, the
// metadata server - whatever identity that is, a person's or a machine's: Secret Manager's own
// IAM decides what it may read and write. GCP, unlike AWS, needs no wrong-project read check:
// every call already names the project explicitly, so the API itself refuses one in another
// project.
//
// GCP Secret Manager has no soft-delete-with-recovery-window the way AWS does. Instead delete sets
// the secret's own expire_time, which GCP uses as a TTL: the secret (and every version it holds)
// stays readable and restorable - clear expire_time to bring it back - until that moment, and GCP
// deletes it for good once it passes, with no further restore. delete therefore sets expire_time
// to now plus recoveryWindowDays (the same window AWS's delete schedules), and restore clears it;
// the moment delete sets is both "when it is deleted" and "the last moment it can be restored",
// unlike AWS's separate DeletedDate and 7-day minimum recovery window, so secretView.EarliestPurge
// is left unset here (list/show print only the deletion moment).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	secretmanager "cloud.google.com/go/secretmanager/apiv1"
	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/secrets"
	"github.com/sjawhar/envoy/internal/googleidentity"
)

// checkSettingsKMSKeyName refuses, before any GCP call, settings whose secrets_prefix is not a
// Secret Manager prefix, whose gcp_project does not match the project number secrets_prefix
// itself names (the broker derives gcp_project from secrets_prefix the same way,
// internal/broker/api handlers_secrets.go, secrets.GCPProject), or whose kms_key_name is not a
// global-location CMEK key name (secrets.ValidGCPKeyName: automatic replication, the only
// replication the store serves, takes a global key). This is the GCP counterpart of
// checkSettingsKeyARN: it ties the project a write lands in to the resource-name prefix the
// broker itself derived it from, not to a separately-sent field a spoofed or plain-http broker
// could set on its own to steer a write.
func checkSettingsKMSKeyName(settings Settings) error {
	if err := secrets.ValidGCPPrefix(settings.SecretsPrefix); err != nil {
		return fmt.Errorf("the broker's settings name an invalid secrets_prefix: %w", err)
	}
	if project := secrets.GCPProject(settings.SecretsPrefix); settings.GCPProject != project {
		return fmt.Errorf("the broker's settings name project %s, but its secrets prefix %s is in project %s: the broker's settings are inconsistent", settings.GCPProject, settings.SecretsPrefix, project)
	}
	if err := secrets.ValidGCPKeyName(settings.KMSKeyName); err != nil {
		return fmt.Errorf("the broker's settings name an invalid KMS key name: %w", err)
	}
	return nil
}

// gcpCredentialsFile loads the credentials file at path, of whatever type it names, scoped for
// Secret Manager: the file GOOGLE_APPLICATION_CREDENTIALS names and the well-known credentials
// file. A file it cannot read or parse is an error. Tests replace it.
var gcpCredentialsFile = func(ctx context.Context, path string) (*google.Credentials, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var file struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("it is not a JSON credentials file: %w", err)
	}
	return google.CredentialsFromJSONWithType(ctx, data, google.CredentialsType(file.Type), secretmanager.DefaultAuthScopes()...)
}

// metadataGenericPrincipal is how a refusal and the "using" line name the metadata server's
// credential when its email read did not answer in time (gcpMetadataPrincipal): the identity is
// this machine's or this pod's, named no more exactly, so a slow or failed email read never
// keeps the command from signing in under the token.
const metadataGenericPrincipal = "this machine's or this pod's identity"

// gcpMetadataToken answers the machine's own credentials, whose access tokens come from the
// metadata server (googleidentity.AccessTokenSource), reading the first token at once so the
// source is available exactly when the server yields one. googleidentity.ErrNoMetadataServer - no
// connection within googleidentity.ProbeTimeout, no answer within the token read's bound, or an
// answer that is not Google's - is the absence newGCPBackend skips; any other error is a Google
// server that is present but could not mint a token, which stops the command. It never reads the
// email: that is gcpMetadataPrincipal's separate, best-effort job. Tests replace it.
var gcpMetadataToken = func(ctx context.Context) (*google.Credentials, error) {
	source := googleidentity.AccessTokenSource()
	first, err := source.Token()
	if err != nil {
		return nil, err
	}
	return &google.Credentials{TokenSource: oauth2.ReuseTokenSource(first, source)}, nil
}

// gcpMetadataPrincipal is the account the metadata server names for this machine
// (googleidentity.Probe, within ProbeTimeout), best effort: "" where the read fails or is slow,
// so the refusal falls back to metadataGenericPrincipal rather than making a flaky email read
// decide whether a command runs. Tests replace it.
var gcpMetadataPrincipal = func(ctx context.Context) string {
	email, err := googleidentity.Probe(ctx)
	if err != nil {
		return ""
	}
	return email
}

// gcpUserinfo reads the email of the account behind creds from Google's userinfo endpoint, which
// answers only a token that carries an email scope. Only --owner me asks for it
// (gcpBackend.personEmail). Tests replace it so no form makes a network call for it.
var gcpUserinfo = func(ctx context.Context, creds *google.Credentials) (string, error) {
	httpClient := oauth2.NewClient(ctx, creds.TokenSource)
	resp, err := httpClient.Get("https://www.googleapis.com/oauth2/v2/userinfo")
	if err != nil {
		return "", fmt.Errorf("read the Google sign-in's email: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("read the Google sign-in's email: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var userinfo struct {
		Email string `json:"email"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&userinfo); err != nil {
		return "", fmt.Errorf("decode the Google sign-in's email: %w", err)
	}
	return userinfo.Email, nil
}

// gcpClientDial builds the Secret Manager client for creds. Tests replace it to dial a local fake
// Secret Manager server instead of the real API. option.WithLogger discards the SDK's own debug
// log (GOOGLE_SDK_GO_LOGGING_LEVEL): at debug level the SDK logs every request as protojson,
// which for AddSecretVersion includes the secret's value, base64-encoded, in Payload.Data (the
// broker's own GCP store takes the same precaution, internal/broker/secrets/gcp.go).
var gcpClientDial = func(ctx context.Context, creds *google.Credentials) (*secretmanager.Client, error) {
	return secretmanager.NewClient(ctx, option.WithCredentials(creds), option.WithLogger(slog.New(slog.DiscardHandler)))
}

// gcloudTokenLife is how long this command takes an access token `gcloud auth print-access-token`
// prints to last, counted from the moment it starts gcloud: gcloud prints the token alone, never
// its expiry, and refreshes a token that would expire within 300 seconds before it prints one
// (_CREDENTIALS_EXPIRY_WINDOW in the Cloud SDK's googlecloudsdk/core/credentials/store.py), so
// every token it prints lasts at least this long. oauth2.ReuseTokenSource asks gcloud again once
// it has passed, and gcloud answers the token it holds until that one nears its own expiry.
const gcloudTokenLife = 5 * time.Minute

// gcloudCommand is the gcloud a source runs: the path of the first gcloud on PATH. gcloud reads
// the configuration and account to use from the environment this command passes it (gcloudEnv:
// CLOUDSDK_ACTIVE_CONFIG_NAME and CLOUDSDK_CORE_ACCOUNT, as the caller set them), never a flag of
// this command's.
type gcloudCommand struct {
	path string
}

// findGcloud is the first gcloud on PATH.
func findGcloud() (gcloudCommand, error) {
	path, err := exec.LookPath("gcloud")
	if err != nil {
		return gcloudCommand{}, errors.New("no gcloud is on PATH: install the Google Cloud CLI, then sign in once with `gcloud auth login`")
	}
	return gcloudCommand{path: path}, nil
}

// gcloudLogins is every login `gcloud auth list` names, and the one it marks active (its
// core/account property, under gcloud's configuration): the first of a gcloud source's three
// calls, which decides whether the source is present. It runs under storeCallDeadline, as the
// token's refresh does (gcloudToken.Token), so a gcloud that never answers stops the command at
// that deadline rather than holding it. A list gcloud cannot make is an error.
func gcloudLogins(ctx context.Context, gcloud gcloudCommand) (accounts []string, active string, err error) {
	ctx, cancel := withStoreDeadline(ctx)
	defer cancel()
	out, err := runGcloud(ctx, gcloud, "auth", "list", "--format=json")
	if err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return nil, "", fmt.Errorf("`gcloud auth list` did not answer within %s", storeCallDeadlineText())
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

// gcloudSettingsDeadline bounds readGcloudSettings. The read only names whom a login's token acts
// as, which R2 makes best effort, so a gcloud that has not answered by then leaves the principal
// unknown (unreadPrincipal) and the command goes on; each gcloud run starts a Python interpreter,
// which on a loaded machine takes seconds, so the bound leaves room for that.
const gcloudSettingsDeadline = 5 * time.Second

// readGcloudSettings runs `gcloud config list` under gcloudSettingsDeadline. Its error is never
// the command's (signInWithGcloud).
func readGcloudSettings(ctx context.Context, gcloud gcloudCommand) (gcloudSettings, error) {
	var settings gcloudSettings
	ctx, cancel := context.WithTimeout(ctx, gcloudSettingsDeadline)
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

// principalUnknown is the principal a refusal and the "using" line name where the credential
// does not say whose it is: a lent access token, an authorized_user credentials file, and a gcloud
// login whose token comes from an access token file or a credential file override. The CLI asks
// for no email scope to learn it.
const principalUnknown = "principal unknown"

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

// unreadPrincipal is who account's gcloud token acts as where readGcloudSettings failed or did not
// answer in time: the login is named, and whether gcloud's settings make its token act as another
// principal is unknown.
func unreadPrincipal(account string) string {
	return account + " (" + principalUnknown + ": gcloud's auth settings could not be read)"
}

// personAccount reports whether email, as Google's userinfo endpoint names an account, is a
// person's: a plain email the broker accepts as a person owner (policy.ValidPersonOwner,
// lowercased), and no service account's (one ending in .gserviceaccount.com, such as a machine's
// own). --owner me takes only a person's (gcpBackend.personEmail).
func personAccount(email string) bool {
	lower := strings.ToLower(email)
	return policy.ValidPersonOwner(lower) && !strings.HasSuffix(lower, ".gserviceaccount.com")
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
			return "", fmt.Errorf("CLOUDSDK_CORE_ACCOUNT names %s, and gcloud holds no login for it (it holds %s): sign in as %s with `gcloud auth login`, or name another login gcloud holds", named, held, named)
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
// No error carries the token: a failure names the account and what gcloud wrote on its standard
// error.
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
// storeCallDeadline: a refresh runs inside a store call, under that call's context only where
// oauth2 passes one, which it does not, so without its own bound a gcloud that never answered would
// hold the call past its deadline.
func (s gcloudToken) Token() (*oauth2.Token, error) {
	started := time.Now()
	ctx, cancel := withStoreDeadline(s.ctx)
	defer cancel()
	out, err := runGcloud(ctx, s.gcloud, gcloudTokenArgs(s.account)...)
	if err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return nil, fmt.Errorf("`gcloud auth print-access-token` did not answer within %s for %s", storeCallDeadlineText(), s.account)
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
// it on, and its token redaction off, a failed refresh prints its request body, the refresh token
// in it, on the standard error runGcloud quotes in its errors. A variable outranks the
// configuration file, and given twice takes its last value.
func gcloudEnv() []string {
	dropped := []string{"CLOUDSDK_AUTH_ACCESS_TOKEN"}
	env := slices.DeleteFunc(os.Environ(), func(variable string) bool {
		name, _, _ := strings.Cut(variable, "=")
		return slices.Contains(dropped, name)
	})
	return append(env, "CLOUDSDK_CORE_DISABLE_PROMPTS=1", "CLOUDSDK_CORE_DISABLE_FILE_LOGGING=1", "CLOUDSDK_CORE_LOG_HTTP=false")
}

// gcloudWaitDelay is how long runGcloud waits for a killed gcloud's output to close before it gives
// the call up: a second, the standard error kept and nothing else.
const gcloudWaitDelay = time.Second

// runGcloud runs gcloud with args in gcloudEnv and answers what it wrote on its standard output.
// gcloud reads the configuration and account to use from that environment
// (CLOUDSDK_ACTIVE_CONFIG_NAME and CLOUDSDK_CORE_ACCOUNT, as the caller set them), and the account
// to print a token for is named on the command line (gcloudTokenArgs), so no flag of this
// command's picks a configuration.
// gcloud reads /dev/null, never this command's standard input, which can be a secret's value on
// its way in; what it writes on its standard error is kept only to name a failure. Once ctx ends,
// gcloud is killed with its whole process group where the platform has them (killGroupOnCancel),
// not the gcloud process alone, since the gcloud on PATH can be a script whose child, gcloud's
// Python, holds the output open and outlives a kill of the script; the output is then waited for
// no more than gcloudWaitDelay, for a process that left the group.
// The whole group is killed too where a signal ends this command while gcloud runs: gcloud, in a
// group of its own, gets no signal sent to this command's group, such as a terminal's interrupt,
// so runGcloud records each gcloud it runs (runRecorded) and an end by a signal kills each
// recorded one first (exitBySignal, killRunningGcloud).
func runGcloud(ctx context.Context, gcloud gcloudCommand, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, gcloud.path, args...)
	cmd.Env = gcloudEnv()
	killGroupOnCancel(cmd)
	cmd.WaitDelay = gcloudWaitDelay
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := runRecorded(cmd); err != nil {
		if said := strings.TrimSpace(stderr.String()); said != "" {
			return nil, fmt.Errorf("%w: %s", err, said)
		}
		return nil, err
	}
	return stdout.Bytes(), nil
}

// runningGcloud holds the pid of each gcloud runRecorded has started and not yet waited for.
var runningGcloud = struct {
	sync.Mutex
	pids map[int]bool
}{pids: map[int]bool{}}

// formWatchesSignals counts the form watches of createSignals running now (watchCreateSignals),
// each of which ends the form by a signal through exitBySignal itself.
var formWatchesSignals atomic.Int32

// runRecorded runs cmd as cmd.Run does, its pid recorded in runningGcloud from its start until it
// has been waited for, so that an end by a signal (exitBySignal) kills it with its group. Where no
// form watches createSignals, it watches them itself for as long as cmd runs
// (watchSignalsWhileGcloudRuns). Once cmd has been waited for, on every outcome, it kills what is
// left of cmd's group (killGroupLeft): a gcloud that exits by itself can leave a child holding
// its output, which gcloudWaitDelay stops the wait for and does not kill. The group is gcloud's
// own, so nothing else is in it, and while a process is left in it its id names no other group.
func runRecorded(cmd *exec.Cmd) error {
	runningGcloud.Lock()
	if err := cmd.Start(); err != nil {
		runningGcloud.Unlock()
		return err
	}
	pid := cmd.Process.Pid
	runningGcloud.pids[pid] = true
	runningGcloud.Unlock()
	stop := watchSignalsWhileGcloudRuns()
	err := cmd.Wait()
	stop()
	killGroupLeft(pid)
	runningGcloud.Lock()
	delete(runningGcloud.pids, pid)
	runningGcloud.Unlock()
	return err
}

// killRunningGcloud kills each gcloud runRecorded is running, with its group (killGcloudGroup),
// for a command that is ending by a signal. It keeps runningGcloud locked, so no gcloud starts
// after it.
func killRunningGcloud() {
	runningGcloud.Lock()
	for pid := range runningGcloud.pids {
		killGcloudGroup(pid)
	}
}

// watchSignalsWhileGcloudRuns catches each of createSignals this process did not inherit as
// ignored, until stop is called, where no form watches them (formWatchesSignals), and ends the
// process by the first that comes (exitBySignal, which kills the running gcloud first), as the
// signal's default action would have, once the value prompt, where it reads, has finished
// (promptBusy). A signal that came before the stop still ends the process.
func watchSignalsWhileGcloudRuns() (stop func()) {
	if formWatchesSignals.Load() > 0 {
		return func() {}
	}
	var watched []os.Signal
	for _, sig := range createSignals {
		if !signal.Ignored(sig) {
			watched = append(watched, sig)
		}
	}
	if len(watched) == 0 {
		return func() {}
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, watched...)
	end := func(sig os.Signal) {
		promptBusy.Lock()
		exitBySignal(sig)
	}
	done := make(chan struct{})
	go func() {
		select {
		case sig := <-signals:
			end(sig)
		case <-done:
		}
	}()
	return func() {
		close(done)
		signal.Stop(signals)
		select {
		case sig := <-signals:
			end(sig)
		default:
		}
	}
}

// newGCPBackend signs every form, a read and a write alike, in with the credential of the first
// source present, in this order (gcpSource), and dials the Secret Manager client under it
// (gcpClientDial):
//
//  1. CLOUDSDK_AUTH_ACCESS_TOKEN, a lent access token;
//  2. GOOGLE_APPLICATION_CREDENTIALS, the credentials file it names;
//  3. CLOUDSDK_ACTIVE_CONFIG_NAME or CLOUDSDK_CORE_ACCOUNT, the gcloud configuration or login they
//     name;
//  4. the well-known credentials file (wellKnownCredentialsFile);
//  5. the active gcloud account, where gcloud is on PATH and holds a login;
//  6. the metadata server, where it yields a token (gcpMetadataToken).
//
// A source that is absent is skipped. One that is present but fails - a file it cannot parse, a
// refresh that fails, an account gcloud does not hold - stops the command with its error, and no
// later source is tried; with none present the error names each and why it had nothing. A
// gcloud source runs gcloud with gcloud's own settings for its configuration and account
// (gcloudEnv). It refuses no credential for the identity it is: Secret Manager's IAM decides what
// each may read and write, and a refusal of the store's names the credential (identity). The
// read of whom a credential acts as is best effort (R2): it makes no userinfo or tokeninfo call,
// since only --owner me needs an email (personEmail), and a read of the metadata server's email or
// of gcloud's auth settings that fails or is slow names the principal less exactly and never
// stops the command.
func newGCPBackend(ctx context.Context, settings Settings) (*gcpBackend, error) {
	b := &gcpBackend{settings: settings}
	if err := b.signIn(ctx); err != nil {
		return nil, err
	}
	client, err := gcpClientDial(ctx, b.creds)
	if err != nil {
		return nil, fmt.Errorf("create the Secret Manager client: %w", err)
	}
	b.client = client
	return b, nil
}

// signIn sets b's credential from the first source present (newGCPBackend).
func (b *gcpBackend) signIn(ctx context.Context) error {
	var absent []string
	skip := func(source gcpSource, why string) { absent = append(absent, source.name()+": "+why) }

	if token := os.Getenv("CLOUDSDK_AUTH_ACCESS_TOKEN"); token != "" {
		b.source, b.sourcePhrase, b.principal = sourceLentToken, "the access token in CLOUDSDK_AUTH_ACCESS_TOKEN", principalUnknown
		b.creds = &google.Credentials{TokenSource: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token, TokenType: "Bearer"})}
		return nil
	}
	skip(sourceLentToken, "unset")

	if path := os.Getenv("GOOGLE_APPLICATION_CREDENTIALS"); path != "" {
		creds, err := gcpCredentialsFile(ctx, path)
		if err != nil {
			return fmt.Errorf("load the credentials file GOOGLE_APPLICATION_CREDENTIALS names (%s): %w", path, err)
		}
		b.source, b.sourcePhrase, b.principal, b.creds = sourceCredentialsFile, fmt.Sprintf("the credentials file GOOGLE_APPLICATION_CREDENTIALS names (%s)", path), credentialsFilePrincipal(creds), creds
		return nil
	}
	skip(sourceCredentialsFile, "unset")

	if config, account := os.Getenv("CLOUDSDK_ACTIVE_CONFIG_NAME"), os.Getenv("CLOUDSDK_CORE_ACCOUNT"); config != "" || account != "" {
		phrase := fmt.Sprintf("the gcloud configuration %s (CLOUDSDK_ACTIVE_CONFIG_NAME)", config)
		if account != "" {
			phrase = "the gcloud account (CLOUDSDK_CORE_ACCOUNT)"
		}
		gcloud, err := findGcloud()
		if err != nil {
			return fmt.Errorf("%s: %w", phrase, err)
		}
		return b.signInWithGcloud(ctx, sourceChosenGcloud, phrase, gcloud, config, nil)
	}
	skip(sourceChosenGcloud, "both unset")

	if path := wellKnownCredentialsFile(); path == "" {
		skip(sourceWellKnownFile, "no gcloud configuration directory (CLOUDSDK_CONFIG and HOME are unset)")
	} else if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		skip(sourceWellKnownFile, "absent")
	} else {
		creds, err := gcpCredentialsFile(ctx, path)
		if err != nil {
			return fmt.Errorf("load the well-known credentials file %s: %w", path, err)
		}
		b.source, b.sourcePhrase, b.principal, b.creds = sourceWellKnownFile, "the well-known credentials file "+path, credentialsFilePrincipal(creds), creds
		return nil
	}

	if gcloud, err := findGcloud(); err != nil {
		skip(sourceActiveGcloud, "no gcloud on PATH")
	} else {
		accounts, active, err := gcloudLogins(ctx, gcloud)
		if err != nil {
			return fmt.Errorf("%s: %w", sourceActiveGcloud.name(), err)
		}
		if len(accounts) == 0 {
			skip(sourceActiveGcloud, "gcloud holds no login")
		} else {
			return b.signInWithGcloud(ctx, sourceActiveGcloud, fmt.Sprintf("the active gcloud account (gcloud configuration %s)", activeGcloudConfiguration()), gcloud, "", &gcloudLoginList{accounts, active})
		}
	}

	creds, err := gcpMetadataToken(ctx)
	switch {
	case errors.Is(err, googleidentity.ErrNoMetadataServer):
		skip(sourceMetadata, err.Error())
	case err != nil:
		return fmt.Errorf("ask the metadata server for this machine's access token: %w", err)
	default:
		principal := gcpMetadataPrincipal(ctx)
		if principal == "" {
			principal = metadataGenericPrincipal
		}
		b.source, b.sourcePhrase, b.principal, b.creds = sourceMetadata, "the metadata server", principal, creds
		return nil
	}
	return fmt.Errorf("found no Google credential: this command takes the first of these that is present, and none is: %s. Sign in with `gcloud auth login`, or set one of the variables named", strings.Join(absent, "; "))
}

// gcloudLoginList is what `gcloud auth list` answered, for a source that read it to learn
// whether gcloud holds any login at all.
type gcloudLoginList struct {
	accounts []string
	active   string
}

// signInWithGcloud sets b's credential from gcloud's login, as source names it in phrase, under
// the configuration the caller chose (chosen; "" for gcloud's own): it reads which logins gcloud
// holds (gcloudLogins, unless listed already holds that answer), the account a chosen
// configuration names (gcloudConfigurationAccount), gcloud's auth settings (readGcloudSettings),
// which login to use (chooseGcloudAccount), and asks for the first token at once, so a login
// gcloud cannot lend is refused here, before any call is made under it; gcloudToken then runs
// `gcloud auth print-access-token` again as each token nears its end, gcloud refreshing the login
// as it does for itself, through the interface gcloud offers for that, never its credential files.
// The settings only name whom the token acts as, so a read of them that fails or does not answer
// in time names the login with the principal unknown (unreadPrincipal) and stops nothing.
func (b *gcpBackend) signInWithGcloud(ctx context.Context, source gcpSource, phrase string, gcloud gcloudCommand, chosen string, listed *gcloudLoginList) error {
	if listed == nil {
		accounts, active, err := gcloudLogins(ctx, gcloud)
		if err != nil {
			return fmt.Errorf("%s: %w", phrase, err)
		}
		listed = &gcloudLoginList{accounts, active}
	}
	configured := ""
	if chosen != "" {
		var err error
		if configured, err = gcloudConfigurationAccount(chosen); err != nil {
			return fmt.Errorf("%s: %w", phrase, err)
		}
	}
	settings, settingsErr := readGcloudSettings(ctx, gcloud)
	account, err := chooseGcloudAccount(chosen, listed.accounts, listed.active, configured)
	if err != nil {
		return fmt.Errorf("%s: %w", phrase, err)
	}
	tokens := gcloudToken{ctx: ctx, gcloud: gcloud, account: account}
	first, err := tokens.Token()
	if err != nil {
		return fmt.Errorf("%s: %w", phrase, err)
	}
	principal := settings.principal(account)
	if settingsErr != nil {
		principal = unreadPrincipal(account)
	}
	b.source, b.sourcePhrase, b.principal, b.gcloudAccount = source, phrase, principal, account
	b.creds = &google.Credentials{TokenSource: oauth2.ReuseTokenSource(first, tokens)}
	return nil
}

// credentialsFilePrincipal is who a credentials file's credentials act as, by what the file says,
// never by a call: a key file's client_email, the service account an impersonating file names in
// its service_account_impersonation_url (an external_account workload identity federation
// credential, such as a dev box's, or an impersonated_service_account file), else
// principalUnknown (an authorized_user file, a federation credential that impersonates no one).
func credentialsFilePrincipal(creds *google.Credentials) string {
	var parsed struct {
		ClientEmail      string `json:"client_email"`
		ImpersonationURL string `json:"service_account_impersonation_url"`
	}
	if err := json.Unmarshal(creds.JSON, &parsed); err != nil {
		return principalUnknown
	}
	if parsed.ClientEmail != "" {
		return parsed.ClientEmail
	}
	if _, rest, ok := strings.Cut(parsed.ImpersonationURL, "/serviceAccounts/"); ok {
		if account, _, _ := strings.Cut(rest, ":"); account != "" {
			return account
		}
	}
	return principalUnknown
}

// gcpBackend is the secretBackend for store "gcp-secret-manager": GCP Secret Manager under the
// credential of the first Google credential source present (newGCPBackend), pinned to
// settings.GCPProject.
type gcpBackend struct {
	settings Settings
	client   *secretmanager.Client
	creds    *google.Credentials
	// source is the source creds came from, sourcePhrase names it, and principal is who creds act
	// as, or principalUnknown (identity).
	source       gcpSource
	sourcePhrase string
	principal    string
	// gcloudAccount is the gcloud login a gcloud source signed in with: a command the form prints
	// for running again carries it (gcloudPin), so it signs in as this one did.
	gcloudAccount string
}

// id is the Secret Manager resource name of the secret whose name under the prefix is slug.
func (b *gcpBackend) id(slug string) string { return b.settings.SecretsPrefix + slug }

func (b *gcpBackend) parent() string { return "projects/" + b.settings.GCPProject }

// secretID is id(slug) with the parent's "projects/<project>/secrets/" stripped, the bare id
// CreateSecretRequest.SecretId takes.
func (b *gcpBackend) secretID(slug string) string {
	return strings.TrimPrefix(b.id(slug), b.parent()+"/secrets/")
}

func (b *gcpBackend) displayName(id string) string { return displayName(b.settings.SecretsPrefix, id) }

// requireAccount is a no-op for GCP, unlike AWS's: every call this backend makes already names
// projects/<gcp_project>/... explicitly in the resource, so a wrong-project sign-in gets a real
// PermissionDenied or NotFound from the API itself rather than silently reading another
// project's secrets - AWS's ListSecrets takes no project parameter at all, which is the hazard
// requireAccount guards there. A sign-in's own project (its quota project, set from a credentials
// file's project_id, which a person's sign-in does not carry) is in any case not reliably
// comparable with the broker's gcp_project without a Resource Manager call this CLI does not
// make.
func (b *gcpBackend) requireAccount(context.Context) error { return nil }

// personEmail answers, for --owner me, the lowercased email of the person this sign-in is, as
// Google's userinfo endpoint (gcpUserinfo) names the account behind its token. It is the one
// userinfo call any form makes. It refuses a token userinfo does not answer, which is any token
// without an email scope (an access token minted for cloud-platform alone, as a lent token or a
// machine's commonly is), and an account that is no person's (personAccount): a service account's
// email names no person.
func (b *gcpBackend) personEmail(ctx context.Context) (string, error) {
	email, err := gcpUserinfo(ctx, b.creds)
	switch {
	case err != nil:
		return "", fmt.Errorf("Google's userinfo endpoint does not name the account behind %s, as it names none for a token without an email scope: %w", b.sourcePhrase, err)
	case email == "":
		return "", fmt.Errorf("Google's userinfo endpoint names no email for %s", b.sourcePhrase)
	case !personAccount(email):
		return "", fmt.Errorf("Google's userinfo endpoint names %s as %s, which is no person's email", b.sourcePhrase, email)
	}
	return strings.ToLower(email), nil
}

// identity names the credential this backend signs in with, as `<source>: <principal>`: the
// source newGCPBackend took it from, and who it acts as, or that the principal is unknown.
func (b *gcpBackend) identity() string { return b.sourcePhrase + ": " + b.principal }

// chooseAnother is how a refusal of the store's says to sign in with another credential, built
// from the source that signed in and from what outranks what: a lent token outranks every other
// source, so only unsetting it or lending another chooses another; a credentials file outranks the
// gcloud variables, so they choose only once it is unset; under every later source the gcloud
// variables and a credentials file choose. CLOUDSDK_CORE_ACCOUNT, where it is set, outranks the
// account of any configuration CLOUDSDK_ACTIVE_CONFIG_NAME names (chooseGcloudAccount), so a
// configuration chooses only once that variable is unset too, and the advice says so. Where a
// gcloud variable can choose, it lists the gcloud configurations here.
func (b *gcpBackend) chooseAnother() string {
	configurations := " (gcloud configurations here: " + gcloudConfigurationList() + ")"
	account := os.Getenv("CLOUDSDK_CORE_ACCOUNT") != ""
	switch {
	case b.source == sourceLentToken:
		return "choose another by unsetting CLOUDSDK_AUTH_ACCESS_TOKEN, which outranks every other source, or by lending another token in it"
	case b.source == sourceCredentialsFile && account:
		return "choose another by naming another credentials file in GOOGLE_APPLICATION_CREDENTIALS, or by unsetting it, which outranks every gcloud source, to choose an account in CLOUDSDK_CORE_ACCOUNT, which outranks a configuration's account, or, with CLOUDSDK_CORE_ACCOUNT unset too, a gcloud configuration in CLOUDSDK_ACTIVE_CONFIG_NAME" + configurations
	case b.source == sourceCredentialsFile:
		return "choose another by naming another credentials file in GOOGLE_APPLICATION_CREDENTIALS, or by unsetting it, which outranks every gcloud source, to choose a gcloud configuration in CLOUDSDK_ACTIVE_CONFIG_NAME or an account in CLOUDSDK_CORE_ACCOUNT" + configurations
	case account:
		return "choose another with an account in CLOUDSDK_CORE_ACCOUNT, which outranks a configuration's account, or a credentials file in GOOGLE_APPLICATION_CREDENTIALS, or by unsetting CLOUDSDK_CORE_ACCOUNT to choose a gcloud configuration in CLOUDSDK_ACTIVE_CONFIG_NAME" + configurations
	}
	return "choose another with a gcloud configuration in CLOUDSDK_ACTIVE_CONFIG_NAME, an account in CLOUDSDK_CORE_ACCOUNT, or a credentials file in GOOGLE_APPLICATION_CREDENTIALS" + configurations
}

// signInPrefix is the shell assignments a command line a form prints for running again
// (pipeCommand) needs to sign in as this one did, each shell-quoted, where a variable chose the
// source and may have been set for this one command: GOOGLE_APPLICATION_CREDENTIALS for the file
// it names, and CLOUDSDK_ACTIVE_CONFIG_NAME and CLOUDSDK_CORE_ACCOUNT, as set, for the gcloud login
// they chose; "" for every other source. It never writes CLOUDSDK_AUTH_ACCESS_TOKEN: a printed
// command carries no token.
func (b *gcpBackend) signInPrefix() string {
	prefix := ""
	switch b.source {
	case sourceCredentialsFile:
		prefix = "GOOGLE_APPLICATION_CREDENTIALS=" + shellWord(os.Getenv("GOOGLE_APPLICATION_CREDENTIALS")) + " "
	case sourceChosenGcloud:
		for _, name := range []string{"CLOUDSDK_ACTIVE_CONFIG_NAME", "CLOUDSDK_CORE_ACCOUNT"} {
			if value := os.Getenv(name); value != "" {
				prefix += name + "=" + shellWord(value) + " "
			}
		}
	}
	return prefix
}

// hasCurrentVersion reports whether id is served while versions/latest is ENABLED.
func (b *gcpBackend) hasCurrentVersion(ctx context.Context, id string) (bool, error) {
	version, err := b.client.GetSecretVersion(ctx, &secretmanagerpb.GetSecretVersionRequest{Name: id + "/versions/latest"})
	if status.Code(err) == codes.NotFound {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return version.GetState() == secretmanagerpb.SecretVersion_ENABLED, nil
}

// viewOf is secret as a secretView, less its versions, which only show reads.
func (b *gcpBackend) viewOf(ctx context.Context, secret *secretmanagerpb.Secret) (secretView, error) {
	hasValue, err := b.hasCurrentVersion(ctx, secret.GetName())
	if err != nil {
		return secretView{}, err
	}
	view := secretView{
		Name: b.displayName(secret.GetName()), SecretName: secret.GetName(),
		HasValue: hasValue,
	}.withTags(secret.GetAnnotations())
	if t := secret.GetCreateTime(); t != nil {
		created := t.AsTime()
		view.Created = &created
	}
	if t := secret.GetExpireTime(); t != nil {
		deleted := t.AsTime()
		view.Deleted = &deleted
	}
	return view, nil
}

func (b *gcpBackend) list(ctx context.Context) ([]secretView, error) {
	views := []secretView{}
	it := b.client.ListSecrets(ctx, &secretmanagerpb.ListSecretsRequest{Parent: b.parent()})
	for {
		secret, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("list the agent secrets: %w", err)
		}
		if !strings.HasPrefix(secret.GetName(), b.settings.SecretsPrefix) {
			continue // the parent held more than the namespace
		}
		view, err := b.viewOf(ctx, secret)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", secret.GetName(), err)
		}
		views = append(views, view)
	}
	return views, nil
}

// versionsOf maps each of id's version numbers to its one-element state, the GCP counterpart of
// AWS's version-to-stages map.
func (b *gcpBackend) versionsOf(ctx context.Context, id string) (map[string][]string, error) {
	versions := map[string][]string{}
	it := b.client.ListSecretVersions(ctx, &secretmanagerpb.ListSecretVersionsRequest{Parent: id})
	for {
		v, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return nil, err
		}
		number := v.GetName()[strings.LastIndex(v.GetName(), "/")+1:]
		versions[number] = []string{v.GetState().String()}
	}
	return versions, nil
}

func (b *gcpBackend) show(ctx context.Context, slug string) (secretView, error) {
	id := b.id(slug)
	secret, err := b.client.GetSecret(ctx, &secretmanagerpb.GetSecretRequest{Name: id})
	if err != nil {
		return secretView{}, err
	}
	view, err := b.viewOf(ctx, secret)
	if err != nil {
		return secretView{}, err
	}
	versions, err := b.versionsOf(ctx, id)
	if err != nil {
		return secretView{}, err
	}
	view.Versions = versions
	return view, nil
}

// set adds a version. Create, which says its own outcome, calls addVersion itself; set answers
// an error that is no definite refusal as outcomeUnknown (gcpOutcome).
func (b *gcpBackend) set(ctx context.Context, slug, value string) error {
	return gcpOutcome(b.addVersion(ctx, b.id(slug), value))
}

// gcpOutcome is a write call's error as the backend answers it: nil, a definite refusal
// (gcpDefiniteRefusal) as it is, and any other as outcomeUnknown, since UNAVAILABLE,
// DEADLINE_EXCEEDED, INTERNAL or a lost connection may follow a call the store carried out. The
// client makes one attempt of each write (their default call options retry nothing), so the code
// it answers is the one attempt's.
func gcpOutcome(err error) error {
	if err == nil || gcpDefiniteRefusal(err) {
		return err
	}
	return outcomeUnknown{err}
}

func (b *gcpBackend) addVersion(ctx context.Context, parent, value string) error {
	_, err := b.client.AddSecretVersion(ctx, &secretmanagerpb.AddSecretVersionRequest{
		Parent:  parent,
		Payload: &secretmanagerpb.SecretPayload{Data: []byte(value)},
	})
	return err
}

func (b *gcpBackend) currentTags(ctx context.Context, slug string) (heldTags, error) {
	secret, err := b.client.GetSecret(ctx, &secretmanagerpb.GetSecretRequest{Name: b.id(slug)})
	if err != nil {
		return heldTags{}, err
	}
	return heldTags{tags: secret.GetAnnotations(), etag: secret.GetEtag()}, nil
}

// readValue reads the secret's value with AccessSecretVersion on versions/latest, the version the
// broker serves, and refuses one that is empty or not valid UTF-8, as the broker's own read does
// (secrets.GCP.Read). Its error never holds the value's bytes.
func (b *gcpBackend) readValue(ctx context.Context, slug string) (string, error) {
	latest := b.id(slug) + "/versions/latest"
	out, err := b.client.AccessSecretVersion(ctx, &secretmanagerpb.AccessSecretVersionRequest{Name: latest})
	if err != nil {
		return "", err
	}
	data := out.GetPayload().GetData()
	if len(data) == 0 || !utf8.Valid(data) {
		return "", errors.New("its value is not a non-empty UTF-8 string")
	}
	return string(data), nil
}

// valueReadPermission is what AccessSecretVersion needs of a person's sign-in. The roles people
// hold on the broker's secrets commonly lack it: they add versions and change annotations, and
// read none.
func (b *gcpBackend) valueReadPermission() string {
	return "secretmanager.versions.access on the secret"
}

// tagReadPermission is what GetSecret needs of a person's sign-in: whoever stores a value reads
// that secret's annotations first, so a write grant that does not carry this read fails `secret
// set` before anything is written.
func (b *gcpBackend) tagReadPermission() string {
	return "secretmanager.secrets.get on the secret"
}

// setTags writes tags into the annotations held holds, and removeTags deletes keys from them, each
// in one UpdateSecret that keeps every annotation it does not name. An UpdateSecret whose mask names
// "annotations" replaces the whole map, so each sends held's annotations, changed, with held's
// etag: the write lands only on the annotations the retag checked, and a write another writer made
// since held was read refuses it with FAILED_PRECONDITION (errTagsChanged), naming the change,
// rather than being merged into annotations no one checked.
func (b *gcpBackend) setTags(ctx context.Context, slug string, held heldTags, tags map[string]string) error {
	return b.updateAnnotations(ctx, slug, held, func(annotations map[string]string) { maps.Copy(annotations, tags) })
}

func (b *gcpBackend) removeTags(ctx context.Context, slug string, held heldTags, keys []string) error {
	return b.updateAnnotations(ctx, slug, held, func(annotations map[string]string) {
		for _, key := range keys {
			delete(annotations, key)
		}
	})
}

// updateAnnotations sends held's annotations, as change leaves a copy of them, with held's etag.
// FAILED_PRECONDITION is the etag refusing it (errTagsChanged), and any other error is answered
// as every write's is (gcpOutcome).
func (b *gcpBackend) updateAnnotations(ctx context.Context, slug string, held heldTags, change func(map[string]string)) error {
	annotations := maps.Clone(held.tags)
	if annotations == nil {
		annotations = map[string]string{}
	}
	change(annotations)
	_, err := b.client.UpdateSecret(ctx, &secretmanagerpb.UpdateSecretRequest{
		Secret: &secretmanagerpb.Secret{
			Name:        b.id(slug),
			Annotations: annotations,
			Etag:        held.etag,
		},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"annotations"}},
	})
	if status.Code(err) == codes.FailedPrecondition {
		return fmt.Errorf("%w, so the store refused this write: %w", errTagsChanged, err)
	}
	return gcpOutcome(err)
}

func (b *gcpBackend) delete(ctx context.Context, slug string) (*time.Time, error) {
	expire := time.Now().UTC().Add(time.Duration(recoveryWindowDays) * 24 * time.Hour)
	_, err := b.client.UpdateSecret(ctx, &secretmanagerpb.UpdateSecretRequest{
		Secret: &secretmanagerpb.Secret{
			Name:       b.id(slug),
			Expiration: &secretmanagerpb.Secret_ExpireTime{ExpireTime: timestamppb.New(expire)},
		},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"expire_time"}},
	})
	if err != nil {
		return nil, gcpOutcome(err)
	}
	return &expire, nil
}

// restore clears expire_time: the field mask names it with no value in the body, which Secret
// Manager reads as unsetting it.
func (b *gcpBackend) restore(ctx context.Context, slug string) error {
	_, err := b.client.UpdateSecret(ctx, &secretmanagerpb.UpdateSecretRequest{
		Secret:     &secretmanagerpb.Secret{Name: b.id(slug)},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"expire_time"}},
	})
	return gcpOutcome(err)
}

func (b *gcpBackend) isAccessDenied(err error) bool {
	return status.Code(err) == codes.PermissionDenied
}

// close releases the Secret Manager client's underlying gRPC connection.
func (b *gcpBackend) close() error { return b.client.Close() }

var _ secretBackend = (*gcpBackend)(nil)
