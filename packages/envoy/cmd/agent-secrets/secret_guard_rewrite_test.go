package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"
	"github.com/aws/smithy-go"
	"google.golang.org/grpc/codes"

	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/policy/policytest"
	"github.com/sjawhar/envoy/internal/broker/secrets"
)

// TestAFailedGuardRewriteSaysWhatTheRequestersTagHolds: a removal retag whose second write, the
// requesters rewrite, IAM refuses (refusingTags: a definite refusal, so the store did not act) says
// what the requesters tag holds now, which is what it held before.
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
			useAWS(t, refusingTags{local}, testAccount, adaSignIn)
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
// "delete" or "restore". A refused write is countingTagWrites' (refuseUntag) or refusingTags'.
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

// sdkTags is a secrets.Local whose TagResource and UntagResource go through a real Secrets Manager
// client, so the SDK's own retries and the backend's attempt record (awsAttempts) run as they do in
// production; every other call goes to the Local.
type sdkTags struct {
	*secrets.Local
	sm *secretsmanager.Client
}

func (r sdkTags) TagResource(ctx context.Context, in *secretsmanager.TagResourceInput, o ...func(*secretsmanager.Options)) (*secretsmanager.TagResourceOutput, error) {
	return r.sm.TagResource(ctx, in, o...)
}

func (r sdkTags) UntagResource(ctx context.Context, in *secretsmanager.UntagResourceInput, o ...func(*secretsmanager.Options)) (*secretsmanager.UntagResourceOutput, error) {
	return r.sm.UntagResource(ctx, in, o...)
}

// sdkScript is, per Secrets Manager operation, what each of its attempts does in turn: apply,
// apply-drop (apply, then close the connection with no answer), deny, notfound, validation or ise.
// The last step repeats.
type sdkScript map[string][]string

// sdkServer is a Secrets Manager endpoint that applies tag calls and store writes to a Local as
// its script says, counting each operation's attempts.
type sdkServer struct {
	mu       sync.Mutex
	attempts map[string]int
}

// startSDKServer serves script over local and answers a real SDK client for it, with no backoff
// between retries.
func startSDKServer(t *testing.T, local *secrets.Local, script sdkScript) (*secretsmanager.Client, *sdkServer) {
	t.Helper()
	st := &sdkServer{attempts: map[string]int{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		op := strings.TrimPrefix(r.Header.Get("X-Amz-Target"), "secretsmanager.")
		st.mu.Lock()
		n := st.attempts[op]
		st.attempts[op]++
		st.mu.Unlock()
		steps, step := script[op], "apply"
		if n < len(steps) {
			step = steps[n]
		} else if len(steps) > 0 {
			step = steps[len(steps)-1]
		}
		apply := func() bool {
			body, _ := io.ReadAll(r.Body)
			var err error
			switch op {
			case "TagResource":
				var in secretsmanager.TagResourceInput
				if err = json.Unmarshal(body, &in); err == nil {
					_, err = local.TagResource(r.Context(), &in)
				}
			case "UntagResource":
				var in secretsmanager.UntagResourceInput
				if err = json.Unmarshal(body, &in); err == nil {
					_, err = local.UntagResource(r.Context(), &in)
				}
			case "PutSecretValue":
				var in secretsmanager.PutSecretValueInput
				if err = json.Unmarshal(body, &in); err == nil {
					// The SDK fills a ClientRequestToken, which the Local refuses.
					in.ClientRequestToken = nil
					_, err = local.PutSecretValue(r.Context(), &in)
				}
			case "DeleteSecret":
				var in secretsmanager.DeleteSecretInput
				if err = json.Unmarshal(body, &in); err == nil {
					_, err = local.DeleteSecret(r.Context(), &in)
				}
			case "RestoreSecret":
				var in secretsmanager.RestoreSecretInput
				if err = json.Unmarshal(body, &in); err == nil {
					_, err = local.RestoreSecret(r.Context(), &in)
				}
			default:
				t.Errorf("unexpected operation %q", op)
				return false
			}
			if err != nil {
				t.Errorf("apply %s: %v", op, err)
				return false
			}
			return true
		}
		fail := func(status int, code, msg string) {
			w.Header().Set("Content-Type", "application/x-amz-json-1.1")
			w.Header().Set("X-Amzn-ErrorType", code)
			w.WriteHeader(status)
			_, _ = io.WriteString(w, `{"__type":"`+code+`","Message":"`+msg+`"}`)
		}
		switch step {
		case "apply":
			if apply() {
				w.Header().Set("Content-Type", "application/x-amz-json-1.1")
				_, _ = io.WriteString(w, `{}`)
			}
		case "apply-drop":
			if apply() {
				conn, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				_ = conn.Close()
			}
		case "deny":
			fail(400, "AccessDeniedException", "not authorized to perform: secretsmanager:"+op)
		case "notfound":
			fail(400, "ResourceNotFoundException", "Secrets Manager can't find the specified secret.")
		case "validation":
			fail(400, "ValidationException", "a tag is invalid")
		case "ise":
			fail(500, "InternalServiceError", "an error occurred on the server side")
		case "limit":
			// A quota refusal, which the SDK's standard retryer retries as a throttle.
			fail(400, "LimitExceededException", "the request would exceed a quota")
		}
	}))
	t.Cleanup(srv.Close)
	sm := secretsmanager.New(secretsmanager.Options{
		Region: "us-east-1", BaseEndpoint: aws.String(srv.URL),
		Credentials: credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "not-a-secret", ""),
		Retryer: retry.NewStandard(func(o *retry.StandardOptions) {
			o.Backoff = retry.BackoffDelayerFunc(func(int, error) (time.Duration, error) { return 0, nil })
		}),
	})
	return sm, st
}

