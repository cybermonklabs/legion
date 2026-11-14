package policy_test

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/smithy-go"

	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/policy/policytest"
	"github.com/sjawhar/envoy/internal/broker/secrets"
	"github.com/sjawhar/envoy/internal/metrics"
)

// undecodable is a stored value no kind's decoder reads: it is not JSON.
const undecodable = "not json"

// refusingReads is a secrets.Local whose GetSecretValue answers AccessDeniedException, as IAM
// refuses a role the read, while refusing is set.
type refusingReads struct {
	*secrets.Local
	mu       sync.Mutex
	refusing bool
}

func (r *refusingReads) GetSecretValue(ctx context.Context, in *secretsmanager.GetSecretValueInput, opts ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error) {
	r.mu.Lock()
	refusing := r.refusing
	r.mu.Unlock()
	if refusing {
		return nil, &smithy.GenericAPIError{Code: "AccessDeniedException", Message: "not authorized to perform: secretsmanager:GetSecretValue"}
	}
	return r.Local.GetSecretValue(ctx, in, opts...)
}

func (r *refusingReads) refuse(refusing bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refusing = refusing
}

// valueCurrent is the Current a pod boots over store, read through its refusals (bootThrough).
func valueCurrent(t *testing.T, store *refusingReads) (*policy.Current, *metrics.Registry, fmt.Stringer) {
	t.Helper()
	return bootThrough(t, store.Local, store)
}

// valueReads is how many values the policy over registry has read: its read="value" count.
func valueReads(t *testing.T, registry *metrics.Registry) int {
	t.Helper()
	for line := range strings.Lines(samplesOf(t, registry, "broker_policy_store_reads_total")) {
		if count, ok := strings.CutPrefix(line, `broker_policy_store_reads_total{read="value"} `); ok {
			n, err := strconv.Atoi(strings.TrimSpace(count))
			if err != nil {
				t.Fatalf("read=value sample %q: %v", line, err)
			}
			return n
		}
	}
	return 0
}

// logChecker answers, each time it is called, what logged gained since the call before.
func logChecker(logged fmt.Stringer) func() string {
	seen := 0
	return func() string {
		all := logged.String()
		gained := all[seen:]
		seen = len(all)
		return gained
	}
}

// refresh is cur.Refresh, failing t on its error.
func refresh(t *testing.T, cur *policy.Current) {
	t.Helper()
	if err := cur.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
}

// TestTheBootLoadRefusesAValueTheDecoderRefusesBeforeTheFirstRequest: NewCurrent reads each kind
// secret's value before it answers, so the policy it answers already refuses a github-app secret
// whose value does not decode, as value-malformed with its owner, in one first_load
// RefusedMessage line and one broker_unservable_secrets series, while a kind secret whose value
// decodes is served and a secret of no kind is never read. A reload, and a reread of either name,
// read no value and write nothing, and the reread of the refused one answers its reason.
//
// The named revert: drop readValues' decode (take every read as decoded); the first load then
// serves BAD_APP and this test fails.
func TestTheBootLoadRefusesAValueTheDecoderRefusesBeforeTheFirstRequest(t *testing.T) {
	store := &refusingReads{Local: secrets.NewLocal(
		kindSecret("BAD_APP", policy.KindGitHubApp, undecodable),
		kindSecret("GOOD_LOGIN", policy.KindGoogleLogin, policytest.KindValue(policy.KindGoogleLogin)),
		policytest.Secret("PLAIN_KEY", kindOwner, policy.TierAgent, undecodable),
	)}
	cur, registry, logged := valueCurrent(t, store)
	gained := logChecker(logged)
	bad := policytest.ID("BAD_APP")

	set := cur.Get()
	if _, served := set.Secrets["BAD_APP"]; served {
		t.Fatal("the first load serves BAD_APP, whose value the decoder refuses")
	}
	refusedAs(t, set, "BAD_APP", policy.ReasonValueMalformed)
	for _, name := range []string{"GOOD_LOGIN", "PLAIN_KEY"} {
		if _, served := set.Secrets[name]; !served {
			t.Fatalf("the first load does not serve %s", name)
		}
	}
	if got, want := gained(), refusedLine(bad, policy.ReasonValueMalformed, kindOwner, true, ""); got != want {
		t.Fatalf("the first load logged:\n%s\nwant:\n%s", got, want)
	}
	if got, want := samplesOf(t, registry, "broker_unservable_secrets"), unservable(bad, policy.ReasonValueMalformed, kindOwner); got != want {
		t.Fatalf("broker_unservable_secrets reads\n%s\nwant\n%s", got, want)
	}
	if n := valueReads(t, registry); n != 2 {
		t.Fatalf("the first load read %d values; want 2, one for each kind secret and none for PLAIN_KEY", n)
	}

	refresh(t, cur)
	lk, err := cur.RefreshOne(context.Background(), "BAD_APP")
	if err != nil || lk.Served || lk.Reason != policy.ReasonValueMalformed || lk.Owner != kindOwner {
		t.Fatalf("RefreshOne(BAD_APP) = %+v, %v; want refused as %s", lk, err, policy.ReasonValueMalformed)
	}
	if lk, err := cur.RefreshOne(context.Background(), "GOOD_LOGIN"); err != nil || !lk.Served {
		t.Fatalf("RefreshOne(GOOD_LOGIN) = %+v, %v; want served", lk, err)
	}
	if got := gained(); got != "" {
		t.Fatalf("a reload and rereads of names whose values are decoded logged:\n%s", got)
	}
	if n := valueReads(t, registry); n != 2 {
		t.Fatalf("after a reload and two rereads the policy has read %d values; want the first load's 2", n)
	}
}

