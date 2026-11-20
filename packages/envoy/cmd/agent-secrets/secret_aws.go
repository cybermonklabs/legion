// packages/envoy/cmd/agent-secrets/secret_aws.go
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
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/ssocreds"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	smtypes "github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go"
	"github.com/aws/smithy-go/middleware"

	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/secrets"
	"github.com/sjawhar/envoy/internal/redact"
)

// secretsAPI is the Secrets Manager calls the secret forms make: ListSecrets and DescribeSecret to
// read, GetSecretValue for the value retag decodes (readValue), and writes for values, tags and
// deletion. *secretsmanager.Client and secrets.Local satisfy it.
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
// naming the variable, never the path the SDK's own error quotes. Tests replace it.
var awsClients = func(ctx context.Context, region string) (secretsAPI, stsAPI, []aws.CredentialSource, error) {
	options := []func(*config.LoadOptions) error{config.WithRegion(region)}
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

// missingAWSProfile is the refusal of variable naming a profile the shared config and credentials
// files do not hold. The profile is not printed, though the AWS SDK's own error quotes it: a
// profile the files do not hold is the caller's text, and a variable meant for a name is where a
// pasted credential lands. A profile they hold is a label, and the using line prints it.
func missingAWSProfile(variable string) error {
	return fmt.Errorf("load your AWS sign-in: %s names a profile the shared config and credentials files do not hold (not printed)", variable)
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
// which is their email: --owner me reads it there (awsBackend.personEmail).
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

// awsSignInFailure is err, the AWS SDK's failure to load or to read the sign-in this command's
// environment gives it, as what failed and the SDK's failure in words of this client's own
// (awsSDKWords). It unwraps to err, so errors.As still finds the SDK's own error.
func awsSignInFailure(what string, err error) error {
	return fmt.Errorf("%s: %w", what, redact.Masked(err, awsSDKWords(err)))
}

// awsStore is a Secrets Manager client whose every failure prints as awsStoreFailure: the store
// calls' errors reach the forms' refusals, and the SDK's text quotes AWS's own message and, for a
// request that did not complete, the address AWS_ENDPOINT_URL or AWS_ENDPOINT_URL_SECRETS_MANAGER
// names, with its user, password and query. openSecretBackend wraps every AWS store in it.
type awsStore struct{ api secretsAPI }

func (s awsStore) ListSecrets(ctx context.Context, in *secretsmanager.ListSecretsInput, o ...func(*secretsmanager.Options)) (*secretsmanager.ListSecretsOutput, error) {
	out, err := s.api.ListSecrets(ctx, in, o...)
	return out, awsStoreFailure("ListSecrets", err)
}

func (s awsStore) DescribeSecret(ctx context.Context, in *secretsmanager.DescribeSecretInput, o ...func(*secretsmanager.Options)) (*secretsmanager.DescribeSecretOutput, error) {
	out, err := s.api.DescribeSecret(ctx, in, o...)
	return out, awsStoreFailure("DescribeSecret", err)
}

func (s awsStore) GetSecretValue(ctx context.Context, in *secretsmanager.GetSecretValueInput, o ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error) {
	out, err := s.api.GetSecretValue(ctx, in, o...)
	return out, awsStoreFailure("GetSecretValue", err)
}

func (s awsStore) CreateSecret(ctx context.Context, in *secretsmanager.CreateSecretInput, o ...func(*secretsmanager.Options)) (*secretsmanager.CreateSecretOutput, error) {
	out, err := s.api.CreateSecret(ctx, in, o...)
	return out, awsStoreFailure("CreateSecret", err)
}

func (s awsStore) PutSecretValue(ctx context.Context, in *secretsmanager.PutSecretValueInput, o ...func(*secretsmanager.Options)) (*secretsmanager.PutSecretValueOutput, error) {
	out, err := s.api.PutSecretValue(ctx, in, o...)
	return out, awsStoreFailure("PutSecretValue", err)
}

func (s awsStore) TagResource(ctx context.Context, in *secretsmanager.TagResourceInput, o ...func(*secretsmanager.Options)) (*secretsmanager.TagResourceOutput, error) {
	out, err := s.api.TagResource(ctx, in, o...)
	return out, awsStoreFailure("TagResource", err)
}

func (s awsStore) UntagResource(ctx context.Context, in *secretsmanager.UntagResourceInput, o ...func(*secretsmanager.Options)) (*secretsmanager.UntagResourceOutput, error) {
	out, err := s.api.UntagResource(ctx, in, o...)
	return out, awsStoreFailure("UntagResource", err)
}

func (s awsStore) DeleteSecret(ctx context.Context, in *secretsmanager.DeleteSecretInput, o ...func(*secretsmanager.Options)) (*secretsmanager.DeleteSecretOutput, error) {
	out, err := s.api.DeleteSecret(ctx, in, o...)
	return out, awsStoreFailure("DeleteSecret", err)
}

func (s awsStore) RestoreSecret(ctx context.Context, in *secretsmanager.RestoreSecretInput, o ...func(*secretsmanager.Options)) (*secretsmanager.RestoreSecretOutput, error) {
	out, err := s.api.RestoreSecret(ctx, in, o...)
	return out, awsStoreFailure("RestoreSecret", err)
}

// awsStoreFailure is err, the failure of the Secrets Manager call operation, as a message prints
// it: the call, and the code AWS answered it with, never AWS's message, which whoever answers at
// the store's address writes what it likes in, and which can repeat a value the call sent (a
// ValidationException quotes the field it refused); a call its context ended by that end alone; a
// request that did not complete by its class beside the address settings that are set
// (awsAddressesSet); and anything else in fixed words. It unwraps to err, so isAccessDenied,
// awsDefiniteRefusal and every errors.As and errors.Is still read the SDK's own error.
func awsStoreFailure(operation string, err error) error {
	if err == nil {
		return nil
	}
	call := "Secrets Manager " + operation
	if words, ok := awsAPIWords(err); ok {
		return redact.Masked(err, call+": "+words)
	}
	var request *url.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return redact.Masked(err, call+" failed: context deadline exceeded")
	case errors.Is(err, context.Canceled):
		return redact.Masked(err, call+" failed: context canceled")
	case errors.As(err, &request):
		return redact.Masked(err, call+" failed: "+redact.Class(err).Error()+awsAddressesSet())
	}
	return redact.Masked(err, call+" failed (the AWS SDK's message is not printed: it can quote an AWS setting's value)")
}

// awsAPIWords is err's AWS API error as a message prints it, and whether err is one: its code
// alone, where the code has the shape of one AWS registers (awsCode), never AWS's message.
func awsAPIWords(err error) (string, bool) {
	var api smithy.APIError
	if !errors.As(err, &api) {
		return "", false
	}
	if code := api.ErrorCode(); awsCode.MatchString(code) {
		return "AWS refused it: " + code + " (AWS's message is not printed)", true
	}
	return "AWS refused it (its code and message are not printed)", true
}

// awsCode is the shape of an error code AWS registers (AccessDenied, ExpiredToken,
// InvalidClientTokenId): a word of letters and digits, at most 64 long.
var awsCode = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9._]{0,63}$`)

// awsSDKWords is err, the AWS SDK's failure to sign in, as a message prints it. The SDK's own
// error text quotes what it was configured with: the path of a shared config or credentials file
// twice ("failed to load shared config file, <path>, read all: read <path>: ..."), the path
// AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE names, an endpoint's or a container's credentials URL
// whole, with its user, password and query (net/http's *url.Error, and the SDK's own "Failed to
// parse uri" where it does not parse), the value of a setting it cannot read (AWS_MAX_ATTEMPTS),
// and a service's own message, which the service writes what it likes in. So it prints:
//   - an AWS API error by its code alone, where the code has the shape of one AWS registers;
//   - an IAM Identity Center session that has expired as the SDK's own fixed words;
//   - a file call by the setting that names the file, and the call's operation and the system's
//     reason (redact.File): the SDK formats a file's error with %v, which drops the file call's
//     own error from the chain, so the file it names (SharedConfigLoadError.Filename), or the
//     file an AWS file setting names that cannot be read, is read again here for the reason;
//   - a request that failed by its class (redact.Class), naming the AWS address settings that are
//     set, never their values;
//   - anything else in fixed words that say where to look.
func awsSDKWords(err error) string {
	if words, ok := awsAPIWords(err); ok {
		return words
	}
	var invalid *ssocreds.InvalidTokenError
	var load config.SharedConfigLoadError
	var path *fs.PathError
	var request *url.Error
	switch {
	case errors.As(err, &invalid):
		return "the IAM Identity Center (SSO) session has expired or is invalid: run `aws sso login` again"
	case errors.As(err, &load):
		return "read " + awsFileName(load.Filename) + ": " + awsFileReason(load.Filename)
	case errors.As(err, &path):
		return "read " + awsFileName(path.Path) + ": " + redact.File(err).Error()
	case errors.As(err, &request):
		return "a request the AWS SDK made failed: " + redact.Class(err).Error() + awsAddressesSet()
	}
	if setting := os.Getenv("AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE"); setting != "" {
		if _, err := os.ReadFile(setting); err != nil {
			return "read the file AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE names: " + redact.File(err).Error()
		}
	}
	return "the AWS SDK could not sign in from this environment (its message is not printed: it can quote an AWS setting's value); check the AWS_* variables and ~/.aws/config"
}

// awsFileReason is why the AWS SDK could not load the shared config or credentials file at path:
// the file call's operation and the system's reason where reading it fails (redact.File), and
// that it does not parse where it reads.
func awsFileReason(path string) string {
	if _, err := os.ReadFile(path); err != nil {
		return redact.File(err).Error()
	}
	return "it does not parse as an AWS config or credentials file (the SDK's message is not printed: it can quote the file's lines)"
}

// awsFileName is the file at path, which the AWS SDK failed to read, as a message names it: by the
// setting that names it, or by where the SDK looks with none set, never by the path itself, which
// is a setting's value.
func awsFileName(path string) string {
	for _, setting := range []string{"AWS_CONFIG_FILE", "AWS_SHARED_CREDENTIALS_FILE", "AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE"} {
		if value := os.Getenv(setting); value != "" && value == path {
			return "the file " + setting + " names"
		}
	}
	if cached, err := ssocreds.StandardCachedTokenFilepath("x"); err == nil && filepath.Dir(path) == filepath.Dir(cached) {
		return "the IAM Identity Center sign-in's cached token (run `aws sso login` to make it)"
	}
	if path == config.DefaultSharedConfigFilename() {
		return "~/.aws/config"
	}
	if path == config.DefaultSharedCredentialsFilename() {
		return "~/.aws/credentials"
	}
	return "a file the AWS SDK reads"
}

// awsAddressesSet names the AWS address settings that are set, for a request that failed: one of
// them may name the wrong address. It names the settings alone, never their values, which can
// carry a user, a password or a token in their query.
func awsAddressesSet() string {
	var set []string
	for _, setting := range []string{"AWS_ENDPOINT_URL", "AWS_ENDPOINT_URL_STS", "AWS_ENDPOINT_URL_SECRETS_MANAGER", "AWS_CONTAINER_CREDENTIALS_FULL_URI", "AWS_CONTAINER_CREDENTIALS_RELATIVE_URI"} {
		if os.Getenv(setting) != "" {
			set = append(set, setting)
		}
	}
	if len(set) == 0 {
		return ""
	}
	return " (the address is not printed; " + strings.Join(set, ", ") + " is set)"
}

// checkSettingsKeyARN refuses, before any AWS call, settings whose aws_account_id or aws_region does
// not match the account and region in kms_key_arn. The broker derives both from the key ARN the same
// way (internal/broker/api handlers_secrets.go, secrets.AWSKeyARNParts), so the account the sign-in is
// checked against (requireAccount) and the region the SDK is pinned to are the key ARN's, not a
// separately-sent field a spoofed or plain-http broker could set on its own to steer a write.
func checkSettingsKeyARN(settings Settings) error {
	if !secrets.ValidAWSKeyARN(settings.KMSKeyARN) {
		return fmt.Errorf("the broker's settings name an invalid KMS key ARN %q (expected arn:aws:kms:<region>:<account>:key/<key id>)", settings.KMSKeyARN)
	}
	region, account := secrets.AWSKeyARNParts(settings.KMSKeyARN)
	if settings.AWSAccountID != account {
		return fmt.Errorf("the broker's settings name account %s, but its KMS key ARN %s is in account %s: the broker's settings are inconsistent", settings.AWSAccountID, settings.KMSKeyARN, account)
	}
	if settings.AWSRegion != region {
		return fmt.Errorf("the broker's settings name region %s, but its KMS key ARN %s is in region %s: the broker's settings are inconsistent", settings.AWSRegion, settings.KMSKeyARN, region)
	}
	return nil
}

// isAccessDenied reports whether err is AWS's AccessDeniedException, which the SDK answers as a
// generic API error: secretsmanager's types declare no type for it.
func isAccessDenied(err error) bool {
	var apiErr smithy.APIError
	return errors.As(err, &apiErr) && apiErr.ErrorCode() == "AccessDeniedException"
}

// awsBackend is the secretBackend for store "" or secrets.StoreAWS: AWS Secrets Manager under the
// AWS sign-in this command's environment gives it, pinned to the key ARN's account and region.
type awsBackend struct {
	settings Settings
	sm       secretsAPI
	st       stsAPI
	// source is where the SDK took the sign-in from (awsSourceOf).
	source awsSource
	// arn is the sign-in requireAccount read, which every form reads first.
	arn string
}

// agentSecrets is an agent-secrets command line for printing. Its sign-in prefix (signInPrefix)
// carries the variables that chose the sign-in, so it reads the store as the identity that wrote
// to it.
func (b *awsBackend) agentSecrets(words ...string) string {
	return pipeCommand(b.signInPrefix(), words...)
}

// awsSource is a source the AWS SDK takes the sign-in from (awsSourceOf): phrase names it in the
// using line and in a refusal, another says how to choose another (chooseAnother), and prefix is
// the shell assignments a printed command carries to sign in as this one did (signInPrefix).
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
// names no key and no token, only the variables and the file's path. The path, and the prefix
// that carries it, print only once STS took the token the file holds (the "using" line and a
// store's refusal follow requireAccount): the file was read and signed in with, so its path is no
// token pasted in its place. A file that cannot be read is refused before then, naming the
// variable alone (webIdentityToken).
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
				prefix += name + "=" + shellWord(value) + " "
			}
		}
		return awsSource{
			phrase:  "the web identity token file AWS_WEB_IDENTITY_TOKEN_FILE names (" + os.Getenv("AWS_WEB_IDENTITY_TOKEN_FILE") + ")" + outranks("outranks"),
			another: "choose another by unsetting AWS_WEB_IDENTITY_TOKEN_FILE, which outranks AWS_PROFILE, or by naming another token file and role in it and AWS_ROLE_ARN",
			prefix:  prefix,
		}
	case profileVar != "":
		source := awsSource{
			phrase:  "the AWS profile " + profile + " (" + profileVar + ")",
			another: "choose another with AWS_PROFILE",
			prefix:  profileVar + "=" + shellWord(profile) + " ",
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

func (b *awsBackend) id(slug string) string { return b.settings.SecretsPrefix + slug }

func (b *awsBackend) displayName(id string) string { return displayName(b.settings.SecretsPrefix, id) }

func (b *awsBackend) requireAccount(ctx context.Context) error {
	arn, err := requireAccount(ctx, b.st, b.settings, b.source.another)
	b.arn = arn
	return err
}

// personEmail answers, for --owner me, the session name of an Identity Center sign-in, lowercased,
// which is the person's email. Any other sign-in - a machine's role, an assumed service role, an
// IAM user - names no person. It reads the ARN requireAccount read, which every form reads before
// it calls anything else.
func (b *awsBackend) personEmail(context.Context) (string, error) {
	m := ssoRole.FindStringSubmatch(b.arn)
	if m == nil {
		return "", fmt.Errorf("%s is no IAM Identity Center sign-in, so it names no person", b.arn)
	}
	return strings.ToLower(m[1]), nil
}

// identity is where the sign-in came from and its ARN, as requireAccount read it.
func (b *awsBackend) identity() string { return b.source.phrase + ": " + b.arn }

// chooseAnother is how a refusal of the store's says to sign in with another AWS credential, by
// the source that signed in (awsSource.another): the variable that outranks it.
func (b *awsBackend) chooseAnother() string { return b.source.another }

// signInPrefix is the shell assignments a printed command carries so it signs in as this one did
// (awsSource.prefix): the profile variable that chose the profile, or the web identity token file
// and role; "" for access keys, which a printed command never carries, and for the rest of the
// SDK's chain.
func (b *awsBackend) signInPrefix() string { return b.source.prefix }

func (b *awsBackend) list(ctx context.Context) ([]secretView, error) {
	views := []secretView{}
	pages := secretsmanager.NewListSecretsPaginator(b.sm, &secretsmanager.ListSecretsInput{
		Filters:                []smtypes.Filter{{Key: smtypes.FilterNameStringTypeName, Values: []string{b.settings.SecretsPrefix}}},
		IncludePlannedDeletion: aws.Bool(true),
	})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list the agent secrets: %w", err)
		}
		for _, e := range page.SecretList {
			id := aws.ToString(e.Name)
			if !strings.HasPrefix(id, b.settings.SecretsPrefix) {
				continue // the name filter matched more than the namespace
			}
			views = append(views, secretView{
				Name: b.displayName(id), SecretName: id,
				HasValue: secrets.HasCurrentVersion(e.SecretVersionsToStages),
				Created:  e.CreatedDate, LastChanged: e.LastChangedDate,
				Deleted: e.DeletedDate, EarliestPurge: earliestPurge(e.DeletedDate),
			}.withTags(secrets.TagMap(e.Tags)))
		}
	}
	return views, nil
}

func (b *awsBackend) show(ctx context.Context, slug string) (secretView, error) {
	out, err := b.sm.DescribeSecret(ctx, &secretsmanager.DescribeSecretInput{SecretId: aws.String(b.id(slug))})
	if err != nil {
		return secretView{}, err
	}
	return secretView{
		Name: b.displayName(aws.ToString(out.Name)), SecretName: aws.ToString(out.Name),
		HasValue: secrets.HasCurrentVersion(out.VersionIdsToStages),
		Created:  out.CreatedDate, LastChanged: out.LastChangedDate,
		Deleted: out.DeletedDate, EarliestPurge: earliestPurge(out.DeletedDate),
		Versions: out.VersionIdsToStages,
	}.withTags(secrets.TagMap(out.Tags)), nil
}

// prepareCreate makes nothing: AWS creates the secret with its value in store.
func (b *awsBackend) prepareCreate(slug string, tags map[string]string) newSecret {
	return &awsNewSecret{b: b, slug: slug, tags: tags}
}

// awsNewSecret is a secret AWS has not made yet: store makes it, with its tags and value. begun,
// which unsettled reads while store may be in flight, is set once store begins the CreateSecret.
type awsNewSecret struct {
	b     *awsBackend
	slug  string
	tags  map[string]string
	begun atomic.Bool
}

func (*awsNewSecret) begin(context.Context) (discardOutcome, error) { return madeNothing, nil }

// store makes the secret with its tags and value, under storeCallDeadline. Its error says whether
// the store may hold the secret: a create whose every attempt the SDK made ended in an answer
// Secrets Manager gives before it acts (attempts.definiteRefusal) says nothing was stored; any
// other, a deadline among them, says the store may hold the secret with its value, or nothing,
// naming the command that tells which.
func (n *awsNewSecret) store(ctx context.Context, value string) error {
	n.begun.Store(true)
	ctx, cancel := withStoreDeadline(ctx)
	defer cancel()
	var attempts awsAttempts
	_, err := n.b.sm.CreateSecret(ctx, &secretsmanager.CreateSecretInput{
		Name:         aws.String(n.b.id(n.slug)),
		KmsKeyId:     aws.String(n.b.settings.KMSKeyARN),
		SecretString: aws.String(value),
		Tags:         secrets.TagList(n.tags),
	}, attempts.record)
	switch {
	case err == nil:
		return nil
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return fmt.Errorf("%w; Secrets Manager did not answer within %s, so %s", err, storeCallDeadlineText(), n.unsettled())
	case attempts.definiteRefusal(err):
		return fmt.Errorf("%w; Secrets Manager refused the create before acting on it, so nothing was stored", err)
	}
	return fmt.Errorf("%w; Secrets Manager's answer leaves open whether it made the secret, so %s", err, n.unsettled())
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

// definiteRefusal reports whether the call err ended may have made nothing for certain: it made
// exactly one attempt, and that attempt ended in an error whose code awsDefiniteRefusals lists. More
// than one attempt is never certain: an attempt the SDK retried may have reached the store, which
// then answers the retry as a refusal (a name taken, by the secret that attempt made). A client
// that runs no SDK middleware, and so records no attempt, is judged by err alone.
func (a *awsAttempts) definiteRefusal(err error) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	switch len(a.errs) {
	case 0:
		return awsDefiniteRefusal(err)
	case 1:
		return awsDefiniteRefusal(a.errs[0])
	}
	return false
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

// discard has nothing to remove: AWS makes the secret only in store.
func (*awsNewSecret) discard(context.Context) (discardOutcome, error) {
	return madeNothing, nil
}

// madeBeforeValue is false: AWS makes the secret only with its value, so nothing waits for it.
func (*awsNewSecret) madeBeforeValue() bool { return false }

// unsettled says what the store may hold should the form end now: nothing before the CreateSecret,
// and from then the secret with its value or nothing, which `secret show` tells apart; never an
// empty secret, which Secrets Manager does not make.
func (n *awsNewSecret) unsettled() string {
	if !n.begun.Load() {
		return "nothing stays in the store, since Secrets Manager makes a secret only with its value"
	}
	name := n.b.displayName(n.b.id(n.slug))
	return fmt.Sprintf("the store may hold %s, made with its value, or no %s at all: `%s` says which, and no empty secret stays to clear, since Secrets Manager makes a secret only with its value", name, name, n.b.agentSecrets("secret", "show", name))
}

func (b *awsBackend) set(ctx context.Context, slug, value string) error {
	_, err := b.sm.PutSecretValue(ctx, &secretsmanager.PutSecretValueInput{
		SecretId: aws.String(b.id(slug)), SecretString: aws.String(value),
	})
	return err
}

func (b *awsBackend) currentTags(ctx context.Context, slug string) (heldTags, error) {
	held, err := b.sm.DescribeSecret(ctx, &secretsmanager.DescribeSecretInput{SecretId: aws.String(b.id(slug))})
	if err != nil {
		return heldTags{}, err
	}
	return heldTags{tags: secrets.TagMap(held.Tags)}, nil
}

// readValue reads the secret's AWSCURRENT value with GetSecretValue, and refuses one that is not a
// non-empty string, as the broker's own read does (secrets.AWS.Read).
func (b *awsBackend) readValue(ctx context.Context, slug string) (string, error) {
	out, err := b.sm.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{SecretId: aws.String(b.id(slug))})
	if err != nil {
		return "", err
	}
	if out.SecretString == nil || *out.SecretString == "" {
		return "", errors.New("its value is not a non-empty string")
	}
	return *out.SecretString, nil
}

// valueReadPermission is what GetSecretValue needs of a person's sign-in: the call itself, and
// the key the value is encrypted under, since Secrets Manager decrypts it as the caller.
func (b *awsBackend) valueReadPermission() string {
	return "secretsmanager:GetSecretValue on the secret and kms:Decrypt on its key"
}

// tagReadPermission is what DescribeSecret needs of a person's sign-in: whoever stores a value
// reads that secret's tags first, so a write grant that does not carry this read fails `secret
// set` before anything is written.
func (b *awsBackend) tagReadPermission() string {
	return "secretsmanager:DescribeSecret on the secret"
}

