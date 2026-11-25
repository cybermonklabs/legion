// The writer the agent-secrets CLI writes GCP Secret Manager with: the Secret Manager calls its
// secret forms make, under the Google credential of the first source the command finds present,
// in the order newWriter gives (source.go): CLOUDSDK_AUTH_ACCESS_TOKEN,
// GOOGLE_APPLICATION_CREDENTIALS, CLOUDSDK_ACTIVE_CONFIG_NAME or CLOUDSDK_CORE_ACCOUNT, the
// well-known credentials file, the active gcloud account, the metadata server - whatever identity
// that is, a person's or a machine's: Secret Manager's own IAM decides what it may read and write.
// GCP, unlike AWS, needs no wrong-project read check: every call already names the project
// explicitly, so the API itself refuses one in another project.
//
// GCP Secret Manager has no soft-delete-with-recovery-window the way AWS does. Instead Delete sets
// the secret's own expire_time, which GCP uses as a TTL: the secret (and every version it holds)
// stays readable and restorable - clear expire_time to bring it back - until that moment, and GCP
// deletes it for good once it passes, with no further restore. Delete therefore sets expire_time
// to now plus recoveryWindowDays (the same window AWS's delete schedules), and Restore clears it;
// the moment Delete sets is both "when it is deleted" and "the last moment it can be restored",
// unlike AWS's separate DeletedDate and 7-day minimum recovery window, so a listing's
// EarliestPurge is left unset here (list and show print only the deletion moment).

package gcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"cloud.google.com/go/auth/credentials"
	"cloud.google.com/go/auth/oauth2adapt"
	secretmanager "cloud.google.com/go/secretmanager/apiv1"
	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/secretstore"
	"github.com/sjawhar/envoy/internal/cipherclient"
	"github.com/sjawhar/envoy/internal/googleidentity"
	"github.com/sjawhar/envoy/internal/redact"
)

// recoveryWindowDays is the recovery window Delete schedules every deletion with, 30 days: the
// expire_time it sets is that far ahead, the window AWS's delete schedules too.
const recoveryWindowDays = 30

// writerSettings are the broker's settings the writer reads: the prefix, this store's fields of
// GET /v1/settings (SettingsFields), and how a field prints in a message (answer,
// secretstore.Settings.Words).
type writerSettings struct {
	SecretsPrefix, GCPProject, KMSKeyName string
	answer                                func(string) string
}

// settingsOf is s as the writer reads it, each field by its SettingsFields JSON name.
func settingsOf(s secretstore.Settings) writerSettings {
	return writerSettings{SecretsPrefix: s.Prefix, GCPProject: s.Fields["gcp_project"], KMSKeyName: s.Fields["kms_key_name"], answer: s.Words}
}

// words is text, a field the broker's settings named, as the broker's address lets its answer
// print; text itself where the settings say nothing of how.
func (s writerSettings) words(text string) string {
	if s.answer == nil {
		return text
	}
	return s.answer(text)
}

// checkSettingsKMSKeyName refuses, before any GCP call, settings whose secrets_prefix is not a
// Secret Manager prefix, whose gcp_project does not match the project number secrets_prefix
// itself names (the broker derives gcp_project from secrets_prefix the same way,
// internal/cipher/api handlers_secrets.go, Project), or whose kms_key_name is not a
// global-location CMEK key name (ValidKeyName: automatic replication, the only
// replication the store serves, takes a global key). This is the GCP counterpart of the AWS
// writer's checkSettingsKeyARN: it ties the project a write lands in to the resource-name prefix the
// broker itself derived it from, not to a separately-sent field a spoofed or plain-http broker
// could set on its own to steer a write. Each field a refusal names, and each validator's words,
// which quote the field, print as the broker's address lets its answer print (writerSettings.words).
func checkSettingsKMSKeyName(settings writerSettings) error {
	if err := ValidPrefix(settings.SecretsPrefix); err != nil {
		return fmt.Errorf("the broker's settings name an invalid secrets_prefix: %s", settings.words(err.Error()))
	}
	if project := Project(settings.SecretsPrefix); settings.GCPProject != project {
		return fmt.Errorf("the broker's settings name project %s, but its secrets prefix %s is in project %s: the broker's settings are inconsistent", settings.words(settings.GCPProject), settings.words(settings.SecretsPrefix), settings.words(project))
	}
	if err := ValidKeyName(settings.KMSKeyName); err != nil {
		return fmt.Errorf("the broker's settings name an invalid KMS key name: %s", settings.words(err.Error()))
	}
	return nil
}