// TestANewVersionIsReadOnceAndASecretOfNoKindIsNeverRead: the policy reads a kind secret's value
// once for each version a load lists, never at each load: the boot reads it, two reloads and a
// reread read nothing, a new value is read by the one load that first lists it, and the load
// after reads nothing. A secret of no kind is never read, however its value changes. A new value
// moves no policy Version, so a value write ends no grant.
//
// The named revert: decode on every load (ignore the decoded key); the second load's read=value
// count then fails this test.
func TestANewVersionIsReadOnceAndASecretOfNoKindIsNeverRead(t *testing.T) {
	login := kindSecret("LOGIN_KEY", policy.KindGoogleLogin, policytest.KindValue(policy.KindGoogleLogin))
	store := &refusingReads{Local: secrets.NewLocal(login, policytest.Secret("PLAIN_KEY", kindOwner, policy.TierAgent, "plain-v1"))}
	cur, registry, logged := valueCurrent(t, store)
	version := cur.Get().Version

	for _, step := range []struct {
		name  string
		act   func()
		reads int
	}{
		{"the first load", func() {}, 1},
		{"a reload over the same store", func() { refresh(t, cur) }, 1},
		{"a second reload", func() { refresh(t, cur) }, 1},
		{"rereads of both names", func() {
			for _, name := range []string{"LOGIN_KEY", "PLAIN_KEY"} {
				if lk, err := cur.RefreshOne(context.Background(), name); err != nil || !lk.Served {
					t.Fatalf("RefreshOne(%s) = %+v, %v; want served", name, lk, err)
				}
			}
		}, 1},
		{"a new value of the kind secret", func() {
			// The same fields with a trailing line feed: new bytes, so a new version, which the
			// shape rule trims back to a value the decoder reads.
			changed := login
			changed.Value += "\n"
			store.Put(changed)
			refresh(t, cur)
		}, 2},
		{"the load after it", func() { refresh(t, cur) }, 2},
		{"a new value of the secret of no kind", func() {
			store.Put(policytest.Secret("PLAIN_KEY", kindOwner, policy.TierAgent, "plain-v2"))
			refresh(t, cur)
		}, 2},
	} {
		step.act()
		if n := valueReads(t, registry); n != step.reads {
			t.Fatalf("after %s the policy has read %d values; want %d", step.name, n, step.reads)
		}
		if _, served := cur.Get().Secrets["LOGIN_KEY"]; !served {
			t.Fatalf("after %s LOGIN_KEY is not served", step.name)
		}
	}
	if cur.Get().Version != version {
		t.Fatal("new values moved the policy's Version; a value write must end no grant")
	}
	if logged.String() != "" {
		t.Fatalf("reading values that decode logged:\n%s", logged)
	}
}