// setTags writes tags in one TagResource, which keeps every tag it does not name. TagResource takes
// no condition on held, so the write checks its own race afterwards (raceCheck).
func (b *awsBackend) setTags(ctx context.Context, slug string, held heldTags, tags map[string]string) error {
	if err := b.tag(ctx, slug, tags); err != nil {
		return err
	}
	return b.raceCheck(ctx, slug, held)
}

// removeTags removes keys in one UntagResource, which takes no condition on held either, and checks
// its own race afterwards (raceCheck).
func (b *awsBackend) removeTags(ctx context.Context, slug string, held heldTags, keys []string) error {
	if err := b.untag(ctx, slug, keys); err != nil {
		return err
	}
	return b.raceCheck(ctx, slug, held)
}

func (b *awsBackend) tag(ctx context.Context, slug string, tags map[string]string) error {
	_, err := b.sm.TagResource(ctx, &secretsmanager.TagResourceInput{
		SecretId: aws.String(b.id(slug)),
		Tags:     secrets.TagList(tags),
	})
	return err
}

func (b *awsBackend) untag(ctx context.Context, slug string, keys []string) error {
	_, err := b.sm.UntagResource(ctx, &secretsmanager.UntagResourceInput{
		SecretId: aws.String(b.id(slug)),
		TagKeys:  keys,
	})
	return err
}

