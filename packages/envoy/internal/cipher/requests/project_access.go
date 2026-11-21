// packages/envoy/internal/cipher/requests/project_access.go
// project_access.go is the request machine's half of a private project's secrets
// (dispatch://INFRA-3862, R11): what a credential names, and what Dispatch holds of each project a
// requested secret is tagged with, resolved on the pool before any transaction begins and handed
// to policy.Set.Evaluate as plain data. No call to Dispatch is made inside a transaction, under a
// row lock or under an advisory lock, and none at all for a request, release or airlock
// connection that holds no tagged secret.
package requests

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/cipher/exchange"
	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
	"github.com/sjawhar/envoy/internal/cipher/record"
	"github.com/sjawhar/envoy/internal/oidc"
)

// ErrProjectAccessUnavailable is a request, release, decision or airlock connection that holds a
// secret tagged with a project the broker could not ask Dispatch about: its project-access client
// failed, or the broker holds none. Nothing is released; the session asks again once Dispatch
// answers. A request or release of a secret with no tag never meets it. A person's decision, and a
// route that names a record or grant for a person, meet it too when Dispatch cannot say whether
// that person may see the row (maySeeRecord, which reads the projects its reason's issue keys
// name): nothing is decided or written and the request stays pending. An approver's list never
// fails with it; it leaves that one row out (logRowUnanswered).
var ErrProjectAccessUnavailable = errors.New("the broker could not ask Dispatch about the project of a requested secret")

// errProjectNotNamed is stillAllowed's refusal of a granted name whose private project the
// session no longer names, or whose project Dispatch no longer holds: the project turned
// private, its person left the project's access list, or the tag now names another project.
// Values, the airlock and reuseLiveGrant end such a grant for good (endGrant), so making the
// project public again, or putting the person back, never revives it; the session asks again.
var errProjectNotNamed = errors.New("project not named")

// airlockProjectRecheck bounds how long the airlock proxy may hold the values of a grant that
// holds a project-tagged name, and a project-channels value released to a credential that names
// any project: every such AirlockSecret expires no later than the read time plus this, and the
// proxy drops a value within its own mintAgainBefore of that expiry, so a pod holding values
// re-runs the release checks (AirlockValues, checkRelease) within this bound of the read. Whether
// a project is private, and which channels its owner allows, are Dispatch's, not the policy
// version's, so nothing else would make the proxy read a held value again when access is lost or
// a named project turns private.
const airlockProjectRecheck = 10 * time.Minute

// projectFacts is what a release's policy check reads of Dispatch for one credential and one set
// of names: the projects the credential names that its person may see, and the state of each
// project a tagged name among them carries. It is plain data, resolved before any transaction.
type projectFacts struct {
	projects []string
	states   map[string]policy.ProjectState
	// named are the projects the credential names, before the visibility filter projects is the
	// result of, resolved only when a requested name carries a project-channels tag, which is the
	// one rule that reads them (policy.Requester.ListsPrivate).
	named []string
	// person is who the credential acts for, for the channel limit's own reads.
	person string
	// asked is when the oldest answer of Dispatch's these facts rest on was asked, and zero for
	// facts that asked Dispatch nothing: the age a decision made on them is checked by
	// (atDecision).
	asked time.Time
}

// requester is r with these facts, the form policy.Set.Evaluate reads them in.
func (f projectFacts) requester(r policy.Requester) policy.Requester {
	r.Projects, r.ProjectStates, r.Named = f.projects, f.states, f.named
	return r
}

// atDecision is the one rule for how old a project fact a decision rests on may be: younger than
// projectaccess.CacheTTL when the decision is made, the bound on how long a project turned private,
// or a person taken off its list, goes on passing a check here. collect reads Dispatch and answers
// what the decision rests on, the decision itself included, and when the oldest answer under it
// was asked; collect returning is the moment of the decision, so the age is measured then and
// never at a read's start. Each answer is young when it is read, from the cache or from Dispatch,
// but the reads are one at a time and each may take up to 10 s, so an early answer can age past
// the bound while the later ones are read. Such a decision is collected once more under
// projectaccess.Fresh, which asks Dispatch every question again, with again set, so a collect that
// decides on facts its caller read before it reads those again too; one whose answers aged past
// the bound a second time is ErrProjectAccessUnavailable: it fails closed, and nothing is decided
// on an aged fact. An error collect answers is answered as it is.
func atDecision[T any](ctx context.Context, a projectaccess.Access, collect func(ctx context.Context, again bool) (T, time.Time, error)) (T, error) {
	got, asked, err := collect(ctx, false)
	if err != nil || !projectaccess.Stale(a, asked) {
		return got, err
	}
	got, asked, err = collect(projectaccess.Fresh(ctx), true)
	if err != nil || !projectaccess.Stale(a, asked) {
		return got, err
	}
	var none T
	return none, fmt.Errorf("%w: the answers a decision rests on aged past %s before it was made, and again when asked afresh", ErrProjectAccessUnavailable, projectaccess.CacheTTL)
}

