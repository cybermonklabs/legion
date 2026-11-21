package contracts

import (
	"fmt"
	"regexp"
	"strings"
)

// The readers of a message name's project, and of the two forms of the names the rename moves:
// an issue's events, an agent's messages and a role's lane each gained the project as a token of
// their own (DispatchIssueSubject, AgentSubject, RoleSubject). Every reader takes both forms
// until the old one is refused. These are the hand-written twins of packages/contracts'
// subject-project.ts, and packages/contracts/fixtures/subject-project.json is one table both run.

const (
	exceptionPrefix = "notifications.envoy.exceptions."
	keptPrefix      = "notifications.envoy.kept."
)

var projectKey = regexp.MustCompile(ProjectKeyPattern)

// isLiteral reports whether token names one value: it is neither empty nor a wildcard.
func isLiteral(token string) bool {
	return token != "" && token != "*" && token != ">"
}

// projectOfToken reads a token in a new-form name's project position: a project key is itself,
// and NoProject, a wildcard and anything else name no project.
func projectOfToken(token string) string {
	if projectKey.MatchString(token) {
		return token
	}
	return NoProject
}

// IssueProject is the project of an issue key (INFRA of INFRA-12), and false for a token that is
// no issue key: a project key, a hyphen and a number.
func IssueProject(key string) (string, bool) {
	project, number, found := strings.Cut(key, "-")
	if !found || number == "" || strings.Trim(number, "0123456789") != "" || !projectKey.MatchString(project) {
		return "", false
	}
	return project, true
}

// legionTokenProject is the project of a Legion deployment's project token: its upper case when
// the token is one (lower-case letters and digits, as claim.ProjectToken writes it) and that is a
// project key, and NoProject otherwise. A token is checked before it is upper-cased, so a name
// whose upper case only a Unicode table makes a key (ßk) reads the same in every language.
func legionTokenProject(token string) string {
	if token == "" || strings.Trim(token, "abcdefghijklmnopqrstuvwxyz0123456789") != "" {
		return NoProject
	}
	return projectOfToken(strings.ToUpper(token))
}

// RoleProject is the project a role works for, read from the role's own name: a Legion role,
// legion-<token>-… (ControllerRoleToken and the daemon's issue role tokens), works for the
// project whose key is its token in upper case, and every other role, a lead role among them, for
// none (NoProject).
func RoleProject(role string) string {
	rest, found := strings.CutPrefix(role, "legion-")
	if !found {
		return NoProject
	}
	token, after, found := strings.Cut(rest, "-")
	if !found || after == "" {
		return NoProject
	}
	return legionTokenProject(token)
}

// roleLaneProject reads a role lane's project from the tokens after the lane's prefix: the new
// form's first of two, the old form's role.
func roleLaneProject(rest []string) string {
	if len(rest) >= 2 {
		return projectOfToken(rest[0])
	}
	return RoleProject(rest[0])
}

// subjectProjectRow reads one family's names for their project: family is the pattern naming all
// of it, and read is handed the tokens after the family's prefix.
type subjectProjectRow struct {
	family string
	read   func(rest []string) (project, repo string)
}

func noProject([]string) (string, string) { return NoProject, "" }

// subjectProjectRows has one row for every family the notification stream carries and for the
// role lanes, which it does not. A family with no row reads as no project, which is never a guess
// at a project: the bus census (internal/bus subject_project_coverage_test.go) holds this table to
// the stream's own subject list, so a family the stream gains has a row before any reader meets
// it. It is filled in init, since the exception row reads the table itself.
var subjectProjectRows []subjectProjectRow

