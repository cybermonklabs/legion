package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"
	"github.com/aws/smithy-go"
	"google.golang.org/grpc/codes"

	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/policy/policytest"
	"github.com/sjawhar/envoy/internal/broker/secrets"
)

// refusedTagWrites is a secrets.Local whose every TagResource IAM refuses (AccessDeniedException,
// a definite refusal: the store did not act), so a removal retag's second write, the requesters
// rewrite, fails after its untag landed and leaves the requesters tag as it was.
type refusedTagWrites struct {
	*secrets.Local
}

func (r *refusedTagWrites) TagResource(context.Context, *secretsmanager.TagResourceInput, ...func(*secretsmanager.Options)) (*secretsmanager.TagResourceOutput, error) {
	return nil, &smithy.GenericAPIError{Code: "AccessDeniedException", Message: "not authorized to perform: secretsmanager:TagResource"}
}

// TestAFailedGuardRewriteSaysWhatTheRequestersTagHolds: a removal retag whose second write, the
// requesters rewrite, fails says what the requesters tag holds now, which is what it held before.
// Where the guard was leaving the tag (the last project tag removed), it still holds the guard,
// which only a broker from legion-envoy-v29.23.0 up to the release before the project tags reads.
// Where the guard was joining it (a secret that keeps a dispatch-project tag written without the
// guard, and a removal of another tag), the tag does not hold it yet, and a broker from before the
// project tags would serve the secret: the message must not claim the guard is there.
func TestAFailedGuardRewriteSaysWhatTheRequestersTagHolds(t *testing.T) {
	for _, c := range []struct {
		name, held, args, want, notWant string
		tags                            map[string]string
	}{
		{
			name: "the guard leaving", held: guarded("ada@example.com"), args: "--project none",
			tags: map[string]string{policy.TagProject: "PRIV"},
			want: "still holds " + policy.RequestersRollbackGuard, notWant: "does not hold",
		},
		{
			name: "the guard joining", held: "ada@example.com", args: "--max-grant none",
			tags: map[string]string{policy.TagProject: "PRIV", policy.TagMaxGrant: "1h"},
			want: "does not hold " + policy.RequestersRollbackGuard + " yet", notWant: "still holds",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			secret := policytest.Secret("API_KEY", "ada@example.com", policy.TierHuman, "v1")
			secret.Tags[policy.TagRequesters] = c.held
			for k, v := range c.tags {
				secret.Tags[k] = v
			}
			local := secrets.NewLocal(secret)
			startSecretBroker(t, servedBy(local))
			useAWS(t, &refusedTagWrites{Local: local}, testAccount, adaSignIn)
			_, stderr, code := runSecret(append([]string{"retag", "API_KEY"}, strings.Fields(c.args)...)...)
			if code != 1 || !strings.Contains(stderr, "the removal stands") {
				t.Fatalf("retag %s with the rewrite refused: exit %d stderr %q; want exit 1 and the removal standing", c.args, code, stderr)
			}
			if !strings.Contains(stderr, c.want) || strings.Contains(stderr, c.notWant) {
				t.Fatalf("retag %s with the rewrite refused said %q; want it to say %q and not %q", c.args, stderr, c.want, c.notWant)
			}
		})
	}
}

// raceOnTag is a store (any secretsAPI, so it wraps the package's other fakes) on which a write
// lands and something then goes wrong. Another writer retags the secret API_KEY a github-app with
// no owners, a pair the broker refuses, just before the first write of the kind racing names:
// "untag" (a removal retag's removal) or "tag" (a tag write: a set retag's write, or a removal
// retag's second write, the requesters rewrite). failReadBack names the kind of write after which
// every DescribeSecret fails with readBackErr (a plain error when nil), as a read-back the network
// lost, or IAM refused, does. loseAnswer names the call that applies and then answers lostAnswer,
// as a call the store carried out and whose answer was lost: "tag", "untag", "put" (a value),
// "delete" or "restore". A refused write is countingTagWrites' (refuseUntag) or refusedTagWrites'.
type raceOnTag struct {
	secretsAPI
	racing, failReadBack, loseAnswer string
	readBackErr                      error
	raced, tagged, untagged          bool
}

// lostAnswer is what a call loseAnswer names answers once it applied: a service fault, which
// Secrets Manager may answer after acting.
var lostAnswer = &types.InternalServiceError{Message: aws.String("the fake applied the call and its answer was lost")}

