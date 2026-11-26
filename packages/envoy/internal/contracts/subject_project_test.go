package contracts

//ci:reads packages/contracts/fixtures/subject-project.json the shared subject-project cases, read through ../../../contracts

import (
	"encoding/json"
	"os"
	"slices"
	"testing"
)

// subjectProjectFixture is packages/contracts/fixtures/subject-project.json, which the TypeScript
// readers run too (subject-project.test.ts and subject.test.ts): one table, so a case added for
// either language is run by both and the two readers of a message name cannot drift apart.
type subjectProjectFixture struct {
	Families []string `json:"families"`
	Builders []struct {
		Fn   string   `json:"fn"`
		Args []string `json:"args"`
		Want string   `json:"want"`
	} `json:"builders"`
	IssueProject []struct {
		Key     string `json:"key"`
		Project string `json:"project"`
		OK      bool   `json:"ok"`
	} `json:"issueProject"`
	RoleProject []struct {
		Role    string `json:"role"`
		Project string `json:"project"`
	} `json:"roleProject"`
	SubjectProject []struct {
		Subject string `json:"subject"`
		Project string `json:"project"`
		Repo    string `json:"repo"`
		OK      bool   `json:"ok"`
	} `json:"subjectProject"`
	CanonicalTopic []struct {
		Topic     string `json:"topic"`
		Canonical string `json:"canonical"`
		Legacy    bool   `json:"legacy"`
	} `json:"canonicalTopic"`
	LegacyTwin []struct {
		Canonical string  `json:"canonical"`
		Twin      *string `json:"twin"`
	} `json:"legacyTwin"`
	CanonicalRole []struct {
		Role    string `json:"role"`
		Project string `json:"project"`
		Name    string `json:"name"`
		Legacy  bool   `json:"legacy"`
	} `json:"canonicalRole"`
	TopicForms []struct {
		Topic string   `json:"topic"`
		Forms []string `json:"forms"`
	} `json:"topicForms"`
	RoleClaimKey []struct {
		Project string `json:"project"`
		Role    string `json:"role"`
		Key     string `json:"key"`
	} `json:"roleClaimKey"`
	IssueEventKey []struct {
		Subject string `json:"subject"`
		Key     string `json:"key"`
		Legacy  bool   `json:"legacy"`
		OK      bool   `json:"ok"`
	} `json:"issueEventKey"`
	ClaimProject []struct {
		Role      string `json:"role"`
		Requested string `json:"requested"`
		Project   string `json:"project"`
		Error     bool   `json:"error"`
	} `json:"claimProject"`
}

func readSubjectProjectFixture(t *testing.T) subjectProjectFixture {
	t.Helper()
	raw, err := os.ReadFile("../../../contracts/fixtures/subject-project.json")
	if err != nil {
		t.Fatalf("read the shared cases: %v", err)
	}
	var fixture subjectProjectFixture
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatalf("read the shared cases: %v", err)
	}
	// Each section held at least this many rows when this was written: a read that answers a
	// handful of them, or none, passes while covering nothing.
	for section, count := range map[string][2]int{
		"builders":       {len(fixture.Builders), 7},
		"issueProject":   {len(fixture.IssueProject), 8},
		"roleProject":    {len(fixture.RoleProject), 10},
		"subjectProject": {len(fixture.SubjectProject), 38},
		"canonicalTopic": {len(fixture.CanonicalTopic), 23},
		"legacyTwin":     {len(fixture.LegacyTwin), 20},
		"canonicalRole":  {len(fixture.CanonicalRole), 8},
		"topicForms":     {len(fixture.TopicForms), 7},
		"roleClaimKey":   {len(fixture.RoleClaimKey), 9},
		"issueEventKey":  {len(fixture.IssueEventKey), 13},
		"claimProject":   {len(fixture.ClaimProject), 12},
	} {
		if count[0] < count[1] {
			t.Fatalf("the shared cases' %s section holds %d rows, want at least %d", section, count[0], count[1])
		}
	}
	return fixture
}

func TestTheGeneratedBuildersWriteTheSharedFixturesNames(t *testing.T) {
	builders := map[string]func(args []string) string{
		"dispatchIssueSubject": func(args []string) string { return DispatchIssueSubject(args[0], args[1], args[2]) },
		"agentSubject":         func(args []string) string { return AgentSubject(args[0], args[1]) },
		"legacyAgentSubject":   func(args []string) string { return LegacyAgentSubject(args[0]) },
		"roleSubject":          func(args []string) string { return RoleSubject(args[0], args[1]) },
		"legionWakeSubject":    func(args []string) string { return LegionWakeSubject(args[0]) },
	}
	for _, row := range readSubjectProjectFixture(t).Builders {
		build, ok := builders[row.Fn]
		if !ok {
			t.Errorf("the shared cases name a builder, %s, with no Go twin", row.Fn)
			continue
		}
		if got := build(row.Args); got != row.Want {
			t.Errorf("%s(%q) = %q, want %q", row.Fn, row.Args, got, row.Want)
		}
	}
}

func TestAnIssueKeyNamesItsProject(t *testing.T) {
	for _, row := range readSubjectProjectFixture(t).IssueProject {
		if project, ok := IssueProject(row.Key); project != row.Project || ok != row.OK {
			t.Errorf("IssueProject(%q) = %q, %v; want %q, %v", row.Key, project, ok, row.Project, row.OK)
		}
	}
}

func TestALegionRoleNamesItsTokensProject(t *testing.T) {
	for _, row := range readSubjectProjectFixture(t).RoleProject {
		if got := RoleProject(row.Role); got != row.Project {
			t.Errorf("RoleProject(%q) = %q, want %q", row.Role, got, row.Project)
		}
	}
}