// raceCheck is how a tag write on AWS, whose tag calls take no condition, answers for the
// github-owners rule when another writer retags the same secret between the write's read (held)
// and the write itself: it reads the tags back, and when the type and github-owners there are a
// pair the broker refuses (policy.GitHubOwnersFor), it answers errTagsChanged wrapping
// errTagsRaced, naming the tags it read, the tags it found and the retag that sets the secret
// right. It writes nothing: the tags it read are no more current than the other writer's, so
// putting them back would take that writer's retag away, and more than one writer is the normal
// case. It answers nil when the pair is one the broker serves.
func (b *awsBackend) raceCheck(ctx context.Context, slug string, held heldTags) error {
	now, err := b.currentTags(ctx, slug)
	if err != nil {
		return fmt.Errorf("the retag was written, but reading its tags back failed, so whether another write left it a type without the GitHub owners it needs is unknown: %w", err)
	}
	secretType, owners := policy.TypeFromTags(now.tags), now.tags[policy.TagGitHubOwners]
	if _, err := policy.GitHubOwnersFor(secretType, owners); err == nil {
		return nil
	}
	name := b.displayName(b.id(slug))
	return fmt.Errorf("%w, leaving type %q with github-owners %q, which the broker refuses; %w. This retag read %s and found %s. Give that type its owners with `%s`, or drop the type with `%s`",
		errTagsChanged, secretType, owners, errTagsRaced, tagPairs(held.tags), tagPairs(now.tags),
		b.agentSecrets("secret", "retag", name, "--type", secretType, "--github-owners", "LOGINS"),
		b.agentSecrets("secret", "retag", name, "--type", policy.TagRemoved))
}

func (b *awsBackend) delete(ctx context.Context, slug string) (*time.Time, error) {
	out, err := b.sm.DeleteSecret(ctx, &secretsmanager.DeleteSecretInput{
		SecretId: aws.String(b.id(slug)), RecoveryWindowInDays: aws.Int64(recoveryWindowDays),
	})
	if err != nil {
		return nil, err
	}
	return out.DeletionDate, nil
}

func (b *awsBackend) restore(ctx context.Context, slug string) error {
	_, err := b.sm.RestoreSecret(ctx, &secretsmanager.RestoreSecretInput{SecretId: aws.String(b.id(slug))})
	return err
}

func (b *awsBackend) isAccessDenied(err error) bool { return isAccessDenied(err) }

// close is a no-op: the AWS SDK's HTTP-based clients hold no persistent connection worth
// releasing explicitly.
func (b *awsBackend) close() error { return nil }

var _ secretBackend = (*awsBackend)(nil)
