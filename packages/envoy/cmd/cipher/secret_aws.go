// packages/envoy/cmd/cipher/secret_aws.go
//
// The AWS side of the secret forms: the Secrets Manager and STS calls they make, under the AWS
// sign-in this command's environment gives it, whatever principal that is, a person's or a
// machine's: Secrets Manager's own IAM decides what it may read and write. The one check is the
// one that refuses a read or a write under a sign-in in another account than the broker's.
package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go"
	"github.com/aws/smithy-go/logging"
	"github.com/aws/smithy-go/middleware"

	"github.com/sjawhar/envoy/internal/cipher/secretstore"
	awsstore "github.com/sjawhar/envoy/internal/cipher/secretstore/aws"
	"github.com/sjawhar/envoy/internal/redact"
)

// secretsAPI is the Secrets Manager calls the secret forms make: ListSecrets and DescribeSecret to
// read, GetSecretValue for the value retag decodes (Read), and writes for values, tags and
// deletion. *secretsmanager.Client and awstest.Local satisfy it.
type secretsAPI interface {
	secretsmanager.ListSecretsAPIClient
	DescribeSecret(ctx context.Context, in *secretsmanager.DescribeSecretInput, o ...func(*secretsmanager.Options)) (*secretsmanager.DescribeSecretOutput, error)
	GetSecretValue(ctx context.Context, in *secretsmanager.GetSecretValueInput, o ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error)
	CreateSecret(ctx context.Context, in *secretsmanager.CreateSecretInput, o ...func(*secretsmanager.Options)) (*secretsmanager.CreateSecretOutput, error)
	PutSecretValue(ctx context.Context, in *secretsmanager.PutSecretValueInput, o ...func(*secretsmanager.Options)) (*secretsmanager.PutSecretValueOutput, error)
	TagResource(ctx context.Context, in *secretsmanager.TagResourceInput, o ...func(*secretsmanager.Options)) (*secretsmanager.TagResourceOutput, error)
	UntagResource(ctx context.Context, in *secretsmanager.UntagResourceInput, o ...func(*secretsmanager.Options)) (*secretsmanager.UntagResourceOutput, error)
	DeleteSecret(ctx context.Context, in *secretsmanager.DeleteSecretInput, o ...func(*secretsmanager.Options)) (*secretsmanager.DeleteSecretOutput, error)
	RestoreSecret(ctx context.Context, in *secretsmanager.RestoreSecretInput, o ...func(*secretsmanager.Options)) (*secretsmanager.RestoreSecretOutput, error)
}

// stsAPI is the one STS call the secret forms make: whose sign-in this is.
type stsAPI interface {
	GetCallerIdentity(ctx context.Context, in *sts.GetCallerIdentityInput, o ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error)
}

var (
	_ secretsAPI = (*secretsmanager.Client)(nil)
	_ stsAPI     = (*sts.Client)(nil)
)

// awsClients loads the AWS sign-in this command's environment gives it, pinned to the broker's
// region, and answers the credential sources the SDK resolved it from
// (aws.CredentialProviderSource, read without asking for a credential): the SDK reads every
// source from the environment itself, in its own order (awsSourceOf), so this takes none. A
// profile AWS_PROFILE or AWS_DEFAULT_PROFILE names that the shared config and credentials files do
// not hold is refused, as R1 refuses a source that is present but fails: the SDK refuses such an
// AWS_PROFILE itself, and ignores such an AWS_DEFAULT_PROFILE
// (config.loadSharedConfigIgnoreNotExist), which would sign in under the rest of its chain - an
// instance role among them - while the profile was the caller's choice. Where the sign-in is the
// web identity token file AWS_WEB_IDENTITY_TOKEN_FILE names, the file is read here first, and the
// SDK reads it through webIdentityToken every time after, so a file that cannot be read is refused
// naming the variable, never the path the SDK's own error quotes. The SDK's own log lines go
// nowhere (logging.Nop): it writes them to the process's stderr with no redaction - a response's
// Date header it cannot parse, quoted whole, which whoever answers at the store's or STS's address
// writes, among them - as gcpClientDial discards Secret Manager's. Tests replace it.
var awsClients = func(ctx context.Context, region string) (secretsAPI, stsAPI, []aws.CredentialSource, error) {
	options := []func(*config.LoadOptions) error{config.WithRegion(region), config.WithLogger(logging.Nop{})}
	if file := awsWebIdentityTokenFile(); file != "" {
		if _, err := file.GetIdentityToken(); err != nil {
			return nil, nil, nil, fmt.Errorf("load your AWS sign-in: %w", err)
		}
		options = append(options, config.WithWebIdentityRoleCredentialOptions(func(o *stscreds.WebIdentityRoleOptions) {
			o.TokenRetriever = file
		}))
	}
	cfg, err := config.LoadDefaultConfig(ctx, options...)
	var missing config.SharedConfigProfileNotExistError
	if errors.As(err, &missing) && missing.Profile != "" && missing.Profile == os.Getenv("AWS_PROFILE") {
		return nil, nil, nil, missingAWSProfile("AWS_PROFILE")
	}
	if err != nil {
		return nil, nil, nil, awsSignInFailure("load your AWS sign-in", err)
	}
	if named := os.Getenv("AWS_DEFAULT_PROFILE"); named != "" && os.Getenv("AWS_PROFILE") == "" && !loadedProfile(cfg) {
		return nil, nil, nil, missingAWSProfile("AWS_DEFAULT_PROFILE")
	}
	var sources []aws.CredentialSource
	if provider, ok := cfg.Credentials.(aws.CredentialProviderSource); ok {
		sources = provider.ProviderSources()
	}
	return secretsmanager.NewFromConfig(cfg), sts.NewFromConfig(cfg), sources, nil
}