func TestEachFamilysNameIsReadForItsProject(t *testing.T) {
	for _, row := range readSubjectProjectFixture(t).SubjectProject {
		project, repo, ok := SubjectProject(row.Subject)
		if project != row.Project || repo != row.Repo || ok != row.OK {
			t.Errorf("SubjectProject(%q) = %q, %q, %v; want %q, %q, %v", row.Subject, project, repo, ok, row.Project, row.Repo, row.OK)
		}
	}
}

func TestAnOldFormTopicReadsAsItsNewForm(t *testing.T) {
	fixture := readSubjectProjectFixture(t)
	for _, row := range fixture.CanonicalTopic {
		canonical, legacy := CanonicalTopic(row.Topic)
		if canonical != row.Canonical || legacy != row.Legacy {
			t.Errorf("CanonicalTopic(%q) = %q, %v; want %q, %v", row.Topic, canonical, legacy, row.Canonical, row.Legacy)
		}
		// An old-form topic and its new form are each other's twins.
		if row.Legacy {
			if twin, ok := LegacyTwin(row.Canonical); !ok || twin != row.Topic {
				t.Errorf("LegacyTwin(%q) = %q, %v; want %q, true", row.Canonical, twin, ok, row.Topic)
			}
		}
	}
}

func TestANewFormTopicNamesItsOldForm(t *testing.T) {
	for _, row := range readSubjectProjectFixture(t).LegacyTwin {
		twin, ok := LegacyTwin(row.Canonical)
		want := ""
		if row.Twin != nil {
			want = *row.Twin
		}
		if twin != want || ok != (row.Twin != nil) {
			t.Errorf("LegacyTwin(%q) = %q, %v; want %q, %v", row.Canonical, twin, ok, want, row.Twin != nil)
		}
	}
}

func TestAClaimedRoleReadsInEitherForm(t *testing.T) {
	for _, row := range readSubjectProjectFixture(t).CanonicalRole {
		project, name, legacy := CanonicalRole(row.Role)
		if project != row.Project || name != row.Name || legacy != row.Legacy {
			t.Errorf("CanonicalRole(%q) = %q, %q, %v; want %q, %q, %v", row.Role, project, name, legacy, row.Project, row.Name, row.Legacy)
		}
	}
}

func TestATopicsFormsAreItselfAndItsOtherForm(t *testing.T) {
	for _, row := range readSubjectProjectFixture(t).TopicForms {
		if got := TopicForms(row.Topic); !slices.Equal(got, row.Forms) {
			t.Errorf("TopicForms(%q) = %q, want %q", row.Topic, got, row.Forms)
		}
	}
}

// A role claim is keyed by its bare name whenever its project is the one the name itself gives
// (RoleProject), which is every claim a build made before any producer addressed a role under
// another project, and by its project and name only when the project differs; each key reads back
// to the pair it was written from.
func TestARoleClaimIsKeyedByItsBareNameUnlessItsProjectDiffers(t *testing.T) {
	for _, row := range readSubjectProjectFixture(t).RoleClaimKey {
		if got := RoleClaimKey(row.Project, row.Role); got != row.Key {
			t.Errorf("RoleClaimKey(%q, %q) = %q, want %q", row.Project, row.Role, got, row.Key)
		}
		if project, role := ParseRoleClaimKey(row.Key); project != row.Project || role != row.Role {
			t.Errorf("ParseRoleClaimKey(%q) = %q, %q; want %q, %q", row.Key, project, role, row.Project, row.Role)
		}
	}
}

// An issue event's name carries its issue's key in either form, the new one under the key's own
// project; a name of the family whose key is in neither form, and a name of any other family,
// carries none.
func TestAnIssueEventNamesItsKeyInEitherForm(t *testing.T) {
	for _, row := range readSubjectProjectFixture(t).IssueEventKey {
		key, legacy, ok := IssueEventKey(row.Subject)
		if key != row.Key || legacy != row.Legacy || ok != row.OK {
			t.Errorf("IssueEventKey(%q) = %q, %v, %v; want %q, %v, %v", row.Subject, key, legacy, ok, row.Key, row.Legacy, row.OK)
		}
	}
}

// A claim's project, once a claim keys on its project (ClaimProject, which t-c-8 wires into the
// listener's route; in this slice the route decides with RoleProject and calls nothing here): a
// Legion role token works for the project its own name gives, whatever a claim requests, and only
// a free-form role name takes a requested project, which must be a project key or NoProject. A
// request the rule refuses is an error, never a claim keyed under a project the role does not work
// for.
func TestAClaimsProjectIsItsRolesOrTheOneItAsksFor(t *testing.T) {
	for _, row := range readSubjectProjectFixture(t).ClaimProject {
		project, err := ClaimProject(row.Role, row.Requested)
		if row.Error {
			if err == nil {
				t.Errorf("ClaimProject(%q, %q) = %q, nil; want an error", row.Role, row.Requested, project)
			}
			continue
		}
		if err != nil || project != row.Project {
			t.Errorf("ClaimProject(%q, %q) = %q, %v; want %q", row.Role, row.Requested, project, err, row.Project)
		}
	}
}

// The families SubjectProject reads each name the whole family as a pattern, X.>, which is what
// the bus census (internal/bus subject_project_coverage_test.go) compares with the stream's own
// subjects.
func TestEverySubjectProjectFamilyIsAPrefixAndWildcard(t *testing.T) {
	families := SubjectProjectFamilies()
	if want := readSubjectProjectFixture(t).Families; !slices.Equal(families, want) {
		t.Fatalf("SubjectProjectFamilies = %q, want the shared cases' %q", families, want)
	}
	for _, family := range families {
		if len(family) < len("notifications.x.>") || family[len(family)-2:] != ".>" {
			t.Errorf("family %q is not a literal prefix followed by .>", family)
		}
	}
}