// loadCredentialsFile loads the credentials file at path, of whatever type it names, scoped for
// Secret Manager: the file GOOGLE_APPLICATION_CREDENTIALS names and the well-known credentials
// file. The file is checked first, in the machine source's words, before any reader reads it
// (cipherclient.CredentialsFileFailure): one that repeats a key, or an X.509 certificate's whose
// files cannot be used, is refused before Google's library reads a configuration that would make
// it start an enterprise certificate proxy's signer. An external_account file is loaded by
// externalAccountCredentials. A file it cannot read is the os error, one the check refuses a
// fileRefusal, and one it cannot load errUnloadableCredentials, never a library's or
// encoding/json's error, which quote the file's fields (credentialsFileRefusal). It is
// Hooks.CredentialsFile unless a test swaps it.
func loadCredentialsFile(ctx context.Context, path string) (*google.Credentials, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if reason := cipherclient.CredentialsFileFailure(data); reason != "" {
		return nil, fileRefusal(reason)
	}
	var file struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(data, &file) != nil {
		return nil, errUnloadableCredentials
	}
	if file.Type == string(google.ExternalAccount) {
		return externalAccountCredentials(data)
	}
	creds, err := google.CredentialsFromJSONWithType(ctx, data, google.CredentialsType(file.Type), secretmanager.DefaultAuthScopes()...)
	if err != nil {
		return nil, errUnloadableCredentials
	}
	return creds, nil
}