func (s *sdkServer) count(op string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attempts[op]
}

// TestAnAWSTagWriteIsJudgedByItsAttempts drives retag's tag writes through a real SDK client, so
// the backend's attempt record runs. One attempt answered with a code awsDefiniteRefusals lists is
// a refusal: nothing changed, nothing is reread, and a shared secret's refusal names the
// administrator. Anything else is an open outcome: a service fault the SDK retried, and an attempt
// that applied and lost its answer before a denied retry. The retag says so, rereads once, makes no
// second write after an open removal, and neither names the administrator nor points the person
// at another sign-in.
func TestAnAWSTagWriteIsJudgedByItsAttempts(t *testing.T) {
	rewrite := guarded("ada@example.com")
	open := []string{"does not hold", "still holds", sharedRetagRefused, "choose another with AWS_PROFILE"}
	for _, c := range []struct {
		name                  string
		owner                 string
		project               bool
		args                  []string
		script                sdkScript
		want, notWant         []string
		tagN, untagN, rereads int
		tier, requesters      string
		maxGrantKept          bool
	}{
		{
			name: "set: one AccessDenied (control)", owner: policy.OwnerShared, args: []string{"--tier", "human"},
			script: sdkScript{"TagResource": {"deny"}}, tagN: 1,
			want: []string{"AccessDeniedException", sharedRetagRefused}, notWant: []string{"leaves open"},
			tier: policy.TierAgent, maxGrantKept: true,
		},
		{
			name: "set: one ResourceNotFound", owner: "ada@example.com", args: []string{"--tier", "human"},
			script: sdkScript{"TagResource": {"notfound"}}, tagN: 1,
			want: []string{"ResourceNotFoundException"}, notWant: []string{"leaves open"},
			tier: policy.TierAgent, maxGrantKept: true,
		},
		{
			name: "set: one ValidationException", owner: "ada@example.com", args: []string{"--tier", "human"},
			script: sdkScript{"TagResource": {"validation"}}, tagN: 1,
			want: []string{"ValidationException"}, notWant: []string{"leaves open"},
			tier: policy.TierAgent, maxGrantKept: true,
		},
		{
			name: "set: InternalServiceError every attempt", owner: "ada@example.com", args: []string{"--tier", "human"},
			script: sdkScript{"TagResource": {"ise"}}, tagN: 3,
			want: []string{"InternalServiceError", "leaves open whether this write landed"}, notWant: open,
			rereads: 1, tier: policy.TierAgent, maxGrantKept: true,
		},
		{
			name: "set: LimitExceededException every attempt (a throttle the SDK retried)", owner: policy.OwnerShared, args: []string{"--tier", "human"},
			script: sdkScript{"TagResource": {"limit"}}, tagN: 3,
			want: []string{"LimitExceededException"}, notWant: []string{"leaves open"},
			tier: policy.TierAgent, maxGrantKept: true,
		},
		{
			name: "set: a shared secret's write applied, answer lost, retry denied", owner: policy.OwnerShared, args: []string{"--tier", "human"},
			script: sdkScript{"TagResource": {"apply-drop", "deny"}}, tagN: 2,
			want: []string{"leaves open whether this write landed"}, notWant: open,
			rereads: 1, tier: policy.TierHuman, maxGrantKept: true,
		},
		{
			name: "removal: one AccessDenied (control)", owner: "ada@example.com", project: true, args: []string{"--max-grant", "none"},
			script: sdkScript{"UntagResource": {"deny"}}, untagN: 1,
			want: []string{"AccessDeniedException"}, notWant: []string{"leaves open", "the removal"},
			tier: policy.TierAgent, requesters: "ada@example.com", maxGrantKept: true,
		},
		{
			name: "removal: applied, answer lost, retry denied", owner: "ada@example.com", project: true, args: []string{"--max-grant", "none"},
			script: sdkScript{"UntagResource": {"apply-drop", "deny"}}, untagN: 2,
			want:    []string{"leaves open whether this write landed", "whether the removal landed is unknown, so its second write"},
			notWant: open, rereads: 1, tier: policy.TierAgent, requesters: "ada@example.com",
		},
		{
			name: "rewrite: applied, answer lost, retry denied", owner: "ada@example.com", project: true, args: []string{"--max-grant", "none"},
			script: sdkScript{"UntagResource": {"apply"}, "TagResource": {"apply-drop", "deny"}}, untagN: 1, tagN: 2,
			want:    []string{"the removal stands, but whether API_KEY's requesters were written as " + `"` + rewrite + `"` + " is unknown"},
			notWant: open, rereads: 1, tier: policy.TierAgent, requesters: rewrite,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			secret := policytest.Secret("API_KEY", c.owner, policy.TierAgent, "v1")
			secret.Tags[policy.TagMaxGrant] = "1h"
			if c.project {
				secret.Tags[policy.TagRequesters] = "ada@example.com"
				secret.Tags[policy.TagProject] = "PRIV"
			}
			local := secrets.NewLocal(secret)
			sm, st := startSDKServer(t, local, c.script)
			broker := startSecretBroker(t, servedBy(local))
			useAWS(t, sdkTags{Local: local, sm: sm}, testAccount, adaSignIn)
			_, stderr, code := runSecret(append([]string{"retag", "API_KEY"}, c.args...)...)
			tags := secrets.TagMap(describe(t, local, "API_KEY").Tags)
			rereads, _ := broker.recorded()
			if code != 1 {
				t.Errorf("exit %d; want 1", code)
			}
			for _, said := range c.want {
				if !strings.Contains(stderr, said) {
					t.Errorf("stderr %q lacks %q", stderr, said)
				}
			}
			for _, said := range c.notWant {
				if strings.Contains(stderr, said) {
					t.Errorf("stderr %q has %q", stderr, said)
				}
			}
			if st.count("TagResource") != c.tagN || st.count("UntagResource") != c.untagN {
				t.Errorf("attempts TagResource=%d UntagResource=%d; want %d, %d", st.count("TagResource"), st.count("UntagResource"), c.tagN, c.untagN)
			}
			if len(rereads) != c.rereads {
				t.Errorf("rereads %v; want %d", rereads, c.rereads)
			}
			if tags[policy.TagTier] != c.tier {
				t.Errorf("tier %q; want %q", tags[policy.TagTier], c.tier)
			}
			if c.project && tags[policy.TagRequesters] != c.requesters {
				t.Errorf("requesters %q; want %q", tags[policy.TagRequesters], c.requesters)
			}
			if _, kept := tags[policy.TagMaxGrant]; kept != c.maxGrantKept {
				t.Errorf("max-grant kept %v; want %v", kept, c.maxGrantKept)
			}
		})
	}
}