// webIdentityToken is the web identity token file AWS_WEB_IDENTITY_TOKEN_FILE names, as the AWS
// SDK's stscreds.IdentityTokenRetriever reads it: a file that cannot be read is refused naming the
// variable and the system's reason alone (redact.File). The SDK's own IdentityTokenFile quotes the
// path twice ("unable to read file at <path>: open <path>: ..."), with %v, so no redaction of its
// error can reach the path, and the variable is where a person who pasted the token itself puts it.
type webIdentityToken string

func (f webIdentityToken) GetIdentityToken() ([]byte, error) {
	data, err := os.ReadFile(string(f))
	if err != nil {
		return nil, fmt.Errorf("read the web identity token file AWS_WEB_IDENTITY_TOKEN_FILE names: %w", redact.File(err))
	}
	return data, nil
}

// awsWebIdentityTokenFile is the file AWS_WEB_IDENTITY_TOKEN_FILE names where it is the source the
// SDK signs in with (awsSourceOf: no access keys in the environment outrank it), else "".
func awsWebIdentityTokenFile() webIdentityToken {
	idVar, _ := firstSet("AWS_ACCESS_KEY_ID", "AWS_ACCESS_KEY")
	secretVar, _ := firstSet("AWS_SECRET_ACCESS_KEY", "AWS_SECRET_KEY")
	if idVar != "" && secretVar != "" {
		return ""
	}
	return webIdentityToken(os.Getenv("AWS_WEB_IDENTITY_TOKEN_FILE"))
}

// loadedProfile reports whether the SDK found the profile it looked for in the shared config and
// credentials files: the config.SharedConfig among cfg's sources names it, and is empty where the
// SDK ignored a profile it did not find.
func loadedProfile(cfg aws.Config) bool {
	for _, source := range cfg.ConfigSources {
		if shared, ok := source.(config.SharedConfig); ok && shared.Profile != "" {
			return true
		}
	}
	return false
}

// ssoRole matches an IAM Identity Center sign-in and captures its session name. The deployment
// repository's Identity Center permission sets map the session name to the person's userName,
// which is their email: --owner me reads it there (awsBackend.PersonEmail).
var ssoRole = regexp.MustCompile(`^arn:aws[a-z-]*:sts::[0-9]{12}:assumed-role/AWSReservedSSO_[^/]+/(.+)$`)

// requireAccount refuses, before any read or write, a sign-in in an account other than the
// broker's, naming both accounts and how to choose another sign-in (another, awsSource.another): a
// read there would list another account's secrets as if they were the agent secrets. It answers
// the sign-in's ARN.
func requireAccount(ctx context.Context, st stsAPI, settings Settings, another string) (arn string, err error) {
	id, err := st.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return "", awsSignInFailure("read your AWS sign-in", err)
	}
	arn, account := aws.ToString(id.Arn), aws.ToString(id.Account)
	if account != settings.AWSAccountID {
		return "", fmt.Errorf("your AWS sign-in %s is in account %s, but the agent secrets are in account %s: sign in to account %s; %s",
			arn, account, settings.AWSAccountID, settings.AWSAccountID, another)
	}
	return arn, nil
}

