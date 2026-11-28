package aws

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"

	"github.com/sjawhar/envoy/internal/cipher/secretstore"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/aws/awstest"
)

// stubSMAPI answers GetSecretValue with out and err; its other Secrets Manager calls are an empty
// Local's, which a Read never makes.
type stubSMAPI struct {
	*awstest.Local
	out *secretsmanager.GetSecretValueOutput
	err error
}

func (s stubSMAPI) GetSecretValue(_ context.Context, _ *secretsmanager.GetSecretValueInput, _ ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error) {
	return s.out, s.err
}

// TestHasCurrentVersionIsTheRuleListServesBy pins the exported rule both the AWS store's
// HasCurrentValue and the agent-secrets CLI's VALUE column read: a secret has a value while one of
// its versions carries AWSCURRENT, and not otherwise.
func TestHasCurrentVersionIsTheRuleListServesBy(t *testing.T) {
	for _, tc := range []struct {
		stages map[string][]string
		want   bool
	}{
		{map[string][]string{"v1": {"AWSCURRENT"}}, true},
		{map[string][]string{"v1": {"AWSPREVIOUS"}, "v2": {"AWSCURRENT"}, "v3": {"AWSPENDING"}}, true},
		{nil, false},
		{map[string][]string{}, false},
		{map[string][]string{"v1": {"AWSPENDING"}}, false},
		{map[string][]string{"v1": {"AWSPREVIOUS"}}, false},
	} {
		if got := HasCurrentVersion(tc.stages); got != tc.want {
			t.Fatalf("HasCurrentVersion(%v) = %v, want %v", tc.stages, got, tc.want)
		}
	}
}

// stagedSecrets answers ListSecrets and DescribeSecret with one secret, dev/key, whose versions
// carry stages; its other calls are an empty Local's.
type stagedSecrets struct {
	*awstest.Local
	stages map[string][]string
}

func (s stagedSecrets) ListSecrets(context.Context, *secretsmanager.ListSecretsInput, ...func(*secretsmanager.Options)) (*secretsmanager.ListSecretsOutput, error) {
	return &secretsmanager.ListSecretsOutput{SecretList: []types.SecretListEntry{{Name: aws.String("dev/key"), ARN: aws.String(awstest.ARN("dev/key")), SecretVersionsToStages: s.stages}}}, nil
}

func (s stagedSecrets) DescribeSecret(context.Context, *secretsmanager.DescribeSecretInput, ...func(*secretsmanager.Options)) (*secretsmanager.DescribeSecretOutput, error) {
	return &secretsmanager.DescribeSecretOutput{Name: aws.String("dev/key"), ARN: aws.String(awstest.ARN("dev/key")), VersionIdsToStages: s.stages}, nil
}

// TestAWSListingNamesTheVersionGetSecretValueReads: List and Describe answer, as a Listing's
// Version, the id of the version labelled AWSCURRENT in the stages each call already carries
// (ListSecrets' SecretVersionsToStages, DescribeSecret's VersionIdsToStages), and "" for a secret
// no version of which carries it.
func TestAWSListingNamesTheVersionGetSecretValueReads(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		stages map[string][]string
		want   string
	}{
		{map[string][]string{"v-old": {"AWSPREVIOUS"}, "v-new": {"AWSPENDING", "AWSCURRENT"}}, "v-new"},
		{map[string][]string{"v-pending": {"AWSPENDING"}}, ""},
		{nil, ""},
	} {
		store := Store{Client: stagedSecrets{Local: awstest.NewLocal(), stages: tc.stages}}
		listed, err := store.List(ctx, "dev/")
		if err != nil || len(listed) != 1 || listed[0].Version != tc.want {
			t.Fatalf("List over stages %v = %+v, %v; want one listing of version %q", tc.stages, listed, err, tc.want)
		}
		if described, err := store.Describe(ctx, "dev/key"); err != nil || described.Version != tc.want {
			t.Fatalf("Describe over stages %v = %+v, %v; want version %q", tc.stages, described, err, tc.want)
		}
	}
}

