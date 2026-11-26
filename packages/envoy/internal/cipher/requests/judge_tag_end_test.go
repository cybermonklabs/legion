// packages/envoy/internal/cipher/requests/judge_tag_end_test.go
// What a later change of a key's judge tag does to a grant the model judge already made of it.
package requests

import (
	"errors"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/secrets"
	"github.com/sjawhar/envoy/internal/logtest"
)

// TestAJudgeTagLoweredBelowAJudgedGrantOrTakenOffEndsItForGood: the broker's docs say that
// `agent-secrets secret retag --judge none` "ends each live grant the judge made of the secret at
// its next release", and a change of the seconds ends the grants it no longer admits. A max-grant
// tag lowered below a live grant does exactly that (TestAMaxGrantLoweredBelowAGrantAndRaisedBackLeavesItEnded):
// the release that finds the tag no longer admits the grant ends it in the store, so putting the
// tag back does not bring the grant back. A judge tag taken off the key, or lowered below the
// grant's lifetime, is held to the same rule here. The control row lowers the tag to exactly the
// grant's lifetime, which still admits it: that grant keeps releasing, before and after.
//
// A key the policy stops serving for a reason that is not its judge tag pauses the grant instead,
// as it pauses a person's (noLongerServed): the paused row writes a judge tag of 1800, which the
// load refuses (judge-out-of-range), so the key is served nowhere. The release is refused as a key
// no longer served, with no broken-chain line and no end, and once the key is served again with a
// tag that admits the grant, the same grant releases.
//
// At 3b501fde both changes only pause the grant: the release is refused as a broken chain
// (record.MayJudge, ahead of judgeStillDecides), nothing ends it, and once the tag is back at 900
// the same grant releases again. At ab4e4ca the paused row ends the grant for good instead, with
// the reason that the key carries no judge tag.
func TestAJudgeTagLoweredBelowAJudgedGrantOrTakenOffEndsItForGood(t *testing.T) {
	// Not parallel: it captures the process's default logger, whose records are every test's that logs meanwhile (logtest.SetDefault, logtest.Swap, slog.SetDefault).
	deel := func(judge int) secrets.LocalSecret {
		s := policytest.Secret("DEEL_API_KEY", fixtureOperator, policy.TierHuman, "deel-v1")
		if judge == 0 {
			return s
		}
		return policytest.Judged(s, judge, "")
	}
	for name, c := range map[string]struct {
		judge  int
		ends   bool
		pauses bool
	}{
		"control: the tag lowered to exactly the grant's lifetime": {judge: 600},
		"the tag lowered below the grant's lifetime":               {judge: 599, ends: true},
		"the tag taken off the key":                                {judge: 0, ends: true},
		"a judge tag the load refuses, so the key is not served":   {judge: 1800, pauses: true},
	} {
		t.Run(name, func(t *testing.T) {
			m, session, key := newJudgeFixture(t)
			judged := judgedGrant(t, m, session, key, 600, "DEEL_API_KEY")
			if release, err := m.Values(t.Context(), judged, session, ReleaseCall{}); err != nil || releasedValue(release, "DEEL_API_KEY") == "" {
				t.Fatalf("Values before the retag = %+v, %v; want DEEL_API_KEY released", release, err)
			}
			capture := &logtest.Recorder{}
			logtest.SetDefault(t, capture)
			retag(t, m, deel(c.judge))
			release, err := m.Values(t.Context(), judged, session, ReleaseCall{})
			if c.pauses {
				if _, served := m.Policy.Get().Secrets["DEEL_API_KEY"]; served {
					t.Fatalf("a judge tag of %d left DEEL_API_KEY served; this row needs a key the load refuses", c.judge)
				}
				if !errors.Is(err, ErrGrantNotLive) || !strings.Contains(err.Error(), "DEEL_API_KEY is no longer an agent secret") {
					t.Fatalf("Values while DEEL_API_KEY is not served = %+v, %v; want the grant paused as a key no longer served", release, err)
				}
				if lines := loggedLines(t, capture, GrantChainInvalidMessage); len(lines) != 0 {
					t.Fatalf("a key that is not served wrote %d %q lines: %v; want none, since its chain is not what failed", len(lines), GrantChainInvalidMessage, lines)
				}
				if by, audits := endedForGood(t, m, judged); by != "" || audits != 0 {
					t.Fatalf("grant revoked_by = %q with %d grant.revoked rows; want neither: a key the policy stops serving pauses its grants", by, audits)
				}
				retag(t, m, deel(900))
				if release, err := m.Values(t.Context(), judged, session, ReleaseCall{}); err != nil || releasedValue(release, "DEEL_API_KEY") == "" {
					t.Fatalf("Values once DEEL_API_KEY is served again under judge 900 = %+v, %v; want the paused grant released", release, err)
				}
				return
			}
			if !c.ends {
				if err != nil || releasedValue(release, "DEEL_API_KEY") == "" {
					t.Fatalf("Values under judge %d = %+v, %v; want DEEL_API_KEY released: the tag still admits the grant", c.judge, release, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Values under judge %d = %+v; want the grant refused", c.judge, release)
			}
			retag(t, m, deel(900))
			if release, err := m.Values(t.Context(), judged, session, ReleaseCall{}); err == nil || releasedValue(release, "DEEL_API_KEY") != "" {
				t.Fatalf("Values once the tag is back at 900 = %+v, %v; want the grant to stay ended, as the docs say the change ended it", release, err)
			}
			if by, audits := endedForGood(t, m, judged); by != GrantEndedActor || audits != 1 {
				t.Fatalf("grant revoked_by = %q with %d grant.revoked rows by it; want %s and 1: the change of the judge tag ends the grant in the store", by, audits, GrantEndedActor)
			}
		})
	}
}