func init() {
	subjectProjectRows = []subjectProjectRow{
		// Dispatch's issue events: the new form names the project first, and the old form's
		// project is its key's.
		{family: "notifications.dispatch.issue.>", read: func(rest []string) (string, string) {
			if project, ok := IssueProject(rest[0]); ok {
				return project, ""
			}
			return projectOfToken(rest[0]), ""
		}},
		// Dispatch's document and project events, each named under its project already.
		{family: "notifications.dispatch.document.>", read: func(rest []string) (string, string) { return projectOfToken(rest[0]), "" }},
		{family: "notifications.dispatch.project.>", read: func(rest []string) (string, string) { return projectOfToken(rest[0]), "" }},
		// Dispatch's own start (startup.Subject), which is no project's.
		{family: "notifications.dispatch.server.>", read: noProject},
		// One session's messages: the new form names a project, the old form none.
		{family: "notifications.agent.>", read: func(rest []string) (string, string) {
			if len(rest) >= 2 {
				return projectOfToken(rest[0]), ""
			}
			return NoProject, ""
		}},
		{family: "notifications.role.>", read: func(rest []string) (string, string) { return roleLaneProject(rest), "" }},
		// The Legion daemon's lanes: <token>.controller, <token>.<issue> and <token>.wake are the
		// deployment's, and the one wake every deployment shares is none's.
		{family: "notifications.legion.>", read: func(rest []string) (string, string) {
			if len(rest) >= 2 {
				return legionTokenProject(rest[0]), ""
			}
			return NoProject, ""
		}},
		// A repository's events: the project is the one Dispatch maps the repository to.
		{family: "notifications.github.>", read: func(rest []string) (string, string) {
			if len(rest) >= 2 {
				return "", rest[0] + "/" + rest[1]
			}
			return "", ""
		}},
		{family: "notifications.slack.>", read: noProject},
		{family: "notifications.ghostwispr.>", read: noProject},
		{family: "notifications.whatsapp.>", read: noProject},
		{family: "notifications.team.>", read: noProject},
		// A failed delivery's report is the project's of the name it reports on.
		{family: "notifications.envoy.exceptions.>", read: func(rest []string) (string, string) {
			project, repo, ok := SubjectProject(strings.Join(rest, "."))
			if !ok {
				return NoProject, ""
			}
			return project, repo
		}},
		// The listener's kept role messages and their drain cursors, each a role lane's.
		{family: "notifications.envoy.kept.>", read: func(rest []string) (string, string) {
			if (rest[0] == "role" || rest[0] == "cursor") && len(rest) >= 2 {
				return roleLaneProject(rest[1:]), ""
			}
			return NoProject, ""
		}},
		// Where a deleted session consumer stopped, which names a session and no project.
		{family: "notifications.envoy.resume.>", read: noProject},
		// A role a session took from another: the role's own project, as its kept lane reads it.
		{family: "notifications.envoy.takeover.>", read: func(rest []string) (string, string) {
			return RoleProject(rest[0]), ""
		}},
	}
}

// SubjectProjectFamilies reports the families SubjectProject has a row for, each as the pattern
// naming all of it (notifications.dispatch.document.>). Each call returns a fresh slice.
func SubjectProjectFamilies() []string {
	families := make([]string, len(subjectProjectRows))
	for i, row := range subjectProjectRows {
		families[i] = row.family
	}
	return families
}

// SubjectProject reads the project a message name belongs to. It has one row for every family the
// stream carries (bus.StreamCarriedSubjects) and for the role lanes, which the stream does not:
// the issue, document, project and server families of notifications.dispatch (the document and
// project families already carry the project as token 4, so neither is renamed); agent; role; the
// legion notice and wake lanes; the exception, kept and resume lanes, each taking the project of
// the name it wraps; and GitHub, whose project is "" with its sanitized "owner/name" in repo,
// resolved against the private-projects bucket. Every other family the stream carries - slack,
// whatsapp, ghostwispr, team - and any subject this build does not know answer NoProject, never
// "unknown": a reader that cannot name a family must not be free to guess. A wildcard where a
// project would be answers NoProject too. ok is false only for a subject that is no notification
// topic at all (it does not begin notifications.), which no delivery carries.
func SubjectProject(subject string) (project, repo string, ok bool) {
	if !strings.HasPrefix(subject, "notifications.") {
		return "", "", false
	}
	for _, row := range subjectProjectRows {
		prefix := strings.TrimSuffix(row.family, ">")
		if len(subject) > len(prefix) && strings.HasPrefix(subject, prefix) {
			project, repo = row.read(strings.Split(subject[len(prefix):], "."))
			return project, repo, true
		}
	}
	return NoProject, "", true
}

// keptLane splits a kept lane's role or cursor name into its kind and the tokens after it, and
// reports false for any other name.
func keptLane(topic string) (kind string, rest []string, ok bool) {
	lane, found := strings.CutPrefix(topic, keptPrefix)
	if !found {
		return "", nil, false
	}
	kind, after, _ := strings.Cut(lane, ".")
	if kind != "role" && kind != "cursor" {
		return "", nil, false
	}
	return kind, strings.Split(after, "."), true
}

