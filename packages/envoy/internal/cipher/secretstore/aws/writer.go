// The writer the agent-secrets CLI writes AWS Secrets Manager with: the Secrets Manager calls its
// secret forms make, under the AWS sign-in the command's environment gives it (signin.go),
// whatever principal that is, a person's or a machine's: Secrets Manager's own IAM decides what
// it may read and write. The one check is the one that refuses a read or a write under a sign-in
// in another account than the broker's.

package aws

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go"
	"github.com/aws/smithy-go/middleware"

	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/secretstore"
)

// WriterAPI is the Secrets Manager calls the writer makes: ListSecrets and DescribeSecret to
// read, GetSecretValue for the value a retag decodes (Read), and writes for values, tags and
// deletion. *secretsmanager.Client and awstest.Local satisfy it.
type WriterAPI interface {
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

var (
	_ WriterAPI = (*secretsmanager.Client)(nil)
	_ STSAPI    = (*sts.Client)(nil)
)

// recoveryWindowDays is the recovery window Delete schedules every deletion with, 30 days: the
// longest Secrets Manager allows, during which Restore brings the secret back.
const recoveryWindowDays = 30

// writerSettings are the broker's settings the writer reads: the prefix, this store's fields of
// GET /v1/settings (SettingsFields), and how a field prints in a message (answer,
// secretstore.Settings.Words).
type writerSettings struct {
	SecretsPrefix, KMSKeyARN, AWSAccountID, AWSRegion string
	answer                                            func(string) string
}

// settingsOf is s as the writer reads it, each field by its SettingsFields JSON name.
func settingsOf(s secretstore.Settings) writerSettings {
	return writerSettings{
		SecretsPrefix: s.Prefix, KMSKeyARN: s.Fields["kms_key_arn"], AWSAccountID: s.Fields["aws_account_id"],
		AWSRegion: s.Fields["aws_region"], answer: s.Words,
	}
}

// words is text, a field the broker's settings named, as the broker's address lets its answer
// print; text itself where the settings say nothing of how.
func (s writerSettings) words(text string) string {
	if s.answer == nil {
		return text
	}
	return s.answer(text)
}

// checkSettingsKeyARN refuses, before any AWS call, settings whose aws_account_id or aws_region does
// not match the account and region in kms_key_arn. The broker derives both from the key ARN the same
// way (internal/cipher/api handlers_secrets.go, aws.KeyARNParts), so the account the sign-in is
// checked against (requireAccount) and the region the SDK is pinned to are the key ARN's, not a
// separately-sent field a spoofed or plain-http broker could set on its own to steer a write. Each
// field a refusal names prints as the broker's address lets its answer print (writerSettings.words).
func checkSettingsKeyARN(settings writerSettings) error {
	if !ValidKeyARN(settings.KMSKeyARN) {
		return fmt.Errorf("the broker's settings name an invalid KMS key ARN %s (expected arn:aws:kms:<region>:<account>:key/<key id>)", settings.words(strconv.Quote(settings.KMSKeyARN)))
	}
	region, account := KeyARNParts(settings.KMSKeyARN)
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

// writer is the secretstore.Writer for store "" or Name: AWS Secrets Manager under the AWS
// sign-in the command's environment gives it, pinned to the key ARN's account and region. Its
// list, describe and value reads are the broker's own Store's over the same client, whose
// failures that client words (awsStore, ClientErrors).
type writer struct {
	Store
	settings writerSettings
	sm       WriterAPI
	st       STSAPI
	// source is where the SDK took the sign-in from (awsSourceOf).
	source awsSource
	// arn is the sign-in RequireAccount read, which every form reads first.
	arn string
}

// newWriter is the writer over sm and st, which Hooks.Clients loaded from sources, its reads the
// broker's own Store's over the same Secrets Manager client wrapped so its failures read as the
// CLI words them (awsStore). It reads no KMS alias: the forms never ask a listing's OnKey.
func newWriter(settings writerSettings, sm WriterAPI, st STSAPI, sources []aws.CredentialSource) *writer {
	worded := awsStore{sm}
	return &writer{
		Store:    Store{Client: worded, KeyARN: settings.KMSKeyARN, ClientErrors: true},
		settings: settings, sm: worded, st: st, source: awsSourceOf(sources),
	}
}

// agentSecrets is an agent-secrets command line for printing. Its sign-in prefix (RerunPrefix)
// carries the variables that chose the sign-in, so it reads the store as the identity that wrote
// to it.
func (b *writer) agentSecrets(words ...string) string {
	return secretstore.Command(b.RerunPrefix(), words...)
}

// displayName is the NAME a session asks for the secret named id (policy.DisplayName).
func (b *writer) displayName(id string) string {
	return policy.DisplayName(b.settings.SecretsPrefix, id)
}

// PrepareCreate makes nothing: AWS creates the secret with its value in Store.
func (b *writer) PrepareCreate(id string, tags map[string]string) secretstore.Create {
	return &newSecret{b: b, id: id, tags: tags}
}

// newSecret is a secret AWS has not made yet: Store makes it, with its tags and value. begun,
// which Unsettled reads while Store may be in flight, is set once Store begins the CreateSecret.
type newSecret struct {
	b     *writer
	id    string
	tags  map[string]string
	begun atomic.Bool
}

func (*newSecret) Begin(context.Context) (secretstore.DiscardOutcome, error) {
	return secretstore.MadeNothing, nil
}

// Store makes the secret with its tags and value, under secretstore.CallDeadline. Its error says whether
// the store may hold the secret: a create whose every attempt the SDK made ended in an answer
// Secrets Manager gives before it acts (attempts.definiteRefusal) says nothing was stored; any
// other, a deadline among them, says the store may hold the secret with its value, or nothing,
// naming the command that tells which.
func (n *newSecret) Store(ctx context.Context, value string) error {
	n.begun.Store(true)
	ctx, cancel := secretstore.WithCallDeadline(ctx)
	defer cancel()
	var attempts awsAttempts
	_, err := n.b.sm.CreateSecret(ctx, &secretsmanager.CreateSecretInput{
		Name:         aws.String(n.id),
		KmsKeyId:     aws.String(n.b.settings.KMSKeyARN),
		SecretString: aws.String(value),
		Tags:         TagList(n.tags),
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
func (*newSecret) Discard(context.Context) (secretstore.DiscardOutcome, error) {
	return secretstore.MadeNothing, nil
}

// MadeBeforeValue is false: AWS makes the secret only with its value, so nothing waits for it.
func (*newSecret) MadeBeforeValue() bool { return false }

// Unsettled says what the store may hold should the form end now: nothing before the CreateSecret,
// and from then the secret with its value or nothing, which `secret show` tells apart; never an
// empty secret, which Secrets Manager does not make.
func (n *newSecret) Unsettled() string {
	if !n.begun.Load() {
		return "nothing stays in the store, since Secrets Manager makes a secret only with its value"
	}
	name := n.b.displayName(n.id)
	return fmt.Sprintf("the store may hold %s, made with its value, or no %s at all: `%s` says which, and no empty secret stays to clear, since Secrets Manager makes a secret only with its value", name, name, n.b.agentSecrets("secret", "show", name))
}

func (b *writer) Set(ctx context.Context, id, value string) error {
	_, err := b.sm.PutSecretValue(ctx, &secretsmanager.PutSecretValueInput{
		SecretId: aws.String(id), SecretString: aws.String(value),
	})
	return err
}

// CurrentTags reads the secret's tags with DescribeSecret, and its ARN, the source Read takes.
// Secrets Manager's tag writes take no condition, so the read has no etag.
func (b *writer) CurrentTags(ctx context.Context, id string) (secretstore.HeldTags, error) {
	held, err := b.sm.DescribeSecret(ctx, &secretsmanager.DescribeSecretInput{SecretId: aws.String(id)})
	if err != nil {
		return secretstore.HeldTags{}, err
	}
	return secretstore.HeldTags{Tags: TagMap(held.Tags), Source: aws.ToString(held.ARN)}, nil
}

// ValueReadPermission is what GetSecretValue needs of a person's sign-in: the call itself, and
// the key the value is encrypted under, since Secrets Manager decrypts it as the caller.
func (b *writer) ValueReadPermission() string {
	return "secretsmanager:GetSecretValue on the secret and kms:Decrypt on its key"
}

// TagReadPermission is what DescribeSecret needs of a person's sign-in: whoever stores a value
// reads that secret's tags first, so a write grant that does not carry this read fails `secret
// set` before anything is written.
func (b *writer) TagReadPermission() string {
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

// CheckTagValues refuses, before any write, a tag value AWS would refuse: one longer than
// awsTagValueMax characters, or holding a character that is no Unicode letter, number or space
// separator (a tab or a newline is none) and none of awsTagValuePunctuation. The refusal names the
// tag, the store's limit and, for a character, the first that fails and where, as a flag's
// refusal does, and prints no other part of the value. tags is every tag the write sends. The CLI
// reads each refusal as a usage error: a flag's value is what it refuses.
func (*writer) CheckTagValues(tags map[string]string) error {
	for _, key := range slices.Sorted(maps.Keys(tags)) {
		value := tags[key]
		if n := utf8.RuneCountInString(value); n > awsTagValueMax {
			return fmt.Errorf("the %s tag is %d characters, and AWS Secrets Manager holds at most %d in a tag value: shorten it", key, n, awsTagValueMax)
		}
		for i, r := range value {
			if unicode.IsLetter(r) || unicode.IsNumber(r) || unicode.Is(unicode.Z, r) || strings.ContainsRune(awsTagValuePunctuation, r) {
				continue
			}
			return fmt.Errorf("the %s tag holds %q at character %d, and AWS Secrets Manager takes in a tag value only Unicode letters, digits, spaces and %s: write it without that character", key, string(r), utf8.RuneCountInString(value[:i])+1, strings.Join(strings.Split(awsTagValuePunctuation, ""), " "))
		}
	}
	return nil
}

// SetTags writes tags in one TagResource, which keeps every tag it does not name. TagResource takes
// no condition on held, so the write reads its tags back after it and holds them to check
// (readBack).
func (b *writer) SetTags(ctx context.Context, id string, _ secretstore.HeldTags, tags map[string]string, check secretstore.TagCheck) error {
	if err := b.tag(ctx, id, tags); err != nil {
		return err
	}
	return b.readBack(ctx, id, check)
}

// RemoveTags removes keys in one UntagResource, which takes no condition on held either, and reads
// its tags back after it (readBack).
func (b *writer) RemoveTags(ctx context.Context, id string, _ secretstore.HeldTags, keys []string, check secretstore.TagCheck) error {
	if err := b.untag(ctx, id, keys); err != nil {
		return err
	}
	return b.readBack(ctx, id, check)
}

// tag and untag answer an error that is no definite refusal (awsAttempts.definiteRefusal: every
// attempt ending in a code awsDefiniteRefusals lists) as secretstore.TagsUnknown: a service fault,
// an attempt that ended any other way before a retry, or a lost connection may follow a call the
// store carried out.
func (b *writer) tag(ctx context.Context, id string, tags map[string]string) error {
	var attempts awsAttempts
	_, err := b.sm.TagResource(ctx, &secretsmanager.TagResourceInput{
		SecretId: aws.String(id),
		Tags:     TagList(tags),
	}, attempts.record)
	return tagOutcome(err, &attempts)
}

func (b *writer) untag(ctx context.Context, id string, keys []string) error {
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
func (b *writer) readBack(ctx context.Context, id string, check secretstore.TagCheck) error {
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
func (b *writer) Delete(ctx context.Context, id string) (time.Time, error) {
	out, err := b.sm.DeleteSecret(ctx, &secretsmanager.DeleteSecretInput{
		SecretId: aws.String(id), RecoveryWindowInDays: aws.Int64(recoveryWindowDays),
	})
	if err != nil {
		return time.Time{}, err
	}
	return aws.ToTime(out.DeletionDate), nil
}

func (b *writer) Restore(ctx context.Context, id string) error {
	_, err := b.sm.RestoreSecret(ctx, &secretsmanager.RestoreSecretInput{SecretId: aws.String(id)})
	return err
}

// AccessDenied is AWS's AccessDeniedException (isAccessDenied).
func (b *writer) AccessDenied(err error) bool { return isAccessDenied(err) }

// DefiniteRefusal is an answer Secrets Manager gives before it acts (awsDefiniteRefusal).
func (b *writer) DefiniteRefusal(err error) bool { return awsDefiniteRefusal(err) }

// MissingPermission is the IAM action AWS's message says err's refusal lacks
// (awsMissingPermission), "" for an error that is no AWS API error or names none.
func (b *writer) MissingPermission(err error) string {
	var api smithy.APIError
	if !errors.As(err, &api) {
		return ""
	}
	return awsMissingPermission(api.ErrorMessage())
}

// Close is a no-op: the AWS SDK's HTTP-based clients hold no persistent connection worth
// releasing explicitly.
func (b *writer) Close() error { return nil }

var (
	_ secretstore.Writer       = (*writer)(nil)
	_ secretstore.AccountGuard = (*writer)(nil)
)