// TestARetagOfTheKindOrShapeOnTheSameVersionDecodesTheValueAgain: a retag changes what the value
// is read as and leaves its version as it was, so the policy decodes the value again under the
// kind and shape each retag lists: a shape retag reads it once more and still serves it, a kind
// retag to github-app reads it once more and refuses the google-login value as value-malformed,
// and the retag back serves it again, each with one line, the value's version the same throughout.
func TestARetagOfTheKindOrShapeOnTheSameVersionDecodesTheValueAgain(t *testing.T) {
	value := policytest.KindValue(policy.KindGoogleLogin)
	login := kindSecret("RETAGGED_KEY", policy.KindGoogleLogin, value)
	store := &refusingReads{Local: secrets.NewLocal(login)}
	cur, registry, logged := valueCurrent(t, store)
	gained := logChecker(logged)
	id, version := policytest.ID("RETAGGED_KEY"), cur.Get().ValueVersion("RETAGGED_KEY")
	if version == "" {
		t.Fatal("the policy lists no version of RETAGGED_KEY's value")
	}
	shaped := login
	shaped.Tags = map[string]string{policy.TagShape: policy.ShapeToken}
	for key, tag := range login.Tags {
		shaped.Tags[key] = tag
	}
	app := kindSecret("RETAGGED_KEY", policy.KindGitHubApp, value)
	app.Tags[policy.TagShape] = policy.ShapeToken

	for _, step := range []struct {
		name   string
		secret secrets.LocalSecret
		reads  int
		served bool
		lines  string
	}{
		{"a shape retag", shaped, 2, true, ""},
		{"a kind retag to github-app", app, 3, false, refusedLine(id, policy.ReasonValueMalformed, kindOwner, false, "")},
		{"the kind retag back", shaped, 4, true, servableLine(id, policy.ReasonValueMalformed, kindOwner, "servable")},
	} {
		store.Put(step.secret)
		refresh(t, cur)
		if got := cur.Get().ValueVersion("RETAGGED_KEY"); got != version {
			t.Fatalf("after %s the value's version is %q, was %q: a retag must leave it", step.name, got, version)
		}
		if n := valueReads(t, registry); n != step.reads {
			t.Fatalf("after %s the policy has read %d values; want %d", step.name, n, step.reads)
		}
		if _, served := cur.Get().Secrets["RETAGGED_KEY"]; served != step.served {
			t.Fatalf("after %s RETAGGED_KEY served = %t, want %t", step.name, served, step.served)
		}
		if got := gained(); got != step.lines {
			t.Fatalf("%s logged:\n%s\nwant:\n%s", step.name, got, step.lines)
		}
	}
}

// TestAFailedValueReadMarksNothingAndLogsWhenItStartsAndEnds: a read of a new value that the store
// refuses marks nothing: the secret, served before, stays served through three loads that each
// read again, and the run writes one ValueReadFailedMessage line, naming the secret, the version
// read and the store's refusal and never the value. The load whose read succeeds writes one
// ValueReadRecoveredMessage line, and, the value being one the decoder refuses, its
// RefusedMessage line; the load after writes nothing.
//
// The named revert: write the failure line on every failed read, not only the first of a run; the
// second failing load then fails this test.
func TestAFailedValueReadMarksNothingAndLogsWhenItStartsAndEnds(t *testing.T) {
	store := &refusingReads{Local: secrets.NewLocal(kindSecret("APP_KEY", policy.KindGitHubApp, policytest.KindValue(policy.KindGitHubApp)))}
	cur, registry, logged := valueCurrent(t, store)
	gained := logChecker(logged)
	id := policytest.ID("APP_KEY")
	if n := valueReads(t, registry); n != 1 {
		t.Fatalf("the first load read %d values; want 1", n)
	}

	store.refuse(true)
	store.Put(kindSecret("APP_KEY", policy.KindGitHubApp, undecodable))
	var failed string
	for load := 1; load <= 3; load++ {
		refresh(t, cur)
		if _, served := cur.Get().Secrets["APP_KEY"]; !served {
			t.Fatalf("after failing load %d APP_KEY is not served; a failed read must keep its state", load)
		}
		if n := valueReads(t, registry); n != 1+load {
			t.Fatalf("after failing load %d the policy has read %d values; want %d, one more for each load", load, n, 1+load)
		}
		failed += gained()
	}
	version := cur.Get().ValueVersion("APP_KEY")
	if strings.Count(failed, "\n") != 1 || !strings.HasPrefix(failed, "WARN "+policy.ValueReadFailedMessage+" name="+id+" version="+version+" error=") ||
		!strings.Contains(failed, "AccessDeniedException") || strings.Contains(failed, undecodable) {
		t.Fatalf("three failing loads logged:\n%s\nwant one %q line naming %s, version %s and the store's refusal, and never the value", failed, policy.ValueReadFailedMessage, id, version)
	}
	if got := samplesOf(t, registry, "broker_unservable_secrets"); got != "" {
		t.Fatalf("failed reads added broker_unservable_secrets series:\n%s", got)
	}

	store.refuse(false)
	refresh(t, cur)
	if got, want := gained(), "INFO "+policy.ValueReadRecoveredMessage+" name="+id+" version="+version+"\n"+refusedLine(id, policy.ReasonValueMalformed, kindOwner, false, ""); got != want {
		t.Fatalf("the load whose read succeeds logged:\n%s\nwant:\n%s", got, want)
	}
	refresh(t, cur)
	if got := gained(); got != "" {
		t.Fatalf("the load after logged:\n%s", got)
	}
	if n := valueReads(t, registry); n != 5 {
		t.Fatalf("the policy has read %d values; want 5", n)
	}
}