func (r *raceOnTag) race() {
	if r.raced {
		return
	}
	r.raced = true
	if _, err := r.secretsAPI.TagResource(context.Background(), &secretsmanager.TagResourceInput{
		SecretId: aws.String(policytest.ID("API_KEY")), Tags: secrets.TagList(map[string]string{policy.TagType: policy.TypeGitHubApp}),
	}); err != nil {
		panic(err)
	}
}

func (r *raceOnTag) TagResource(ctx context.Context, in *secretsmanager.TagResourceInput, o ...func(*secretsmanager.Options)) (*secretsmanager.TagResourceOutput, error) {
	if r.racing == "tag" {
		r.race()
	}
	r.tagged = true
	out, err := r.secretsAPI.TagResource(ctx, in, o...)
	if err == nil && r.loseAnswer == "tag" {
		return nil, lostAnswer
	}
	return out, err
}

func (r *raceOnTag) UntagResource(ctx context.Context, in *secretsmanager.UntagResourceInput, o ...func(*secretsmanager.Options)) (*secretsmanager.UntagResourceOutput, error) {
	if r.racing == "untag" {
		r.race()
	}
	r.untagged = true
	out, err := r.secretsAPI.UntagResource(ctx, in, o...)
	if err == nil && r.loseAnswer == "untag" {
		return nil, lostAnswer
	}
	return out, err
}

func (r *raceOnTag) DescribeSecret(ctx context.Context, in *secretsmanager.DescribeSecretInput, o ...func(*secretsmanager.Options)) (*secretsmanager.DescribeSecretOutput, error) {
	if (r.failReadBack == "tag" && r.tagged) || (r.failReadBack == "untag" && r.untagged) {
		if r.readBackErr != nil {
			return nil, r.readBackErr
		}
		return nil, errors.New("DescribeSecret lost for this test")
	}
	return r.secretsAPI.DescribeSecret(ctx, in, o...)
}

func (r *raceOnTag) PutSecretValue(ctx context.Context, in *secretsmanager.PutSecretValueInput, o ...func(*secretsmanager.Options)) (*secretsmanager.PutSecretValueOutput, error) {
	out, err := r.secretsAPI.PutSecretValue(ctx, in, o...)
	if err == nil && r.loseAnswer == "put" {
		return nil, lostAnswer
	}
	return out, err
}

func (r *raceOnTag) DeleteSecret(ctx context.Context, in *secretsmanager.DeleteSecretInput, o ...func(*secretsmanager.Options)) (*secretsmanager.DeleteSecretOutput, error) {
	out, err := r.secretsAPI.DeleteSecret(ctx, in, o...)
	if err == nil && r.loseAnswer == "delete" {
		return nil, lostAnswer
	}
	return out, err
}

func (r *raceOnTag) RestoreSecret(ctx context.Context, in *secretsmanager.RestoreSecretInput, o ...func(*secretsmanager.Options)) (*secretsmanager.RestoreSecretOutput, error) {
	out, err := r.secretsAPI.RestoreSecret(ctx, in, o...)
	if err == nil && r.loseAnswer == "restore" {
		return nil, lostAnswer
	}
	return out, err
}