// TestAWSListingIsUnfilledOnlyForATaggedSecretWithNoVersion: a Secrets Manager listing is Unfilled
// only where the secret carries the create tag (CreateLabel, which `agent-secrets secret create`
// never writes on AWS, whose create takes the value with the tags) and holds no version at all:
// one without the tag, or with any version, is not.
func TestAWSListingIsUnfilledOnlyForATaggedSecretWithNoVersion(t *testing.T) {
	ctx := context.Background()
	tagged := awstest.LocalSecret{Name: "dev/tagged-empty", Tags: map[string]string{secretstore.CreateLabel: "0123456789abcdef0123456789abcdef"}}
	untagged := awstest.LocalSecret{Name: "dev/untagged-empty", Tags: map[string]string{}}
	filled := awstest.LocalSecret{Name: "dev/tagged-filled", Tags: map[string]string{secretstore.CreateLabel: "0123456789abcdef0123456789abcdef"}, Value: "v1"}
	store := Store{Client: awstest.NewLocal(tagged, untagged, filled)}
	listed, err := store.List(ctx, "dev/")
	if err != nil || len(listed) != 3 {
		t.Fatalf("List = %+v, %v; want three listings", listed, err)
	}
	for _, ls := range listed {
		described, err := store.Describe(ctx, ls.ID)
		if err != nil {
			t.Fatal(err)
		}
		want := ls.ID == "dev/tagged-empty"
		if ls.Unfilled != want || described.Unfilled != want {
			t.Errorf("%s: listed Unfilled %t, described %t; want %t", ls.ID, ls.Unfilled, described.Unfilled, want)
		}
	}
}

func TestAWSReadReturnsErrNotFoundOnResourceNotFoundException(t *testing.T) {
	r := Store{Client: stubSMAPI{err: &types.ResourceNotFoundException{}}}
	_, err := r.Read(context.Background(), "arn:aws:secretsmanager:us-east-1:1:secret:missing")
	if !errors.Is(err, secretstore.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

// TestAWSReadReturnsErrNotFoundForASecretScheduledForDeletion pins that a secret deleted with a
// recovery window, which GetSecretValue refuses as InvalidRequestException rather than
// ResourceNotFoundException, reads as not in the store, both from Secrets Manager's own answer
// and from a Local holding such a secret.
func TestAWSReadReturnsErrNotFoundForASecretScheduledForDeletion(t *testing.T) {
	scheduled := &types.InvalidRequestException{Message: aws.String("You can't perform this operation on the secret because it was marked for deletion.")}
	if _, err := (Store{Client: stubSMAPI{err: scheduled}}).Read(context.Background(), "arn:aws:secretsmanager:us-east-1:1:secret:deleting"); !errors.Is(err, secretstore.ErrNotFound) {
		t.Fatalf("Read(InvalidRequestException) = %v, want ErrNotFound", err)
	}
	deleting := awstest.LocalSecret{Name: "dev/agent-secrets/deleting", Value: "v", DeletedAt: new(time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC))}
	if v, err := (Store{Client: awstest.NewLocal(deleting)}).Read(context.Background(), deleting.Name); !errors.Is(err, secretstore.ErrNotFound) || v != "" {
		t.Fatalf("Read(a Local secret scheduled for deletion) = %q, %v; want ErrNotFound and no value", v, err)
	}
}

// TestAWSReadOfAValueThatIsNotTextIsErrValueNotText: a secret whose current value is SecretBinary
// alone, or an empty SecretString, holds nothing the broker can hand a command, and Read answers
// it as ErrValueNotText, never as a secret the store does not hold.
func TestAWSReadOfAValueThatIsNotTextIsErrValueNotText(t *testing.T) {
	for name, out := range map[string]*secretsmanager.GetSecretValueOutput{
		"binary": {SecretBinary: []byte{0xff, 0x00}},
		"empty":  {SecretString: aws.String("")},
	} {
		_, err := (Store{Client: stubSMAPI{out: out}}).Read(context.Background(), "arn:aws:secretsmanager:us-east-1:1:secret:"+name)
		if !errors.Is(err, secretstore.ErrValueNotText) || errors.Is(err, secretstore.ErrNotFound) || strings.Contains(err.Error(), "\xff") {
			t.Fatalf("Read(a %s value) = %v; want ErrValueNotText, never quoting the bytes", name, err)
		}
	}
}

func TestAWSReadWrapsGenericErrorInsteadOfMappingToErrNotFound(t *testing.T) {
	boom := errors.New("boom")
	r := Store{Client: stubSMAPI{err: boom}}
	source := "arn:aws:secretsmanager:us-east-1:1:secret:present"
	_, err := r.Read(context.Background(), source)
	if errors.Is(err, secretstore.ErrNotFound) {
		t.Fatalf("a generic error must not be mapped to ErrNotFound, got %v", err)
	}
	if !errors.Is(err, boom) {
		t.Fatalf("expected the generic error to be wrapped, got %v", err)
	}
	if !strings.Contains(err.Error(), source) {
		t.Fatalf("expected the error to name the source %q, got %q", source, err.Error())
	}
	if strings.Contains(err.Error(), "shh") {
		t.Fatalf("error message must not contain a secret value, got %q", strings.ReplaceAll(err.Error(), "shh", "[REDACTED]"))
	}
}
