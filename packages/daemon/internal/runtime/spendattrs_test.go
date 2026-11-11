package runtime

import (
	"fmt"
	"maps"
	"net/url"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// liveValue is the value the live deployment's operator writes, one per project
// (legion/Pulumi.prod.yaml of theorem-labs/agent-infra), with the project's own lane.
func liveValue(lane string) string {
	return "theorem.project=legion,theorem.lane=" + lane + ",theorem.credential=gateway,theorem.person=sami@theorem.dev"
}

// liveAttributes is what the detector reads out of liveValue("legion-infra"), with the pairs a
// case expects on top of it.
func liveAttributes(pairs ...string) map[string]string {
	attrs := map[string]string{
		"theorem.project":    "legion",
		"theorem.lane":       "legion-infra",
		"theorem.credential": "gateway",
		"theorem.person":     "sami@theorem.dev",
	}
	for i := 0; i < len(pairs); i += 2 {
		attrs[pairs[i]] = pairs[i+1]
	}
	return attrs
}

// detectorAttributes is the parse the worker image's Oh My Pi runs over the composed value
// (@opentelemetry/resources' EnvDetector._parseResourceAttributes, read out of /opt/omp/bin/omp
// on 2026-11-11), written out here so that each case is checked against what the agent would
// read rather than against the string the composer happened to build: split on commas, drop a
// member whose trimmed text is empty, throw on a member that is not exactly one `=`, trim and
// percent-decode both sides, and assign last-wins.
func detectorAttributes(t *testing.T, value string) map[string]string {
	t.Helper()
	attrs := map[string]string{}
	for _, member := range strings.Split(value, ",") {
		if strings.TrimSpace(member) == "" {
			continue
		}
		parts := strings.Split(member, "=")
		if len(parts) != 2 {
			t.Fatalf("the detector throws on member %q of %q: it does not split on = into exactly two parts", member, value)
		}
		key, err := url.PathUnescape(strings.TrimSpace(parts[0]))
		if err != nil {
			t.Fatalf("the detector throws on the key of member %q of %q: %v", member, value, err)
		}
		if key == "" {
			t.Fatalf("the detector throws on member %q of %q: its key is empty", member, value)
		}
		decoded, err := url.PathUnescape(strings.TrimSpace(parts[1]))
		if err != nil {
			t.Fatalf("the detector throws on the value of member %q of %q: %v", member, value, err)
		}
		attrs[key] = decoded
	}
	return attrs
}

// The composed value keeps the operator's own attributes, adds the launch's issue and role after
// them so that the detector's last-wins assignment gives the daemon's pairs the key, writes no
// empty member, and leaves out a pair the spec has no fact for: the controller's issue, and both
// of the boot gate's.
func TestSpendResourceAttributesAppendsTheLaunchAfterTheOperatorsOwn(t *testing.T) {
	const (
		live       = "theorem.project=legion,theorem.lane=legion-infra,theorem.credential=gateway,theorem.person=sami@theorem.dev"
		issue      = "INFRA-312"
		issuePair  = "legion.issue=INFRA-312"
		launchOnly = issuePair + ",legion.role=implementer"
	)
	if live != liveValue("legion-infra") {
		t.Fatalf("liveValue(legion-infra) = %q, want %q", liveValue("legion-infra"), live)
	}
	for _, tc := range []struct {
		name     string
		operator string
		spec     SpawnSpec
		want     string
		attrs    map[string]string
	}{
		{
			"an empty operator value", "",
			SpawnSpec{Issue: issue, Role: claim.RoleImplementer},
			launchOnly,
			map[string]string{"legion.issue": issue, "legion.role": "implementer"},
		},
		{
			"a trailing comma", live + ",",
			SpawnSpec{Issue: issue, Role: claim.RoleImplementer},
			live + "," + launchOnly,
			liveAttributes("legion.issue", issue, "legion.role", "implementer"),
		},
		{
			"trailing spaces after a trailing comma", live + ",  ",
			SpawnSpec{Issue: issue, Role: claim.RoleImplementer},
			live + "," + launchOnly,
			liveAttributes("legion.issue", issue, "legion.role", "implementer"),
		},
		{
			"an operator value of only commas and spaces", " , , ",
			SpawnSpec{Issue: issue, Role: claim.RoleImplementer},
			launchOnly,
			map[string]string{"legion.issue": issue, "legion.role": "implementer"},
		},
		{
			"the shape the live deployment writes", live,
			SpawnSpec{Issue: issue, Role: claim.RoleImplementer},
			live + "," + launchOnly,
			liveAttributes("legion.issue", issue, "legion.role", "implementer"),
		},
		{
			"a controller launch, which holds no issue", live,
			SpawnSpec{Role: claim.RoleController},
			live + ",legion.role=controller",
			liveAttributes("legion.role", "controller"),
		},
		{
			"the architect", live,
			SpawnSpec{Issue: issue, Role: claim.RoleArchitect},
			live + "," + issuePair + ",legion.role=architect",
			liveAttributes("legion.issue", issue, "legion.role", "architect"),
		},
		{
			"the planner", live,
			SpawnSpec{Issue: issue, Role: claim.RolePlanner},
			live + "," + issuePair + ",legion.role=planner",
			liveAttributes("legion.issue", issue, "legion.role", "planner"),
		},
		{
			"the implementer", live,
			SpawnSpec{Issue: issue, Role: claim.RoleImplementer},
			live + "," + issuePair + ",legion.role=implementer",
			liveAttributes("legion.issue", issue, "legion.role", "implementer"),
		},
		{
			"the tester", live,
			SpawnSpec{Issue: issue, Role: claim.RoleTester},
			live + "," + issuePair + ",legion.role=tester",
			liveAttributes("legion.issue", issue, "legion.role", "tester"),
		},
		{
			"the reviewer", live,
			SpawnSpec{Issue: issue, Role: claim.RoleReviewer},
			live + "," + issuePair + ",legion.role=reviewer",
			liveAttributes("legion.issue", issue, "legion.role", "reviewer"),
		},
		{
			"the merger", live,
			SpawnSpec{Issue: issue, Role: claim.RoleMerger},
			live + "," + issuePair + ",legion.role=merger",
			liveAttributes("legion.issue", issue, "legion.role", "merger"),
		},
		{
			"the boot gate's probe, which is no role of no issue", live,
			SpawnSpec{},
			live,
			liveAttributes(),
		},
		{
			"the boot gate's probe with no operator value", "",
			SpawnSpec{},
			"",
			map[string]string{},
		},
		{
			"an operator value that already names the daemon's keys", "legion.issue=OLD-1," + live,
			SpawnSpec{Issue: issue, Role: claim.RoleImplementer},
			"legion.issue=OLD-1," + live + "," + launchOnly,
			liveAttributes("legion.issue", issue, "legion.role", "implementer"),
		},
		{
			"an issue and a role that need escaping", "",
			SpawnSpec{Issue: "INFRA-312,x=y", Role: claim.Role("role one")},
			"legion.issue=INFRA-312%2Cx%3Dy,legion.role=role%20one",
			map[string]string{"legion.issue": "INFRA-312,x=y", "legion.role": "role one"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := SpendResourceAttributes(tc.operator, tc.spec)
			if got != tc.want {
				t.Errorf("SpendResourceAttributes(%q, %s) = %q, want %q", tc.operator, tc.spec.Role, got, tc.want)
			}
			if attrs := detectorAttributes(t, got); !maps.Equal(attrs, tc.attrs) {
				t.Errorf("the detector reads %v out of %q, want %v", attrs, got, tc.attrs)
			}
		})
	}
}

// Nothing the daemon writes is refused by the names it writes itself, and the two keys are the
// ones the refusal keeps an operator out of.
func TestSpendAttributeKeysAreTheOnesTheComposerWrites(t *testing.T) {
	composed := SpendResourceAttributes("", SpawnSpec{Issue: "INFRA-312", Role: claim.RoleTester})
	for _, key := range []string{SpendIssueAttribute, SpendRoleAttribute} {
		if !strings.Contains(composed, key+"=") {
			t.Errorf("the composed value %q holds no %s member", composed, key)
		}
		if SpendAttributesRefusal(key+"=x") == "" {
			t.Errorf("SpendAttributesRefusal(%s=x) refuses nothing; the runtime writes that key itself", key)
		}
	}
	if SpendAttributesVariable != "OTEL_RESOURCE_ATTRIBUTES" {
		t.Errorf("SpendAttributesVariable = %q, want the variable the detector reads", SpendAttributesVariable)
	}
}

// An issue key and a role, which are unreserved throughout, are encoded without allocating.
func TestSpendEncodeDoesNotAllocateWhatNeedsNoEscape(t *testing.T) {
	var sink string
	if n := testing.AllocsPerRun(100, func() { sink = spendEncode("INFRA-312") }); n != 0 {
		t.Errorf("spendEncode(INFRA-312) allocates %v times, want none", n)
	}
	if sink != "INFRA-312" {
		t.Errorf("spendEncode(INFRA-312) = %q, want it back as it stands", sink)
	}
}

// Every value the live deployment writes is accepted, and each way the detector throws is
// refused by the member that would throw, named with what is wrong with it.
func TestSpendAttributesRefusalAppliesTheDetectorsOwnRules(t *testing.T) {
	const prefix = "theorem.project=legion,theorem.note="
	long := prefix + strings.Repeat("a", 256)
	for _, tc := range []struct {
		name, value, want string
	}{
		{"the smoke project's value", liveValue("legion-smoke"), ""},
		{"the linux project's value", liveValue("legion-linux"), ""},
		{"the bet project's value", liveValue("legion-bet"), ""},
		{"the infra project's value", liveValue("legion-infra"), ""},
		{"the ossv project's value", liveValue("legion-ossv"), ""},
		{"an empty operator value", "", ""},
		{"only commas and spaces", " , , ", ""},
		{"a member with two =", "theorem.project=legion,a=b=c", `its member "a=b=c" does not split on = into exactly one key and one value`},
		{"a member with no =", "theorem.project", `its member "theorem.project" does not split on = into exactly one key and one value`},
		{"a member with a bare %", "theorem.project=100%", `its member "theorem.project=100%" has a value with a malformed percent escape`},
		{"a key with a bare %", "theorem.%=legion", `its member "theorem.%=legion" has a key with a malformed percent escape`},
		{"a percent sequence that is not valid UTF-8", "theorem.project=%FF", `its member "theorem.project=%FF" has a value with percent escapes that are not valid UTF-8`},
		{"a 256-character decoded value", long, fmt.Sprintf("its member %q has a value of 256 characters once percent-decoded, over the detector's 255", "theorem.note="+strings.Repeat("a", 256))},
		{"a 255-character decoded value", prefix + strings.Repeat("a", 255), ""},
		{"an empty key", "=value", `its member "=value" has an empty key`},
		{"a member naming legion.issue", "theorem.project=legion,legion.issue=INFRA-1", `its member "legion.issue=INFRA-1" names legion.issue, which the runtime writes into every launch itself`},
		{"a member naming legion.role, percent-encoded", "legion%2Erole=tester", `its member "legion%2Erole=tester" names legion.role, which the runtime writes into every launch itself`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := SpendAttributesRefusal(tc.value); got != tc.want {
				t.Errorf("SpendAttributesRefusal(%q) = %q, want %q", tc.value, got, tc.want)
			}
		})
	}
}

// The reason names the one member it refuses, never the rest of the operator's attributes.
func TestSpendAttributesRefusalNamesOnlyTheMemberItRefuses(t *testing.T) {
	got := SpendAttributesRefusal(liveValue("legion-infra") + ",a=b=c")
	if got == "" {
		t.Fatal("SpendAttributesRefusal refuses nothing for a member with two =")
	}
	for _, other := range []string{"theorem.person", "sami@theorem.dev", "gateway", "legion-infra"} {
		if strings.Contains(got, other) {
			t.Errorf("SpendAttributesRefusal = %q, which repeats %q from another member", got, other)
		}
	}
}