// anyChannelLimited reports whether any of names carries a project-channels tag, the one tag
// whose rule reads the credential's own projects rather than the secret's.
func anyChannelLimited(set *policy.Set, names []string) bool {
	return slices.ContainsFunc(names, func(name string) bool { return set.Secrets[name].ProjectChannels != "" })
}

// taggedProjects is each distinct project a name of names is tagged with in set, sorted, and nil
// when none is.
func taggedProjects(set *policy.Set, names []string) []string {
	var tagged []string
	for _, name := range names {
		if project := set.Secrets[name].Project; project != "" && !slices.Contains(tagged, project) {
			tagged = append(tagged, project)
		}
	}
	slices.Sort(tagged)
	return tagged
}

// anyProjectTagged reports whether set tags any of granted with a project: a grant of such a name
// is checked again on every release, since whether its project is private is Dispatch's state and
// not in the policy version.
func anyProjectTagged(set *policy.Set, granted []requestedSecret) bool {
	return slices.ContainsFunc(granted, func(g requestedSecret) bool { return set.Secrets[g.name].Project != "" })
}

// credentialProjects is the person a credential acts for and the projects it names: a person's
// session names the projects its enrollment recorded, and so does a person's pod while
// BROKER_POD_OPERATORS still maps its account to its person (personPodGrant, unmappedPersonPod),
// since it named them when it enrolled as that session does and was checked against that person
// (enroll.Service.Create); a pod of no person names the one project its operator's entry maps its
// service account to, checked against that entry's person, and so does a person's pod the
// setting no longer maps, which acts for no person — but only while that entry names the issuer
// whose token enrolled the pod (exchange.Set.PodProjects), since the same account name in another
// cluster is another ServiceAccount, whose token the exchange refuses too. The enrollment records
// that issuer's URL, read as its name in BROKER_K8S_OIDC_ISSUER (Issuers, oidc.IssuerName): one
// recording none was enrolled before enrollments recorded one, under the first issuer, and a URL
// the setting no longer holds is no issuer's, so such a pod names no project. A machine's own
// session names the one its fleet's Google account maps to; anything else names none.
func (m *Machine) credentialProjects(e enrollmentRow) (person string, projects []string) {
	switch {
	case e.Kind == "session" && e.Operator != nil:
		return *e.Operator, e.Projects
	case personPodGrant(e) && !m.unmappedPersonPod(e):
		return *e.Operator, e.Projects
	case m.Exchange == nil || e.Subject == nil:
		return "", nil
	case e.Kind == "pod":
		issuer, known := oidc.IssuerName(m.Issuers, e.Issuer)
		if !known {
			return "", nil
		}
		person, projects, _ := m.Exchange.PodProjects(*e.Subject, issuer)
		return person, projects
	case e.Kind == "session":
		if read := exchange.ReadSubject(*e.Subject); read.Kind == exchange.SubjectMachine {
			person, projects, _ := m.Exchange.MappedProjects(exchange.GooglePrincipal(read.Account))
			return person, projects
		}
	}
	return "", nil
}

// projectFacts resolves, on the pool and before any transaction, what Evaluate needs of Dispatch
// for e asking for names under set: one state per distinct project tag among names
// (projectaccess.Access.States: present, private or public; Missing, missing), and the projects
// the credential names that its person may still see, of those tags alone, since Evaluate reads
// the credential's list for a tagged private secret and for nothing else. Where a requested name
// carries a project-channels tag, the states of the credential's own named projects are resolved
// too, because that rule reads them whatever project the secret belongs to
// (policy.Requester.ListsPrivate). A request holding neither tag asks Dispatch nothing at all. A
// failure to ask is ErrProjectAccessUnavailable.
//
// Every answer the facts rest on is younger than projectaccess.CacheTTL when projectFacts returns
// (atDecision), and the caller decides on them then: a release (checkRelease) reads the
// requester's own rows first and evaluates the names with nothing between, as JudgePolicy and the
// airlock's connection check (airlockProjectRule) do. A request (Create) and a decision
// (applyDecisionAttempt) evaluate the names again inside their transaction, where Dispatch is
// never asked; they release no value, and every grant of a name these facts are read for is
// checked again on facts of its own at each release (checkRelease's anyProjectTagged and
// anyChannelLimited).
func (m *Machine) projectFacts(ctx context.Context, e enrollmentRow, names []string, set *policy.Set) (projectFacts, error) {
	return atDecision(ctx, m.Projects, func(ctx context.Context, _ bool) (projectFacts, time.Time, error) {
		facts, err := m.collectProjectFacts(ctx, e, names, set)
		return facts, facts.asked, err
	})
}

