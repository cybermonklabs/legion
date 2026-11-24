package policy_test

import (
	"context"
	"strconv"
	"testing"
	"testing/synctest"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"

	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/secretstore"
	awsstore "github.com/sjawhar/envoy/internal/cipher/secretstore/aws"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/aws/awstest"
	"github.com/sjawhar/envoy/internal/metrics"
)

// pendingLine is the one line Current writes when it first finds the secret whose whole name in the
// store is id waiting for its value, owned by owner, on the pod's first load or not.
func pendingLine(id, owner string, firstLoad bool) string {
	return "INFO " + policy.PendingMessage + " name=" + id + " reason=" + policy.ReasonPendingValue + " owner=" + textValue(owner) + " first_load=" + strconv.FormatBool(firstLoad) + "\n"
}

// createdWithoutValue is a secret owned by owner at tier agent, holding no value, that the store
// created at created, carrying the create label `agent-secrets secret create` gives a secret it
// makes before its value (secretstore.CreateLabel).
func createdWithoutValue(name string, created time.Time) awstest.LocalSecret {
	s := policytest.Secret(name, owner, policy.TierAgent, "")
	s.Tags[secretstore.CreateLabel] = "0123456789abcdef0123456789abcdef"
	s.CreatedAt = &created
	return s
}

// TestASecretWithoutAValueIsPendingForItsFirstTenMinutes: a secret a create made before its value
// (it carries the create label and holds no version) that the store created less than ten minutes
// ago is a create still waiting for its value, not an unservable secret: the policy serves it to
// nobody, refuses it for nothing, counts it in no broker_unservable_secrets series, writes one INFO
// line when it first finds it, and answers a reread pending-value. One second before ten minutes
// it is still pending, and nothing more is written however often it is read; at ten minutes, the
// moment `agent-secrets secret create` ends its prompt and discards the secret, the load refuses
// it no-current-value, with the ERROR line and the series that refusal has always had. The ten
// minutes are written here as a literal, so a change of the window fails this test.
func TestASecretWithoutAValueIsPendingForItsFirstTenMinutes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		created := time.Now()
		store := awstest.NewLocal(createdWithoutValue("WAITING_KEY", created))
		registry := metrics.New()
		logged := policytest.CaptureLog(t)
		cur := policytest.MeteredCurrent(t, store, registry)
		id := policytest.ID("WAITING_KEY")
		ctx := context.Background()

		want := pendingLine(id, owner, true) + noProfilesLine
		requirePending := func(when string) {
			t.Helper()
			set := cur.Get()
			_, served := set.Secrets["WAITING_KEY"]
			_, refused := set.Refused[id]
			if served || refused || set.Pending[id] != (policy.Pending{Owner: owner, Created: created}) {
				t.Fatalf("%s: served %t, refused %+v, pending %+v; want WAITING_KEY pending alone", when, served, set.Refused, set.Pending)
			}
			if got := samplesOf(t, registry, "broker_unservable_secrets"); got != "" {
				t.Fatalf("%s: broker_unservable_secrets reads\n%s\nwant no series", when, got)
			}
			if logged.String() != want {
				t.Fatalf("%s: logged\n%s\nwant\n%s", when, logged, want)
			}
		}
		requirePending("the first load")

		time.Sleep(10*time.Minute - time.Second)
		if lk, err := cur.RefreshOne(ctx, "WAITING_KEY"); err != nil || lk.Served || lk.Reason != policy.ReasonPendingValue || lk.Owner != owner {
			t.Fatalf("a reread a second before the window ends = %+v, %v; want pending-value, owned by %s", lk, err, owner)
		}
		if err := cur.Refresh(ctx); err != nil {
			t.Fatal(err)
		}
		requirePending("a reread and a load a second before the window ends")

		time.Sleep(time.Second)
		if err := cur.Refresh(ctx); err != nil {
			t.Fatal(err)
		}
		set := cur.Get()
		if refusal := set.Refused[id]; refusal != (policy.Refusal{Reason: policy.ReasonNoCurrentValue, Owner: owner}) || len(set.Pending) != 0 {
			t.Fatalf("at the window's end: refused %+v, pending %+v; want WAITING_KEY refused no-current-value and nothing pending", set.Refused, set.Pending)
		}
		if want += refusedLine(id, policy.ReasonNoCurrentValue, owner, false, ""); logged.String() != want {
			t.Fatalf("at the window's end: logged\n%s\nwant\n%s", logged, want)
		}
		if got, want := samplesOf(t, registry, "broker_unservable_secrets"), unservable(id, policy.ReasonNoCurrentValue, owner); got != want {
			t.Fatalf("at the window's end: broker_unservable_secrets reads\n%s\nwant\n%s", got, want)
		}
	})
}