// TestTheUncheckedLineFollowsOnlyALandedWrite: a retag to a type, by a sign-in the store refuses
// the value read, says the value was not checked only once its write is known to have landed: a
// write IAM refused before acting changed nothing, and one whose answer was lost may not have
// changed anything, so neither says "the tags are written".
func TestTheUncheckedLineFollowsOnlyALandedWrite(t *testing.T) {
	for _, c := range []struct {
		name          string
		store         func(*valueReads) secretsAPI
		landed, typed bool
	}{
		{"a refused write", func(v *valueReads) secretsAPI { return refusingTags{v} }, false, false},
		{"a write whose answer was lost", func(v *valueReads) secretsAPI { return &raceOnTag{secretsAPI: v, loseAnswer: "tag"} }, false, true},
		{"a landed write whose read-back failed (control)", func(v *valueReads) secretsAPI { return &raceOnTag{secretsAPI: v, failReadBack: "tag"} }, true, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			local := secrets.NewLocal(policytest.Secret("API_KEY", "ada@example.com", policy.TierAgent, "v1"))
			startSecretBroker(t, servedBy(local))
			refused := &valueReads{Local: local, refuse: &smithy.GenericAPIError{Code: "AccessDeniedException", Message: "not authorized to perform: secretsmanager:GetSecretValue"}}
			useAWS(t, c.store(refused), testAccount, adaSignIn)
			_, stderr, code := runSecret("retag", "API_KEY", "--type", policy.TypeGitHubApp, "--github-owners", "acme-org")
			typed := secrets.TagMap(describe(t, local, "API_KEY").Tags)[policy.TagType] == policy.TypeGitHubApp
			said := strings.Contains(stderr, "the tags are written, but API_KEY's value was not checked")
			if code != 1 || said != c.landed || typed != c.typed {
				t.Fatalf("%s: exit %d, the unchecked line printed %v (want %v), type written %v (want %v); stderr %q", c.name, code, said, c.landed, typed, c.typed, stderr)
			}
		})
	}
}

