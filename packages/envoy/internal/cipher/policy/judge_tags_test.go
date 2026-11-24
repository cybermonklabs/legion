package policy_test

import (
	"context"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/aws/awstest"
)

// TestAKeysJudgeAndGuidanceAreItsOwnTags: who decides a key's requests is written on the key. A
// judge tag of whole seconds from 1 to 900 makes it a key the model judge may decide for that
// long, its guidance tag is what the judge reads, and Set.JudgeSeconds answers the seconds for a
// served key that carries the tag and nothing for one that does not, or for a name not served.
func TestAKeysJudgeAndGuidanceAreItsOwnTags(t *testing.T) {
	judged := policytest.Secret("JUDGED_KEY", owner, policy.TierHuman, "v")
	judged.Tags[policy.TagJudge] = "900"
	judged.Tags[policy.TagGuidance] = "grant for the release job named in the reason"
	shortest := policytest.Secret("SHORTEST_KEY", owner, policy.TierHuman, "v")
	shortest.Tags[policy.TagJudge] = "1"
	plain := policytest.Secret("PLAIN_KEY", owner, policy.TierHuman, "v")

	set, err := policytest.Loader(awstest.NewLocal(judged, shortest, plain)).Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(set.Refused) != 0 {
		t.Fatalf("refused %+v, want every key served", set.Refused)
	}
	for name, want := range map[string]struct {
		seconds  uint32
		judged   bool
		guidance string
	}{
		"JUDGED_KEY":   {900, true, "grant for the release job named in the reason"},
		"SHORTEST_KEY": {1, true, ""},
		"PLAIN_KEY":    {0, false, ""},
		"NO_SUCH_KEY":  {0, false, ""},
	} {
		seconds, judged := set.JudgeSeconds(name)
		if seconds != want.seconds || judged != want.judged {
			t.Errorf("JudgeSeconds(%s) = %d, %v; want %d, %v", name, seconds, judged, want.seconds, want.judged)
		}
		if got := set.Secrets[name].Guidance; got != want.guidance {
			t.Errorf("%s's guidance = %q, want %q", name, got, want.guidance)
		}
	}
}

// TestAJudgeTagOutsideOneToNineHundredIsRefusedNamingTheKey holds the judge's bound at the policy
// load: a judge tag that is not a whole number of seconds from 1 to 900 refuses its key, as
// ReasonJudgeOutOfRange, and the first load's line names that key. A judge of 900 seconds is the
// bound itself and refuses nothing.
func TestAJudgeTagOutsideOneToNineHundredIsRefusedNamingTheKey(t *testing.T) {
	store := awstest.NewLocal()
	want := map[string]policy.Refusal{}
	for name, value := range map[string]string{
		"JUDGE_1800":     "1800",
		"JUDGE_901":      "901",
		"JUDGE_ZERO":     "0",
		"JUDGE_NEGATIVE": "-1",
		"JUDGE_PLUS":     "+900",
		"JUDGE_WORD":     "fifteen",
		"JUDGE_DURATION": "15m",
		"JUDGE_SECONDS":  "900s",
		"JUDGE_SPACED":   " 900",
		"JUDGE_EMPTY":    "",
		"JUDGE_HUGE":     "4294968196",
	} {
		s := policytest.Secret(name, owner, policy.TierHuman, "v")
		s.Tags[policy.TagJudge] = value
		store.Put(s)
		want[policytest.ID(name)] = policy.Refusal{Reason: policy.ReasonJudgeOutOfRange, Owner: owner}
	}
	bound := policytest.Secret("JUDGE_900", owner, policy.TierHuman, "v")
	bound.Tags[policy.TagJudge] = "900"
	store.Put(bound)

	logged := policytest.CaptureLog(t)
	loader := policytest.Loader(store)
	set, err := loader.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !maps.Equal(set.Refused, want) {
		t.Errorf("Load refused\n%+v\nwant each key whose judge tag is outside 1 to 900 refused judge-out-of-range\n%+v", set.Refused, want)
	}
	if seconds, judged := set.JudgeSeconds("JUDGE_900"); len(set.Secrets) != 1 || seconds != 900 || !judged {
		t.Errorf("served %+v with JudgeSeconds(JUDGE_900) = %d, %v; want the 900-second key alone, judged for 900", set.Secrets, seconds, judged)
	}

	if _, err := policy.NewCurrent(t.Context(), loader, time.Hour); err != nil {
		t.Fatalf("NewCurrent: %v", err)
	}
	var lines strings.Builder
	for _, id := range slices.Sorted(maps.Keys(want)) {
		lines.WriteString(refusedLine(id, policy.ReasonJudgeOutOfRange, owner, true, ""))
	}
	lines.WriteString(noProfilesLine)
	if logged.String() != lines.String() {
		t.Errorf("the first load logged:\n%s\nwant one line naming each refused key:\n%s", logged, lines.String())
	}
}