// checkSettingsKeyARN refuses, before any AWS call, settings whose aws_account_id or aws_region does
// not match the account and region in kms_key_arn. The broker derives both from the key ARN the same
// way (internal/cipher/api handlers_secrets.go, aws.KeyARNParts), so the account the sign-in is
// checked against (requireAccount) and the region the SDK is pinned to are the key ARN's, not a
// separately-sent field a spoofed or plain-http broker could set on its own to steer a write. Each
// field a refusal names prints as the broker's address lets its answer print (Settings.words).
func checkSettingsKeyARN(settings Settings) error {
	if !awsstore.ValidKeyARN(settings.KMSKeyARN) {
		return fmt.Errorf("the broker's settings name an invalid KMS key ARN %s (expected arn:aws:kms:<region>:<account>:key/<key id>)", settings.words(strconv.Quote(settings.KMSKeyARN)))
	}
	region, account := awsstore.KeyARNParts(settings.KMSKeyARN)
	if settings.AWSAccountID != account {
		return fmt.Errorf("the broker's settings name account %s, but its KMS key ARN %s is in account %s: the broker's settings are inconsistent", settings.words(settings.AWSAccountID), settings.words(settings.KMSKeyARN), settings.words(account))
	}
	if settings.AWSRegion != region {
		return fmt.Errorf("the broker's settings name region %s, but its KMS key ARN %s is in region %s: the broker's settings are inconsistent", settings.words(settings.AWSRegion), settings.words(settings.KMSKeyARN), settings.words(region))
	}
	return nil
}

// isAccessDenied reports whether err is AWS's AccessDeniedException, which the SDK answers as a
// generic API error: secretsmanager's types declare no type for it.
func isAccessDenied(err error) bool {
	var apiErr smithy.APIError
	return errors.As(err, &apiErr) && apiErr.ErrorCode() == "AccessDeniedException"
}

// awsBackend is the secretstore.Writer for store "" or aws.Name: AWS Secrets Manager under the
// AWS sign-in this command's environment gives it, pinned to the key ARN's account and region. Its
// list, describe and value reads are the broker's own store's (awsstore.Store) over the same
// client, whose failures that client words (awsStore, ClientErrors).
type awsBackend struct {
	awsstore.Store
	settings Settings
	sm       secretsAPI
	st       stsAPI
	// source is where the SDK took the sign-in from (awsSourceOf).
	source awsSource
	// arn is the sign-in RequireAccount read, which every form reads first.
	arn string
}

// newAWSBackend is the AWS writer over sm and st, which awsClients loaded from sources, its reads
// the broker's own store's over the same Secrets Manager client wrapped so its failures read as
// this CLI words them (awsStore). It reads no KMS alias: the forms never ask a listing's OnKey.
func newAWSBackend(settings Settings, sm secretsAPI, st stsAPI, sources []aws.CredentialSource) *awsBackend {
	worded := awsStore{sm}
	return &awsBackend{
		Store:    awsstore.Store{Client: worded, KeyARN: settings.KMSKeyARN, ClientErrors: true},
		settings: settings, sm: worded, st: st, source: awsSourceOf(sources),
	}
}

// agentSecrets is an agent-secrets command line for printing. Its sign-in prefix (RerunPrefix)
// carries the variables that chose the sign-in, so it reads the store as the identity that wrote
// to it.
func (b *awsBackend) agentSecrets(words ...string) string {
	return secretstore.Command(b.RerunPrefix(), words...)
}

// awsSource is a source the AWS SDK takes the sign-in from (awsSourceOf): phrase names it in the
// using line and in a refusal, another says how to choose another (ChooseAnother), and prefix is
// the shell assignments a printed command carries to sign in as this one did (RerunPrefix).
type awsSource struct {
	phrase, another, prefix string
}

// firstSet is the first of names the environment sets to a value that is not empty, and that
// value, as the SDK reads a variable and its older name (config.setStringFromEnvVal); "", "" where
// none is set.
func firstSet(names ...string) (name, value string) {
	for _, name := range names {
		if value := os.Getenv(name); value != "" {
			return name, value
		}
	}
	return "", ""
}