// TestARewriteGCPRefusesForARaceSaysAnotherWriterChangedTheTags: on GCP the requesters rewrite is
// sent with the etag of a read made after the removal, so another writer's change in between makes
// the store refuse it (FAILED_PRECONDITION). The retag says another writer changed the tags, names
// the `secret show` that reads them, and claims nothing about what the requesters tag holds, which
// is that writer's. The broker is asked to reread once, since the removal landed.
func TestARewriteGCPRefusesForARaceSaysAnotherWriterChangedTheTags(t *testing.T) {
	fake := newFakeSecretManager()
	startFakeSecretManager(t, fake)
	useGCPIdentity(t, "authorized_user", "ada@example.com")
	settings := gcpTestSettings()
	broker := startSecretBrokerWith(t, settings, gcpServedBy(fake, settings))
	id := testGCPPrefix + "api-key"
	annotations := policytest.Secret("API_KEY", "ada@example.com", policy.TierHuman, "v1").Tags
	annotations[policy.TagRequesters] = "ada@example.com"
	annotations[policy.TagProject] = "PRIV"
	annotations[policy.TagMaxGrant] = "1h"
	fake.secrets[id] = &fakeSecret{
		secret:      &secretmanagerpb.Secret{Name: id, Annotations: annotations, Etag: "1"},
		versions:    []*secretmanagerpb.SecretVersion{{Name: id + "/versions/1", State: secretmanagerpb.SecretVersion_ENABLED}},
		payloads:    [][]byte{[]byte("v1")},
		etagCounter: 1,
	}
	updates := 0
	fake.beforeCall = func(method string) {
		if method != "UpdateSecret" {
			return
		}
		updates++
		if updates != 2 {
			return
		}
		// Another writer changes the requesters tag between the rewrite's read and its write.
		fake.mu.Lock()
		entry := fake.secrets[id]
		entry.secret.Annotations[policy.TagRequesters] = "bob@example.com"
		entry.etagCounter++
		entry.secret.Etag = strconv.Itoa(entry.etagCounter)
		fake.mu.Unlock()
	}
	_, stderr, code := runSecret("retag", "API_KEY", "--max-grant", "none")
	if code != 1 || !strings.Contains(stderr, "the removal stands, but another writer changed API_KEY's tags after it, so its requesters were not written as") {
		t.Fatalf("retag whose GCP rewrite raced: exit %d stderr %q; want 1, saying another writer changed the tags", code, stderr)
	}
	for _, claim := range []string{"still holds", "does not hold"} {
		if strings.Contains(stderr, claim) {
			t.Fatalf("stderr %q claims %q about a requesters tag another writer just changed", stderr, claim)
		}
	}
	fake.mu.Lock()
	requesters := fake.secrets[id].secret.GetAnnotations()[policy.TagRequesters]
	fake.mu.Unlock()
	if requesters != "bob@example.com" {
		t.Fatalf("requesters after the race = %q; want the other writer's, bob@example.com", requesters)
	}
	if rereads, _ := broker.recorded(); len(rereads) != 1 || rereads[0] != "API_KEY" {
		t.Fatalf("rereads = %v; want one of API_KEY, since the removal landed", rereads)
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
// the retag cannot tell whether its write landed. It says so, never what the tags hold, names the
// `secret show` that reads them once, and asks the broker to reread the secret. Each row's store
// applies the write and then answers the error.
//   - A removal retag's second write, the requesters rewrite, answers it: the message says whether
//     the requesters were written is unknown, never that the tag "does not hold" the guard "yet".
//   - The removal answers it: the message says whether the removal landed is unknown, so its second
//     write was not made; a removal that needs no second write says to retag again if the tags do
//     not hold its change.
//   - A set retag's one write answers it, on AWS and on GCP, and says the same.
//   - The answer to a removal, to a requesters rewrite or to a set is lost and the broker then
//     refuses the reread, as a dropped network does to both: the retag says the broker could not
//     be asked and names the reread to ask for, and never that the write stands or that the broker
//     refuses the secret after it, which a person may read as "nothing more to check". After a
//     rewrite, the one thing it says stands is the removal, which landed.
func TestATagWriteWhoseAnswerIsLostSaysTheOutcomeIsUnknown(t *testing.T) {
	rewrite := guarded("ada@example.com")
	notGuessed := []string{"does not hold " + policy.RequestersRollbackGuard + " yet", "still holds " + policy.RequestersRollbackGuard, "the removal landed, so", "and API_KEY's requesters were written as"}
	const show = "secret show API_KEY`"
	const retagIfNot = "read the tags as they now stand with `agent-secrets secret show API_KEY`, and run this retag again if they do not hold its change"
	const notAsked = "could not be asked to reread API_KEY"
	for _, c := range []struct {
		name             string
		lose             string
		unlisted         bool
		rereadFails      bool
		removalStands    bool
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
			name: "the answer to a removal with no second write was lost", lose: "untag", unlisted: true, args: []string{"--max-grant", "none"},
			want:       []string{"leaves open whether this write landed", retagIfNot},
			requesters: "", tier: policy.TierHuman, removed: true,
		},
		{
			name: "a set retag's answer was lost", lose: "tag", args: []string{"--tier", policy.TierAgent},
			want:       []string{"leaves open whether this write landed", retagIfNot},
			requesters: rewrite, tier: policy.TierAgent, removed: false,
		},
		{
			name: "the removal's answer was lost and the reread refused", lose: "untag", rereadFails: true, args: []string{"--max-grant", "none"},
			want:       []string{"leaves open whether this write landed", "whether the removal landed is unknown", notAsked},
			requesters: "ada@example.com", tier: policy.TierHuman, removed: true,
		},
		{
			name: "the requesters rewrite's answer was lost and the reread refused", lose: "tag", rereadFails: true, removalStands: true, args: []string{"--max-grant", "none"},
			want:       []string{"leaves open whether this write landed", "the removal stands, but whether API_KEY's requesters were written as " + `"` + rewrite + `"` + " is unknown", notAsked},
			requesters: rewrite, tier: policy.TierHuman, removed: true,
		},
		{
			name: "a set retag's answer was lost and the reread refused", lose: "tag", rereadFails: true, args: []string{"--tier", policy.TierAgent},
			want:       []string{"leaves open whether this write landed", retagIfNot, notAsked},
			requesters: rewrite, tier: policy.TierAgent, removed: false,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			secret := policytest.Secret("API_KEY", "ada@example.com", policy.TierHuman, "v1")
			if !c.unlisted {
				secret.Tags[policy.TagRequesters] = "ada@example.com"
				secret.Tags[policy.TagProject] = "PRIV"
			}
			secret.Tags[policy.TagMaxGrant] = "1h"
			local := secrets.NewLocal(secret)
			broker := startSecretBroker(t, servedBy(local))
			if c.rereadFails {
				broker.failRereads(http.StatusServiceUnavailable)
			}
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
			if c.rereadFails {
				said := stderr
				if c.removalStands {
					said = strings.ReplaceAll(said, "the removal stands", "")
				}
				for _, claim := range []string{"stands", "after the write", "its next reload"} {
					if strings.Contains(said, claim) {
						t.Fatalf("%s: stderr %q says %q of a write whose outcome is unknown", c.name, stderr, claim)
					}
				}
			}
			if n := strings.Count(stderr, show); n != 1 {
				t.Fatalf("%s: stderr %q names %q %d times; want once", c.name, stderr, show, n)
			}
			tags := secrets.TagMap(describe(t, local, "API_KEY").Tags)
			if _, kept := tags[policy.TagMaxGrant]; kept == c.removed || tags[policy.TagRequesters] != c.requesters || tags[policy.TagTier] != c.tier {
				t.Fatalf("tags after %s = %v; want the store's own (applied) result: max-grant removed %v, requesters %q, tier %q", c.name, tags, c.removed, c.requesters, c.tier)
			}
			if rereads, _ := broker.recorded(); !c.rereadFails && (len(rereads) != 1 || rereads[0] != "API_KEY") {
				t.Fatalf("%s: rereads %v; want one of API_KEY, since the write may have changed the tags", c.name, rereads)
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
		if code != 1 || !strings.Contains(stderr, "leaves open whether this write landed") || strings.Count(stderr, show) != 1 {
			t.Fatalf("gcp retag whose UpdateSecret answer was lost: exit %d stderr %q; want 1, saying the outcome is unknown and naming %q once", code, stderr, show)
		}
		fake.mu.Lock()
		tier := fake.secrets[id].secret.GetAnnotations()[policy.TagTier]
		fake.mu.Unlock()
		if tier != policy.TierAgent {
			t.Fatalf("tier after the lost answer = %q; want %q: the fake applied the write", tier, policy.TierAgent)
		}
		if rereads, _ := broker.recorded(); len(rereads) != 1 || rereads[0] != "API_KEY" {
			t.Fatalf("rereads after a GCP write whose answer was lost = %v; want one of API_KEY", rereads)
		}
	})
}