// TestAGuidanceTagLongerThanTheJudgeReadsIsRefused: the judge shows a key's guidance whole to its
// model, so a guidance tag past MaxGuidanceRunes refuses the key rather than reach the model cut.
// Runes are counted, not bytes.
func TestAGuidanceTagLongerThanTheJudgeReadsIsRefused(t *testing.T) {
	atBound := policytest.Secret("GUIDED_AT_BOUND", owner, policy.TierHuman, "v")
	atBound.Tags[policy.TagJudge] = "300"
	atBound.Tags[policy.TagGuidance] = strings.Repeat("é", policy.MaxGuidanceRunes)
	over := policytest.Secret("GUIDED_TOO_LONG", owner, policy.TierHuman, "v")
	over.Tags[policy.TagJudge] = "300"
	over.Tags[policy.TagGuidance] = strings.Repeat("a", policy.MaxGuidanceRunes+1)

	set, err := policytest.Loader(awstest.NewLocal(atBound, over)).Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := map[string]policy.Refusal{policytest.ID("GUIDED_TOO_LONG"): {Reason: policy.ReasonGuidanceTooLong, Owner: owner}}
	if !maps.Equal(set.Refused, want) {
		t.Errorf("Load refused %+v, want %+v", set.Refused, want)
	}
	if _, served := set.Secrets["GUIDED_AT_BOUND"]; !served {
		t.Errorf("a guidance of exactly %d runes was refused", policy.MaxGuidanceRunes)
	}
	if reason := policy.TagRules(policy.TagSet{Owner: owner, Tier: policy.TierHuman, Guidance: over.Tags[policy.TagGuidance]}); reason != policy.ReasonGuidanceTooLong {
		t.Errorf("TagRules of the long guidance = %q, want %q, so a create or retag refuses it before it writes", reason, policy.ReasonGuidanceTooLong)
	}
}

// TestTheJudgeTagIsInTheVersionAndTheGuidanceIsNot: the judge seconds decide what a live judged
// grant may still be (requests' judgeStillDecides), so a judge tag added, lowered or removed moves
// the policy version and each grant of the key is checked again at its next release. Guidance tells
// the judge how to decide requests to come and changes no live grant, so it moves nothing. A key
// with a judge tag ends its line with "\tjudge=<seconds>", and one without writes the bytes it
// always did, so no deployed version shifts. The expected values are the SHA-256 of the lines
// printed by hand:
// printf 'A_B\tada@example.com\thuman\t<arn>/a-b\nA0\tada@example.com\tagent\t<arn>/a0\n
// DEEL_API_KEY\tshared\thuman\t<arn>/deel-api-key\nGOLDEN_JUDGED\tada@example.com\thuman\t
// <arn>/golden-judged<tail>\n' | sha256sum, with <arn>
// arn:aws:secretsmanager:local:000000000000:secret:example/agent-secrets and <tail> empty or
// "\t0s\t\tjudge=900".
func TestTheJudgeTagIsInTheVersionAndTheGuidanceIsNot(t *testing.T) {
	version := func(tags map[string]string) string {
		t.Helper()
		store := goldenFixture()
		s := policytest.Secret("GOLDEN_JUDGED", "ada@example.com", policy.TierHuman, "v1")
		maps.Copy(s.Tags, tags)
		store.Put(s)
		set, err := policytest.Loader(store).Load(context.Background())
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if len(set.Refused) != 0 {
			t.Fatalf("refused %+v", set.Refused)
		}
		return set.Version
	}
	const (
		untagged = "3acd51b9ea6efc763192e9e5fc16177a6b9dcb9791d7967e6d85d4aa3e34e308"
		judged   = "b81cd49e8910f31160a2cc9e08def5220ac10397226e3fb37d8f8e842ab556eb"
	)
	if got := version(nil); got != untagged {
		t.Errorf("Version with no judge tag = %s, want the golden %s", got, untagged)
	}
	if got := version(map[string]string{policy.TagJudge: "900"}); got != judged {
		t.Errorf("Version with judge 900 = %s, want the golden %s", got, judged)
	}
	if got := version(map[string]string{policy.TagGuidance: "grant for the release job"}); got != untagged {
		t.Errorf("a guidance tag alone moved the version to %s, want %s", got, untagged)
	}
	if got := version(map[string]string{policy.TagJudge: "900", policy.TagGuidance: "grant for the release job"}); got != judged {
		t.Errorf("guidance beside the judge tag moved the version to %s, want %s", got, judged)
	}
	if got := version(map[string]string{policy.TagJudge: "600"}); got == judged || got == untagged {
		t.Errorf("a judge of 600 gave the version %s, want one of its own", got)
	}
}

