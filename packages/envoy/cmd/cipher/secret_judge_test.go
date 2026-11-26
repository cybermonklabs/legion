package main

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"

	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/secrets"
)

// TestCreateAndRetagWriteTheJudgeTags: create and retag --judge SECONDS and --guidance TEXT write
// the secret's judge and guidance tags beside its owner and tier, on each store, and keep every
// other tag; --judge none and --guidance none each remove their tag, as every other none removes
// its own, and a removal beside another change is refused before anything is written, since it is
// a retag of its own.
func TestCreateAndRetagWriteTheJudgeTags(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (main.onEachStore, main.startSecretBrokerWith, t.Setenv in secret_test.go); it signals the test process or changes its signal handling (main.onEachStore, main.useGCPIdentity, main.userinfoOf, (gcloudToken).Token, main.runGcloud, main.runRecorded, main.watchSignalsWhileGcloudRuns, signal.Notify in secret_gcp.go); it sets a package variable the package's other tests read (main.useStdin, secretStdin set in secret_test.go).
	onEachStore(t, func(t *testing.T, store typeStore, _ *secretBroker) {
		useStdin(t, "probe-value")
		if _, stderr, code := runSecret("create", "JUDGED_KEY", "--owner", "me", "--tier", "human", "--judge", "900", "--guidance", "grant k14-grant"); code != 0 {
			t.Fatalf("create --judge 900 --guidance: exit %d, stderr %q", code, stderr)
		}
		tags := store.tags(t, "JUDGED_KEY")
		if tags[policy.TagJudge] != "900" || tags[policy.TagGuidance] != "grant k14-grant" || tags[policy.TagOwner] != "ada@example.com" || tags[policy.TagTier] != policy.TierHuman || len(tags) != 4 {
			t.Fatalf("JUDGED_KEY's tags = %v, want judge=900 and its guidance beside its owner and tier alone", tags)
		}

		store.put(t, "API_KEY", map[string]string{policy.TagOwner: "ada@example.com", policy.TagTier: policy.TierHuman, policy.TagMaxGrant: "1h"}, "v1")
		if _, stderr, code := runSecret("retag", "API_KEY", "--judge", "600", "--guidance", "grant for the release job"); code != 0 {
			t.Fatalf("retag --judge 600 --guidance: exit %d, stderr %q", code, stderr)
		}
		tags = store.tags(t, "API_KEY")
		if tags[policy.TagJudge] != "600" || tags[policy.TagGuidance] != "grant for the release job" || tags[policy.TagMaxGrant] != "1h" || tags[policy.TagOwner] != "ada@example.com" {
			t.Fatalf("tags after retag --judge 600 = %v, want the judge and guidance beside the kept max-grant and owner", tags)
		}

		_, stderr, code := runSecret("retag", "API_KEY", "--judge", "none", "--tier", "agent")
		if code != exitUsageError || !strings.Contains(stderr, "a retag of its own") {
			t.Fatalf("retag --judge none --tier agent: exit %d stderr %q; want a usage error saying a removal is a retag of its own", code, stderr)
		}
		if got := store.tags(t, "API_KEY"); got[policy.TagJudge] != "600" || got[policy.TagTier] != policy.TierHuman {
			t.Fatalf("a refused retag changed the tags to %v", got)
		}

		if _, stderr, code := runSecret("retag", "API_KEY", "--judge", "none"); code != 0 {
			t.Fatalf("retag --judge none: exit %d, stderr %q", code, stderr)
		}
		if _, stderr, code := runSecret("retag", "API_KEY", "--guidance", "none"); code != 0 {
			t.Fatalf("retag --guidance none: exit %d, stderr %q", code, stderr)
		}
		tags = store.tags(t, "API_KEY")
		if _, judged := tags[policy.TagJudge]; judged {
			t.Fatalf("tags after --judge none = %v, want no judge tag", tags)
		}
		if _, guided := tags[policy.TagGuidance]; guided || tags[policy.TagMaxGrant] != "1h" || tags[policy.TagOwner] != "ada@example.com" {
			t.Fatalf("tags after --guidance none = %v, want no guidance tag and the rest unchanged", tags)
		}
	})

	t.Run("a value the broker would refuse is refused before any write", func(t *testing.T) {
		local := secrets.NewLocal(policytest.Secret("API_KEY", "ada@example.com", policy.TierHuman, "v1"))
		writes := &countingTagWrites{Local: local}
		startSecretBroker(t, servedBy(local))
		useAWS(t, writes, testAccount, adaSignIn)
		longGuidance := strings.Repeat("g", policy.MaxGuidanceRunes+1)
		for _, c := range []struct {
			args   []string
			flag   string
			reason string
			quoted string // a value the refusal must not print
		}{
			{[]string{"create", "NEW_KEY", "--owner", "me", "--tier", "human", "--judge", "1800"}, "--judge", policy.ReasonJudgeOutOfRange, "1800"},
			{[]string{"create", "NEW_KEY", "--owner", "me", "--tier", "human", "--judge", "0"}, "--judge", policy.ReasonJudgeOutOfRange, ""},
			{[]string{"create", "NEW_KEY", "--owner", "me", "--tier", "human", "--judge", "15m"}, "--judge", policy.ReasonJudgeOutOfRange, "15m"},
			{[]string{"create", "NEW_KEY", "--owner", "me", "--tier", "human", "--judge", "none"}, "--judge", policy.ReasonJudgeOutOfRange, ""},
			{[]string{"retag", "API_KEY", "--judge", "901"}, "--judge", policy.ReasonJudgeOutOfRange, "901"},
			{[]string{"retag", "API_KEY", "--judge", "-900"}, "--judge", policy.ReasonJudgeOutOfRange, "-900"},
			{[]string{"create", "NEW_KEY", "--owner", "me", "--tier", "human", "--guidance", "none"}, "--guidance", "a retag", ""},
			{[]string{"create", "NEW_KEY", "--owner", "me", "--tier", "human", "--judge", "300", "--guidance", longGuidance}, "--guidance", policy.ReasonGuidanceTooLong, longGuidance},
			{[]string{"retag", "API_KEY", "--guidance", longGuidance}, "--guidance", policy.ReasonGuidanceTooLong, longGuidance},
		} {
			useStdin(t, "probe-value")
			_, stderr, code := runSecret(c.args...)
			if code != exitUsageError || !strings.Contains(stderr, c.flag) || !strings.Contains(stderr, c.reason) {
				t.Errorf("%q: exit %d stderr %q; want %d naming %s and %s", c.args, code, stderr, exitUsageError, c.flag, c.reason)
			}
			if c.quoted != "" && strings.Contains(stderr, strconv.Quote(c.quoted)) {
				t.Errorf("%q: stderr %q prints the value it refused", c.args, stderr)
			}
		}
		if writes.describes != 0 || writes.tags != 0 || writes.untags != 0 {
			t.Fatalf("refused commands made %d DescribeSecret, %d TagResource and %d UntagResource calls; want none", writes.describes, writes.tags, writes.untags)
		}
		if _, err := local.DescribeSecret(context.Background(), &secretsmanager.DescribeSecretInput{SecretId: aws.String(policytest.ID("NEW_KEY"))}); err == nil {
			t.Fatal("a refused create made NEW_KEY")
		}
	})
}
