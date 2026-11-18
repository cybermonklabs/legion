package store

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
)

// kindCounter is a LegacySubjectCounter that records the kind label of each count.
type kindCounter struct{ kinds []string }

func (c *kindCounter) Inc(labels ...[2]string) {
	for _, label := range labels {
		if label[0] == "kind" {
			c.kinds = append(c.kinds, label[1])
		}
	}
}

// legacyFixture is the rows of packages/contracts/fixtures/subject-project.json the registry is
// run over: the one table both languages' readers run, so the registry names and counts exactly
// the old forms contracts.CanonicalTopic and contracts.CanonicalRole read.
type legacyFixture struct {
	CanonicalTopic []struct {
		Topic     string `json:"topic"`
		Canonical string `json:"canonical"`
		Legacy    bool   `json:"legacy"`
	} `json:"canonicalTopic"`
	CanonicalRole []struct {
		Role    string `json:"role"`
		Project string `json:"project"`
		Name    string `json:"name"`
		Legacy  bool   `json:"legacy"`
	} `json:"canonicalRole"`
}

func readLegacyFixture(t *testing.T) legacyFixture {
	t.Helper()
	raw, err := os.ReadFile("../../../contracts/fixtures/subject-project.json")
	if err != nil {
		t.Fatalf("read the shared cases: %v", err)
	}
	var fixture legacyFixture
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatalf("read the shared cases: %v", err)
	}
	// 23 topic rows and 8 role rows when this was written: a read that finds none passes while
	// covering nothing.
	if len(fixture.CanonicalTopic) < 23 || len(fixture.CanonicalRole) < 8 {
		t.Fatalf("the shared cases hold %d topic rows and %d role rows, want at least 23 and 8", len(fixture.CanonicalTopic), len(fixture.CanonicalRole))
	}
	return fixture
}

// Each topic a registration names in the old form is counted once (kind="interest") and named in a
// warning with its new form, which the subscribe route passes on in its answer's warnings; a topic
// in the new form, or of a family the rename leaves alone, is neither. A role's lane is named with
// the claim that reaches it instead of a registration of its new form: an interest in a role's lane
// delivers nothing (a role is held through POST /v1/roles/set), so advising one would send the
// caller to a second topic that delivers nothing either.
func TestEachOldFormTopicIsNamedWithItsNewFormAndCounted(t *testing.T) {
	for _, row := range readLegacyFixture(t).CanonicalTopic {
		counter := &kindCounter{}
		r := &Registry{legacySubjects: counter}
		warnings := r.LegacyTopicWarnings("ses_reg", []string{row.Topic})
		var want []string
		var wantKinds []string
		if row.Legacy {
			advice := "; register " + row.Canonical
			if role, isRole := strings.CutPrefix(row.Canonical, contracts.RoleTopicPrefix); isRole {
				advice = "; a role's lane delivers nothing to an interest: claim " + role + " through POST /v1/roles/set"
			}
			want = []string{row.Topic + " is the old form of " + row.Canonical + advice}
			wantKinds = []string{"interest"}
		}
		if !slices.Equal(warnings, want) || !slices.Equal(counter.kinds, wantKinds) {
			t.Errorf("LegacyTopicWarnings(%q) = %q counting %q; want %q counting %q", row.Topic, warnings, counter.kinds, want, wantKinds)
		}
	}
}

// A plugin registers its session's own agent subject itself, in the old form, on every heartbeat:
// that is counted, since it is a reader the old form still has, and named in no warning, since the
// caller named no such topic and could do nothing with one.
func TestTheSessionsOwnOldAgentSubjectIsCountedAndNamedInNoWarning(t *testing.T) {
	counter := &kindCounter{}
	r := &Registry{legacySubjects: counter}
	if warnings := r.LegacyTopicWarnings("ses_own", []string{contracts.LegacyAgentSubject("ses_own")}); len(warnings) != 0 {
		t.Fatalf("warnings = %q, want none for the session's own agent subject", warnings)
	}
	if !slices.Equal(counter.kinds, []string{"interest"}) {
		t.Fatalf("counted %q, want one interest", counter.kinds)
	}
	// A registry the listener opened with no counter counts nothing and still names the topic.
	bare := &Registry{}
	if warnings := bare.LegacyTopicWarnings("ses_own", []string{"notifications.dispatch.issue.INFRA-12.>"}); len(warnings) != 1 {
		t.Fatalf("warnings with no counter = %q, want one", warnings)
	}
}

