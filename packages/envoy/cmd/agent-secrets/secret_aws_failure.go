// packages/envoy/cmd/agent-secrets/secret_aws_failure.go
//
// How the AWS forms word a failure of the AWS SDK's and of AWS's: a sign-in it could not load or
// read, a store call AWS refused, and a profile the shared files do not hold. The SDK's own text
// quotes what it was configured with and AWS's message is text whoever answers writes, so each
// prints in words of this client's own, naming settings, never their values.
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

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/ssocreds"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/smithy-go"

	"github.com/sjawhar/envoy/internal/redact"
)

// missingAWSProfile is the refusal of variable naming a profile the shared config and credentials
// files do not hold. The profile is not printed, though the AWS SDK's own error quotes it: a
// profile the files do not hold is the caller's text, and a variable meant for a name is where a
// pasted credential lands. A profile they hold is a label, and the using line prints it, so the
// refusal lists the profiles they hold (heldAWSProfiles), as the GCP refusals list the held
// configurations and logins.
func missingAWSProfile(variable string) error {
	held := "none"
	if profiles := heldAWSProfiles(); len(profiles) > 0 {
		held = strings.Join(profiles, ", ")
	}
	return fmt.Errorf("load your AWS sign-in: %s names a profile the shared config and credentials files do not hold (not printed); they hold: %s", variable, held)
}