// collectProjectFacts is projectFacts without the bound on its answers' age, for a caller that
// reads Dispatch further before it decides and so checks the age of everything it read at once
// (atDecision): the approval's decision, the judge's project check and the listings.
func (m *Machine) collectProjectFacts(ctx context.Context, e enrollmentRow, names []string, set *policy.Set) (projectFacts, error) {
	tagged := taggedProjects(set, names)
	person, named := m.credentialProjects(e)
	limited := anyChannelLimited(set, names)
	// A project-channels name read by a credential that names no project is unlimited, and so is
	// decided without asking Dispatch anything, as an untagged name is.
	if len(tagged) == 0 && (!limited || len(named) == 0) {
		return projectFacts{}, nil
	}
	if m.Projects == nil {
		// A broker with no project-access client loads no dispatch-project secret (the loader
		// refuses one project-unchecked) and cannot know any project is private, so a
		// project-channels name is unlimited here, as at the merge base (R9), whatever projects
		// the credential names. A tagged name cannot reach this point, and fails closed if it does.
		if len(tagged) == 0 {
			return projectFacts{}, nil
		}
		return projectFacts{}, fmt.Errorf("%w: this broker holds no client of Dispatch's project access", ErrProjectAccessUnavailable)
	}
	asking := tagged
	if limited {
		asking = slices.Concat(tagged, named)
		slices.Sort(asking)
		asking = slices.Compact(asking)
	}
	answers, err := m.Projects.States(ctx, asking)
	if err != nil {
		return projectFacts{}, fmt.Errorf("%w: %v", ErrProjectAccessUnavailable, err)
	}
	facts := projectFacts{states: make(map[string]policy.ProjectState, len(asking)), person: person}
	if limited {
		facts.named = named
	}
	for _, project := range asking {
		answer, held := answers[project]
		facts.asked = projectaccess.Oldest(facts.asked, answer.AskedAt)
		// Only a project Dispatch answered "public" is public: any other visibility, an empty one
		// included, is no state, and the facts fail closed as when Dispatch cannot be asked.
		switch {
		case !held || answer.Missing:
			facts.states[project] = policy.ProjectMissing
		case answer.Private():
			facts.states[project] = policy.ProjectPrivate
		case answer.Public():
			facts.states[project] = policy.ProjectPublic
		default:
			return projectFacts{}, fmt.Errorf("%w: %w: %q for %s", ErrProjectAccessUnavailable, projectaccess.ErrUnknownVisibility, answer.Visibility, project)
		}
	}
	for _, project := range named {
		if facts.states[project] != policy.ProjectPrivate {
			continue
		}
		// A project the person may not see is left out of the facts, which refuses whatever its
		// answer's age; one they may see is kept, aged from its own question.
		asked, err := projectaccess.VisibleAsOf(ctx, m.Projects, person, []string{project})
		switch {
		case err == nil:
			facts.projects = append(facts.projects, project)
			facts.asked = projectaccess.Oldest(facts.asked, asked)
		case !errors.Is(err, projectaccess.ErrNotVisible):
			return projectFacts{}, fmt.Errorf("%w: %v", ErrProjectAccessUnavailable, err)
		}
	}
	return facts, nil
}

// ChannelsMissingMessage is the log line for a private project whose Dispatch answered no
// slack_channels key at all, a Dispatch from before dispatch://INFRA-3862's #t-a-31: the broker
// limits that project's credentials to no channel rather than read the missing key as no limit,
// so Dispatch and the broker deploy in either order safely.
const ChannelsMissingMessage = "a private project's access answer carries no slack_channels; its credentials post to no channel until Dispatch has dispatch://INFRA-3862's #t-a-31"

// logThrottle is when each key's log line was last written, so a line a release or a listing
// would repeat on every call is written once per key per projectaccess.CacheTTL, the age after
// which the broker asks Dispatch again anyway. A Machine holds one per line it throttles.
type logThrottle struct {
	mu sync.Mutex
	at map[string]time.Time
}

// due reports whether key's line may be written now, and if so counts it written. Keys last
// written longer than projectaccess.CacheTTL ago are dropped as it goes, so the map holds only
// the keys of the last CacheTTL.
func (l *logThrottle) due(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if at, ok := l.at[key]; ok && now.Sub(at) < projectaccess.CacheTTL {
		return false
	}
	if l.at == nil {
		l.at = map[string]time.Time{}
	}
	for held, at := range l.at {
		if now.Sub(at) >= projectaccess.CacheTTL {
			delete(l.at, held)
		}
	}
	l.at[key] = now
	return true
}