// A topic a stored row holds in the old form is counted (kind="interest") each time the row is read
// into the session's consumer filter, over every old form the shared cases read, so the counter
// keeps moving while any record still holds one; a topic in the new form, or of a family the rename
// leaves alone, is not. The session's own agent subject in the old form is never counted there:
// the listener writes it into every row itself, and counting it would move the counter for ever.
func TestAStoredOldFormTopicIsCountedEachTimeItIsReadIntoAFilter(t *testing.T) {
	for _, row := range readLegacyFixture(t).CanonicalTopic {
		counter := &kindCounter{}
		r := &Registry{legacySubjects: counter}
		r.CountStoredLegacyTopics("ses_stored", []string{row.Topic})
		r.CountStoredLegacyTopics("ses_stored", []string{row.Topic})
		var want []string
		if row.Legacy {
			want = []string{"interest", "interest"}
		}
		if !slices.Equal(counter.kinds, want) {
			t.Errorf("two reads of a row holding %q counted %q, want %q", row.Topic, counter.kinds, want)
		}
	}
	counter := &kindCounter{}
	(&Registry{legacySubjects: counter}).CountStoredLegacyTopics("ses_stored", []string{
		contracts.LegacyAgentSubject("ses_stored"),
		contracts.AgentSubject(contracts.NoProject, "ses_stored"),
	})
	if len(counter.kinds) != 0 {
		t.Fatalf("the session's own agent subject was counted %q, want nothing", counter.kinds)
	}
	(&Registry{}).CountStoredLegacyTopics("ses_stored", []string{"notifications.dispatch.issue.INFRA-12.>"})
}

// A role claimed in the old form, its name alone, is read as that name, counted once
// (kind="role") and named with its new form, project first; a role claimed in the new form is
// read as its name and neither counted nor named.
func TestEachOldFormRoleIsReadAsItsNameAndCounted(t *testing.T) {
	for _, row := range readLegacyFixture(t).CanonicalRole {
		counter := &kindCounter{}
		r := &Registry{legacySubjects: counter}
		name, warning := r.ClaimedRole(row.Role)
		want, wantKinds := "", []string(nil)
		if row.Legacy {
			current := row.Project + "." + row.Name
			want = row.Role + " is the old form of " + current + "; claim " + current
			wantKinds = []string{"role"}
		}
		if name != row.Name || warning != want || !slices.Equal(counter.kinds, wantKinds) {
			t.Errorf("ClaimedRole(%q) = %q, %q counting %q; want %q, %q counting %q", row.Role, name, warning, counter.kinds, row.Name, want, wantKinds)
		}
	}
}

