// Package aws is the secret store over AWS Secrets Manager, with the agent-secrets key in AWS KMS:
// Store, the broker's reads, and Backend, its registry entry. Its fake of the Secrets Manager and
// KMS calls is awstest.Local.
package aws

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"

	"github.com/sjawhar/envoy/internal/cipher/secretstore"
)

// Name is BROKER_SECRETS_STORE's value for AWS Secrets Manager, with the agent-secrets key in AWS
// KMS.
const Name = "aws-secrets-manager"

// awsKeyARNPattern is a KMS key's ARN, whose last segment is the key id.
var awsKeyARNPattern = regexp.MustCompile(`^arn:aws[a-z-]*:kms:[a-z0-9-]+:[0-9]{12}:key/[A-Za-z0-9-]+$`)

// ValidKeyARN reports whether arn names a KMS key by its key ARN, the form Store.KeyARN takes.
func ValidKeyARN(arn string) bool {
	return awsKeyARNPattern.MatchString(arn)
}

// KeyARNParts answers the region and account of a key ARN ValidKeyARN accepts,
// arn:aws:kms:<region>:<account>:key/<key id>: the region and account every agent secret lives in.
func KeyARNParts(arn string) (region, account string) {
	parts := strings.Split(arn, ":")
	return parts[3], parts[4]
}

// currentStage is the staging label GetSecretValue reads when it names no version.
const currentStage = "AWSCURRENT"

// HasCurrentVersion reports whether a secret's version stages (ListSecrets'
// SecretVersionsToStages, DescribeSecret's VersionIdsToStages) name a version labelled AWSCURRENT,
// the one GetSecretValue reads: the broker serves a secret only then, and the agent-secrets CLI
// shows it as having a value only then.
func HasCurrentVersion(versionsToStages map[string][]string) bool {
	return currentVersion(versionsToStages) != ""
}

// currentVersion is the id of the version a secret's version stages (ListSecrets'
// SecretVersionsToStages, DescribeSecret's VersionIdsToStages) label AWSCURRENT, the one
// GetSecretValue reads, and "" when none carries the label: a Listing's Version, and whether it
// has a current value at all (HasCurrentVersion).
func currentVersion(versionsToStages map[string][]string) string {
	for id, stages := range versionsToStages {
		if slices.Contains(stages, currentStage) {
			return id
		}
	}
	return ""
}

// SecretsManagerAPI is the three Secrets Manager calls the AWS store reads with: ListSecrets for
// List, DescribeSecret for Describe and GetSecretValue for Read. Secrets Manager's own client and
// awstest.Local both implement it.
type SecretsManagerAPI interface {
	secretsmanager.ListSecretsAPIClient
	DescribeSecret(ctx context.Context, in *secretsmanager.DescribeSecretInput, opts ...func(*secretsmanager.Options)) (*secretsmanager.DescribeSecretOutput, error)
	GetSecretValue(ctx context.Context, in *secretsmanager.GetSecretValueInput, opts ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error)
}

// KMSAPI is the KMS call the AWS store reads with: ListAliases, for a secret that names the
// agent-secrets key by an alias. KMS's own client and awstest.Local both implement it.
type KMSAPI interface {
	kms.ListAliasesAPIClient
}

// Store is the store over AWS Secrets Manager: from Secrets Manager and KMS themselves in
// production, from an awstest.Local in tests.
type Store struct {
	// Client lists, describes and reads the secrets.
	Client SecretsManagerAPI
	// KMS resolves the aliases that point at KeyARN, read only when a secret names its key by an
	// alias: Secrets Manager reports a secret's key in whatever form it was set with.
	KMS KMSAPI
	// KeyARN is the agent-secrets key's ARN (BROKER_SECRETS_KMS_KEY_ARN); a secret encrypted with
	// any other key is not OnKey.
	KeyARN string
}

// awsListLag is how stale ListSecrets may be: AWS documents that it "might not reflect changes from
// the last five minutes" (ListSecrets API reference). DescribeSecret answers a write at once.
const awsListLag = 5 * time.Minute

// ListLag is ListSecrets' documented lag, five minutes.
func (Store) ListLag() time.Duration { return awsListLag }

// maxSecretNameLength is the longest name Secrets Manager gives a secret. DescribeSecret answers a
// longer SecretId as invalid input (ValidationException, or InvalidParameterException once it is
// long enough), not as a secret it does not hold, so a caller would take it for a failed read.
const maxSecretNameLength = 512

// ValidName refuses a name past Secrets Manager's 512-character limit. Secrets Manager names are
// ASCII, so the length counts bytes.
func (a Store) ValidName(id string) error {
	if len(id) > maxSecretNameLength {
		return fmt.Errorf("under the prefix it makes a %d-character secret name, past Secrets Manager's %d-character limit", len(id), maxSecretNameLength)
	}
	return nil
}