// channelLimit is the channels a grant's credential may post to, and whether it is limited at
// all. It is limited when the grant holds a project-channels name and one of the projects its
// credential names is private (policy.Requester.ListsPrivate, read from the unfiltered list). The
// channels are then those every such project's owner allowed: the intersection, sorted, never
// nil, so content of any named project lands only where all of their owners allow. They are empty
// when the person can no longer see one of those projects, when a project's answer carries no
// slack_channels key, when Dispatch did not answer, and when the lists share no channel; they
// are never unlimited. A grant holding no project-channels name, or a credential naming no
// private project, is unlimited and asks Dispatch nothing here; the answers it reads otherwise
// are the ones projectFacts already put in the client's cache.
//
// It decides on the release's own facts, which the values' reads since checkRelease have aged,
// and on its own reads of the channels, so the decision is held to atDecision's bound like any
// other: facts that aged past it are read again, afresh, and decided on as read, and a limit whose
// answers aged past it again is the limit to no channel.
func (m *Machine) channelLimit(ctx context.Context, checked grantRelease) ([]string, bool) {
	names := make([]string, len(checked.granted))
	for i, g := range checked.granted {
		names[i] = g.name
	}
	if !anyChannelLimited(checked.set, names) {
		return nil, false
	}
	limit, err := atDecision(ctx, m.Projects, func(ctx context.Context, again bool) (channelGrant, time.Time, error) {
		facts := checked.facts
		if again {
			var err error
			if facts, err = m.collectProjectFacts(ctx, checked.enr, names, checked.set); err != nil {
				return channelGrant{}, time.Time{}, err
			}
		}
		limit, asked := m.channelsOf(ctx, facts)
		return limit, projectaccess.Oldest(facts.asked, asked), nil
	})
	if err != nil {
		return []string{}, true
	}
	return limit.channels, limit.limited
}

// channelGrant is what channelLimit answers: the channels, and whether they limit at all.
type channelGrant struct {
	channels []string
	limited  bool
}

// channelsOf is channelLimit's decision on facts, and when the oldest answer it read itself was
// asked: zero when it read none.
func (m *Machine) channelsOf(ctx context.Context, facts projectFacts) (channelGrant, time.Time) {
	none := channelGrant{channels: []string{}, limited: true}
	if !facts.requester(policy.Requester{}).ListsPrivate() {
		return channelGrant{}, time.Time{}
	}
	var allowed []string
	var asked time.Time
	first := true
	for _, project := range facts.named {
		if facts.states[project] != policy.ProjectPrivate {
			continue
		}
		if !slices.Contains(facts.projects, project) {
			return none, asked
		}
		answer, err := m.Projects.Access(ctx, project, facts.person)
		if err != nil {
			return none, asked
		}
		asked = projectaccess.Oldest(asked, answer.AskedAt)
		if !answer.SlackChannelsPresent {
			m.logChannelsMissing(project)
			return none, asked
		}
		listed := slices.Compact(slices.Sorted(slices.Values(answer.SlackChannels)))
		if first {
			allowed, first = listed, false
			continue
		}
		allowed = slices.DeleteFunc(allowed, func(channel string) bool { return !slices.Contains(listed, channel) })
	}
	if allowed == nil {
		return none, asked
	}
	return channelGrant{channels: allowed, limited: true}, asked
}

// logChannelsMissing writes ChannelsMissingMessage for project once per projectaccess.CacheTTL.
func (m *Machine) logChannelsMissing(project string) {
	if m.channelsMissingLogged.due(project) {
		slog.Warn(ChannelsMissingMessage, "project", project)
	}
}

// rowUnansweredMessage is the log line for a row an approver's list leaves out because the
// broker could not ask Dispatch whether the reader may see it (ErrProjectAccessUnavailable): the
// row is hidden from that list alone, failing closed, and the rest of the list is answered. A
// row whose responsible person is someone else has lines of its own (fallbackUnreadMessage,
// fallbackCutMessage), since such a row belongs on the list only where that person may not see it.
const rowUnansweredMessage = "an approver's list leaves out a row whose project Dispatch could not be asked about; it is listed again once Dispatch answers"

// logRowUnanswered writes rowUnansweredMessage for the one row key names, with attrs and err,
// once per projectaccess.CacheTTL however often its list is read: a list is read on every 15 s
// poll of every open Inbox tab, and one line per row says all an operator needs.
func (m *Machine) logRowUnanswered(key string, err error, attrs ...any) {
	if m.rowsUnansweredLogged.due(key) {
		slog.Warn(rowUnansweredMessage, append(attrs, "error", err)...)
	}
}

// fallbackUnreadMessage is the log line for a row whose responsible person is someone else
// (listedInStead) that an approver's list leaves out because a read of Dispatch failed, the
// reader's or that person's. All the broker knows is that it could not tell whether the row
// belongs on the list, which it does only where that person may not see it; so the line names
// the person, and an operator who finds that person off the row's project has found a request
// waiting on nobody's list.
const fallbackUnreadMessage = "an approver's list leaves out a row whose responsible person is someone else, since Dispatch could not be asked whether the row belongs there too; it belongs there only where that person may not see it"