// awsSourceOf is the source the AWS SDK takes the sign-in awsClients loads from, in the SDK's own
// order (aws-sdk-go-v2/config, resolveCredentialChain), as this command's environment sets it:
//
//  1. access keys in the environment, both an access key id (AWS_ACCESS_KEY_ID, else
//     AWS_ACCESS_KEY) and a secret access key (AWS_SECRET_ACCESS_KEY, else AWS_SECRET_KEY);
//  2. a web identity token file, AWS_WEB_IDENTITY_TOKEN_FILE, with the role in AWS_ROLE_ARN;
//  3. the profile AWS_PROFILE, else AWS_DEFAULT_PROFILE, names;
//  4. the rest of the SDK's default chain: the default profile, a container's credentials, the
//     instance's role.
//
// Only a profile set in code outranks 1 and 2, and this command sets none, as the AWS CLI does not
// for AWS_PROFILE: so where keys or a token file are set, a profile variable chooses nothing, and
// the phrase says so. A profile that holds no credential of its own (no keys, sign-in, process,
// source profile or credential source) chooses nothing either: the SDK goes on to a container's
// credentials endpoint or the instance's role (resolveCredsFromProfile), and sources, the ones it
// resolved, then start with that one (profileFellThrough), so the phrase names it too. The phrase
// names no key and no token, and no file's path: the web identity token file holds a token, so it
// is named by its variable, as every file that holds a credential is, and AWS_WEB_IDENTITY_TOKEN_FILE
// is where a person who pasted the token itself puts it. Only the prefix, the command a person
// pastes to run again (RerunPrefix), carries the path, once STS took the token the file holds
// (the pasted commands follow requireAccount): the file was read and signed in with, so its path
// is no token pasted in its place. A file that cannot be read is refused naming the variable alone
// (webIdentityToken).
func awsSourceOf(sources []aws.CredentialSource) awsSource {
	profileVar, profile := firstSet("AWS_PROFILE", "AWS_DEFAULT_PROFILE")
	outranks := func(verb string) string {
		if profileVar == "" {
			return ""
		}
		return ", which " + verb + " " + profileVar
	}
	idVar, _ := firstSet("AWS_ACCESS_KEY_ID", "AWS_ACCESS_KEY")
	secretVar, _ := firstSet("AWS_SECRET_ACCESS_KEY", "AWS_SECRET_KEY")
	switch {
	case idVar != "" && secretVar != "":
		keys := idVar + " and " + secretVar
		return awsSource{
			phrase:  "the access keys in " + keys + outranks("outrank"),
			another: "choose another by unsetting " + keys + ", which outrank AWS_PROFILE, or by exporting other keys",
		}
	case os.Getenv("AWS_WEB_IDENTITY_TOKEN_FILE") != "":
		prefix := ""
		for _, name := range []string{"AWS_WEB_IDENTITY_TOKEN_FILE", "AWS_ROLE_ARN"} {
			if value := os.Getenv(name); value != "" {
				prefix += name + "=" + secretstore.ShellWord(value) + " "
			}
		}
		return awsSource{
			phrase:  "the web identity token file AWS_WEB_IDENTITY_TOKEN_FILE names" + outranks("outranks"),
			another: "choose another by unsetting AWS_WEB_IDENTITY_TOKEN_FILE, which outranks AWS_PROFILE, or by naming another token file and role in it and AWS_ROLE_ARN",
			prefix:  prefix,
		}
	case profileVar != "":
		source := awsSource{
			phrase:  "the AWS profile " + profile + " (" + profileVar + ")",
			another: "choose another with AWS_PROFILE",
			prefix:  profileVar + "=" + secretstore.ShellWord(profile) + " ",
		}
		if profileVar == "AWS_DEFAULT_PROFILE" {
			source.another += ", which outranks AWS_DEFAULT_PROFILE"
		}
		if fell := profileFellThrough(sources); fell != "" {
			source.phrase += ", which holds no credential, so " + fell + " signed in"
			source.another += ", naming a profile that holds a credential"
		}
		return source
	}
	return awsSource{phrase: "the AWS SDK's default credential chain", another: "choose another with AWS_PROFILE"}
}