// CanonicalTopic is the new form of an old-form topic, and whether topic was in the old form: an
// issue's topic naming its key alone (notifications.dispatch.issue.INFRA-12.>) gains its key's
// project, a session's agent subject (notifications.agent.<session>) gains a wildcard project, and
// a role lane gains its role's project (RoleProject); a failed-delivery report of one, and a kept
// role lane, follow the name they carry. Every other topic, a wildcard where the old form's key,
// session or role would be among them, is its own new form. Until the old form is refused only.
func CanonicalTopic(topic string) (canonical string, legacy bool) {
	if rest, found := strings.CutPrefix(topic, DispatchIssueTopicPrefix); found {
		key, _, _ := strings.Cut(rest, ".")
		if project, ok := IssueProject(key); ok {
			return DispatchIssueTopicPrefix + project + "." + rest, true
		}
		return topic, false
	}
	if session, found := strings.CutPrefix(topic, AgentTopicPrefix); found {
		if !strings.Contains(session, ".") && isLiteral(session) {
			return AgentTopicPrefix + "*." + session, true
		}
		return topic, false
	}
	if role, found := strings.CutPrefix(topic, RoleTopicPrefix); found {
		if !strings.Contains(role, ".") && isLiteral(role) {
			return RoleTopicPrefix + RoleProject(role) + "." + role, true
		}
		return topic, false
	}
	if wrapped, found := strings.CutPrefix(topic, exceptionPrefix); found {
		if canonical, legacy := CanonicalTopic(wrapped); legacy {
			return exceptionPrefix + canonical, true
		}
		return topic, false
	}
	if kind, rest, ok := keptLane(topic); ok && len(rest) == 1 && isLiteral(rest[0]) {
		return keptPrefix + kind + "." + RoleProject(rest[0]) + "." + rest[0], true
	}
	return topic, false
}

// LegacyIssueFilter reads a filter that reaches the issue family, which CanonicalTopic reads as its
// own new form, and reports it old when its first token after the family's names an issue key, or
// when it matches some old-form issue event's name (the key, then the event's type) and no new-form
// one (the key's project, the key, then the type), with its new form, any project first. So
// notifications.dispatch.issue.*.comment.created hears old-form names alone, since the new form of
// an event has one token more, and its new form is notifications.dispatch.issue.*.*.comment.created.
// A filter reaches the family when each of its first three tokens is the family's (notifications,
// dispatch, issue) or a wildcard, none of them `>`: notifications.dispatch.*.*.comment.created is
// such a filter, and old as surely. A failed-delivery report of one follows the filter it carries.
// A filter that hears both forms (notifications.dispatch.issue.*.*.>, notifications.dispatch.*.>)
// is not old, nor is one that hears the new form alone (notifications.dispatch.issue.*.INFRA-12.>).
// An event's type is read as two tokens or more: every Dispatch event type is a noun and a verb
// or longer (comment.created, settings.repo_project.updated). The listener's count of old-form
// names reads it beside CanonicalTopic (store.LegacyTopicWarnings, store.CountStoredLegacyTopics),
// and it is the rule packages/envoy/scripts/legacy-subjects-gate.sh holds a filter to (old_form in
// lib/legacy-subjects-gate-forms.jq), so the gate and the count see the same filters. It is not one
// of TopicForms' forms: what a session's consumer filters is unchanged. Until the old form is
// refused only.
func LegacyIssueFilter(topic string) (canonical string, legacy bool) {
	if wrapped, found := strings.CutPrefix(topic, exceptionPrefix); found {
		if canonical, legacy := LegacyIssueFilter(wrapped); legacy {
			return exceptionPrefix + canonical, true
		}
		return "", false
	}
	tokens := strings.Split(topic, ".")
	if len(tokens) < 4 || !familyToken(tokens[0], "notifications") || !familyToken(tokens[1], "dispatch") || !familyToken(tokens[2], "issue") {
		return "", false
	}
	rest := tokens[3:]
	if issueKeyToken(rest[0]) || (issueFilterMatches(rest, issueKeyToken) && !issueFilterMatches(rest, projectKey.MatchString, issueKeyToken)) {
		return strings.Join(tokens[:3], ".") + ".*." + strings.Join(rest, "."), true
	}
	return "", false
}

// familyToken reports whether a filter's token reaches a family's token name: it is that name, or
// a wildcard over one token.
func familyToken(token, name string) bool { return token == name || token == "*" }

