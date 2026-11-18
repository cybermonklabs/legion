// packages/envoy/cmd/agent-secrets/secret_create_test.go
//
// The deadline on each store call `secret create` makes from CreateSecret until the value is stored
// or the discard is done (storeCallDeadline), on both stores: a call the store never answers ends
// at the deadline, and the create then fails as it fails on any store error, never waiting on it.
package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"
	"github.com/aws/smithy-go"

	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/policy/policytest"
	"github.com/sjawhar/envoy/internal/broker/secrets"
)

// useStoreCallDeadline makes d the deadline on each store call of a create, for the rest of t.
func useStoreCallDeadline(t *testing.T, d time.Duration) {
	t.Helper()
	restore := storeCallDeadline
	storeCallDeadline = d
	t.Cleanup(func() { storeCallDeadline = restore })
}

// hungCreate is a Secrets Manager whose CreateSecret answers nothing until release is closed, and
// then creates as local does, or until its context ends, when it answers the context's error, as
// the AWS SDK does for a request whose context ended.
type hungCreate struct {
	*secrets.Local
	release chan struct{}
}

func (h hungCreate) CreateSecret(ctx context.Context, in *secretsmanager.CreateSecretInput, o ...func(*secretsmanager.Options)) (*secretsmanager.CreateSecretOutput, error) {
	select {
	case <-h.release:
		return h.Local.CreateSecret(ctx, in, o...)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// TestSecretCreateEndsAStoreCallThatNeverAnswersAtItsDeadline: a store call of the create that the
// store never answers - GCP's AddSecretVersion, AWS's one CreateSecret - ends at the deadline on
// each such call, and the create fails as on any store error rather than waiting for it: on GCP,
// where a call that ran out of time may have stored the value or may still, it lists the versions,
// finds none, keeps the secret, says a late add may still land and names the command that checks
// it, deleting nothing, and asks the broker to reread the name; on AWS, whose CreateSecret makes
// the secret with its value in one call, it says that the store may hold the secret or not and
// names the command that says which. Each exits 1 well before the store would answer.
func TestSecretCreateEndsAStoreCallThatNeverAnswersAtItsDeadline(t *testing.T) {
	for _, store := range []string{"gcp", "aws"} {
		t.Run(store, func(t *testing.T) {
			useStoreCallDeadline(t, 300*time.Millisecond)
			release := make(chan struct{})
			releaseOnce := sync.OnceFunc(func() { close(release) })
			t.Cleanup(releaseOnce)
			var fake *fakeSecretManager
			var broker *secretBroker
			var local *secrets.Local
			want := []string{}
			switch store {
			case "gcp":
				fake = newFakeSecretManager()
				fake.beforeCall = func(method string) {
					if method == "AddSecretVersion" {
						<-release
					}
				}
				startFakeSecretManager(t, fake)
				useGCPIdentity(t, "authorized_user", "ada@example.com")
				broker = startSecretBrokerWith(t, gcpTestSettings(), gcpServedBy(fake, gcpTestSettings()))
				want = append(want, "DeadlineExceeded", "a late add may still land", "`agent-secrets secret show NEW_TEST`")
			case "aws":
				local = secrets.NewLocal()
				broker = startSecretBroker(t, servedBy(local))
				useAWS(t, hungCreate{Local: local, release: release}, testAccount, adaSignIn)
				want = append(want, "context deadline exceeded", "did not answer within 300ms", "the store may hold NEW_TEST, made with its value, or no NEW_TEST at all", "`agent-secrets secret show NEW_TEST` says which")
			}
			useStdin(t, "shh")

			type result struct {
				stderr string
				code   int
			}
			done := make(chan result, 1)
			start := time.Now()
			go func() {
				_, stderr, code := runSecret("create", "NEW_TEST", "--owner", "me", "--tier", "agent")
				done <- result{stderr, code}
			}()
			var got result
			select {
			case got = <-done:
			case <-time.After(10 * time.Second):
				releaseOnce()
				got = <-done
				t.Fatalf("the create waited on a store call the store never answered for more than 10s, past the 300ms deadline; once the store answered: exit %d, stderr %q", got.code, got.stderr)
			}
			if got.code != 1 {
				t.Fatalf("exit %d after %s, stderr %q; want 1", got.code, time.Since(start), got.stderr)
			}
			for _, w := range want {
				if !strings.Contains(got.stderr, w) {
					t.Fatalf("stderr %q; want it to name %q", got.stderr, w)
				}
			}
			switch store {
			case "gcp":
				calls := fake.called()
				if added, listed := slices.Index(calls, "AddSecretVersion"), slices.Index(calls, "ListSecretVersions"); added < 0 || listed < added || slices.Contains(calls, "DeleteSecret") || fake.written() != 1 {
					t.Fatalf("Secret Manager calls %v, %d secrets held; want the add, then its versions listed, and the secret kept", calls, fake.written())
				}
				if rereads, _ := broker.recorded(); !slices.Equal(rereads, []string{"NEW_TEST"}) {
					t.Fatalf("rereads %v; want NEW_TEST once", rereads)
				}
			case "aws":
				if _, err := local.DescribeSecret(context.Background(), &secretsmanager.DescribeSecretInput{SecretId: aws.String(policytest.ID("NEW_TEST"))}); err == nil {
					t.Fatal("the create whose CreateSecret ran out of time made NEW_TEST")
				}
				if rereads, _ := broker.recorded(); len(rereads) != 0 {
					t.Fatalf("rereads %v; want none, the create having made nothing", rereads)
				}
			}
		})
	}
}

// TestAWSSecretCreateReadsEveryAttemptTheSDKMade: the AWS SDK retries a CreateSecret whose answer
// it lost, and its error is the last attempt's alone. Over a real Secrets Manager client and HTTP:
// a store that carries out the first CreateSecret and drops its answer, then refuses the retry as
// ResourceExistsException - the secret it made a moment ago - leaves the create unable to say
// nothing was stored, so it says the store may hold the secret, with its value, and names `secret
// show`. A store that refuses the one attempt it sees says nothing was stored. Each exits 1.
func TestAWSSecretCreateReadsEveryAttemptTheSDKMade(t *testing.T) {
	for _, row := range []struct {
		name      string
		dropFirst bool
		attempts  int32
		want      string
		not       string
	}{
		{"the first answer dropped, the retry refused", true, 2, "may hold NEW_TEST, made with its value", "nothing was stored"},
		{"one attempt refused", false, 1, "nothing was stored", "may hold NEW_TEST"},
	} {
		t.Run(row.name, func(t *testing.T) {
			var attempts atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("X-Amz-Target") != "secretsmanager.CreateSecret" {
					http.Error(w, "unexpected call "+r.Header.Get("X-Amz-Target"), http.StatusBadRequest)
					return
				}
				_, _ = io.ReadAll(r.Body)
				if attempts.Add(1) == 1 && row.dropFirst {
					// The store carries the create out, and its answer is lost on the way.
					conn, _, err := w.(http.Hijacker).Hijack()
					if err == nil {
						_ = conn.Close()
					}
					return
				}
				w.Header().Set("Content-Type", "application/x-amz-json-1.1")
				w.Header().Set("X-Amzn-ErrorType", "ResourceExistsException")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"__type":"ResourceExistsException","Message":"the secret already exists"}`)
			}))
			t.Cleanup(srv.Close)
			sm := secretsmanager.New(secretsmanager.Options{
				Region:       "us-east-1",
				BaseEndpoint: aws.String(srv.URL),
				Credentials:  credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "not-a-secret", ""),
			})
			startSecretBroker(t, servedBy(secrets.NewLocal()))
			useAWS(t, sm, testAccount, adaSignIn)
			useStdin(t, "shh")

			_, stderr, code := runSecret("create", "NEW_TEST", "--owner", "ada@example.com", "--tier", "agent")
			if code != 1 || !strings.Contains(stderr, row.want) || strings.Contains(stderr, row.not) || strings.Contains(stderr, "shh") {
				t.Fatalf("exit %d, stderr %q; want 1 saying %q and not %q, the value never printed", code, stderr, row.want, row.not)
			}
			if got := attempts.Load(); got != row.attempts {
				t.Fatalf("the store saw %d CreateSecret attempts, want %d", got, row.attempts)
			}
		})
	}
}

// failedCreate is a Secrets Manager whose CreateSecret answers err, after it has created the
// secret as local does where stored is set, as a call the store carried out and whose answer was
// lost or broken reaches the caller.
type failedCreate struct {
	*secrets.Local
	stored bool
	err    error
}

func (f failedCreate) CreateSecret(ctx context.Context, in *secretsmanager.CreateSecretInput, o ...func(*secretsmanager.Options)) (*secretsmanager.CreateSecretOutput, error) {
	if f.stored {
		if _, err := f.Local.CreateSecret(ctx, in, o...); err != nil {
			return nil, err
		}
	}
	return nil, f.err
}

// TestAWSSecretCreateSaysWhetherAFailedCreateMayHaveStoredTheValue: on AWS the one CreateSecret
// makes the secret with its value. An error Secrets Manager answers before it acts - a name
// already taken, access denied, a request or a parameter refused, a limit reached - says nothing
// was stored. Any other error, which can follow a create the store carried out, says the store may
// hold the secret with its value, or nothing, and names `secret show NAME`, which tells them apart.
// Each exits 1.
func TestAWSSecretCreateSaysWhetherAFailedCreateMayHaveStoredTheValue(t *testing.T) {
	// The two facts the refusal tells apart: a definite refusal says nothing was stored; any other
	// says the store may hold the secret and names the command that tells.
	const nothing = "nothing was stored"
	const mayHold = "may hold NEW_TEST"
	for _, row := range []struct {
		name   string
		held   bool // the store holds NEW_TEST before the create
		fail   error
		stored bool // the failing create stores the secret first
		want   string
	}{
		{"a name already taken", true, nil, false, nothing},
		{"access denied", false, &smithy.GenericAPIError{Code: "AccessDeniedException", Message: "not authorized"}, false, nothing},
		{"a parameter refused", false, &types.InvalidParameterException{Message: aws.String("bad")}, false, nothing},
		{"a limit reached", false, &types.LimitExceededException{Message: aws.String("too many")}, false, nothing},
		{"a service fault after the store made it", false, &types.InternalServiceError{Message: aws.String("oops")}, true, mayHold},
		{"a dropped connection after the store made it", false, errors.New("read tcp: connection reset by peer"), true, mayHold},
		{"a service fault before the store made it", false, &smithy.GenericAPIError{Code: "ServiceUnavailable", Message: "try later"}, false, mayHold},
	} {
		t.Run(row.name, func(t *testing.T) {
			local := secrets.NewLocal()
			if row.held {
				local.Put(policytest.Secret("NEW_TEST", "ada@example.com", policy.TierAgent, "theirs"))
			}
			startSecretBroker(t, servedBy(local))
			var sm secretsAPI = local
			if row.fail != nil {
				sm = failedCreate{Local: local, stored: row.stored, err: row.fail}
			}
			useAWS(t, sm, testAccount, adaSignIn)
			useStdin(t, "shh")
			_, stderr, code := runSecret("create", "NEW_TEST", "--owner", "me", "--tier", "agent")
			other := mayHold
			if row.want == mayHold {
				other = nothing
			}
			if code != 1 || !strings.Contains(stderr, row.want) || strings.Contains(stderr, other) || strings.Contains(stderr, "shh") {
				t.Fatalf("exit %d, stderr %q; want 1 saying %q and not %q, the value never printed", code, stderr, row.want, other)
			}
			if row.want == mayHold && !strings.Contains(stderr, "`agent-secrets secret show NEW_TEST`") {
				t.Fatalf("stderr %q; want it to name `agent-secrets secret show NEW_TEST`", stderr)
			}
		})
	}
}

// TestCreateSignalAfterAFailedAWSStoreSaysWhatTheStoreMayHold: a signal whose cleanup runs once an
// AWS CreateSecret has failed, before the form has abandoned the create, reports that failure, so
// what the store may hold is said though the form, ending by the signal, never prints it: once,
// naming the command that tells which. The cleanup is run here as the signal would run it, right
// after the store's answer.
func TestCreateSignalAfterAFailedAWSStoreSaysWhatTheStoreMayHold(t *testing.T) {
	local := secrets.NewLocal()
	startSecretBroker(t, servedBy(local))
	useAWS(t, failedCreate{Local: local, stored: true, err: &types.InternalServiceError{Message: aws.String("oops")}}, testAccount, adaSignIn)
	s, err := openSession(context.Background(), io.Discard, "secret create")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var stderr strings.Builder
	p := &pendingCreate{s: s, form: "secret create", name: "NEW_TEST", stdout: io.Discard, stderr: &stderr, created: s.backend.prepareCreate("new-test", map[string]string{policy.TagOwner: "ada@example.com", policy.TagTier: policy.TierAgent})}
	if err := p.begin(); err != nil {
		t.Fatal(err)
	}
	if err := p.store("shh"); err == nil {
		t.Fatal("store = nil; want the create's failure")
	}
	p.interrupted()
	if out := stderr.String(); strings.Count(out, "the store may hold NEW_TEST") != 1 || !strings.Contains(out, "`agent-secrets secret show NEW_TEST`") || strings.Contains(out, "shh") {
		t.Fatalf("the signal's cleanup printed %q; want what the store may hold, once, naming `agent-secrets secret show NEW_TEST`, and never the value", out)
	}
}

// TestAWSSecretCreateRecoveryCommandSignsInAsTheCreateDid: the command a create prints for telling
// whether a failed CreateSecret made the secret carries the AWS_PROFILE prefix the create ran
// under, so it reads the store as the identity that wrote to it.
func TestAWSSecretCreateRecoveryCommandSignsInAsTheCreateDid(t *testing.T) {
	local := secrets.NewLocal()
	startSecretBroker(t, servedBy(local))
	useAWS(t, failedCreate{Local: local, stored: true, err: &types.InternalServiceError{Message: aws.String("oops")}}, testAccount, adaSignIn)
	t.Setenv("AWS_PROFILE", "work")
	useStdin(t, "shh")
	_, stderr, code := runSecret("create", "NEW_TEST", "--owner", "ada@example.com", "--tier", "agent")
	if code != 1 || !strings.Contains(stderr, "`AWS_PROFILE=work agent-secrets secret show NEW_TEST`") {
		t.Fatalf("exit %d, stderr %q; want 1 naming `AWS_PROFILE=work agent-secrets secret show NEW_TEST`", code, stderr)
	}
}

// TestAWSSecretCreateValueReadHasNoWindow: on AWS the create makes nothing before it has the value,
// so no secret waits in the store and the broker holds nothing pending: its read of the value has
// no window. With GCP's window shortened to a nanosecond, which is over before any value is read -
// a create given that deadline refuses the value, whenever it comes - a value piped in at once is
// still stored on AWS.
func TestAWSSecretCreateValueReadHasNoWindow(t *testing.T) {
	restore := valueReadWindow
	valueReadWindow = time.Nanosecond
	t.Cleanup(func() { valueReadWindow = restore })
	local := secrets.NewLocal()
	startSecretBroker(t, servedBy(local))
	useAWS(t, local, testAccount, adaSignIn)
	useStdin(t, "shh")

	if _, stderr, code := runSecret("create", "NEW_TEST", "--owner", "ada@example.com", "--tier", "agent"); code != 0 {
		t.Fatalf("exit %d, stderr %q; want 0: the value is read with no window on AWS", code, stderr)
	}
	if v := valueOf(t, local, "NEW_TEST"); v != "shh" {
		t.Fatalf("NEW_TEST = %q, want the value piped in", v)
	}
}