// profileFellThrough names what signed in where the profile the SDK loaded holds no credential of
// its own: the SDK records the profile's own kind of credential first in sources (keys, a sign-in,
// a process, a source profile, a named credential source), and only where it holds none does the
// first one name the container's credentials endpoint or the instance's role, which it then
// reached on its own. "" for any other sources, none included.
func profileFellThrough(sources []aws.CredentialSource) string {
	if len(sources) == 0 {
		return ""
	}
	switch sources[0] {
	case aws.CredentialSourceHTTP:
		if variable, _ := firstSet("AWS_CONTAINER_CREDENTIALS_RELATIVE_URI", "AWS_CONTAINER_CREDENTIALS_FULL_URI"); variable != "" {
			return "the container credentials endpoint " + variable + " names"
		}
		return "the container credentials endpoint"
	case aws.CredentialSourceIMDS:
		return "this machine's instance role"
	}
	return ""
}

// slug is the secret's name under the prefix, of its full name id.
func (b *awsBackend) slug(id string) string { return strings.TrimPrefix(id, b.settings.SecretsPrefix) }

func (b *awsBackend) displayName(id string) string { return displayName(b.settings.SecretsPrefix, id) }

// RequireAccount refuses a sign-in whose account does not hold the agent secrets (requireAccount),
// and keeps the ARN it read for PersonEmail and Identity.
func (b *awsBackend) RequireAccount(ctx context.Context) error {
	arn, err := requireAccount(ctx, b.st, b.settings, b.source.another)
	b.arn = arn
	return err
}

// PersonEmail answers, for --owner me, the session name of an Identity Center sign-in, lowercased,
// which is the person's email. Any other sign-in - a machine's role, an assumed service role, an
// IAM user - names no person. It reads the ARN RequireAccount read, which every form reads before
// it calls anything else.
func (b *awsBackend) PersonEmail(context.Context) (string, error) {
	m := ssoRole.FindStringSubmatch(b.arn)
	if m == nil {
		return "", fmt.Errorf("%s is no IAM Identity Center sign-in, so it names no person", b.arn)
	}
	return strings.ToLower(m[1]), nil
}

// Identity is where the sign-in came from and its ARN, as RequireAccount read it.
func (b *awsBackend) Identity() string { return b.source.phrase + ": " + b.arn }

// ChooseAnother is how a refusal of the store's says to sign in with another AWS credential, by
// the source that signed in (awsSource.another): the variable that outranks it.
func (b *awsBackend) ChooseAnother() string { return b.source.another }

// RerunPrefix is the shell assignments a printed command carries so it signs in as this one did
// (awsSource.prefix): the profile variable that chose the profile, or the web identity token file
// and role; "" for access keys, which a printed command never carries, and for the rest of the
// SDK's chain.
func (b *awsBackend) RerunPrefix() string { return b.source.prefix }

// PrepareCreate makes nothing: AWS creates the secret with its value in Store.
func (b *awsBackend) PrepareCreate(id string, tags map[string]string) secretstore.Create {
	return &awsNewSecret{b: b, id: id, tags: tags}
}

// awsNewSecret is a secret AWS has not made yet: Store makes it, with its tags and value. begun,
// which Unsettled reads while Store may be in flight, is set once Store begins the CreateSecret.
type awsNewSecret struct {
	b     *awsBackend
	id    string
	tags  map[string]string
	begun atomic.Bool
}

func (*awsNewSecret) Begin(context.Context) (secretstore.DiscardOutcome, error) {
	return secretstore.MadeNothing, nil
}

// Store makes the secret with its tags and value, under secretstore.CallDeadline. Its error says whether
// the store may hold the secret: a create whose every attempt the SDK made ended in an answer
// Secrets Manager gives before it acts (attempts.definiteRefusal) says nothing was stored; any
// other, a deadline among them, says the store may hold the secret with its value, or nothing,
// naming the command that tells which.
func (n *awsNewSecret) Store(ctx context.Context, value string) error {
	n.begun.Store(true)
	ctx, cancel := secretstore.WithCallDeadline(ctx)
	defer cancel()
	var attempts awsAttempts
	_, err := n.b.sm.CreateSecret(ctx, &secretsmanager.CreateSecretInput{
		Name:         aws.String(n.id),
		KmsKeyId:     aws.String(n.b.settings.KMSKeyARN),
		SecretString: aws.String(value),
		Tags:         awsstore.TagList(n.tags),
	}, attempts.record)
	switch {
	case err == nil:
		return nil
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return fmt.Errorf("%w; Secrets Manager did not answer within %s, so %s", err, secretstore.CallDeadlineText(), n.Unsettled())
	case attempts.definiteRefusal(err):
		return fmt.Errorf("%w; Secrets Manager refused the create before acting on it, so nothing was stored", err)
	}
	return fmt.Errorf("%w; Secrets Manager's answer leaves open whether it made the secret, so %s", err, n.Unsettled())
}