// logFallbackUnread writes fallbackUnreadMessage for the record recordID, naming responsible, the
// person its record names, and err, the read's error, once per projectaccess.CacheTTL however
// often its list is read, as logRowUnanswered does.
func (m *Machine) logFallbackUnread(recordID, responsible string, err error) {
	if m.rowsUnansweredLogged.due("fallback " + recordID) {
		slog.Warn(fallbackUnreadMessage, "list", "pending", "record", recordID, "responsible", responsible, "error", err)
	}
}

// fallbackCutMessage is the log line for the rows whose responsible person is someone else
// (listedInStead) that an approver's list leaves out unread because its time for them ran out
// (listFallbackBound). Each such row is listed for its responsible person wherever that person may
// see it, and the broker read nothing that says which rows are not; so the line counts them, one
// line for the list, rather than name every row as a request that may wait on nobody's list.
const fallbackCutMessage = "an approver's list leaves out, unread, rows whose responsible person is someone else, since its time for reading them ran out; each belongs there only where that person may not see it"

// logFallbackCut writes fallbackCutMessage for reader's list, counting rows and naming err, the
// list's clock's error, once per reader per projectaccess.CacheTTL however often that list is
// read: a list is read on every 15 s poll of every open Inbox tab, and while Dispatch is slow
// every one of them runs out of the clock.
func (m *Machine) logFallbackCut(reader string, rows int, err error) {
	if m.rowsUnansweredLogged.due("cut " + reader) {
		slog.Warn(fallbackCutMessage, "list", "pending", "reader", reader, "rows", rows, "bound", listFallbackBound, "error", err)
	}
}

// ReasonUnreadMessage is the log line for a stored row whose reason names the issue keys of more
// projects than a new request may (record.MaxReasonProjects): the row can only be one stored before
// Create refused such a reason, so neither rule reads any of its keys (reasonProjects), and they
// count as naming no project, which is how every reader saw the row when it was written. Its other
// projects are still asked.
const ReasonUnreadMessage = "a stored request's reason names the issue keys of more projects than a new request may; its keys are not read, and count as naming no project"

// logReasonUnread writes ReasonUnreadMessage for the one row row names ("record <id>" or
// "grant <id>"), at info and once per projectaccess.CacheTTL however often the row is read. It
// names the row and the count, never the reason's text.
func (m *Machine) logReasonUnread(row string, projects int) {
	if m.reasonsUnreadLogged.due(row) {
		slog.Info(ReasonUnreadMessage, "row", row, "projects", projects, "bound", record.MaxReasonProjects)
	}
}

// reasonProjects is the project of each issue key reason names (record.ReasonProjects), the
// projects the person rule (maySee) and the session rule (hideFromSession) join to a row's own,
// read for the row row names. A reason naming the keys of more than record.MaxReasonProjects
// projects is one stored before Create refused such a reason (record.CheckReasonProjects): it
// answers none, so its keys count as naming no project, which is how every reader saw the row when
// it was written, and a line naming the row says so (logReasonUnread).
func (m *Machine) reasonProjects(row, reason string) []string {
	projects := record.ReasonProjects(reason)
	if len(projects) > record.MaxReasonProjects {
		m.logReasonUnread(row, len(projects))
		return nil
	}
	return projects
}

// projectNotNamed is the refusal of a granted name the session may no longer have for its
// project's sake, which ends the grant for good (errProjectNotNamed).
func projectNotNamed(name string) error {
	return fmt.Errorf("%w: %w: %s belongs to a private project this session does not name", ErrGrantNotLive, errProjectNotNamed, name)
}

// projectDeny reports whether d is a Deny the project tag decided.
func projectDeny(d policy.Decision) bool {
	return d.Outcome == policy.Deny && (d.Reason == policy.ReasonProjectNotNamed || d.Reason == policy.ReasonProjectMissing)
}

// SecretProject is one requested secret's project, for the approval page: the secret, the key its
// dispatch-project tag names, and whether that project is private (or one Dispatch does not hold,
// which the page shows the same way).
type SecretProject struct {
	Name, Project string
	Private       bool
}

// secretProjects answers the project of each of names the live policy tags with one, in names'
// order, and when the oldest answer of Dispatch's the list rests on was asked; a name of no
// project is left out, and a set of names none of which is tagged asks Dispatch nothing. It
// decides nothing itself: a reader that decides on the list (maySeeRecord, hideFromSession) holds
// the list's age and its own reads' to atDecision's bound together.
func (m *Machine) secretProjects(ctx context.Context, names []string) ([]SecretProject, time.Time, error) {
	set := m.Policy.Get()
	tagged := taggedProjects(set, names)
	if len(tagged) == 0 {
		return nil, time.Time{}, nil
	}
	facts, err := m.collectProjectFacts(ctx, enrollmentRow{}, names, set)
	if err != nil {
		return nil, time.Time{}, err
	}
	var out []SecretProject
	for _, name := range names {
		if project := set.Secrets[name].Project; project != "" {
			out = append(out, SecretProject{Name: name, Project: project, Private: facts.states[project] != policy.ProjectPublic})
		}
	}
	return out, facts.asked, nil
}