// awsProfileName is the shape of a profile name the refusal lists: a label of letters, digits and
// the punctuation AWS's own examples use. A section of another shape is not listed: the files are
// the person's own, but a line that is no profile header is no label of theirs.
var awsProfileName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@+-]{0,127}$`)

// heldAWSProfiles is the profiles the shared config and credentials files hold, sorted and each
// once: the files AWS_CONFIG_FILE and AWS_SHARED_CREDENTIALS_FILE name, else the SDK's defaults
// (~/.aws/config and ~/.aws/credentials), read by their section headers as the SDK reads them - a
// config file's [default] and [profile <name>], a credentials file's [<name>]. A file that cannot
// be read holds none.
func heldAWSProfiles() []string {
	configFile, credentialsFile := os.Getenv("AWS_CONFIG_FILE"), os.Getenv("AWS_SHARED_CREDENTIALS_FILE")
	if configFile == "" {
		configFile = config.DefaultSharedConfigFilename()
	}
	if credentialsFile == "" {
		credentialsFile = config.DefaultSharedCredentialsFilename()
	}
	var held []string
	for _, file := range []struct {
		path   string
		config bool
	}{{configFile, true}, {credentialsFile, false}} {
		data, err := os.ReadFile(file.path)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			section, ok := strings.CutPrefix(strings.TrimSpace(line), "[")
			if section, ok = strings.CutSuffix(section, "]"); !ok {
				continue
			}
			name := strings.TrimSpace(section)
			if file.config && name != "default" {
				if name, ok = strings.CutPrefix(name, "profile "); !ok {
					continue
				}
				name = strings.TrimSpace(name)
			}
			if awsProfileName.MatchString(name) {
				held = append(held, name)
			}
		}
	}
	slices.Sort(held)
	return slices.Compact(held)
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
	if expired, answered := awsSSOSession(err); expired {
		return redact.Masked(err, call+" failed: the IAM Identity Center (SSO) session has expired or is invalid"+answered+": run `aws sso login` again")
	}
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
// alone, where it is a code this client names (awsCodes), and the permission AWS's message says is
// missing, where it names one in the shape AWS writes it (awsMissingPermission); never the rest of
// AWS's message.
func awsAPIWords(err error) (string, bool) {
	var api smithy.APIError
	if !errors.As(err, &api) {
		return "", false
	}
	words := "AWS refused it: an AWS error (its code is none this client names, and AWS's message is not printed)"
	if code := api.ErrorCode(); awsCodes[code] {
		words = "AWS refused it: " + code + " (AWS's message is not printed)"
		if permission := awsMissingPermission(api.ErrorMessage()); permission != "" {
			words = "AWS refused it: " + code + " (missing permission: " + permission + "; AWS's message is not printed)"
		}
	}
	return words, true
}

// awsPermission is how AWS's message names the IAM action a refusal lacks: "is not authorized to
// perform: <service>:<Action>", the service a prefix of lower-case letters, digits and dashes, the
// action a word that starts with a capital letter, as every IAM action does.
var awsPermission = regexp.MustCompile(`not authorized to perform: ([a-z0-9-]{1,64}:[A-Z][A-Za-z0-9]{0,127})(?:[\s.,;)]|$)`)

// awsMissingPermission is the IAM action message, AWS's own text, says the caller is not
// authorized to perform, where it names one in AWS's shape (awsPermission), else "". Nothing else
// of the message prints: it can name an ARN, and whoever answers at the store's address writes
// it, but an action's name is no secret, and a person must know which one to ask for.
func awsMissingPermission(message string) string {
	if match := awsPermission.FindStringSubmatch(message); match != nil {
		return match[1]
	}
	return ""
}

// awsCodes are the AWS error codes a message prints by name: the ones the forms act on
// (awsDefiniteRefusals, isAccessDenied), and the ones STS, IAM Identity Center, KMS and Secrets
// Manager answer that the docs name. A code is the answering server's text, and whoever answers
// at AWS_ENDPOINT_URL chooses it, so any other code prints as "an AWS error" (awsAPIWords), as
// redact.Answered prints only an OAuth or Google code it lists.
var awsCodes = func() map[string]bool {
	codes := map[string]bool{}
	for _, code := range append(slices.Clone(awsDefiniteRefusals),
		"AccessDenied", "ExpiredToken", "InvalidIdentityToken", "IDPCommunicationError", "IDPRejectedClaim",
		"RegionDisabledException", "SignatureDoesNotMatch", "MalformedPolicyDocument", "PackedPolicyTooLarge",
		"ThrottlingException", "Throttling", "TooManyRequestsException", "RequestLimitExceeded",
		"KMSAccessDeniedException", "KMSDisabledException", "KMSInvalidStateException", "KMSNotFoundException",
		"InternalServiceError", "InternalFailure", "InternalServiceErrorException", "ServiceUnavailable", "ServiceUnavailableException",
		"InvalidGrantException", "InvalidClientException", "UnauthorizedException", "UnauthorizedClientException",
	) {
		codes[code] = true
	}
	return codes
}()

// awsSDKWords is err, the AWS SDK's failure to sign in, as a message prints it. The SDK's own
// error text quotes what it was configured with: the path of a shared config or credentials file
// twice ("failed to load shared config file, <path>, read all: read <path>: ..."), the path
// AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE names, an endpoint's or a container's credentials URL
// whole, with its user, password and query (net/http's *url.Error, and the SDK's own "Failed to
// parse uri" where it does not parse), the value of a setting it cannot read (AWS_MAX_ATTEMPTS),
// and a service's own message, which the service writes what it likes in. So it prints, in this
// order, so an IAM Identity Center refusal is said as the session it is, not as AWS's code:
//   - an IAM Identity Center session that has expired or is refused (awsSSOSession) as the SDK's
//     own fixed words, with the code IAM Identity Center answered where it is one this client
//     names;
//   - a web identity sign-in with no role as the setting it lacks (the SDK's "role ARN is not
//     set");
//   - an AWS API error by its code alone, where it is one this client names (awsAPIWords);
//   - a file call by the setting that names the file, and the call's operation and the system's
//     reason (redact.File): the SDK formats a file's error with %v, which drops the file call's
//     own error from the chain, so the file it names (SharedConfigLoadError.Filename), or the
//     file an AWS file setting names that cannot be read, is read again here for the reason;
//   - a request that failed by its class (redact.Class), naming the AWS address settings that are
//     set, never their values;
//   - anything else in fixed words that say where to look: the AWS settings the SDK's text names
//     and the address settings that are set (awsSettingsNamed), else the AWS_* variables.
func awsSDKWords(err error) string {
	if expired, answered := awsSSOSession(err); expired {
		return "the IAM Identity Center (SSO) session has expired or is invalid" + answered + ": run `aws sso login` again"
	}
	if strings.Contains(err.Error(), "role ARN is not set") {
		return "the web identity sign-in names no role to assume: set AWS_ROLE_ARN, or role_arn in the profile, beside its token file"
	}
	if words, ok := awsAPIWords(err); ok {
		return words
	}
	var load config.SharedConfigLoadError
	var path *fs.PathError
	var request *url.Error
	switch {
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
	check := "the AWS_* variables"
	if named := awsSettingsNamed(err); len(named) > 0 {
		check = strings.Join(named, ", ")
	}
	return "the AWS SDK could not sign in from this environment (its message is not printed: it can quote an AWS setting's value); check " + check + " and ~/.aws/config"
}

// awsSSOSession reports whether err is an IAM Identity Center sign-in the SDK could not use: a
// legacy profile's expired or unreadable token (ssocreds.InvalidTokenError), an sso_session
// profile's token it cannot refresh (the SDK's own "cannot be refreshed"), or IAM Identity
// Center's refusal of the refresh or of the role's credentials (an API error of the SSO OIDC or
// SSO service: InvalidGrantException, ExpiredTokenException, UnauthorizedException), which the
// chain holds beneath the operation that needed the credential (STS's, or a store call's).
// answered is " (IAM Identity Center answered <code>)" for a refusal whose code this client
// names, else "".
func awsSSOSession(err error) (expired bool, answered string) {
	var invalid *ssocreds.InvalidTokenError
	if errors.As(err, &invalid) || strings.Contains(err.Error(), "cached SSO token is expired, or not present, and cannot be refreshed") {
		return true, ""
	}
	for _, link := range errorChain(err) {
		op, ok := link.(*smithy.OperationError)
		if !ok || (op.Service() != "SSO OIDC" && op.Service() != "SSO") {
			continue
		}
		var api smithy.APIError
		if !errors.As(op, &api) {
			return false, ""
		}
		if code := api.ErrorCode(); awsCodes[code] {
			return true, " (IAM Identity Center answered " + code + ")"
		}
		return true, ""
	}
	return false, ""
}

// errorChain is err and every error it wraps, depth first, through Unwrap() error and
// Unwrap() []error alike.
func errorChain(err error) []error {
	if err == nil {
		return nil
	}
	chain := []error{err}
	switch wrapped := err.(type) {
	case interface{ Unwrap() error }:
		chain = append(chain, errorChain(wrapped.Unwrap())...)
	case interface{ Unwrap() []error }:
		for _, each := range wrapped.Unwrap() {
			chain = append(chain, errorChain(each)...)
		}
	}
	return chain
}

// awsVariableName is the shape of an AWS setting's name as the SDK's text names it.
var awsVariableName = regexp.MustCompile(`\bAWS_[A-Z0-9_]{1,64}\b`)

// awsSettingsNamed is the AWS settings a sign-in failure whose own text is not printed names, so
// the line says where to look: each AWS variable the SDK's text names and the environment sets -
// the SDK quotes a value it cannot read beside its variable's name ("invalid value
// AWS_MAX_ATTEMPTS=<value>") - then each address setting that is set (awsAddressSettings), since
// the SDK quotes an address that does not parse without naming its setting ("Failed to parse uri:
// <value>"). Names alone, never a value: a name prints only where it is a variable the environment
// sets, so no part of a value the text quotes passes as one.
func awsSettingsNamed(err error) []string {
	var named []string
	for _, name := range append(awsVariableName.FindAllString(err.Error(), -1), awsAddressSettings...) {
		if os.Getenv(name) != "" && !slices.Contains(named, name) {
			named = append(named, name)
		}
	}
	return named
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
	for _, setting := range []string{"AWS_CONFIG_FILE", "AWS_SHARED_CREDENTIALS_FILE", "AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE", "AWS_WEB_IDENTITY_TOKEN_FILE"} {
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

// awsAddressSettings are the settings that name an address the AWS SDK calls: an endpoint, or a
// container's credentials endpoint.
var awsAddressSettings = []string{"AWS_ENDPOINT_URL", "AWS_ENDPOINT_URL_STS", "AWS_ENDPOINT_URL_SECRETS_MANAGER", "AWS_CONTAINER_CREDENTIALS_FULL_URI", "AWS_CONTAINER_CREDENTIALS_RELATIVE_URI"}

// awsAddressesSet names the AWS address settings that are set, for a request that failed: one of
// them may name the wrong address. It names the settings alone, never their values, which can
// carry a user, a password or a token in their query.
func awsAddressesSet() string {
	var set []string
	for _, setting := range awsAddressSettings {
		if os.Getenv(setting) != "" {
			set = append(set, setting)
		}
	}
	if len(set) == 0 {
		return ""
	}
	return " (the address is not printed; " + strings.Join(set, ", ") + " is set)"
}