// awsAttempts is the error each attempt of one call ended with, as the SDK made them: record, a
// per-call option, puts a middleware right after the SDK's retry middleware ("Retry", in the
// Finalize step), which runs once for each attempt. The SDK's own error is the last attempt's
// alone, which cannot say whether an earlier attempt reached the store.
type awsAttempts struct {
	mu   sync.Mutex
	errs []error
}

// record is the per-call option that logs each attempt's error.
func (a *awsAttempts) record(o *secretsmanager.Options) {
	o.APIOptions = append(o.APIOptions, func(stack *middleware.Stack) error {
		return stack.Finalize.Insert(middleware.FinalizeMiddlewareFunc("agentSecretsAttempts", func(ctx context.Context, in middleware.FinalizeInput, next middleware.FinalizeHandler) (middleware.FinalizeOutput, middleware.Metadata, error) {
			out, md, err := next.HandleFinalize(ctx, in)
			a.mu.Lock()
			a.errs = append(a.errs, err)
			a.mu.Unlock()
			return out, md, err
		}), "Retry", middleware.After)
	})
}

// definiteRefusal reports whether the call err ended may have made nothing for certain: every
// attempt it made ended in an error whose code awsDefiniteRefusals lists. An attempt answered that
// way acted on nothing, so a call the SDK retried for such a code (it retries a throttle such as
// LimitExceededException, a quota refusal) and that met it every time made nothing either. One
// attempt that ended any other way may have reached the store, which can then answer a retry as a
// refusal (a name taken, by the secret that attempt made), so the call's outcome is open. A
// client that runs no SDK middleware, and so records no attempt, is judged by err alone.
func (a *awsAttempts) definiteRefusal(err error) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.errs) == 0 {
		return awsDefiniteRefusal(err)
	}
	return !slices.ContainsFunc(a.errs, func(e error) bool { return !awsDefiniteRefusal(e) })
}

// awsDefiniteRefusals are the error codes Secrets Manager, or AWS's request handling in front of
// it, answers before it acts on a call: the secret's own refusals of CreateSecret (a name taken,
// a request, a parameter or a policy refused, a precondition unmet, a limit reached, KMS refusing
// the key), and the sign-in's and the request's refusals. InternalServiceError, ServiceUnavailable
// and every code not listed may follow a call the store carried out.
var awsDefiniteRefusals = []string{
	"AccessDeniedException", "ResourceExistsException", "InvalidRequestException", "InvalidParameterException",
	"LimitExceededException", "MalformedPolicyDocumentException", "PreconditionNotMetException",
	"ResourceNotFoundException", "EncryptionFailure", "DecryptionFailure", "ValidationException",
	"UnrecognizedClientException", "InvalidClientTokenId", "ExpiredTokenException", "IncompleteSignature",
	"InvalidSignatureException", "MissingAuthenticationToken", "NotAuthorized", "RequestExpired", "OptInRequired",
}

// awsDefiniteRefusal reports whether err, one attempt's error, is an answer Secrets Manager gives
// before it acts: an API error whose code awsDefiniteRefusals lists. An error that is no API error
// at all (a dropped connection, a reset) may follow a call the store carried out.
func awsDefiniteRefusal(err error) bool {
	var apiErr smithy.APIError
	return errors.As(err, &apiErr) && slices.Contains(awsDefiniteRefusals, apiErr.ErrorCode())
}

// Discard has nothing to remove: AWS makes the secret only in Store.
func (*awsNewSecret) Discard(context.Context) (secretstore.DiscardOutcome, error) {
	return secretstore.MadeNothing, nil
}

// MadeBeforeValue is false: AWS makes the secret only with its value, so nothing waits for it.
func (*awsNewSecret) MadeBeforeValue() bool { return false }

// Unsettled says what the store may hold should the form end now: nothing before the CreateSecret,
// and from then the secret with its value or nothing, which `secret show` tells apart; never an
// empty secret, which Secrets Manager does not make.
func (n *awsNewSecret) Unsettled() string {
	if !n.begun.Load() {
		return "nothing stays in the store, since Secrets Manager makes a secret only with its value"
	}
	name := n.b.displayName(n.id)
	return fmt.Sprintf("the store may hold %s, made with its value, or no %s at all: `%s` says which, and no empty secret stays to clear, since Secrets Manager makes a secret only with its value", name, name, n.b.agentSecrets("secret", "show", name))
}