// TestAGuardRewriteAfterAnAWSRaceSaysWhatLanded: on AWS a tag write is made first and checked
// after (awsBackend.raceCheck), so a removal retag's two writes can fail after they landed. The
// secret keeps a dispatch-project tag whose requesters list lacks the rollback guard, and
// `--max-grant none` removes the max-grant, then writes the list with the guard after it.
//   - The rewrite raced, or its read-back failed: the rewrite landed. The requesters tag holds the
//     guard, and the message says it was written, never that the tag "does not hold" the guard
//     "yet".
//   - The removal raced, or its read-back failed: it landed, and the rewrite was never made, since
//     the retag writes nothing more on tags it cannot trust. The message says the second write was
//     not made and names the command that reads the tags as they now stand; the requesters tag is
//     untouched.
//   - The removal was refused: it never landed, so the message claims no landed write, the
//     max-grant is still there, and nothing asks the broker to reread.
//
// Every landed row asks the broker to reread the secret: its tags changed. After a race the
// broker refuses the github-app the other writer left with no owners (github-owners-missing), as
// its loader does, and the retag prints that refusal.
func TestAGuardRewriteAfterAnAWSRaceSaysWhatLanded(t *testing.T) {
	rewrite := guarded("ada@example.com")
	written := "the removal stands, and API_KEY's requesters were written as " + `"` + rewrite + `"`
	notMade := "its second write, API_KEY's requesters as " + `"` + rewrite + `"` + ", was not made"
	for _, c := range []struct {
		name            string
		store           func(*secrets.Local) secretsAPI
		want, notWant   []string
		requesters      string
		removed, reread bool
	}{
		{
			name:       "the rewrite raced",
			store:      func(l *secrets.Local) secretsAPI { return &raceOnTag{secretsAPI: l, racing: "tag"} },
			want:       []string{written, "this retag's write landed beside another writer's", "(reason=" + policy.ReasonGitHubOwnersMissing + ")"},
			requesters: rewrite, removed: true, reread: true,
		},
		{
			name:       "the rewrite's read-back failed",
			store:      func(l *secrets.Local) secretsAPI { return &raceOnTag{secretsAPI: l, failReadBack: "tag"} },
			want:       []string{written, "reading its tags back failed"},
			requesters: rewrite, removed: true, reread: true,
		},
		{
			name:       "the removal raced",
			store:      func(l *secrets.Local) secretsAPI { return &raceOnTag{secretsAPI: l, racing: "untag"} },
			want:       []string{"this retag's write landed beside another writer's", notMade, "agent-secrets secret show API_KEY", "(reason=" + policy.ReasonGitHubOwnersMissing + ")"},
			requesters: "ada@example.com", removed: true, reread: true,
		},
		{
			name:       "the removal's read-back failed",
			store:      func(l *secrets.Local) secretsAPI { return &raceOnTag{secretsAPI: l, failReadBack: "untag"} },
			want:       []string{"reading its tags back failed", notMade, "agent-secrets secret show API_KEY"},
			requesters: "ada@example.com", removed: true, reread: true,
		},
		{
			name:       "the removal was refused",
			store:      func(l *secrets.Local) secretsAPI { return &countingTagWrites{Local: l, refuseUntag: true} },
			want:       []string{"secretsmanager:UntagResource"},
			notWant:    []string{"the removal landed", "the removal stands"},
			requesters: "ada@example.com", removed: false, reread: false,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			secret := policytest.Secret("API_KEY", "ada@example.com", policy.TierHuman, "v1")
			secret.Tags[policy.TagRequesters] = "ada@example.com"
			secret.Tags[policy.TagProject] = "PRIV"
			secret.Tags[policy.TagMaxGrant] = "1h"
			local := secrets.NewLocal(secret)
			broker := startSecretBroker(t, servedBy(local))
			useAWS(t, c.store(local), testAccount, adaSignIn)
			_, stderr, code := runSecret("retag", "API_KEY", "--max-grant", "none")
			if code != 1 {
				t.Fatalf("retag --max-grant none, %s: exit %d stderr %q; want 1", c.name, code, stderr)
			}
			for _, said := range c.want {
				if !strings.Contains(stderr, said) {
					t.Fatalf("retag --max-grant none, %s: stderr %q does not say %q", c.name, stderr, said)
				}
			}
			for _, claim := range append([]string{"does not hold " + policy.RequestersRollbackGuard + " yet", "still holds " + policy.RequestersRollbackGuard}, c.notWant...) {
				if strings.Contains(stderr, claim) {
					t.Fatalf("retag --max-grant none, %s: stderr %q claims %q, which is not what happened", c.name, stderr, claim)
				}
			}
			tags := secrets.TagMap(describe(t, local, "API_KEY").Tags)
			if _, kept := tags[policy.TagMaxGrant]; kept == c.removed {
				t.Fatalf("tags after %s = %v; want the max-grant removed %v", c.name, tags, c.removed)
			}
			if tags[policy.TagRequesters] != c.requesters {
				t.Fatalf("requesters after %s = %q; want %q", c.name, tags[policy.TagRequesters], c.requesters)
			}
			if rereads, _ := broker.recorded(); (len(rereads) > 0) != c.reread {
				t.Fatalf("rereads after %s = %v; want a reread %v: a write that landed changed the tags", c.name, rereads, c.reread)
			}
		})
	}
}

