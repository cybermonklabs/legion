package record

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// reasonNaming is a reason whose issue keys name n distinct projects, P00 to P(n-1).
func reasonNaming(n int) string {
	keys := make([]string, n)
	for i := range keys {
		keys[i] = fmt.Sprintf("P%02d-1", i)
	}
	return strings.Join(keys, " ")
}

// TestANewRequestsReasonNamesAtMostSixteenProjects is round 4's B9 on cybermonklabs/legion#4135:
// a new request's reason may name the issues of at most MaxReasonProjects projects, counted as
// the person rule counts them (ReasonProjects), and one past that is refused as
// ErrRequestInvalid naming the bound. The rule is a creation rule: a request signed with such a
// reason still verifies (VerifyRequestObject), so one stored before the rule existed is read
// again as it always was.
func TestANewRequestsReasonNamesAtMostSixteenProjects(t *testing.T) {
	if got, want := ReasonProjects("INFRA-12 and INFRA-13, then SHA-256 and CORE-1"), []string{"INFRA", "SHA", "CORE"}; !slices.Equal(got, want) {
		t.Errorf("ReasonProjects = %v; want %v: each project once, in the order the reason first names it", got, want)
	}
	if err := CheckReasonProjects(reasonNaming(MaxReasonProjects)); err != nil {
		t.Errorf("CheckReasonProjects(a reason naming %d projects) = %v; want it allowed", MaxReasonProjects, err)
	}
	err := CheckReasonProjects(reasonNaming(MaxReasonProjects + 1))
	if !errors.Is(err, ErrRequestInvalid) || !strings.Contains(err.Error(), fmt.Sprintf("at most %d", MaxReasonProjects)) {
		t.Errorf("CheckReasonProjects(a reason naming %d projects) = %v; want ErrRequestInvalid naming the bound of %d", MaxReasonProjects+1, err, MaxReasonProjects)
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	const audience = "https://broker.example"
	details := []AuthorizationDetail{{Type: KindAgentSecret, Identifier: "DEEL_API_KEY", Actions: []string{"inject"}}}
	compact, err := Sign(key, audience, details, reasonNaming(MaxReasonProjects+1), "", nil, time.Now())
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if _, err := VerifyRequestObject(compact, audience, time.Minute, time.Now()); err != nil {
		t.Errorf("VerifyRequestObject(a reason naming %d projects) = %v; want it verified: a stored request is read again by this rule's absence", MaxReasonProjects+1, err)
	}
}

// TestIssueKeysInReason: a request's visibility reads every key its reason names, in order, so a
// key-shaped word such as SHA-256 before a private project's issue key cannot hide that project.
func TestIssueKeysInReason(t *testing.T) {
	for _, c := range []struct {
		reason string
		want   []string
	}{
		{"", nil},
		{"deploying the broker", nil},
		{"INFRA-1352", []string{"INFRA-1352"}},
		{"SHA-256 of the PRIV-12 bundle", []string{"SHA-256", "PRIV-12"}},
		{"blocked by LEGION-587, see INFRA-1352", []string{"LEGION-587", "INFRA-1352"}},
		{"dispatch://INFRA-1352/ask/3 and infra-9 lower-cased", []string{"INFRA-1352"}},
	} {
		if got := IssueKeysInReason(c.reason); !slices.Equal(got, c.want) {
			t.Errorf("IssueKeysInReason(%q) = %q, want %q", c.reason, got, c.want)
		}
	}
}