// issueFilterMatches reports whether an issue filter's tokens after the family's prefix match some
// name of lead's classes followed by an event type of two tokens or more: a wildcard matches any
// token, `>` as the last token one token or more, and a literal what its class takes.
func issueFilterMatches(tokens []string, lead ...func(string) bool) bool {
	fixed, open := tokens, false
	if len(tokens) > 0 && tokens[len(tokens)-1] == ">" {
		fixed, open = tokens[:len(tokens)-1], true
	}
	if !open && len(fixed) < len(lead)+2 {
		return false
	}
	for i, token := range fixed {
		class := issueTypeToken
		if i < len(lead) {
			class = lead[i]
		}
		if token != "*" && !class(token) {
			return false
		}
	}
	return true
}

// An issue event name's key token holds a hyphen, which no project key and no event type's token
// does, and an event type's token holds none.
func issueKeyToken(token string) bool  { return strings.Contains(token, "-") }
func issueTypeToken(token string) bool { return !strings.Contains(token, "-") }

// isProjectPosition reports whether token may stand in a new-form name's project position for a
// twin: a project key, NoProject, or a wildcard over every project.
func isProjectPosition(token string) bool {
	return projectKey.MatchString(token) || token == NoProject || token == "*"
}

// LegacyTwin is the old form of a new-form topic, while readers take the old form too, and false
// for a topic with none: an issue's topic under its key's own project names its key alone; one
// session's agent subject and one role's lane under any project name the session and the role
// alone; a failed-delivery report and a kept lane follow the name they carry. A topic over every
// issue of one project (notifications.dispatch.issue.INFRA.>) has no old form, since no subject
// pattern names the issues of one project in the old form. Nor has an issue's topic under a
// wildcard project (notifications.dispatch.issue.*.INFRA-12.>): its old form would overlap it (the
// wildcard against the key, the key against what follows), and a session consumer handed both
// would be reduced to a filter over every project's issues (bus.ReduceFilterSubjects); the new
// form alone already reaches the key's own project. Until the old form is refused only.
func LegacyTwin(canonical string) (string, bool) {
	if rest, found := strings.CutPrefix(canonical, DispatchIssueTopicPrefix); found {
		project, keyed, found := strings.Cut(rest, ".")
		key, _, _ := strings.Cut(keyed, ".")
		keyProject, ok := IssueProject(key)
		if !found || !ok || project != keyProject {
			return "", false
		}
		return DispatchIssueTopicPrefix + keyed, true
	}
	for _, prefix := range []string{AgentTopicPrefix, RoleTopicPrefix} {
		if rest, found := strings.CutPrefix(canonical, prefix); found {
			tokens := strings.Split(rest, ".")
			if len(tokens) == 2 && isProjectPosition(tokens[0]) && isLiteral(tokens[1]) {
				return prefix + tokens[1], true
			}
			return "", false
		}
	}
	if wrapped, found := strings.CutPrefix(canonical, exceptionPrefix); found {
		if twin, ok := LegacyTwin(wrapped); ok {
			return exceptionPrefix + twin, true
		}
		return "", false
	}
	if kind, rest, ok := keptLane(canonical); ok && len(rest) == 2 && isProjectPosition(rest[0]) && isLiteral(rest[1]) {
		return keptPrefix + kind + "." + rest[1], true
	}
	return "", false
}

// TopicForms is every form a reader takes topic in while the old form is read: topic, then its new
// form when it is in the old one (CanonicalTopic), then its old form when it is in the new one
// (LegacyTwin). A topic of a family the rename does not move is its one form. A session's consumer
// filters on each form of its topics (bus.SessionConsumers) and the interest index matches each
// (store), so a message published in either form reaches the session. Until the old form is
// refused only.
func TopicForms(topic string) []string {
	forms := []string{topic}
	if canonical, legacy := CanonicalTopic(topic); legacy {
		forms = append(forms, canonical)
	} else if twin, ok := LegacyTwin(topic); ok {
		forms = append(forms, twin)
	}
	return forms
}

// CanonicalRole reads a claimed role in either form: the new form names its project and then the
// role (INFRA.legion-infra-infra-12-architect, _.lead-reviewer), and the old form, the role alone,
// works for the project its name says (RoleProject), with legacy true. A role's name holds no dot
// (roles.PatternString), so a name that is not the new form is the old one, and is left to the
// role check to refuse. Until the old form is refused only.
func CanonicalRole(role string) (project, name string, legacy bool) {
	if project, name, found := strings.Cut(role, "."); found {
		if (projectKey.MatchString(project) || project == NoProject) && name != "" && !strings.Contains(name, ".") {
			return project, name, false
		}
	}
	return RoleProject(role), role, true
}

