// The AWS writer's sign-in: the credential chain the AWS SDK reads from the command's
// environment, which source it took the sign-in from (awsSourceOf), and the check that refuses a
// sign-in in another account than the broker's (requireAccount).

package aws

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	sdkconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go/logging"

	"github.com/sjawhar/envoy/internal/cipher/secretstore"
	"github.com/sjawhar/envoy/internal/redact"
)

// STSAPI is the one STS call the writer makes: whose sign-in this is.
type STSAPI interface {
	GetCallerIdentity(ctx context.Context, in *sts.GetCallerIdentityInput, o ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error)
}

// loadClients loads the AWS sign-in the command's environment gives it, pinned to the broker's
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
// writes, among them - as the GCP writer discards Secret Manager's. It is Hooks.Clients unless a
// test swaps it (SwapHooks).
func loadClients(ctx context.Context, region string) (WriterAPI, STSAPI, []aws.CredentialSource, error) {
	options := []func(*sdkconfig.LoadOptions) error{sdkconfig.WithRegion(region), sdkconfig.WithLogger(logging.Nop{})}
	if file := awsWebIdentityTokenFile(); file != "" {
		if _, err := file.GetIdentityToken(); err != nil {
			return nil, nil, nil, fmt.Errorf("load your AWS sign-in: %w", err)
		}
		options = append(options, sdkconfig.WithWebIdentityRoleCredentialOptions(func(o *stscreds.WebIdentityRoleOptions) {
			o.TokenRetriever = file
		}))
	}
	cfg, err := sdkconfig.LoadDefaultConfig(ctx, options...)
	var missing sdkconfig.SharedConfigProfileNotExistError
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
		if shared, ok := source.(sdkconfig.SharedConfig); ok && shared.Profile != "" {
			return true
		}
	}
	return false
}

// ssoRole matches an IAM Identity Center sign-in and captures its session name. The deployment
// repository's Identity Center permission sets map the session name to the person's userName,
// which is their email: --owner me reads it there (writer.PersonEmail).
var ssoRole = regexp.MustCompile(`^arn:aws[a-z-]*:sts::[0-9]{12}:assumed-role/AWSReservedSSO_[^/]+/(.+)$`)

// requireAccount refuses, before any read or write, a sign-in in an account other than the
// broker's, naming both accounts and how to choose another sign-in (another, awsSource.another): a
// read there would list another account's secrets as if they were the agent secrets. It answers
// the sign-in's ARN.
func requireAccount(ctx context.Context, st STSAPI, settings writerSettings, another string) (arn string, err error) {
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

// awsSourceOf is the source the AWS SDK takes the sign-in loadClients loads from, in the SDK's own
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

// RequireAccount refuses a sign-in whose account does not hold the agent secrets (requireAccount),
// and keeps the ARN it read for PersonEmail and Identity.
func (b *writer) RequireAccount(ctx context.Context) error {
	arn, err := requireAccount(ctx, b.st, b.settings, b.source.another)
	b.arn = arn
	return err
}

// PersonEmail answers, for --owner me, the session name of an Identity Center sign-in, lowercased,
// which is the person's email. Any other sign-in - a machine's role, an assumed service role, an
// IAM user - names no person. It reads the ARN RequireAccount read, which every form reads before
// it calls anything else.
func (b *writer) PersonEmail(context.Context) (string, error) {
	m := ssoRole.FindStringSubmatch(b.arn)
	if m == nil {
		return "", fmt.Errorf("%s is no IAM Identity Center sign-in, so it names no person", b.arn)
	}
	return strings.ToLower(m[1]), nil
}

// Identity is where the sign-in came from and its ARN, as RequireAccount read it.
func (b *writer) Identity() string { return b.source.phrase + ": " + b.arn }

// ChooseAnother is how a refusal of the store's says to sign in with another AWS credential, by
// the source that signed in (awsSource.another): the variable that outranks it.
func (b *writer) ChooseAnother() string { return b.source.another }

// RerunPrefix is the shell assignments a printed command carries so it signs in as this one did
// (awsSource.prefix): the profile variable that chose the profile, or the web identity token file
// and role; "" for access keys, which a printed command never carries, and for the rest of the
// SDK's chain.
func (b *writer) RerunPrefix() string { return b.source.prefix }