// TestAMintTheDecoderRefusesRefusesTheSecretTheLoadCouldNotRead: a pod whose load could not read a
// value (its one failure line written) still refuses the secret once a mint's decoder refuses it
// (RefuseValue, at the version the policy lists), with one RefusedMessage line and one series; a
// refusal naming a version the policy no longer lists does nothing. Later loads that still cannot
// read keep the refusal while they list that version, and the first that lists a new version
// drops it, though that read fails too, writing ServableMessage. The load that reads that version
// and finds it refused refuses the secret again.
func TestAMintTheDecoderRefusesRefusesTheSecretTheLoadCouldNotRead(t *testing.T) {
	store := &refusingReads{Local: secrets.NewLocal(kindSecret("APP_KEY", policy.KindGitHubApp, undecodable)), refusing: true}
	cur, registry, logged := valueCurrent(t, store)
	gained := logChecker(logged)
	id := policytest.ID("APP_KEY")
	if got := gained(); strings.Count(got, "\n") != 1 || !strings.HasPrefix(got, "WARN "+policy.ValueReadFailedMessage+" name="+id) {
		t.Fatalf("the first load, whose read was refused, logged:\n%s\nwant one %q line", got, policy.ValueReadFailedMessage)
	}
	if _, served := cur.Get().Secrets["APP_KEY"]; !served {
		t.Fatal("the first load could not read APP_KEY's value and does not serve it; a failed read marks nothing")
	}

	cur.RefuseValue(context.Background(), "APP_KEY", "a-version-not-listed", policy.KindGitHubApp, "")
	if _, served := cur.Get().Secrets["APP_KEY"]; !served {
		t.Fatal("a refusal at a version the policy does not list refused APP_KEY")
	}
	cur.RefuseValue(context.Background(), "APP_KEY", cur.Get().ValueVersion("APP_KEY"), policy.KindGitHubApp, "")
	if _, served := cur.Get().Secrets["APP_KEY"]; served {
		t.Fatal("a mint's refusal at the listed version left APP_KEY served")
	}
	refusedAs(t, cur.Get(), "APP_KEY", policy.ReasonValueMalformed)
	if got, want := gained(), refusedLine(id, policy.ReasonValueMalformed, kindOwner, false, ""); got != want {
		t.Fatalf("the mint's refusal logged:\n%s\nwant:\n%s", got, want)
	}
	if got, want := samplesOf(t, registry, "broker_unservable_secrets"), unservable(id, policy.ReasonValueMalformed, kindOwner); got != want {
		t.Fatalf("broker_unservable_secrets reads\n%s\nwant\n%s", got, want)
	}

	refresh(t, cur)
	if lk, err := cur.RefreshOne(context.Background(), "APP_KEY"); err != nil || lk.Reason != policy.ReasonValueMalformed {
		t.Fatalf("RefreshOne(APP_KEY) after a load at the same version = %+v, %v; want still refused as %s", lk, err, policy.ReasonValueMalformed)
	}
	if got := gained(); got != "" {
		t.Fatalf("a load and a reread at the refused version, their reads still refused, logged:\n%s", got)
	}

	store.Put(kindSecret("APP_KEY", policy.KindGitHubApp, undecodable+" either"))
	refresh(t, cur)
	if _, served := cur.Get().Secrets["APP_KEY"]; !served {
		t.Fatal("the load that lists a new version keeps the old version's refusal")
	}
	if got, want := gained(), servableLine(id, policy.ReasonValueMalformed, kindOwner, "servable"); got != want {
		t.Fatalf("the load that lists a new version logged:\n%s\nwant:\n%s", got, want)
	}

	store.refuse(false)
	refresh(t, cur)
	refusedAs(t, cur.Get(), "APP_KEY", policy.ReasonValueMalformed)
	if got, want := gained(), "INFO "+policy.ValueReadRecoveredMessage+" name="+id+" version="+cur.Get().ValueVersion("APP_KEY")+"\n"+refusedLine(id, policy.ReasonValueMalformed, kindOwner, false, ""); got != want {
		t.Fatalf("the load that reads the new version logged:\n%s\nwant:\n%s", got, want)
	}
}

