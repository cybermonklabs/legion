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
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	smtypes "github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go"
	"github.com/aws/smithy-go/middleware"

	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/secrets"
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

// awsClients loads the AWS sign-in this command's environment gives it (AWS_PROFILE, else
// AWS_DEFAULT_PROFILE, else the default chain) pinned to the broker's region. The SDK reads the
// profile from the environment itself, so this takes none. Tests replace it.
var awsClients = func(ctx context.Context, region string) (secretsAPI, stsAPI, error) {
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
	if err != nil {
		return nil, nil, fmt.Errorf("load your AWS sign-in: %w", err)
	}
	return secretsmanager.NewFromConfig(cfg), sts.NewFromConfig(cfg), nil
}

// ssoRole matches an IAM Identity Center sign-in and captures its session name. The deployment
// repository's Identity Center permission sets map the session name to the person's userName,
// which is their email: --owner me reads it there (awsBackend.personEmail).
var ssoRole = regexp.MustCompile(`^arn:aws[a-z-]*:sts::[0-9]{12}:assumed-role/AWSReservedSSO_[^/]+/(.+)$`)

// requireAccount refuses, before any read or write, a sign-in in an account other than the
// broker's, naming both accounts: a read there would list another account's secrets as if they
// were the agent secrets. It answers the sign-in's ARN.
func requireAccount(ctx context.Context, st stsAPI, settings Settings) (arn string, err error) {
	id, err := st.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return "", fmt.Errorf("read your AWS sign-in: %w", err)
	}
	arn, account := aws.ToString(id.Arn), aws.ToString(id.Account)
	if account != settings.AWSAccountID {
		return "", fmt.Errorf("your AWS sign-in %s is in account %s, but the agent secrets are in account %s: sign in to account %s (AWS_PROFILE names the sign-in)",
			arn, account, settings.AWSAccountID, settings.AWSAccountID)
	}
	return arn, nil
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
	// source names where the SDK took the sign-in from (awsProfileSource).
	source string
	// arn is the sign-in requireAccount read, which every form reads first.
	arn string
}

// agentSecrets is an agent-secrets command line for printing. Its sign-in prefix (signInPrefix)
// carries the AWS_PROFILE the SDK read, so it reads the store as the identity that wrote to it.
func (b *awsBackend) agentSecrets(words ...string) string {
	return pipeCommand(b.signInPrefix(), words...)
}

// awsProfileSource names where the AWS SDK takes the sign-in awsClients loads from, in the SDK's
// own order: the profile AWS_PROFILE or AWS_DEFAULT_PROFILE names, else its default credential
// chain (the environment's keys, web identity, the default profile, the instance's role).
func awsProfileSource() string {
	for _, variable := range []string{"AWS_PROFILE", "AWS_DEFAULT_PROFILE"} {
		if named := os.Getenv(variable); named != "" {
			return "the AWS profile " + named + " (" + variable + ")"
		}
	}
	return "the AWS SDK's default credential chain"
}

func (b *awsBackend) id(slug string) string { return b.settings.SecretsPrefix + slug }

func (b *awsBackend) displayName(id string) string { return displayName(b.settings.SecretsPrefix, id) }

func (b *awsBackend) requireAccount(ctx context.Context) error {
	arn, err := requireAccount(ctx, b.st, b.settings)
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
func (b *awsBackend) identity() string { return b.source + ": " + b.arn }

// chooseAnother is how a refusal of the store's says to sign in with another AWS credential.
func (*awsBackend) chooseAnother() string { return "AWS_PROFILE" }

// signInPrefix is the AWS_PROFILE a printed command carries so it signs in as this one did, or ""
// when the SDK's default chain, not a named profile, chose the sign-in.
func (b *awsBackend) signInPrefix() string {
	for _, variable := range []string{"AWS_PROFILE", "AWS_DEFAULT_PROFILE"} {
		if named := os.Getenv(variable); named != "" {
			return variable + "=" + shellWord(named) + " "
		}
	}
	return ""
}

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

// raceCheck is how a tag write on AWS, whose tag calls take no condition, keeps the github-owners
// rule when another writer retags the same secret between the write's read (held) and the write
// itself: it reads the tags back, and when the kind and github-owners there are a pair the broker
// refuses (policy.GitHubOwnersFor), it restores held's tags and answers errTagsChanged wrapping
// errTagsRestored, naming what it found. A retag on AWS is therefore safe only with one writer at
// a time: the restore takes back the other writer's change too. It answers nil when the pair is
// one the broker serves.
func (b *awsBackend) raceCheck(ctx context.Context, slug string, held heldTags) error {
	now, err := b.currentTags(ctx, slug)
	if err != nil {
		return fmt.Errorf("the retag was written, but reading its tags back failed, so whether another write left it a kind without the GitHub owners it needs is unknown: %w", err)
	}
	kind, owners := now.tags[policy.TagKind], now.tags[policy.TagGitHubOwners]
	if _, err := policy.GitHubOwnersFor(kind, owners); err == nil {
		return nil
	}
	raced := fmt.Errorf("%w, leaving kind %q with github-owners %q, which the broker refuses; a retag on AWS is safe only with one writer at a time", errTagsChanged, kind, owners)
	var extra []string
	for key := range now.tags {
		if _, ok := held.tags[key]; !ok {
			extra = append(extra, key)
		}
	}
	slices.Sort(extra)
	if len(held.tags) > 0 {
		err = b.tag(ctx, slug, held.tags)
	}
	if err == nil && len(extra) > 0 {
		err = b.untag(ctx, slug, extra)
	}
	if err != nil {
		return fmt.Errorf("%w; restoring the tags this retag read failed: %w; set them again once the other writer is done", raced, err)
	}
	return fmt.Errorf("%w: %w", raced, errTagsRestored)
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