func (b *awsBackend) Set(ctx context.Context, id, value string) error {
	_, err := b.sm.PutSecretValue(ctx, &secretsmanager.PutSecretValueInput{
		SecretId: aws.String(id), SecretString: aws.String(value),
	})
	return err
}

// CurrentTags reads the secret's tags with DescribeSecret, and its ARN, the source Read takes.
// Secrets Manager's tag writes take no condition, so the read has no etag.
func (b *awsBackend) CurrentTags(ctx context.Context, id string) (secretstore.HeldTags, error) {
	held, err := b.sm.DescribeSecret(ctx, &secretsmanager.DescribeSecretInput{SecretId: aws.String(id)})
	if err != nil {
		return secretstore.HeldTags{}, err
	}
	return secretstore.HeldTags{Tags: awsstore.TagMap(held.Tags), Source: aws.ToString(held.ARN)}, nil
}

// ValueReadPermission is what GetSecretValue needs of a person's sign-in: the call itself, and
// the key the value is encrypted under, since Secrets Manager decrypts it as the caller.
func (b *awsBackend) ValueReadPermission() string {
	return "secretsmanager:GetSecretValue on the secret and kms:Decrypt on its key"
}

// TagReadPermission is what DescribeSecret needs of a person's sign-in: whoever stores a value
// reads that secret's tags first, so a write grant that does not carry this read fails `secret
// set` before anything is written.
func (b *awsBackend) TagReadPermission() string {
	return "secretsmanager:DescribeSecret on the secret"
}

// awsTagValueMax is the most characters AWS takes in one tag value, and awsTagValuePunctuation the
// characters it takes beside Unicode letters, numbers and space separators: the Resource Groups
// Tagging API's own pattern for a tag value, `^([\p{L}\p{Z}\p{N}_.:\/=+\-@]*)$` of length 0 to 256
// (https://docs.aws.amazon.com/resourcegroupstagging/latest/APIReference/API_Tag.html). The
// Secrets Manager user guide's list leaves ':' out, but this client already writes it on AWS
// (owner=group:EMAIL, policy.RequestersRollbackGuard), and CloudFormation tags each secret it
// makes with ARNs, so ':' is in.
const (
	awsTagValueMax         = 256
	awsTagValuePunctuation = "_.:/=+-@"
)

// checkTagValues refuses, before any write, a tag value AWS would refuse: one longer than
// awsTagValueMax characters, or holding a character that is no Unicode letter, number or space
// separator (a tab or a newline is none) and none of awsTagValuePunctuation. The refusal names the
// tag, the store's limit and, for a character, the first that fails and where, as a flag's
// refusal does, and prints no other part of the value. tags is every tag the write sends.
func (*awsBackend) CheckTagValues(tags map[string]string) error {
	for _, key := range slices.Sorted(maps.Keys(tags)) {
		value := tags[key]
		if n := utf8.RuneCountInString(value); n > awsTagValueMax {
			return usageErr{fmt.Errorf("the %s tag is %d characters, and AWS Secrets Manager holds at most %d in a tag value: shorten it", key, n, awsTagValueMax)}
		}
		for i, r := range value {
			if unicode.IsLetter(r) || unicode.IsNumber(r) || unicode.Is(unicode.Z, r) || strings.ContainsRune(awsTagValuePunctuation, r) {
				continue
			}
			return usageErr{fmt.Errorf("the %s tag holds %q at character %d, and AWS Secrets Manager takes in a tag value only Unicode letters, digits, spaces and %s: write it without that character", key, string(r), utf8.RuneCountInString(value[:i])+1, strings.Join(strings.Split(awsTagValuePunctuation, ""), " "))}
		}
	}
	return nil
}

// SetTags writes tags in one TagResource, which keeps every tag it does not name. TagResource takes
// no condition on held, so the write reads its tags back after it and holds them to check
// (readBack).
func (b *awsBackend) SetTags(ctx context.Context, id string, _ secretstore.HeldTags, tags map[string]string, check secretstore.TagCheck) error {
	if err := b.tag(ctx, id, tags); err != nil {
		return err
	}
	return b.readBack(ctx, id, check)
}

// RemoveTags removes keys in one UntagResource, which takes no condition on held either, and reads
// its tags back after it (readBack).
func (b *awsBackend) RemoveTags(ctx context.Context, id string, _ secretstore.HeldTags, keys []string, check secretstore.TagCheck) error {
	if err := b.untag(ctx, id, keys); err != nil {
		return err
	}
	return b.readBack(ctx, id, check)
}