// TestASecretThatLeavesTheListingDropsItsValueRefusal: a refused value is the secret's while the
// listing holds it: a delete ends the refusal (ServableMessage, now=absent, and no series), the
// same value stored again is read again by the load that lists it and refused again, and a new
// value the decoder reads makes it servable.
func TestASecretThatLeavesTheListingDropsItsValueRefusal(t *testing.T) {
	store := &refusingReads{Local: secrets.NewLocal(kindSecret("APP_KEY", policy.KindGitHubApp, undecodable))}
	cur, registry, logged := valueCurrent(t, store)
	gained := logChecker(logged)
	id := policytest.ID("APP_KEY")
	gained() // the first load's refusal, which TestTheBootLoadRefusesAValueTheDecoderRefusesBeforeTheFirstRequest pins

	for _, step := range []struct {
		name   string
		act    func()
		reads  int
		lines  string
		series string
	}{
		{"a delete", func() { store.Delete(id) }, 1, servableLine(id, policy.ReasonValueMalformed, kindOwner, "absent"), ""},
		{"the same value stored again", func() { store.Put(kindSecret("APP_KEY", policy.KindGitHubApp, undecodable)) }, 2,
			refusedLine(id, policy.ReasonValueMalformed, kindOwner, false, ""), unservable(id, policy.ReasonValueMalformed, kindOwner)},
		{"a value the decoder reads", func() {
			store.Put(kindSecret("APP_KEY", policy.KindGitHubApp, policytest.KindValue(policy.KindGitHubApp)))
		}, 3, servableLine(id, policy.ReasonValueMalformed, kindOwner, "servable"), ""},
	} {
		step.act()
		refresh(t, cur)
		if got := gained(); got != step.lines {
			t.Fatalf("after %s the load logged:\n%s\nwant:\n%s", step.name, got, step.lines)
		}
		if n := valueReads(t, registry); n != step.reads {
			t.Fatalf("after %s the policy has read %d values; want %d", step.name, n, step.reads)
		}
		if got := samplesOf(t, registry, "broker_unservable_secrets"); got != step.series {
			t.Fatalf("after %s broker_unservable_secrets reads\n%s\nwant\n%s", step.name, got, step.series)
		}
	}
}

// bootThrough is the Current a pod boots over local, its Secrets Manager calls made through
// client, with the store-read counter and broker_unservable_secrets on a registry of its own as
// main wires them, and the lines it writes captured from before its first load.
func bootThrough(t *testing.T, local *secrets.Local, client secrets.SecretsManagerAPI) (*policy.Current, *metrics.Registry, fmt.Stringer) {
	t.Helper()
	registry := metrics.New()
	loader := policy.Loader{Store: policytest.StoreThrough(local, client), Prefix: policytest.Prefix, StoreReads: policy.NewStoreReads(registry)}
	logged := policytest.CaptureLog(t)
	cur, err := policy.NewCurrent(t.Context(), loader, time.Hour)
	if err != nil {
		t.Fatalf("NewCurrent: %v", err)
	}
	policy.NewUnservableSecrets(registry, cur)
	return cur, registry, logged
}