// TestAStrandedSecretIsRefusedButNeverDeleted: the broker deletes nothing. Only the create that
// made a secret deletes it, so one whose create was killed keeps its create label and its missing
// value: past the window the broker refuses it no-current-value and the store still holds it with
// no version, for its owner to find (`agent-secrets secret show NAME`, `versions: none`) and clear.
func TestAStrandedSecretIsRefusedButNeverDeleted(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		store := awstest.NewLocal(createdWithoutValue("STRANDED_KEY", time.Now()))
		cur := policytest.Current(t, store)
		id := policytest.ID("STRANDED_KEY")

		time.Sleep(10 * time.Minute)
		if err := cur.Refresh(ctx); err != nil {
			t.Fatal(err)
		}
		set := cur.Get()
		if refusal := set.Refused[id]; refusal.Reason != policy.ReasonNoCurrentValue || len(set.Pending) != 0 {
			t.Fatalf("past the window: refused %+v, pending %+v; want STRANDED_KEY refused no-current-value", set.Refused, set.Pending)
		}
		held, err := store.DescribeSecret(ctx, &secretsmanager.DescribeSecretInput{SecretId: aws.String(id)})
		if err != nil || held.DeletedDate != nil || awsstore.HasCurrentVersion(held.VersionIdsToStages) {
			t.Fatalf("the store answers %+v, %v; want it still holding STRANDED_KEY with no current value, since the broker deletes nothing", held, err)
		}
	})
}

// TestAPendingSecretThatGetsItsValueIsServedWithNoMoreLines: a pending secret whose value lands
// within the window is served at the next reread, writing nothing beyond the one pending line;
// one deleted within the window leaves the policy writing nothing either.
func TestAPendingSecretThatGetsItsValueIsServedWithNoMoreLines(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		now := time.Now()
		store := awstest.NewLocal(createdWithoutValue("FILLED_KEY", now), createdWithoutValue("DROPPED_KEY", now))
		logged := policytest.CaptureLog(t)
		cur := policytest.Current(t, store)
		want := pendingLine(policytest.ID("DROPPED_KEY"), owner, true) + pendingLine(policytest.ID("FILLED_KEY"), owner, true) + noProfilesLine
		if logged.String() != want {
			t.Fatalf("the first load logged\n%s\nwant\n%s", logged, want)
		}

		filled := policytest.Secret("FILLED_KEY", owner, policy.TierAgent, "v1")
		filled.CreatedAt = &now
		store.Put(filled)
		if _, err := store.DeleteSecret(context.Background(), &secretsmanager.DeleteSecretInput{SecretId: aws.String(policytest.ID("DROPPED_KEY"))}); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"FILLED_KEY", "DROPPED_KEY"} {
			if _, err := cur.RefreshOne(context.Background(), name); err != nil {
				t.Fatalf("RefreshOne(%s): %v", name, err)
			}
		}
		set := cur.Get()
		if _, served := set.Secrets["FILLED_KEY"]; !served || len(set.Pending) != 0 || len(set.Refused) != 0 {
			t.Fatalf("served %+v, pending %+v, refused %+v; want FILLED_KEY served and nothing pending or refused", set.Secrets, set.Pending, set.Refused)
		}
		if logged.String() != want {
			t.Fatalf("logged\n%s\nwant nothing beyond the pending lines\n%s", logged, want)
		}
	})
}