// JudgeProject answers whether the model judge must leave the request of enrollmentID for name,
// signed with reason, to a person for a project's sake (judgeMayDecide): the secret's
// dispatch-project tag names a private project or a key Dispatch holds no project for, or the
// person rule (maySee) would hide the request from a reader who names no person, because the
// credential that asked names a private project (namedProjects) or an issue key its reason names
// is a private project's. The judge's own token names no person, so it may see no private
// project, and deciding such a request would grant in the name of an approver a private project's
// rule may hide it from. The decision checks the same rule again (applyDecisionAttempt).
//
// Its reads of Dispatch run under JudgeDispatch, whatever ctx the caller passed: the judge's own
// Dispatch phase when the judge asks. A request a broker with this check creates names no more
// projects than record.CheckReasonProjects allows, so the judge, which runs as the request is
// created, meets no reason past that bound.
//
// A broker with no project-access client answers false for an untagged secret, exactly as the
// merge base decides (R9): such a broker loads no tagged secret (project-unchecked) and cannot know
// any project is private, so referring a request only because its credential or its reason names
// a project would change a deployment with nothing private and protect nothing. A request whose
// secret carries no tag, whose credential names no project and whose reason names no issue key
// answers false and asks Dispatch nothing (judge.Requests).
func (m *Machine) JudgeProject(ctx context.Context, enrollmentID, name, reason string) (bool, error) {
	ctx, cancel := JudgeDispatch(ctx)
	defer cancel()
	set := m.Policy.Get()
	// The secret's projects, the enrollment and the person rule's reads are one decision, held to
	// atDecision's bound together: a state read first ages while the rest are read.
	may, err := atDecision(ctx, m.Projects, func(ctx context.Context, _ bool) (bool, time.Time, error) {
		facts, err := m.collectProjectFacts(ctx, enrollmentRow{}, []string{name}, set)
		if err != nil || m.Projects == nil {
			return true, time.Time{}, err
		}
		enr, err := m.enrollment(ctx, enrollmentID)
		if err != nil {
			return false, time.Time{}, err
		}
		may, asked, err := m.judgeMayDecide(ctx, "enrollment "+enrollmentID, facts.states, m.namedProjects(enr), reason)
		return may, projectaccess.Oldest(facts.asked, asked), err
	})
	if err != nil {
		return false, err
	}
	return !may, nil
}

// privateProjects is the project of each of secrets that belongs to a private project.
func privateProjects(secrets []SecretProject) []string {
	var projects []string
	for _, p := range secrets {
		if p.Private {
			projects = append(projects, p.Project)
		}
	}
	return projects
}

// JudgeDispatchBound is how long the model judge may wait on Dispatch at one point of a run: its
// reads before the ask together — the policy check's (JudgePolicy), the project check's
// (JudgeProject) and the issue read's two GETs, which package judge's Dispatch phase holds under
// one JudgeDispatch — and, at the decision, the reads its approval makes before its transaction
// (applyDecisionAttempt). The ask read the reason may cite has a bound of its own of the same
// length (judge's dispatchBound), on the run's context. A read it cuts short hands the request to
// its person.
const JudgeDispatchBound = 5 * time.Second

// JudgeRowReserve is the part of a judge run's time no read of Dispatch may spend: JudgeDispatch
// ends that long before its context does, so a read Dispatch is slow to answer fails while there
// is still time to write the run's request.judged row (judge.Runner.write).
const JudgeRowReserve = time.Second

// judgeDispatchKey marks a context JudgeDispatch made.
type judgeDispatchKey struct{}

// JudgeDispatch is the one bound every read of Dispatch on the model judge's path runs under: ctx,
// ended JudgeDispatchBound from now or JudgeRowReserve before ctx's own end, whichever comes first,
// and marked so a test can tell each read of a run made under it (UnderJudgeDispatch) from one that
// bypassed it. A context it already made is answered as it is, so a read inside the judge's phase
// shares the phase's bound rather than starting one of its own.
func JudgeDispatch(ctx context.Context) (context.Context, context.CancelFunc) {
	if UnderJudgeDispatch(ctx) {
		return ctx, func() {}
	}
	deadline := time.Now().Add(JudgeDispatchBound)
	if end, ok := ctx.Deadline(); ok && end.Add(-JudgeRowReserve).Before(deadline) {
		deadline = end.Add(-JudgeRowReserve)
	}
	bounded, cancel := context.WithDeadline(ctx, deadline)
	return context.WithValue(bounded, judgeDispatchKey{}, true), cancel
}

// UnderJudgeDispatch reports whether ctx is one JudgeDispatch made, or derived from one.
func UnderJudgeDispatch(ctx context.Context) bool {
	marked, _ := ctx.Value(judgeDispatchKey{}).(bool)
	return marked
}