// TestALandedRetagOfASharedSecretIsNotCalledRefused: a person's retag of a shared secret whose
// write landed and whose read-back IAM then refused (AccessDeniedException on DescribeSecret)
// was not refused, so the retag does not add that only an administrator changes a shared secret's
// owner and tier. It says the write landed and its read-back failed, and asks the broker to reread
// the secret, whose tier did change.
func TestALandedRetagOfASharedSecretIsNotCalledRefused(t *testing.T) {
	local := secrets.NewLocal(policytest.Secret("API_KEY", policy.OwnerShared, policy.TierAgent, "v1"))
	broker := startSecretBroker(t, servedBy(local))
	useAWS(t, &raceOnTag{secretsAPI: local, failReadBack: "tag", readBackErr: &smithy.GenericAPIError{Code: "AccessDeniedException", Message: "not authorized to perform: secretsmanager:DescribeSecret"}}, testAccount, adaSignIn)
	_, stderr, code := runSecret("retag", "API_KEY", "--tier", "human")
	if code != 1 || !strings.Contains(stderr, "reading its tags back failed") {
		t.Fatalf("retag --tier human of a shared secret whose read-back IAM refused: exit %d stderr %q; want 1, saying the read-back failed", code, stderr)
	}
	if strings.Contains(stderr, sharedRetagRefused) {
		t.Fatalf("stderr %q says %q, but the write landed: nothing refused it", stderr, sharedRetagRefused)
	}
	if tier := secrets.TagMap(describe(t, local, "API_KEY").Tags)[policy.TagTier]; tier != policy.TierHuman {
		t.Fatalf("tier after the retag = %q; want %q: the write landed", tier, policy.TierHuman)
	}
	if rereads, _ := broker.recorded(); len(rereads) != 1 || rereads[0] != "API_KEY" {
		t.Fatalf("rereads = %v; want one of API_KEY: a write that landed changed the tags", rereads)
	}
}

// TestALandedRetagWhoseValueWasNotCheckedSaysSo: a retag to a type, by a sign-in the store refuses
// the value read (so the value is not checked, and the retag goes ahead), whose write landed and
// whose read-back then failed, says the tags are written and the value was not checked, as after a
// clean write: the write landed, so the value went unchecked as surely. It also says the read-back
// failed, and asks the broker to reread the secret.
func TestALandedRetagWhoseValueWasNotCheckedSaysSo(t *testing.T) {
	local := secrets.NewLocal(policytest.Secret("API_KEY", "ada@example.com", policy.TierAgent, "v1"))
	broker := startSecretBroker(t, servedBy(local))
	refused := &valueReads{Local: local, refuse: &smithy.GenericAPIError{Code: "AccessDeniedException", Message: "not authorized to perform: secretsmanager:GetSecretValue"}}
	useAWS(t, &raceOnTag{secretsAPI: refused, failReadBack: "tag"}, testAccount, adaSignIn)
	_, stderr, code := runSecret("retag", "API_KEY", "--type", policy.TypeGitHubApp, "--github-owners", "acme-org")
	if code != 1 || !strings.Contains(stderr, "reading its tags back failed") {
		t.Fatalf("retag to github-app whose value read was denied and whose read-back failed: exit %d stderr %q; want 1, saying the read-back failed", code, stderr)
	}
	if !strings.Contains(stderr, "the tags are written, but API_KEY's value was not checked as a github-app secret's") {
		t.Fatalf("stderr %q does not say the value was not checked, though the write landed", stderr)
	}
	if tags := secrets.TagMap(describe(t, local, "API_KEY").Tags); tags[policy.TagType] != policy.TypeGitHubApp {
		t.Fatalf("tags after the retag = %v; want github-app: the write landed", tags)
	}
	if rereads, _ := broker.recorded(); len(rereads) != 1 || rereads[0] != "API_KEY" {
		t.Fatalf("rereads = %v; want one of API_KEY: a write that landed changed the tags", rereads)
	}
}