// TestAProfileTakesItsSourcesJudgeAndGuidance: a profile has no judge field of its own, so a
// profile is decided as its source is: by the judge seconds and guidance the source key's tags
// carry, as it takes the source's admin tag.
func TestAProfileTakesItsSourcesJudgeAndGuidance(t *testing.T) {
	source := policytest.Secret("RELEASE_PAT", owner, policy.TierHuman, "v")
	source.Tags[policy.TagJudge] = "600"
	source.Tags[policy.TagGuidance] = "grant for the release job"
	set := policytest.ProfiledCurrent(t, awstest.NewLocal(source), policytest.KindsFile(
		`{"name": "RELEASE_READ", "source": "RELEASE_PAT", "proxy": "github"}`,
	)).Get()
	profile, served := set.Secrets["RELEASE_READ"]
	if !served {
		t.Fatalf("RELEASE_READ was not served: %+v", set.Refused)
	}
	if seconds, judged := set.JudgeSeconds("RELEASE_READ"); seconds != 600 || !judged || profile.Guidance != "grant for the release job" {
		t.Errorf("RELEASE_READ is judged %d, %v with guidance %q; want its source's 600 seconds and guidance", seconds, judged, profile.Guidance)
	}
}

// TestAProfileBehindTheProxyCarriesItsSourcesJudgeInItsDigest: a profile behind the proxy is served
// with its source key's judge seconds in its fingerprint, where its proxy block held them before
// the judge became the key's tag, so a judge tag added, changed or taken off its source ends the
// profile's grants (requests' profileChanged), as an edit of the block's judge did. A profile
// served from a source with no judge tag keeps its file's own fingerprint (Profiles.Digest). The
// merge base's digests of these entries were their proxy blocks'; behind a kind, an entry's digest
// is its new form's, which the move to kinds changes once.
func TestAProfileBehindTheProxyCarriesItsSourcesJudgeInItsDigest(t *testing.T) {
	const file = `{"name": "RELEASE_READ", "source": "RELEASE_PAT", "proxy": "github", "allow": [{"method": "GET", "pathPrefix": "/repos/"}]}`
	digest := func(judge int) string {
		t.Helper()
		source := policytest.Narrowed(policytest.Secret("RELEASE_PAT", owner, policy.TierHuman, "v"))
		if judge != 0 {
			source = policytest.Judged(source, judge, "grant for the release job")
		}
		set := policytest.ProfiledCurrent(t, awstest.NewLocal(source), policytest.KindsFile(file)).Get()
		served, ok := set.Secrets["RELEASE_READ"]
		if !ok {
			t.Fatalf("RELEASE_READ was not served: %+v", set.Refused)
		}
		return served.ProfileDigest
	}
	plain, judged300, judged600 := digest(0), digest(300), digest(600)
	if own := policytest.Profiles(t, policytest.KindsFile(file)).Digest("RELEASE_READ"); plain == own {
		t.Errorf("RELEASE_READ, from a source tagged proxy-narrowed, is served with its file's own digest %s; want the tag in it", own)
	}
	if judged300 == plain || judged600 == plain || judged300 == judged600 {
		t.Errorf("RELEASE_READ's digests under no judge, 300 and 600 = %s, %s, %s; want three", plain, judged300, judged600)
	}
	if again := digest(300); again != judged300 {
		t.Errorf("RELEASE_READ under judge 300 is served with %s and then %s; want one digest", judged300, again)
	}
}