// maySee is the person rule over a row's parts: candidate projects and the reason, the project of
// each of whose issue keys (reasonProjects) joins them. It is the one statement of whose request
// this is, for a person (mayDecide, maySeeRecord), for a session (hideFromSession) and for the
// model judge, which asks it with no person (judgeMayDecide). A word of a key's form that is no
// project ("SHA-256") joins them too, and Dispatch answers it as no project, which hides nothing.
// It sorts a list of its own: the caller's may be an enrollment's or an operator entry's, which
// nothing here owns.
//
// It answers beside a true when the oldest answer of Dispatch's it rests on was asked: every
// project shown, and every key Dispatch answered no project for. A false rests on one answer that
// hides the record, whatever its age, and is answered with no time. It decides nothing alone:
// each caller holds its age to atDecision's bound with the facts it read before it.
//
// A reason naming the issue keys of more than record.MaxReasonProjects projects is one stored
// before Create refused such a reason (record.CheckReasonProjects). Its keys are not read
// (reasonProjects; row is "record <id>", "grant <id>" or, for the judge's own check, "enrollment
// <id>"). The record's other projects are still asked, since its secrets' tags come of the live
// policy, and a secret it names may have been tagged with a private project since it was stored.
func (m *Machine) maySee(ctx context.Context, row, login string, projects []string, reason string) (bool, time.Time, error) {
	if m.Projects == nil {
		return true, time.Time{}, nil
	}
	asking := slices.Concat(projects, m.reasonProjects(row, reason))
	slices.Sort(asking)
	var asked time.Time
	for _, project := range slices.Compact(asking) {
		answer, err := m.Projects.Access(ctx, project, record.CanonicalLogin(login))
		var missing *projectaccess.ProjectNotFoundError
		switch {
		case errors.As(err, &missing):
			asked = projectaccess.Oldest(asked, missing.AskedAt)
		case errors.Is(err, projectaccess.ErrProjectNotFound):
		case err != nil:
			return false, time.Time{}, fmt.Errorf("%w: %v", ErrProjectAccessUnavailable, err)
		case !answer.Visible:
			return false, time.Time{}, nil
		default:
			asked = projectaccess.Oldest(asked, answer.AskedAt)
		}
	}
	return true, asked, nil
}

// judgeMayDecide is the model judge's project rule, the one its run checks before the model
// (JudgeProject) and its decision checks again before the transaction (decisionFacts). states is
// what Dispatch holds of each project the request's secrets are tagged with, and of those alone,
// and named the projects its credential names; the request is the judge's only when each tagged
// project is public and the person rule shows it to a reader naming no person, which Dispatch
// answers by visibility alone. The judge's own token names no person, so it may see no private
// project. It answers maySee's time beside its answer, and no time for a state that refers it.
func (m *Machine) judgeMayDecide(ctx context.Context, row string, states map[string]policy.ProjectState, named []string, reason string) (bool, time.Time, error) {
	for _, state := range states {
		if state != policy.ProjectPublic {
			return false, time.Time{}, nil
		}
	}
	return m.maySee(ctx, row, "", named, reason)
}

// namedProjects is the projects e's credential names: a person's session's enrollment list, the
// project a pod's operator entry maps its account to while that entry names the issuer the pod
// enrolled under, or the one a machine's fleet maps to (credentialProjects). It is the one source
// every reader uses — the release (projectFacts), the decision (mayDecide), the record a person
// reads (ReadRecord) and both approver lists — so the path that releases a secret and the path
// that shows a request cannot disagree on whose it is: a service's pod's enrollment row names no
// project of its own, and reading that column alone showed a private project's pod requests to
// everyone.
func (m *Machine) namedProjects(e enrollmentRow) []string {
	_, named := m.credentialProjects(e)
	return named
}

// credentialsProjects is the projects the credentials a request has had name: asked, the
// enrollment that asked, which the append-only record names, and current, the one the request
// hangs off now, which an adoption rewrites. Every reader of whose request this is reads both —
// the pending list, the record's page, the grant list and the decision (decisionFacts) — so
// a person either would hide the request from neither sees nor decides it. Each project appears
// once, in the order asked then current name them.
func (m *Machine) credentialsProjects(asked, current enrollmentRow) []string {
	var projects []string
	for _, p := range slices.Concat(m.namedProjects(asked), m.namedProjects(current)) {
		if !slices.Contains(projects, p) {
			projects = append(projects, p)
		}
	}
	return projects
}