// issueEventKeyPattern is the shape of an issue key in an issue event's name as the Legion daemon
// has always read it (packages/daemon/internal/claim.IsIssueKey): a capital letter, any capitals
// and digits, a hyphen and a number. It holds no length to the project part, unlike a project key
// (projectKey), because a daemon's own project is held to none: a daemon serves the project its
// configuration names, and its own tests name sixteen-character projects. Dispatch's keys are held
// to projectKey by its database, so every name Dispatch writes is read here too.
var issueEventKeyPattern = regexp.MustCompile(`^([A-Z][A-Z0-9]*)-[0-9]+$`)

// IssueEventKey reads the issue key an issue event's name carries, in either form: the new form
// names the key's own project and then the key (notifications.dispatch.issue.INFRA.INFRA-12.<type>),
// and the old form the key alone (notifications.dispatch.issue.INFRA-12.<type>), with legacy
// true. ok is false for a name of the family whose key is in neither form, a project that is not
// the key's own among them, and for a name of any other family. A key is read by
// issueEventKeyPattern, the shape every daemon has accepted, so a daemon reads every key it read
// before both forms were. The old form is read until it is refused only.
func IssueEventKey(subject string) (key string, legacy, ok bool) {
	rest, found := strings.CutPrefix(subject, DispatchIssueTopicPrefix)
	if !found {
		return "", false, false
	}
	first, after, _ := strings.Cut(rest, ".")
	if issueEventKeyPattern.MatchString(first) {
		return first, true, true
	}
	second, _, _ := strings.Cut(after, ".")
	if match := issueEventKeyPattern.FindStringSubmatch(second); match != nil && match[1] == first {
		return second, false, true
	}
	return "", false, false
}

// RoleClaimKey is the key of a role claim in the envoy_roles bucket: the bare role name when
// project is RoleProject(role), which every claim made before a producer addresses a role under
// another project is, and project + "." + role only when it is not, so no claim that exists ever
// changes key. project is a project key or NoProject. A role's name holds no dot
// (roles.PatternString), so the two kinds of key never collide.
func RoleClaimKey(project, role string) string {
	if project == RoleProject(role) {
		return role
	}
	return project + "." + role
}

// ClaimProject is the project a claim of role works for, given the project the claim requested
// ("" for none), once a claim keys on its project. Nothing calls it in this slice: while every
// claim keys on its bare role name, the listener's POST /v1/roles/set refuses any new-form role
// whose project is not RoleProject(role), so for a free-form role under a project
// (PRIV.lead-reviewer) the route answers 400 where this answers PRIV. Plan C's t-c-8 wires it into
// the route, and t-c-6 reads Dispatch's claims through it, so the two then decide a claim's
// project one way.
//
// A Legion role token (legion-<token>-…) works for the project its own name gives
// (RoleProject), whatever the claim asks for: the role is that project's by construction, and a
// request naming another is refused rather than quietly ignored, since a caller that asked for
// one project and got another would believe a claim it does not hold. Only a free-form role name
// (lead-reviewer, reviewer) takes a requested project, which must be a project key or NoProject;
// a request naming anything else is refused. A claim that requests none works for RoleProject,
// which is NoProject for every free-form name.
func ClaimProject(role, requested string) (string, error) {
	own := RoleProject(role)
	if requested == "" {
		return own, nil
	}
	if own != NoProject {
		if requested != own {
			return "", fmt.Errorf("role %s works for project %s, and a claim of it cannot ask for project %s", role, own, requested)
		}
		return own, nil
	}
	if !projectKey.MatchString(requested) && requested != NoProject {
		return "", fmt.Errorf("a claim of role %s asks for project %s, which is no project key", role, requested)
	}
	return requested, nil
}

// ParseRoleClaimKey reads a role claim's key back to the project and role it was written from
// (RoleClaimKey): a key naming a project and a role is that pair, and a bare key is the role,
// working for the project its name says (RoleProject). Unlike CanonicalRole, which goes with the
// old form of a claimed role, it stays: a bare key is how every claim of a role's own project is
// keyed for good.
func ParseRoleClaimKey(key string) (project, role string) {
	if project, role, found := strings.Cut(key, "."); found {
		if (projectKey.MatchString(project) || project == NoProject) && role != "" && !strings.Contains(role, ".") {
			return project, role
		}
	}
	return RoleProject(key), key
}