// An interest stored in either form matches a publish in either form, through the registry's
// index (Match, MatchAllMachines) and the known sessions' matcher alike (TopicMatcher), so a
// listener's fan-out and a publish's recipient count read a producer that moved to the new form
// as they read it today, and a session that registered the new form is reached by today's
// publishes.
func TestAnInterestInEitherFormMatchesAPublishInEitherForm(t *testing.T) {
	interests := map[string]Interest{
		"ses_old": {SessionID: "ses_old", MachineID: "m", Topics: []string{"notifications.dispatch.issue.INFRA-12.>", contracts.LegacyAgentSubject("ses_old")}},
		"ses_new": {SessionID: "ses_new", MachineID: "m", Topics: []string{contracts.DispatchIssueSubject("INFRA", "INFRA-12", ">"), contracts.AgentSubject("INFRA", "ses_new")}},
	}
	r := holding(t, interests)
	matched := func(interests []Interest) []string {
		var sessions []string
		for _, interest := range interests {
			sessions = append(sessions, interest.SessionID)
		}
		slices.Sort(sessions)
		return sessions
	}
	for _, subject := range []string{
		"notifications.dispatch.issue.INFRA-12.comment.created",
		contracts.DispatchIssueSubject("INFRA", "INFRA-12", "comment.created"),
	} {
		if got := matched(r.Match("m", subject)); !slices.Equal(got, []string{"ses_new", "ses_old"}) {
			t.Errorf("Match(%q) = %q, want both sessions", subject, got)
		}
		if got := matched(r.MatchAllMachines(subject)); !slices.Equal(got, []string{"ses_new", "ses_old"}) {
			t.Errorf("MatchAllMachines(%q) = %q, want both sessions", subject, got)
		}
	}
	for subject, want := range map[string]string{
		contracts.AgentSubject("PRIV", "ses_old"):              "ses_old",
		contracts.AgentSubject(contracts.NoProject, "ses_old"): "ses_old",
		contracts.LegacyAgentSubject("ses_new"):                "ses_new",
	} {
		if got := matched(r.Match("m", subject)); !slices.Equal(got, []string{want}) {
			t.Errorf("Match(%q) = %q, want %s alone", subject, got, want)
		}
	}
	if got := r.Match("m", contracts.AgentSubject("INFRA", "ses_other")); len(got) != 0 {
		t.Errorf("another session's agent subject matched %v", got)
	}

	known := NewTopicMatcher()
	known.Set("ses_old", interests["ses_old"].Topics)
	var visited []string
	known.Match(contracts.DispatchIssueSubject("INFRA", "INFRA-12", "comment.created"), func(key string) { visited = append(visited, key) })
	if !slices.Equal(visited, []string{"ses_old"}) {
		t.Errorf("the known sessions' matcher visited %q for the new form, want ses_old once", visited)
	}
}

// An unsubscribe of either form of a topic removes both from the record: the two forms are one
// interest, and leaving the other would keep the session's consumer filtered on it. An agent's
// name and a role's lane under a project are the rows a reading that takes the new form's
// wildcard (notifications.agent.*.s1) as a literal key leaves behind.
func TestRemovingATopicRemovesEveryFormOfIt(t *testing.T) {
	const kept = "notifications.github.o.r.>"
	for _, tc := range []struct{ name, stored, removed string }{
		{"an issue by its old name", "notifications.dispatch.issue.INFRA.INFRA-12.>", "notifications.dispatch.issue.INFRA-12.>"},
		{"an issue by its new name", "notifications.dispatch.issue.INFRA-12.>", "notifications.dispatch.issue.INFRA.INFRA-12.>"},
		{"an agent by its old name", "notifications.agent._.ses_a", "notifications.agent.ses_a"},
		{"an agent under a project by its old name", "notifications.agent.PRIV.ses_a", "notifications.agent.ses_a"},
		{"an agent by its new name", "notifications.agent.ses_a", "notifications.agent._.ses_a"},
		{"a role by its old name", "notifications.role.PRIV.lead-reviewer", "notifications.role.lead-reviewer"},
		{"a role by its new name", "notifications.role.lead-reviewer", "notifications.role._.lead-reviewer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			item := Interest{Topics: []string{tc.stored, kept}}
			if got := Remove(item, []string{tc.removed}).Topics; !slices.Equal(got, []string{kept}) {
				t.Fatalf("a record holding %q, asked to remove %q, left %q; want the GitHub topic alone", tc.stored, tc.removed, got)
			}
		})
	}
	// Another session's agent subject and another role's lane are other interests, whatever their
	// form: a removal that took them would unsubscribe what the caller never named.
	item := Interest{Topics: []string{"notifications.agent._.ses_other", "notifications.role.PRIV.other-role", kept}}
	if got := Remove(item, []string{"notifications.agent.ses_a", "notifications.role.lead-reviewer"}).Topics; !slices.Equal(got, item.Topics) {
		t.Fatalf("removing one session's and one role's topics left %q, want every topic of another session and another role", got)
	}
}