// mayDecide is the approve path's person rule (maySee), read before its transaction: the record's
// tagged secrets under set, the projects the credentials it has had name (credentialsProjects),
// and its reason. It answers maySee's time beside its answer.
func (m *Machine) mayDecide(ctx context.Context, row, login string, facts projectFacts, sessionProjects []string, reason string) (bool, time.Time, error) {
	var projects []string
	for project, state := range facts.states {
		if state == policy.ProjectPrivate {
			projects = append(projects, project)
		}
	}
	return m.maySee(ctx, row, login, append(projects, sessionProjects...), reason)
}

// decisionFacts is what the approve path reads of Dispatch for recordID before its transaction:
// the state of each project the request's names are tagged with and the projects its credential
// may still name (projectFacts), which the transaction's evaluation reads, and whether who may
// decide the record at all. A person may when the person rule shows them the record (mayDecide),
// to approve it or to refuse it. The model judge's grant may when the request is still of no
// private project (judgeMayDecide, over the tagged projects' states alone, the ones its run's check
// read), which its run checked before the model, and which a project turning private during the
// run or an adoption by an enrollment naming a private project can have changed since; the judge's
// refusal does not come here (applyDecisionAttempt). Both rules read the projects of two
// credentials: the one the request hangs off now, which an adoption rewrites, and the one that
// asked, which the append-only record names and the pending lists and the approval page read, so a
// request either of them would hide from a reader is one that reader does not decide. For the
// judge every read is under JudgeDispatch, so a slow Dispatch leaves the run the time its row
// needs, and asked of Dispatch afresh (projectaccess.Fresh): the run's own check filled the cache
// well inside projectaccess.CacheTTL, and an answer read back from it could not show a change made
// since. The facts and the rule's own reads are one decision, held to atDecision's bound together.
// A refusal of the judge is record.ErrNotApprover. A person's, for a record they may not see, is
// pgx.ErrNoRows alone, the answer every read of the record gives them (HideUnseen) and an id no
// record has gives anyone: so the page that decides tells them "no such record"
// (approval.Page.decide) whatever order its checks are in, and adds no refusal code to the address
// it sends them to, which would say the record exists.
func (m *Machine) decisionFacts(ctx context.Context, recordID string, who decider, pre rowFacts, set *policy.Set) (projectFacts, error) {
	if who.isJudge() {
		bounded, cancel := JudgeDispatch(ctx)
		defer cancel()
		ctx = projectaccess.Fresh(bounded)
	}
	type decided struct {
		facts projectFacts
		may   bool
	}
	got, err := atDecision(ctx, m.Projects, func(ctx context.Context, _ bool) (decided, time.Time, error) {
		facts, err := m.collectProjectFacts(ctx, pre.holder, pre.names, set)
		if err != nil {
			return decided{}, time.Time{}, err
		}
		named := pre.sessionProjects
		var may bool
		var asked time.Time
		if who.isJudge() {
			tagged := make(map[string]policy.ProjectState)
			for _, project := range taggedProjects(set, pre.names) {
				tagged[project] = facts.states[project]
			}
			may, asked, err = m.judgeMayDecide(ctx, "record "+recordID, tagged, named, pre.reason)
		} else {
			may, asked, err = m.mayDecide(ctx, "record "+recordID, who.login, facts, named, pre.reason)
		}
		return decided{facts: facts, may: may}, projectaccess.Oldest(facts.asked, asked), err
	})
	switch {
	case err != nil:
		return projectFacts{}, err
	case got.may:
		return got.facts, nil
	case who.isJudge():
		return projectFacts{}, fmt.Errorf("%w: the request now belongs to a private project, which the model judge leaves to a person", record.ErrNotApprover)
	}
	return projectFacts{}, pgx.ErrNoRows
}

// maySeeRecord is the person rule over a record or a grant for login: every private project among
// the tagged secrets its names carry under the live policy, the projects its credentials name
// (sessionProjects), and the project of every Dispatch issue key its reason names. A person
// outside one of them may neither read the row nor decide it, since its names and its reason are
// that project's. A project Dispatch does not hold hides nothing, so a word of a key's form that is
// no project ("SHA-256") hides nothing and hides no key after it. A row touching no project asks
// Dispatch nothing, and a broker with no project-access client hides nothing, since it serves no
// tagged secret and enrolls no session naming a project. An error is ErrProjectAccessUnavailable:
// whether login may see the row is unknown, and a caller shows it to nobody until Dispatch answers.
// HideUnseen, hideUnseenGrant and both approver lists read it. The names' projects and the person
// rule's reads are one decision, held to atDecision's bound.
func (m *Machine) maySeeRecord(ctx context.Context, row, login string, names, sessionProjects []string, reason string) (bool, error) {
	return atDecision(ctx, m.Projects, func(ctx context.Context, _ bool) (bool, time.Time, error) {
		secrets, asked, err := m.secretProjects(ctx, names)
		if err != nil {
			return false, time.Time{}, err
		}
		may, seen, err := m.maySee(ctx, row, login, append(privateProjects(secrets), sessionProjects...), reason)
		return may, projectaccess.Oldest(asked, seen), err
	})
}