// binaryValues is a secrets.Local whose GetSecretValue answers each value as SecretBinary alone,
// with no SecretString, as Secrets Manager answers a secret whose value was stored as binary.
type binaryValues struct{ *secrets.Local }

func (b binaryValues) GetSecretValue(ctx context.Context, in *secretsmanager.GetSecretValueInput, opts ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error) {
	out, err := b.Local.GetSecretValue(ctx, in, opts...)
	if err != nil {
		return nil, err
	}
	return &secretsmanager.GetSecretValueOutput{Name: out.Name, ARN: out.ARN, SecretBinary: []byte(*out.SecretString)}, nil
}

// TestAValueThatIsNotTextIsRefusedAndReadOnce: a kind secret whose store answers a value that is
// not text (secrets.ErrValueNotText; here Secrets Manager's SecretBinary with no SecretString) is
// the decoder's case, not a failed read, since no mint could use it: the first load refuses it
// as value-malformed, in one first_load RefusedMessage line and one broker_unservable_secrets
// series, and writes no ValueReadFailedMessage. A reload and a reread read the value no more and
// write nothing.
func TestAValueThatIsNotTextIsRefusedAndReadOnce(t *testing.T) {
	local := secrets.NewLocal(kindSecret("BINARY_APP", policy.KindGitHubApp, policytest.KindValue(policy.KindGitHubApp)))
	cur, registry, logged := bootThrough(t, local, binaryValues{local})
	gained := logChecker(logged)
	id := policytest.ID("BINARY_APP")

	refusedAs(t, cur.Get(), "BINARY_APP", policy.ReasonValueMalformed)
	if got, want := gained(), refusedLine(id, policy.ReasonValueMalformed, kindOwner, true, ""); got != want {
		t.Fatalf("the first load logged:\n%s\nwant:\n%s", got, want)
	}
	if got, want := samplesOf(t, registry, "broker_unservable_secrets"), unservable(id, policy.ReasonValueMalformed, kindOwner); got != want {
		t.Fatalf("broker_unservable_secrets reads\n%s\nwant\n%s", got, want)
	}
	refresh(t, cur)
	if lk, err := cur.RefreshOne(context.Background(), "BINARY_APP"); err != nil || lk.Reason != policy.ReasonValueMalformed {
		t.Fatalf("RefreshOne(BINARY_APP) = %+v, %v; want refused as %s", lk, err, policy.ReasonValueMalformed)
	}
	if got := gained(); got != "" {
		t.Fatalf("a reload and a reread of a value refused as no text logged:\n%s", got)
	}
	if n := valueReads(t, registry); n != 1 {
		t.Fatalf("after a reload and a reread the policy has read %d values; want the first load's 1", n)
	}
}

// stalledRead is a secrets.Local whose next GetSecretValue, once stall arms it, waits until
// released and then answers AccessDeniedException; every other read is Local's own.
type stalledRead struct {
	*secrets.Local
	mu       sync.Mutex
	entered  chan struct{}
	released chan struct{}
}

// stall arms the next read, answering the channel it closes once that read is waiting and the
// one to close to let it fail.
func (s *stalledRead) stall() (entered <-chan struct{}, release chan<- struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entered, s.released = make(chan struct{}), make(chan struct{})
	return s.entered, s.released
}

func (s *stalledRead) GetSecretValue(ctx context.Context, in *secretsmanager.GetSecretValueInput, opts ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error) {
	s.mu.Lock()
	entered, released := s.entered, s.released
	s.entered, s.released = nil, nil
	s.mu.Unlock()
	if entered == nil {
		return s.Local.GetSecretValue(ctx, in, opts...)
	}
	close(entered)
	<-released
	return nil, &smithy.GenericAPIError{Code: "AccessDeniedException", Message: "not authorized to perform: secretsmanager:GetSecretValue"}
}