// TestASecretWithoutAValueOrACreationTimeIsRefusedAtOnce: a store that names no creation time for
// a secret with no value - Secrets Manager's ListSecrets names one for every secret, so this is a
// listing that left it out - gives the policy no way to tell a create in flight from an abandoned
// one, so it refuses the secret no-current-value at once, as it always did.
func TestASecretWithoutAValueOrACreationTimeIsRefusedAtOnce(t *testing.T) {
	store := awstest.NewLocal(policytest.Secret("UNDATED_KEY", owner, policy.TierAgent, ""))
	set, err := policytest.Loader(store).Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if refusal := set.Refused[policytest.ID("UNDATED_KEY")]; refusal.Reason != policy.ReasonNoCurrentValue || len(set.Pending) != 0 {
		t.Fatalf("refused %+v, pending %+v; want UNDATED_KEY refused no-current-value", set.Refused, set.Pending)
	}
}

// TestAYoungSecretWithoutAValueOrTheCreateLabelIsRefusedAtOnce: a secret with no value that the
// store created moments ago, but that carries no create label, is no create waiting for its value
// (no `agent-secrets secret create` made it), so the policy refuses it no-current-value at once.
func TestAYoungSecretWithoutAValueOrTheCreateLabelIsRefusedAtOnce(t *testing.T) {
	unlabelled := policytest.Secret("UNLABELLED_KEY", owner, policy.TierAgent, "")
	now := time.Now()
	unlabelled.CreatedAt = &now
	set, err := policytest.Loader(awstest.NewLocal(unlabelled)).Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if refusal := set.Refused[policytest.ID("UNLABELLED_KEY")]; refusal.Reason != policy.ReasonNoCurrentValue || len(set.Pending) != 0 {
		t.Fatalf("refused %+v, pending %+v; want UNLABELLED_KEY refused no-current-value", set.Refused, set.Pending)
	}
}

// TestAPendingAnswerEndsAtTenMinutesWithoutAReload: a request is answered PENDING_VALUE only while
// the secret is less than ten minutes old, even where no load or reread has run since it was found
// pending (one refused by the miss-path limit, or one that failed): one second before ten minutes
// it is pending, at ten minutes it is not, with the same set.
func TestAPendingAnswerEndsAtTenMinutesWithoutAReload(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := awstest.NewLocal(createdWithoutValue("WAITING_KEY", time.Now()))
		cur := policytest.Current(t, store)
		set := cur.Get()
		time.Sleep(10*time.Minute - time.Second)
		if !cur.PendingValue(set, "WAITING_KEY") {
			t.Fatal("one second before ten minutes: PendingValue = false, want true")
		}
		time.Sleep(time.Second)
		if cur.PendingValue(set, "WAITING_KEY") {
			t.Fatal("at ten minutes, with no reload since: PendingValue = true, want false")
		}
	})
}

// TestTheLoadAndThePendingAnswerReadOneClock: the ten-minute rule is one rule on one clock,
// Current's: a load, a reread and the PENDING_VALUE answer all read the age of a secret waiting for
// its value by it. Moved ten minutes past the secret's creation, it makes the load and the reread
// refuse the secret no-current-value and the answer say it is no longer pending, though the wall
// clock has not moved.
func TestTheLoadAndThePendingAnswerReadOneClock(t *testing.T) {
	created := time.Now()
	store := awstest.NewLocal(createdWithoutValue("WAITING_KEY", created))
	cur := policytest.Current(t, store)
	id := policytest.ID("WAITING_KEY")
	if _, waits := cur.Get().Pending[id]; !waits {
		t.Fatalf("pending %+v; want WAITING_KEY pending at its creation", cur.Get().Pending)
	}
	policy.SetNow(cur, func() time.Time { return created.Add(10 * time.Minute) })
	ctx := context.Background()
	if err := cur.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	set := cur.Get()
	if refusal := set.Refused[id]; refusal.Reason != policy.ReasonNoCurrentValue || len(set.Pending) != 0 {
		t.Fatalf("a load at ten minutes by Current's clock: refused %+v, pending %+v; want WAITING_KEY refused no-current-value", set.Refused, set.Pending)
	}
	if lk, err := cur.RefreshOne(ctx, "WAITING_KEY"); err != nil || lk.Reason != policy.ReasonNoCurrentValue {
		t.Fatalf("a reread at ten minutes by Current's clock = %+v, %v; want no-current-value", lk, err)
	}
	if cur.PendingValue(set, "WAITING_KEY") {
		t.Fatal("at ten minutes by Current's clock: PendingValue = true, want false")
	}
}