// List lists every secret under prefix with ListSecrets' name filter, reading every page, and
// keeps those whose name starts with prefix byte for byte, whatever the filter matched. Whether a
// secret has a value, and the id of the version that holds it, are read from the same listing's
// SecretVersionsToStages, with no call per secret; ListSecrets leaves out a secret scheduled for
// deletion unless opts.IncludeDeleted asks it to include one (IncludePlannedDeletion). A
// listing's OnKey reads the key's aliases at most once per List, the first time it is asked about
// a secret that names its key by one, so a secret the policy refuses for its name or tags costs
// no KMS call. A failed Secrets Manager call fails the whole List.
func (a Store) List(ctx context.Context, prefix string, opts secretstore.ReadOptions) ([]secretstore.Listing, error) {
	var listings []secretstore.Listing
	keys := a.keys()
	in := &secretsmanager.ListSecretsInput{
		Filters: []types.Filter{{Key: types.FilterNameStringTypeName, Values: []string{prefix}}},
	}
	if opts.IncludeDeleted {
		in.IncludePlannedDeletion = aws.Bool(true)
	}
	pages := secretsmanager.NewListSecretsPaginator(a.Client, in)
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list secrets under %s: %w", prefix, err)
		}
		for _, e := range page.SecretList {
			if !strings.HasPrefix(aws.ToString(e.Name), prefix) {
				continue
			}
			listings = append(listings, a.listing(awsSecret{
				name: e.Name, arn: e.ARN, kmsKeyID: e.KmsKeyId, tags: e.Tags, versions: e.SecretVersionsToStages,
				created: e.CreatedDate, lastChanged: e.LastChangedDate, deleted: e.DeletedDate,
			}, keys, opts))
		}
	}
	return listings, nil
}

// Describe reads one secret with DescribeSecret, which is read-after-write consistent where
// ListSecrets may lag a change by minutes: ErrNotFound for one Secrets Manager does not hold, and,
// unless opts.IncludeDeleted asks for it, for one scheduled for deletion, which DescribeSecret
// still answers with its DeletedDate. Its version stages (VersionIdsToStages) answer whether it
// has a value and the version holding it.
func (a Store) Describe(ctx context.Context, id string, opts secretstore.ReadOptions) (secretstore.Listing, error) {
	out, err := a.Client.DescribeSecret(ctx, &secretsmanager.DescribeSecretInput{SecretId: aws.String(id)})
	var notFound *types.ResourceNotFoundException
	switch {
	case errors.As(err, &notFound):
		return secretstore.Listing{}, secretstore.ErrNotFound
	case err != nil:
		return secretstore.Listing{}, fmt.Errorf("describe %s: %w", id, err)
	case out.DeletedDate != nil && !opts.IncludeDeleted:
		return secretstore.Listing{}, secretstore.ErrNotFound
	}
	return a.listing(awsSecret{
		name: out.Name, arn: out.ARN, kmsKeyID: out.KmsKeyId, tags: out.Tags, versions: out.VersionIdsToStages,
		created: out.CreatedDate, lastChanged: out.LastChangedDate, deleted: out.DeletedDate,
	}, a.keys(), opts), nil
}

// awsSecret is what ListSecrets and DescribeSecret both answer of one secret, which listing reads.
type awsSecret struct {
	name, arn, kmsKeyID           *string
	tags                          []types.Tag
	versions                      map[string][]string
	created, lastChanged, deleted *time.Time
}

// listing is the Listing of s, its OnKey over keys, with its versions when opts asks for them.
func (a Store) listing(s awsSecret, keys *keyIdentifiers, opts secretstore.ReadOptions) secretstore.Listing {
	version := currentVersion(s.versions)
	tags := TagMap(s.tags)
	ls := secretstore.Listing{
		ID: aws.ToString(s.name), Source: aws.ToString(s.arn), Tags: tags,
		OnKey: a.onKey(aws.ToString(s.kmsKeyID), keys), HasCurrentValue: version != "", Version: version,
		Created: aws.ToTime(s.created), Unfilled: unfilled(tags, s.versions),
		LastChanged: aws.ToTime(s.lastChanged), Deleted: aws.ToTime(s.deleted),
	}
	if s.deleted != nil {
		ls.EarliestPurge = s.deleted.AddDate(0, 0, minRecoveryWindowDays)
	}
	if opts.Versions {
		ls.Versions = maps.Clone(s.versions)
		if ls.Versions == nil {
			ls.Versions = map[string][]string{}
		}
	}
	return ls
}