// TestAFailedReadRecordedAfterASuccessfulReadOfTheSameValueRecordsNothing: a reload and a reread
// read a new value outside the writer lock, so both can read it at once. When the reread's read
// succeeds and is recorded first, the reload's failed read of the same value is stale: it writes
// no ValueReadFailedMessage, and the next load, which reads nothing since that value is decoded,
// writes nothing either.
func TestAFailedReadRecordedAfterASuccessfulReadOfTheSameValueRecordsNothing(t *testing.T) {
	local := secrets.NewLocal(kindSecret("APP_KEY", policy.KindGitHubApp, policytest.KindValue(policy.KindGitHubApp)))
	store := &stalledRead{Local: local}
	cur, registry, logged := bootThrough(t, local, store)
	gained := logChecker(logged)
	if got := gained(); got != "" {
		t.Fatalf("the first load logged:\n%s", got)
	}

	// A new value that decodes, so a new version for both to read.
	local.Put(kindSecret("APP_KEY", policy.KindGitHubApp, policytest.KindValue(policy.KindGitHubApp)+"\n"))
	entered, release := store.stall()
	reloaded := make(chan error, 1)
	go func() { reloaded <- cur.Refresh(context.Background()) }()
	<-entered
	if lk, err := cur.RefreshOne(context.Background(), "APP_KEY"); err != nil || !lk.Served {
		t.Fatalf("RefreshOne(APP_KEY) while the reload's read waits = %+v, %v; want served", lk, err)
	}
	close(release)
	if err := <-reloaded; err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if got := gained(); got != "" {
		t.Fatalf("a failed read of a value the reread had read logged:\n%s", got)
	}
	if n := valueReads(t, registry); n != 3 {
		t.Fatalf("the policy has read %d values; want 3, the boot's, the reload's and the reread's", n)
	}

	refresh(t, cur)
	if got := gained(); got != "" {
		t.Fatalf("the load after logged:\n%s", got)
	}
	if _, served := cur.Get().Secrets["APP_KEY"]; !served {
		t.Fatal("APP_KEY, whose new value decodes, is not served")
	}
}

// TestAMintsRefusalUnderAKeyARetagReplacedRefusesNothing: a mint refuses the value under the
// version, kind and shape its release decoded it as (RefuseValue), and a retag of the shape
// leaves the version and makes another key, which the policy decodes afresh. So a refusal from a
// mint that decoded under the key before the retag refuses nothing, nor does one naming another
// kind, and one under the key listed now refuses the secret, with one RefusedMessage line.
func TestAMintsRefusalUnderAKeyARetagReplacedRefusesNothing(t *testing.T) {
	secret := kindSecret("APP_KEY", policy.KindGitHubApp, policytest.KindValue(policy.KindGitHubApp))
	store := &refusingReads{Local: secrets.NewLocal(secret)}
	cur, _, logged := valueCurrent(t, store)
	gained := logChecker(logged)
	id, version := policytest.ID("APP_KEY"), cur.Get().ValueVersion("APP_KEY")
	shaped := secret
	shaped.Tags = map[string]string{policy.TagShape: policy.ShapeToken}
	for key, tag := range secret.Tags {
		shaped.Tags[key] = tag
	}
	store.Put(shaped)
	refresh(t, cur)
	if got := cur.Get().ValueVersion("APP_KEY"); got != version {
		t.Fatalf("the shape retag moved the value's version from %q to %q", version, got)
	}

	for _, stale := range []struct{ kind, shape string }{{policy.KindGitHubApp, ""}, {policy.KindGoogleLogin, policy.ShapeToken}} {
		cur.RefuseValue(context.Background(), "APP_KEY", version, stale.kind, stale.shape)
		if _, served := cur.Get().Secrets["APP_KEY"]; !served {
			t.Fatalf("a mint's refusal at the listed version as %s under shape %q, a key the policy no longer lists, refused APP_KEY", stale.kind, stale.shape)
		}
	}
	if got := gained(); got != "" {
		t.Fatalf("the retag and the refusals of keys it replaced logged:\n%s", got)
	}
	cur.RefuseValue(context.Background(), "APP_KEY", version, policy.KindGitHubApp, policy.ShapeToken)
	refusedAs(t, cur.Get(), "APP_KEY", policy.ReasonValueMalformed)
	if got, want := gained(), refusedLine(id, policy.ReasonValueMalformed, kindOwner, false, ""); got != want {
		t.Fatalf("the mint's refusal under the listed key logged:\n%s\nwant:\n%s", got, want)
	}
}