// A role's lane topic names its role in either form, so a record's roles and a release of named
// topics read a record that holds the new form as they read one that holds the old.
func TestARoleTopicInEitherFormNamesItsRole(t *testing.T) {
	for topic, want := range map[string]string{
		"notifications.role.legion-priv-priv-1-architect":                 "legion-priv-priv-1-architect",
		contracts.RoleSubject("PRIV", "legion-priv-priv-1-architect"):     "legion-priv-priv-1-architect",
		contracts.RoleSubject(contracts.NoProject, "lead-reviewer"):       "lead-reviewer",
		contracts.RoleSubject("*", "lead-reviewer"):                       "lead-reviewer",
		contracts.RoleSubject("PRIV", "*"):                                "",
		"notifications.role.>":                                            "",
		contracts.DispatchIssueSubject("PRIV", "PRIV-1", "issue.updated"): "",
	} {
		role, ok := roleOfTopic(topic)
		if role != want || ok != (want != "") {
			t.Errorf("roleOfTopic(%q) = %q, %v; want %q, %v", topic, role, ok, want, want != "")
		}
	}
	if got := rolesOfTopics([]string{"notifications.role.lead-reviewer", contracts.RoleSubject(contracts.NoProject, "lead-reviewer")}); !slices.Equal(got, []string{"lead-reviewer"}) {
		t.Errorf("rolesOfTopics of both forms = %q, want lead-reviewer once", got)
	}
}

// What the rename leaves alone behaves as it did at the merge base: a registration naming only
// topics in the new form or of a family the rename does not move is named in no warning and counted
// nowhere, and the record stores each topic as the caller wrote it, whatever its form, as the merge
// base stored it, read back through the bucket by a second registry.
func TestARecordStoresEachTopicAsTheCallerWroteIt(t *testing.T) {
	counter := &kindCounter{}
	unmoved := []string{
		"notifications.github.o.r.>",
		"notifications.team.deploy-squad",
		"notifications.dispatch.document.INFRA.spec.>",
		contracts.DispatchIssueSubject("INFRA", "INFRA-12", ">"),
	}
	if warnings := (&Registry{legacySubjects: counter}).LegacyTopicWarnings("ses_r9", unmoved); len(warnings) != 0 || len(counter.kinds) != 0 {
		t.Fatalf("topics the rename leaves alone were named %q and counted %q, want neither", warnings, counter.kinds)
	}

	conn, cleanup := connectNATS(t)
	defer cleanup()
	writer, err := Open(conn, WithReplicas(1))
	if err != nil {
		t.Fatalf("open the registry: %v", err)
	}
	defer writer.StopWatch()
	reader, err := Open(conn, WithReplicas(1))
	if err != nil {
		t.Fatalf("open the second registry: %v", err)
	}
	defer reader.StopWatch()
	topics := append(slices.Clone(unmoved), "notifications.dispatch.issue.INFRA-12.>", contracts.LegacyAgentSubject("ses_r9"))
	if _, err := writer.Upsert(Interest{SessionID: "ses_r9", MachineID: "m1"}, topics); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	row, err := reader.Get(t.Context(), "ses_r9")
	if err != nil {
		t.Fatalf("read the row back: %v", err)
	}
	want := slices.Sorted(slices.Values(topics))
	if !slices.Equal(row.Topics, want) {
		t.Fatalf("stored topics = %q, want %q as written", row.Topics, want)
	}
	pollMatch(t, reader, "m1", contracts.DispatchIssueSubject("INFRA", "INFRA-12", "comment.created"), 1, 5*time.Second)
}