// externalAccountCredentials loads an external_account credentials file (workload identity
// federation) that loadCredentialsFile checked, with cloud.google.com/go/auth, which reads every
// credential_source, an X.509 certificate's among them (golang.org/x/oauth2 reads none), as the
// x/oauth2 credentials the Secret Manager client and the userinfo read take. The file's own
// exchange and its impersonation URL mint the account's access token. The library logs nothing,
// whatever GOOGLE_SDK_GO_LOGGING_LEVEL says: its log of the exchange and the impersonation holds
// both tokens.
func externalAccountCredentials(data []byte) (*google.Credentials, error) {
	creds, err := credentials.NewCredentialsFromJSON(credentials.ExternalAccount, data, &credentials.DetectOptions{
		Scopes: secretmanager.DefaultAuthScopes(),
		Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		return nil, errUnloadableCredentials
	}
	return oauth2adapt.Oauth2CredentialsFromAuthCredentials(creds), nil
}

// fileRefusal is why a credentials file must not be loaded, in words that print no path and no part
// of it (cipherclient.CredentialsFileFailure).
type fileRefusal string

func (r fileRefusal) Error() string { return string(r) }

// errUnloadableCredentials is a credentials file loadCredentialsFile read but could not load: not
// JSON, or no credential Google's libraries make a token source of.
var errUnloadableCredentials = errors.New("holds no Google Cloud credential this command can load")

// credentialsFileRefusal is the refusal of the credentials file file names (a phrase, such as
// "the credentials file GOOGLE_APPLICATION_CREDENTIALS names") whose load failed with err
// (Hooks.CredentialsFile): fixed text for a file it could not load, the check's reason for one it
// refused, else the system's reason alone (redact.File), which drops the path. The file holds a
// credential, and a variable meant for its path is where a person who pasted the credential itself
// puts it.
func credentialsFileRefusal(file string, err error) error {
	var refusal fileRefusal
	switch {
	case errors.Is(err, errUnloadableCredentials):
		return fmt.Errorf("%s %w", file, errUnloadableCredentials)
	case errors.As(err, &refusal):
		return fmt.Errorf("%s: %w", file, refusal)
	}
	return fmt.Errorf("read %s: %w", file, redact.File(err))
}

// metadataGenericPrincipal is how a refusal and the "using" line name the metadata server's
// credential when its email read did not answer in time (Hooks.MetadataPrincipal): the identity is
// this machine's or this pod's, named no more exactly, so a slow or failed email read never
// keeps the command from signing in under the token.
const metadataGenericPrincipal = "this machine's or this pod's identity"

// metadataToken answers the machine's own credentials, whose access tokens come from the
// metadata server (googleidentity.AccessTokenSource), reading the first token at once so the
// source is available exactly when the server yields one. googleidentity.ErrNoMetadataServer - no
// connection within googleidentity.ProbeTimeout, no answer within the token read's bound, or an
// answer that is not Google's - is the absence newWriter skips; any other error is a Google
// server that is present but could not mint a token, which stops the command. It never reads the
// email: that is metadataPrincipal's separate, best-effort job. It is Hooks.MetadataToken unless
// a test swaps it.
func metadataToken(ctx context.Context) (*google.Credentials, error) {
	source := googleidentity.AccessTokenSource()
	first, err := source.Token()
	if err != nil {
		return nil, err
	}
	return &google.Credentials{TokenSource: oauth2.ReuseTokenSource(first, source)}, nil
}

// metadataPrincipal is the account the metadata server names for this machine
// (googleidentity.Probe, within ProbeTimeout), best effort: "" where the read fails or is slow,
// so the refusal falls back to metadataGenericPrincipal rather than making a flaky email read
// decide whether a command runs. It is Hooks.MetadataPrincipal unless a test swaps it.
func metadataPrincipal(ctx context.Context) string {
	email, err := googleidentity.Probe(ctx)
	if err != nil {
		return ""
	}
	return email
}

// UserinfoURL is Google's userinfo endpoint, which userinfo reads.
const UserinfoURL = "https://www.googleapis.com/oauth2/v2/userinfo"

// userinfo reads the email of the account behind creds from Google's userinfo endpoint, which
// answers only a token that carries an email scope. Only --owner me asks for it
// (writer.PersonEmail). A failure names the endpoint and its status and registered code
// (redact.Answered), or the failure's class (redact.Class), and never Google's body or x/oauth2's
// words: a Google error body has carried a token, and x/oauth2 keeps the token endpoint's body in
// the error a refused refresh answers. It is Hooks.Userinfo unless a test swaps it, so no form's
// test makes a network call for it.
func userinfo(ctx context.Context, creds *google.Credentials) (string, error) {
	httpClient := oauth2.NewClient(ctx, creds.TokenSource)
	resp, err := httpClient.Get(UserinfoURL)
	if err != nil {
		return "", fmt.Errorf("read the Google sign-in's email from %s: %w", UserinfoURL, redact.Class(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("read the Google sign-in's email from %s: %s", UserinfoURL, redact.Answered(resp.StatusCode, body))
	}
	var userinfo struct {
		Email string `json:"email"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&userinfo); err != nil {
		return "", fmt.Errorf("decode the Google sign-in's email: %w", err)
	}
	return userinfo.Email, nil
}

// dialClient builds the Secret Manager client for creds, every call it makes through
// interceptor (storeInterceptor, which words each failure). A test swaps it (Hooks.ClientDial) to
// dial a local fake Secret Manager server instead of the real API, and a replacement installs
// interceptor too: a client it builds on a connection it dialled itself ignores the dial options
// given here. option.WithLogger discards the SDK's own debug log (GOOGLE_SDK_GO_LOGGING_LEVEL): at
// debug level the SDK logs every request as protojson, which for AddSecretVersion includes the
// secret's value, base64-encoded, in Payload.Data (the broker's own GCP store takes the same
// precaution, store.go).
func dialClient(ctx context.Context, creds *google.Credentials, interceptor grpc.UnaryClientInterceptor) (*secretmanager.Client, error) {
	return secretmanager.NewClient(ctx, option.WithCredentials(creds), option.WithLogger(slog.New(slog.DiscardHandler)),
		option.WithGRPCDialOption(grpc.WithChainUnaryInterceptor(interceptor)))
}

// principalUnknown is the principal a refusal and the "using" line name where the credential
// does not say whose it is: a lent access token, an authorized_user credentials file, and a gcloud
// login whose token comes from an access token file or a credential file override. The CLI asks
// for no email scope to learn it.
const principalUnknown = "principal unknown"

// unreadPrincipal is who account's gcloud token acts as where readGcloudSettings failed or did not
// answer in time: the login is named, and whether gcloud's settings make its token act as another
// principal is unknown.
func unreadPrincipal(account string) string {
	return account + " (" + principalUnknown + ": gcloud's auth settings could not be read)"
}

// personAccount reports whether email, as Google's userinfo endpoint names an account, is a
// person's: a plain email the broker accepts as a person owner (policy.ValidPersonOwner,
// lowercased), and no service account's (one ending in .gserviceaccount.com, such as a machine's
// own). --owner me takes only a person's (gcpBackend.PersonEmail).
func personAccount(email string) bool {
	lower := strings.ToLower(email)
	return policy.ValidPersonOwner(lower) && !strings.HasSuffix(lower, ".gserviceaccount.com")
}

// newWriter signs every form, a read and a write alike, in with the credential of the first
// source present, in this order (gcpSource), and dials the Secret Manager client under it
// (Hooks.ClientDial); a gcloud source runs gcloud through run:
//
//  1. CLOUDSDK_AUTH_ACCESS_TOKEN, a lent access token;
//  2. GOOGLE_APPLICATION_CREDENTIALS, the credentials file it names;
//  3. CLOUDSDK_ACTIVE_CONFIG_NAME or CLOUDSDK_CORE_ACCOUNT, the gcloud configuration or login they
//     name;
//  4. the well-known credentials file (wellKnownCredentialsFile);
//  5. the active gcloud account, where gcloud is on PATH and holds a login;
//  6. the metadata server, where it yields a token (Hooks.MetadataToken).
//
// A source that is absent is skipped. One that is present but fails - a file it cannot parse, a
// refresh that fails, an account gcloud does not hold - stops the command with its error, and no
// later source is tried; with none present the error names each and why it had nothing. A
// gcloud source runs gcloud with gcloud's own settings for its configuration and account
// (gcloudEnv). It refuses no credential for the identity it is: Secret Manager's IAM decides what
// each may read and write, and a refusal of the store's names the credential (Identity). The
// read of whom a credential acts as is best effort (R2): it makes no userinfo or tokeninfo call,
// since only --owner me needs an email (PersonEmail), and a read of the metadata server's email or
// of gcloud's auth settings that fails or is slow names the principal less exactly and never
// stops the command. Its list, describe and value reads are the broker's own Store's over the same
// client, whose failures storeInterceptor words (WithClientErrors).
func newWriter(ctx context.Context, settings writerSettings, run secretstore.RunChild) (*writer, error) {
	b := &writer{settings: settings, run: run}
	if err := b.signIn(ctx); err != nil {
		return nil, err
	}
	client, err := hooks.ClientDial(ctx, b.storeCredentials(), b.storeInterceptor)
	if err != nil {
		return nil, fmt.Errorf("create the Secret Manager client: %w", err)
	}
	b.client = client
	b.Store = New(client, settings.KMSKeyName).WithClientErrors()
	return b, nil
}

// storeCredentials is the credential the Secret Manager client signs its calls with: b.creds's,
// through storeTokens (b.tokens), which keeps the token source's last failure for storeFailure,
// and for a credentials file through redactedTokens too, so its token source - Google's
// libraries' own - fails as the failure's class alone. Those libraries keep the body of the token
// endpoint's answer in the error a refused refresh answers (x/oauth2's RetrieveError, and
// cloud.google.com/go/auth's Error, into which oauth2adapt turns any error that wraps one), and
// gRPC prints that error into the call's status message with %v, so by the time a form prints the
// store's error the body is text no redaction can tell apart; the status code stays gRPC's. The
// other sources' token sources fail in this program's own words already: gcloudToken's,
// googleidentity's, and a lent token that never refreshes. b.creds itself is unchanged, so
// gcpUserinfo still classifies a refused refresh by its RetrieveError.
func (b *writer) storeCredentials() *google.Credentials {
	tokens := b.creds.TokenSource
	if b.source == sourceCredentialsFile || b.source == sourceWellKnownFile {
		tokens = redactedTokens{tokens}
	}
	b.tokens = &storeTokens{TokenSource: tokens}
	return &google.Credentials{
		ProjectID: b.creds.ProjectID, TokenSource: b.tokens, JSON: b.creds.JSON,
		UniverseDomainProvider: b.creds.GetUniverseDomain,
	}
}

// redactedTokens is a token source whose failure is its class alone (redact.Class), as text that
// wraps nothing: an error that still wrapped the *oauth2.RetrieveError would be turned back into
// one that prints the token endpoint's body (storeCredentials).
type redactedTokens struct{ oauth2.TokenSource }

func (s redactedTokens) Token() (*oauth2.Token, error) {
	token, err := s.TokenSource.Token()
	if err != nil {
		return nil, errors.New("fetch an access token with the credentials file: " + redact.Class(err).Error())
	}
	return token, nil
}

// signIn sets b's credential from the first source present (newGCPBackend).
func (b *writer) signIn(ctx context.Context) error {
	var absent []string
	skip := func(source gcpSource, why string) { absent = append(absent, source.name()+": "+why) }

	if token := os.Getenv("CLOUDSDK_AUTH_ACCESS_TOKEN"); token != "" {
		b.source, b.sourcePhrase, b.principal = sourceLentToken, "the access token in CLOUDSDK_AUTH_ACCESS_TOKEN", principalUnknown
		b.creds = &google.Credentials{TokenSource: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token, TokenType: "Bearer"})}
		return nil
	}
	skip(sourceLentToken, "unset")

	// The file holds a credential, so neither its refusal nor the phrase that names it in the
	// "using" line and a store's refusal spells its path (credentialsFileRefusal). A command printed
	// for running again does carry the path, as an absolute one (RerunPrefix): it is the file this
	// sign-in read and signed in with, so it is no credential a person pasted.
	if path := os.Getenv("GOOGLE_APPLICATION_CREDENTIALS"); path != "" {
		const file = "the credentials file GOOGLE_APPLICATION_CREDENTIALS names"
		creds, err := hooks.CredentialsFile(ctx, path)
		if err != nil {
			return credentialsFileRefusal(file, err)
		}
		if abs, err := filepath.Abs(path); err == nil {
			path = abs
		}
		b.source, b.sourcePhrase, b.principal, b.creds, b.credentialsFile = sourceCredentialsFile, file, credentialsFilePrincipal(creds), creds, path
		return nil
	}
	skip(sourceCredentialsFile, "unset")

	if config, account := os.Getenv("CLOUDSDK_ACTIVE_CONFIG_NAME"), os.Getenv("CLOUDSDK_CORE_ACCOUNT"); config != "" || account != "" {
		// A configuration this machine does not hold is refused before anything reads or prints
		// it: its name is the caller's text, and a variable meant for a name is where a pasted
		// credential lands. A configuration it holds is a label, and prints as one.
		if config != "" && !slices.Contains(gcloudConfigurations(), config) {
			return fmt.Errorf("CLOUDSDK_ACTIVE_CONFIG_NAME names a gcloud configuration this machine does not hold (not printed; gcloud configurations here: %s)", gcloudConfigurationList())
		}
		phrase := fmt.Sprintf("the gcloud configuration %s (CLOUDSDK_ACTIVE_CONFIG_NAME)", config)
		if account != "" {
			phrase = "the gcloud account (CLOUDSDK_CORE_ACCOUNT)"
		}
		gcloud, err := findGcloud(b.run)
		if err != nil {
			return fmt.Errorf("%s: %w", phrase, err)
		}
		return b.signInWithGcloud(ctx, sourceChosenGcloud, phrase, gcloud, config, nil)
	}
	skip(sourceChosenGcloud, "both unset")

	if path := WellKnownCredentialsFile(); path == "" {
		skip(sourceWellKnownFile, "no gcloud configuration directory (CLOUDSDK_CONFIG and HOME are unset)")
	} else if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		skip(sourceWellKnownFile, "absent")
	} else {
		creds, err := hooks.CredentialsFile(ctx, path)
		if err != nil {
			return credentialsFileRefusal(wellKnownCredentialsName(), err)
		}
		b.source, b.sourcePhrase, b.principal, b.creds = sourceWellKnownFile, wellKnownCredentialsName(), credentialsFilePrincipal(creds), creds
		return nil
	}

	if gcloud, err := findGcloud(b.run); err != nil {
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

	creds, err := hooks.MetadataToken(ctx)
	switch {
	case errors.Is(err, googleidentity.ErrNoMetadataServer):
		skip(sourceMetadata, err.Error())
	case err != nil:
		return fmt.Errorf("ask the metadata server for this machine's access token: %w", err)
	default:
		// The metadata server's answer prints only where it is a service account's: it is any
		// text whoever answers at GCE_METADATA_HOST writes, line breaks included.
		principal := hooks.MetadataPrincipal(ctx)
		if !cipherclient.ServiceAccountShaped(principal) {
			principal = metadataGenericPrincipal
		}
		b.source, b.sourcePhrase, b.principal, b.creds = sourceMetadata, "the metadata server", principal, creds
		return nil
	}
	return fmt.Errorf("found no Google credential: this command takes the first of these that is present, and none is: %s. Sign in with `gcloud auth login`, or set one of the variables named", strings.Join(absent, "; "))
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
func (b *writer) signInWithGcloud(ctx context.Context, source gcpSource, phrase string, gcloud gcloudCommand, chosen string, listed *gcloudLoginList) error {
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
// Either names an account as cipherclient.ServiceAccountWords does, as the token client's
// external_account identity does: whole only where it has a service account's shape, since the
// file is a credential's place and a field of it that is no service account's prints as fixed
// words.
func credentialsFilePrincipal(creds *google.Credentials) string {
	var parsed struct {
		ClientEmail      string `json:"client_email"`
		ImpersonationURL string `json:"service_account_impersonation_url"`
	}
	if err := json.Unmarshal(creds.JSON, &parsed); err != nil {
		return principalUnknown
	}
	account := parsed.ClientEmail
	if _, rest, ok := strings.Cut(parsed.ImpersonationURL, "/serviceAccounts/"); account == "" && ok {
		account, _, _ = strings.Cut(rest, ":")
	}
	if account == "" {
		return principalUnknown
	}
	return cipherclient.ServiceAccountWords(account, "the credentials file")
}

// gcpBackend is the secretstore.Writer for store "gcp-secret-manager": GCP Secret Manager under
// the credential of the first Google credential source present (newGCPBackend), pinned to
// settings.GCPProject. It implements no secretstore.AccountGuard: every call it makes names
// projects/<gcp_project>/... in the resource, so a wrong-project sign-in gets a real
// PermissionDenied or NotFound from the API itself rather than reading another project's
// secrets, where AWS's ListSecrets takes no account at all. A sign-in's own project (its quota
// project, set from a credentials file's project_id, which a person's sign-in does not carry) is
// in any case not reliably comparable with the broker's gcp_project without a Resource Manager
// call this CLI does not make. It implements no tagValueChecker either: Secret Manager holds tags
// as annotations, whose values take any character and no per-value limit, only one of under
// 16 KiB for every key and value of the secret together
// (https://cloud.google.com/secret-manager/docs/reference/rest/v1/projects.secrets, field
// annotations); the longest tag this client writes is guidance, at most policy.MaxGuidanceRunes
// characters (8000 bytes of UTF-8), which the tag rules hold it to, so the tags fit.
type writer struct {
	Store
	settings writerSettings
	// run is the CLI's tracker of the children its forms start, which runs every gcloud.
	run    secretstore.RunChild
	client *secretmanager.Client
	creds  *google.Credentials
	// source is the source creds came from, sourcePhrase names it, and principal is who creds act
	// as, or principalUnknown (Identity).
	source       gcpSource
	sourcePhrase string
	principal    string
	// gcloudAccount is the gcloud login a gcloud source signed in with: a command the form prints
	// for running again carries it (gcloudPin), so it signs in as this one did.
	gcloudAccount string
	// credentialsFile is the absolute path of the file GOOGLE_APPLICATION_CREDENTIALS named, which
	// a credentials file source read and signed in with: a command the form prints for running
	// again carries it (RerunPrefix, gcloudPin).
	credentialsFile string
	// tokens is the token source the Secret Manager client signs with (storeCredentials).
	tokens *storeTokens
}

// id is the Secret Manager resource name of the secret whose name under the prefix is slug.
func (b *writer) id(slug string) string { return b.settings.SecretsPrefix + slug }

func (b *writer) parent() string { return "projects/" + b.settings.GCPProject }

// secretID is id(slug) with the parent's "projects/<project>/secrets/" stripped, the bare id
// CreateSecretRequest.SecretId takes.
func (b *writer) secretID(slug string) string {
	return strings.TrimPrefix(b.id(slug), b.parent()+"/secrets/")
}

func (b *writer) displayName(id string) string {
	return policy.DisplayName(b.settings.SecretsPrefix, id)
}

// RequireAccount is a no-op for GCP, unlike AWS's: every call this backend makes already names
// projects/<gcp_project>/... explicitly in the resource, so a wrong-project sign-in gets a real
// PermissionDenied or NotFound from the API itself rather than silently reading another
// project's secrets - AWS's ListSecrets takes no project parameter at all, which is the hazard
// RequireAccount guards there. A sign-in's own project (its quota project, set from a credentials
// file's project_id, which a person's sign-in does not carry) is in any case not reliably
// comparable with the broker's gcp_project without a Resource Manager call this CLI does not
// make.
func (b *writer) RequireAccount(context.Context) error { return nil }

// CheckTagValues refuses nothing: Secret Manager holds tags as annotations, whose values take any
// character and no per-value limit, only one of under 16 KiB for every key and value of the secret
// together (https://cloud.google.com/secret-manager/docs/reference/rest/v1/projects.secrets, field
// annotations), which the API holds an update to itself.
func (b *writer) CheckTagValues(map[string]string) error { return nil }

// PersonEmail answers, for --owner me, the lowercased email of the person this sign-in is, as
// Google's userinfo endpoint (gcpUserinfo) names the account behind its token. It is the one
// userinfo call any form makes. It refuses a token userinfo does not answer, which is any token
// without an email scope (an access token minted for cloud-platform alone, as a lent token or a
// machine's commonly is), and an account that is no person's (personAccount): a service account's
// email names no person.
func (b *writer) PersonEmail(ctx context.Context) (string, error) {
	email, err := hooks.Userinfo(ctx, b.creds)
	switch {
	case err != nil:
		return "", fmt.Errorf("Google's userinfo endpoint does not name the account behind %s, as it names none for a token without an email scope: %w", b.sourcePhrase, err)
	case email == "":
		return "", fmt.Errorf("Google's userinfo endpoint names no email for %s", b.sourcePhrase)
	case !personAccount(email) && cipherclient.ServiceAccountShaped(email):
		return "", fmt.Errorf("Google's userinfo endpoint names %s as the service account %s, which is no person's email", b.sourcePhrase, email)
	case !personAccount(email):
		return "", fmt.Errorf("Google's userinfo endpoint names %s as an account that is no person's email (not printed: it has neither a person's email's shape nor a service account's)", b.sourcePhrase)
	}
	return strings.ToLower(email), nil
}

// Identity names the credential this backend signs in with, as `<source>: <principal>`: the
// source newGCPBackend took it from, and who it acts as, or that the principal is unknown.
func (b *writer) Identity() string { return b.sourcePhrase + ": " + b.principal }

// ChooseAnother is how a refusal of the store's says to sign in with another credential, built
// from the source that signed in and from what outranks what: a lent token outranks every other
// source, so only unsetting it or lending another chooses another; a credentials file outranks the
// gcloud variables, so they choose only once it is unset; under every later source the gcloud
// variables and a credentials file choose. CLOUDSDK_CORE_ACCOUNT, where it is set, outranks the
// account of any configuration CLOUDSDK_ACTIVE_CONFIG_NAME names (chooseGcloudAccount), so a
// configuration chooses only once that variable is unset too, and the advice says so. Where a
// gcloud variable can choose, it lists the gcloud configurations here.
func (b *writer) ChooseAnother() string {
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

// RerunPrefix is the shell assignments a command line a form prints for running again
// (secretstore.Command) needs to sign in as this one did, each shell-quoted, where a variable chose the
// source and may have been set for this one command: GOOGLE_APPLICATION_CREDENTIALS for the file
// it named, as the absolute path of the file this sign-in read (credentialsFile), so the command
// signs in with that same file in any shell and from any directory, and CLOUDSDK_ACTIVE_CONFIG_NAME
// and CLOUDSDK_CORE_ACCOUNT, as set, for the gcloud login they chose; "" for every other source.
// It never writes CLOUDSDK_AUTH_ACCESS_TOKEN: a printed command carries no token.
func (b *writer) RerunPrefix() string {
	prefix := ""
	switch b.source {
	case sourceCredentialsFile:
		prefix = "GOOGLE_APPLICATION_CREDENTIALS=" + secretstore.ShellWord(b.credentialsFile) + " "
	case sourceChosenGcloud:
		for _, name := range []string{"CLOUDSDK_ACTIVE_CONFIG_NAME", "CLOUDSDK_CORE_ACCOUNT"} {
			if value := os.Getenv(name); value != "" {
				prefix += name + "=" + secretstore.ShellWord(value) + " "
			}
		}
	}
	return prefix
}

// versionsOf maps each of id's version numbers to its one-element state, the GCP counterpart of
// AWS's version-to-stages map.
func (b *writer) versionsOf(ctx context.Context, id string) (map[string][]string, error) {
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

// Set adds value as the secret's new latest version.
func (b *writer) Set(ctx context.Context, id, value string) error {
	return b.addVersion(ctx, id, value)
}

func (b *writer) addVersion(ctx context.Context, parent, value string) error {
	_, err := b.client.AddSecretVersion(ctx, &secretmanagerpb.AddSecretVersionRequest{
		Parent:  parent,
		Payload: &secretmanagerpb.SecretPayload{Data: []byte(value)},
	})
	return err
}

// CurrentTags reads the secret's annotations with GetSecret, that read's etag, which a tag write
// sends, and its resource name, the source Read takes.
func (b *writer) CurrentTags(ctx context.Context, id string) (secretstore.HeldTags, error) {
	secret, err := b.client.GetSecret(ctx, &secretmanagerpb.GetSecretRequest{Name: id})
	if err != nil {
		return secretstore.HeldTags{}, err
	}
	return secretstore.HeldTags{Tags: secret.GetAnnotations(), Etag: secret.GetEtag(), Source: secret.GetName()}, nil
}

// ValueReadPermission is what AccessSecretVersion needs of a person's sign-in. The roles people
// hold on the broker's secrets commonly lack it: they add versions and change annotations, and
// read none.
func (b *writer) ValueReadPermission() string {
	return "secretmanager.versions.access on the secret"
}

// TagReadPermission is what GetSecret needs of a person's sign-in: whoever stores a value reads
// that secret's annotations first, so a write grant that does not carry this read fails `secret
// set` before anything is written.
func (b *writer) TagReadPermission() string {
	return "secretmanager.secrets.get on the secret"
}

// SetTags writes tags into the annotations held holds, and RemoveTags deletes keys from them, each
// in one UpdateSecret that keeps every annotation it does not name. An UpdateSecret whose mask names
// "annotations" replaces the whole map, so each sends held's annotations, changed, with held's
// etag: the write lands only on the annotations the retag checked, and a write another writer made
// since held was read refuses it with FAILED_PRECONDITION (secretstore.ErrTagsChanged), naming the
// change, rather than being merged into annotations no one checked. The etag is the condition, so
// check is not read.
func (b *writer) SetTags(ctx context.Context, id string, held secretstore.HeldTags, tags map[string]string, _ secretstore.TagCheck) error {
	return b.updateAnnotations(ctx, id, held, func(annotations map[string]string) { maps.Copy(annotations, tags) })
}

func (b *writer) RemoveTags(ctx context.Context, id string, held secretstore.HeldTags, keys []string, _ secretstore.TagCheck) error {
	return b.updateAnnotations(ctx, id, held, func(annotations map[string]string) {
		for _, key := range keys {
			delete(annotations, key)
		}
	})
}

// updateAnnotations sends held's annotations, as change leaves a copy of them, with held's etag.
// The client makes one attempt (UpdateSecret's default call options retry nothing), so
// FAILED_PRECONDITION is the etag refusing it (secretstore.ErrTagsChanged), and any other code
// that is no gcpDefiniteRefusal (UNAVAILABLE, DEADLINE_EXCEEDED, INTERNAL, a lost connection) may
// follow an update the store applied: it is answered as secretstore.TagsUnknown.
func (b *writer) updateAnnotations(ctx context.Context, id string, held secretstore.HeldTags, change func(map[string]string)) error {
	annotations := maps.Clone(held.Tags)
	if annotations == nil {
		annotations = map[string]string{}
	}
	change(annotations)
	_, err := b.client.UpdateSecret(ctx, &secretmanagerpb.UpdateSecretRequest{
		Secret: &secretmanagerpb.Secret{
			Name:        id,
			Annotations: annotations,
			Etag:        held.Etag,
		},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"annotations"}},
	})
	switch {
	case status.Code(err) == codes.FailedPrecondition:
		return fmt.Errorf("%w, so the store refused this write: %w", secretstore.ErrTagsChanged, err)
	case err != nil && !gcpDefiniteRefusal(err):
		return secretstore.TagsUnknown{Err: err}
	}
	return err
}

// Delete sets the secret's expire_time recoveryWindowDays from now, the moment Secret Manager
// deletes it for good, and answers that moment.
func (b *writer) Delete(ctx context.Context, id string) (time.Time, error) {
	expire := time.Now().UTC().Add(time.Duration(recoveryWindowDays) * 24 * time.Hour)
	_, err := b.client.UpdateSecret(ctx, &secretmanagerpb.UpdateSecretRequest{
		Secret: &secretmanagerpb.Secret{
			Name:       id,
			Expiration: &secretmanagerpb.Secret_ExpireTime{ExpireTime: timestamppb.New(expire)},
		},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"expire_time"}},
	})
	if err != nil {
		return time.Time{}, err
	}
	return expire, nil
}

// Restore clears expire_time: the field mask names it with no value in the body, which Secret
// Manager reads as unsetting it.
func (b *writer) Restore(ctx context.Context, id string) error {
	_, err := b.client.UpdateSecret(ctx, &secretmanagerpb.UpdateSecretRequest{
		Secret:     &secretmanagerpb.Secret{Name: id},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"expire_time"}},
	})
	return err
}

// AccessDenied is Secret Manager's PERMISSION_DENIED.
func (b *writer) AccessDenied(err error) bool {
	return status.Code(err) == codes.PermissionDenied
}

// DefiniteRefusal is a code Secret Manager answers before it acts (gcpDefiniteRefusal).
func (b *writer) DefiniteRefusal(err error) bool { return gcpDefiniteRefusal(err) }

// MissingPermission is the IAM permission Google's own message says err's refusal lacks
// (gcpMissingPermission), read from the refusal storeFailure worded, "" for one that names none.
func (b *writer) MissingPermission(err error) string {
	var refusal *storeRefusal
	if errors.As(err, &refusal) {
		err = refusal.err
	}
	return gcpMissingPermission(status.Convert(err).Message())
}

// Close releases the Secret Manager client's underlying gRPC connection.
func (b *writer) Close() error { return b.client.Close() }

var _ secretstore.Writer = (*writer)(nil)