// minRecoveryWindowDays is the shortest recovery window Secrets Manager allows a deletion, 7 days.
// Neither DescribeSecret nor ListSecrets answers the window a delete used, which can be anything
// from 7 to 30 days, so the earliest a secret scheduled for deletion can be gone is its
// DeletedDate plus this minimum (Listing.EarliestPurge): a reader told that date never waits past
// the purge to restore it.
const minRecoveryWindowDays = 7

// unfilled reports whether a Secrets Manager secret with tags and versions is Listing.Unfilled:
// it carries the CreateLabel tag and holds no version at all.
func unfilled(tags map[string]string, versions map[string][]string) bool {
	_, labelled := tags[secretstore.CreateLabel]
	return labelled && len(versions) == 0
}

// TagMap is a Secrets Manager secret's tags as a map, the last value of a repeated key winning.
func TagMap(tags []types.Tag) map[string]string {
	m := make(map[string]string, len(tags))
	for _, t := range tags {
		m[aws.ToString(t.Key)] = aws.ToString(t.Value)
	}
	return m
}

// TagList is tags as Secrets Manager's tag list, in key order: the inverse of TagMap.
func TagList(tags map[string]string) []types.Tag {
	list := make([]types.Tag, 0, len(tags))
	for _, key := range slices.Sorted(maps.Keys(tags)) {
		list = append(list, types.Tag{Key: aws.String(key), Value: aws.String(tags[key])})
	}
	return list
}

// keyIdentifiers are the forms a secret's KmsKeyId may name the agent-secrets key in: its ARN, its
// key id, or one of the aliases that currently point at it (read once a List's or Describe's
// OnKey first meets an alias).
type keyIdentifiers struct {
	arn, id string
	aliases map[string]bool
}

// keys is the agent-secrets key's identifiers before any alias is read, shared by every listing of
// one List or Describe.
func (a Store) keys() *keyIdentifiers {
	return &keyIdentifiers{arn: a.KeyARN, id: a.KeyARN[strings.LastIndex(a.KeyARN, "/")+1:]}
}

// onKey answers a listing's OnKey: whether kmsKeyID, a secret's KmsKeyId as Secrets Manager lists
// it, names the agent-secrets key. An empty one is the AWS-managed key.
func (a Store) onKey(kmsKeyID string, keys *keyIdentifiers) func(context.Context) (bool, error) {
	return func(ctx context.Context) (bool, error) {
		switch {
		case kmsKeyID == "":
			return false, nil
		case kmsKeyID == keys.arn || kmsKeyID == keys.id:
			return true, nil
		case !strings.HasPrefix(kmsKeyID, "alias/") && !strings.Contains(kmsKeyID, ":alias/"):
			return false, nil
		}
		if keys.aliases == nil {
			aliases := map[string]bool{}
			pages := kms.NewListAliasesPaginator(a.KMS, &kms.ListAliasesInput{KeyId: aws.String(keys.arn)})
			for pages.HasMorePages() {
				page, err := pages.NextPage(ctx)
				if err != nil {
					return false, fmt.Errorf("list the aliases of %s: %w", keys.arn, err)
				}
				for _, al := range page.Aliases {
					aliases[aws.ToString(al.AliasName)] = true
					aliases[aws.ToString(al.AliasArn)] = true
				}
			}
			keys.aliases = aliases
		}
		return keys.aliases[kmsKeyID], nil
	}
}

// Read answers the secret's current value. A secret Secrets Manager does not hold
// (ResourceNotFoundException) and one scheduled for deletion are both ErrNotFound: a secret
// deleted with a recovery window, the console's and the CLI's default, stays there until the
// window passes and GetSecretValue refuses to read it with InvalidRequestException ("marked for
// deletion") rather than answering it absent, which is the same answer to its caller - the secret
// is no longer readable - and must not be the broker's own 500. InvalidRequestException is that
// state: GetSecretValue's other refusals name the request itself (InvalidParameterException) or the
// key (DecryptionFailure), and this one is the only request state a caller can reach. A value that
// is no non-empty SecretString - one stored as SecretBinary - wraps ErrValueNotText.
func (a Store) Read(ctx context.Context, source string) (string, error) {
	out, err := a.Client.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{SecretId: aws.String(source)})
	var notFound *types.ResourceNotFoundException
	var deleting *types.InvalidRequestException
	if errors.As(err, &notFound) || errors.As(err, &deleting) {
		return "", secretstore.ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("secrets manager %s: %w", source, err)
	}
	if out.SecretString == nil || *out.SecretString == "" {
		return "", fmt.Errorf("secrets manager %s: %w", source, secretstore.ErrValueNotText)
	}
	return *out.SecretString, nil
}