// TestATagWriteWhoseAnswerIsLostSaysTheOutcomeIsUnknown: a tag call whose error is no definite
// refusal (an AWS service fault, or a GCP UNAVAILABLE) may follow a call the store carried out, so
// the retag cannot tell whether its write landed. It says so, never what the tags hold, and asks
// the broker to reread the secret. Each row's store applies the write and then answers the error.
//   - A removal retag's second write, the requesters rewrite, answers it: the message says whether
//     the requesters were written is unknown, never that the tag "does not hold" the guard "yet".
//   - The removal answers it: the message says whether the removal landed is unknown, so its second
//     write was not made.
//   - A set retag's one write answers it, on AWS and on GCP.
func TestATagWriteWhoseAnswerIsLostSaysTheOutcomeIsUnknown(t *testing.T) {
	rewrite := guarded("ada@example.com")
	notGuessed := []string{"does not hold " + policy.RequestersRollbackGuard + " yet", "still holds " + policy.RequestersRollbackGuard, "the removal landed, so", "and API_KEY's requesters were written as"}
	for _, c := range []struct {
		name             string
		lose             string
		args, want       []string
		requesters, tier string
		removed          bool
	}{
		{
			name: "the requesters rewrite's answer was lost", lose: "tag", args: []string{"--max-grant", "none"},
			want:       []string{"leaves open whether this write landed", "the removal stands, but whether API_KEY's requesters were written as " + `"` + rewrite + `"` + " is unknown"},
			requesters: rewrite, tier: policy.TierHuman, removed: true,
		},
		{
			name: "the removal's answer was lost", lose: "untag", args: []string{"--max-grant", "none"},
			want:       []string{"leaves open whether this write landed", "whether the removal landed is unknown, so its second write, API_KEY's requesters as " + `"` + rewrite + `"` + ", was not made"},
			requesters: "ada@example.com", tier: policy.TierHuman, removed: true,
		},
		{
			name: "a set retag's answer was lost", lose: "tag", args: []string{"--tier", policy.TierAgent},
			want:       []string{"leaves open whether this write landed"},
			requesters: rewrite, tier: policy.TierAgent, removed: false,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			secret := policytest.Secret("API_KEY", "ada@example.com", policy.TierHuman, "v1")
			secret.Tags[policy.TagRequesters] = "ada@example.com"
			secret.Tags[policy.TagProject] = "PRIV"
			secret.Tags[policy.TagMaxGrant] = "1h"
			local := secrets.NewLocal(secret)
			broker := startSecretBroker(t, servedBy(local))
			useAWS(t, &raceOnTag{secretsAPI: local, loseAnswer: c.lose}, testAccount, adaSignIn)
			_, stderr, code := runSecret(append([]string{"retag", "API_KEY"}, c.args...)...)
			if code != 1 {
				t.Fatalf("%s: exit %d stderr %q; want 1", c.name, code, stderr)
			}
			for _, said := range c.want {
				if !strings.Contains(stderr, said) {
					t.Fatalf("%s: stderr %q does not say %q", c.name, stderr, said)
				}
			}
			for _, claim := range notGuessed {
				if strings.Contains(stderr, claim) {
					t.Fatalf("%s: stderr %q claims %q, which the retag cannot know", c.name, stderr, claim)
				}
			}
			tags := secrets.TagMap(describe(t, local, "API_KEY").Tags)
			if _, kept := tags[policy.TagMaxGrant]; kept == c.removed || tags[policy.TagRequesters] != c.requesters || tags[policy.TagTier] != c.tier {
				t.Fatalf("tags after %s = %v; want the store's own (applied) result: max-grant removed %v, requesters %q, tier %q", c.name, tags, c.removed, c.requesters, c.tier)
			}
			if rereads, _ := broker.recorded(); len(rereads) == 0 {
				t.Fatalf("%s: no reread; want one, since the write may have changed the tags", c.name)
			}
		})
	}
	t.Run("gcp: a set retag's answer was lost", func(t *testing.T) {
		fake := newFakeSecretManager()
		startFakeSecretManager(t, fake)
		useGCPIdentity(t, "authorized_user", "ada@example.com")
		settings := gcpTestSettings()
		broker := startSecretBrokerWith(t, settings, gcpServedBy(fake, settings))
		id := testGCPPrefix + "api-key"
		fake.secrets[id] = &fakeSecret{
			secret:      &secretmanagerpb.Secret{Name: id, Annotations: policytest.Secret("API_KEY", "ada@example.com", policy.TierHuman, "v1").Tags, Etag: "1"},
			versions:    []*secretmanagerpb.SecretVersion{{Name: id + "/versions/1", State: secretmanagerpb.SecretVersion_ENABLED}},
			payloads:    [][]byte{[]byte("v1")},
			etagCounter: 1,
		}
		fake.answerAfter = map[string]codes.Code{"UpdateSecret": codes.Unavailable}
		_, stderr, code := runSecret("retag", "API_KEY", "--tier", policy.TierAgent)
		if code != 1 || !strings.Contains(stderr, "leaves open whether this write landed") {
			t.Fatalf("gcp retag whose UpdateSecret answer was lost: exit %d stderr %q; want 1, saying the outcome is unknown", code, stderr)
		}
		fake.mu.Lock()
		tier := fake.secrets[id].secret.GetAnnotations()[policy.TagTier]
		fake.mu.Unlock()
		if tier != policy.TierAgent {
			t.Fatalf("tier after the lost answer = %q; want %q: the fake applied the write", tier, policy.TierAgent)
		}
		if rereads, _ := broker.recorded(); len(rereads) == 0 {
			t.Fatal("no reread after a GCP write whose answer was lost; want one")
		}
	})
}