// tag and untag answer an error that is no definite refusal (awsAttempts.definiteRefusal: every
// attempt ending in a code awsDefiniteRefusals lists) as secretstore.TagsUnknown: a service fault,
// an attempt that ended any other way before a retry, or a lost connection may follow a call the
// store carried out.
func (b *awsBackend) tag(ctx context.Context, id string, tags map[string]string) error {
	var attempts awsAttempts
	_, err := b.sm.TagResource(ctx, &secretsmanager.TagResourceInput{
		SecretId: aws.String(id),
		Tags:     awsstore.TagList(tags),
	}, attempts.record)
	return tagOutcome(err, &attempts)
}

func (b *awsBackend) untag(ctx context.Context, id string, keys []string) error {
	var attempts awsAttempts
	_, err := b.sm.UntagResource(ctx, &secretsmanager.UntagResourceInput{
		SecretId: aws.String(id),
		TagKeys:  keys,
	}, attempts.record)
	return tagOutcome(err, &attempts)
}

// tagOutcome is a tag call's error as tag and untag answer it: nil, a definite refusal as it
// is, and any other error as secretstore.TagsUnknown.
func tagOutcome(err error, attempts *awsAttempts) error {
	if err == nil || attempts.definiteRefusal(err) {
		return err
	}
	return secretstore.TagsUnknown{Err: err}
}

// readBack is how a tag write on AWS, whose tag calls take no condition, answers for a race with
// another writer that retags the same secret between the write's read and the write itself: it
// reads the tags back and holds them to check, the retag form's own rule (its github-owners
// check), whose error it answers. It writes nothing: the tags it read are no more current than
// the other writer's, so putting them back would take that writer's retag away, and more than one
// writer is the normal case. Both of its errors come after the write landed, and are marked so
// (secretstore.TagsLanded): check's, and a read-back that failed. With no check it reads nothing.
func (b *awsBackend) readBack(ctx context.Context, id string, check secretstore.TagCheck) error {
	if check == nil {
		return nil
	}
	now, err := b.CurrentTags(ctx, id)
	if err != nil {
		return secretstore.TagsLanded{Err: fmt.Errorf("the retag was written, but reading its tags back failed, so whether another write left it a type without the GitHub owners it needs is unknown: %w", err)}
	}
	if err := check(now.Tags); err != nil {
		return secretstore.TagsLanded{Err: err}
	}
	return nil
}

// Delete schedules the secret's deletion with Secrets Manager's recovery window and answers the
// moment that window ends, DeleteSecret's own DeletionDate.
func (b *awsBackend) Delete(ctx context.Context, id string) (time.Time, error) {
	out, err := b.sm.DeleteSecret(ctx, &secretsmanager.DeleteSecretInput{
		SecretId: aws.String(id), RecoveryWindowInDays: aws.Int64(recoveryWindowDays),
	})
	if err != nil {
		return time.Time{}, err
	}
	return aws.ToTime(out.DeletionDate), nil
}

func (b *awsBackend) Restore(ctx context.Context, id string) error {
	_, err := b.sm.RestoreSecret(ctx, &secretsmanager.RestoreSecretInput{SecretId: aws.String(id)})
	return err
}

// AccessDenied is AWS's AccessDeniedException (isAccessDenied).
func (b *awsBackend) AccessDenied(err error) bool { return isAccessDenied(err) }

// DefiniteRefusal is an answer Secrets Manager gives before it acts (awsDefiniteRefusal).
func (b *awsBackend) DefiniteRefusal(err error) bool { return awsDefiniteRefusal(err) }

// MissingPermission is the IAM action AWS's message says err's refusal lacks
// (awsMissingPermission), "" for an error that is no AWS API error or names none.
func (b *awsBackend) MissingPermission(err error) string {
	var api smithy.APIError
	if !errors.As(err, &api) {
		return ""
	}
	return awsMissingPermission(api.ErrorMessage())
}

// Close is a no-op: the AWS SDK's HTTP-based clients hold no persistent connection worth
// releasing explicitly.
func (b *awsBackend) Close() error { return nil }

var (
	_ secretstore.Writer       = (*awsBackend)(nil)
	_ secretstore.AccountGuard = (*awsBackend)(nil)
)
