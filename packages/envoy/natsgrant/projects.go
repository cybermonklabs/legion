package natsgrant

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
)

// Scope is what one credential may reach on the bus: the projects its token lists and the account
// the token names. It holds no repository: a GitHub event is a message of no project, which every
// service is granted whole (WithinProjects). The auth service builds it for a service whose entry
// is held to projects (natscallout, from the broker's exchange list) and hands it to
// WithinProjects with that service's grant.
type Scope struct {
	// Projects are project keys (contracts.ProjectKeyPattern). None means the grant names no
	// project's subject at all, so a daemon's grant keeps no publish entry and WithinProjects
	// refuses it.
	Projects []string
	// Identity is the account the token names: its sub, or a Google token's email. It names the
	// reply prefix (InboxPrefix), which depends on the identity alone. So one identity must have
	// one scope: two connections of one identity with different project lists would subscribe the
	// same prefix, and the narrower would receive the wider one's pull deliveries. The auth
	// service keeps this by taking one BROKER_NATS_SERVICES entry per account, so each account
	// gets one project list.
	Identity string
}

// inboxPrefixBase begins every reply prefix InboxPrefix makes: nats.go's own prefix, "_INBOX.",
// with its dot written "_", so the token differs from nats.go's "_INBOX" and no prefix reaches
// another's.
var inboxPrefixBase = strings.TrimSuffix(nats.InboxPrefix, ".") + "_"

// InboxPrefix is the reply prefix of the client whose credential names identity: "_INBOX_" and
// identity with each byte outside [A-Za-z0-9-], including '_', escaped as '_' and two lowercase
// hex digits. It is one subject token and distinct identities have distinct prefixes. The client
// sets it with nats.CustomInboxPrefix and the auth service grants InboxPrefix(identity)+".>" in
// place of _INBOX.>; both ends call this function with the identity their credential names.
func InboxPrefix(identity string) string {
	return token(inboxPrefixBase, identity)
}

// token is prefix followed by s with each byte outside [A-Za-z0-9-], '_' included, written '_'
// and its two lowercase hex digits: '_' always opens an escape, so distinct values give distinct
// tokens, and the result holds no '.', wildcard, whitespace or control character.
func token(prefix, s string) string {
	const digits = "0123456789abcdef"
	var out strings.Builder
	out.Grow(len(prefix) + 3*len(s))
	out.WriteString(prefix)
	for i := range len(s) {
		c := s[i]
		if 'A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '-' {
			out.WriteByte(c)
		} else {
			out.WriteByte('_')
			out.WriteByte(digits[c>>4])
			out.WriteByte(digits[c&15])
		}
	}
	return out.String()
}

// daemonConsumerPrefix begins the name of every durable consumer a Legion daemon opens on the
// notification stream, legion-go-<project>-<kind> (packages/daemon/internal/intake/consume.go),
// and of its Dispatch probe, legion-go-<project>-dispatch-probe.
const daemonConsumerPrefix = "legion-go-"

// The entries of a grant WithinProjects rewrites, each the family's subjects under every project,
// and the API calls on the notification stream it drops or holds to the daemon's consumers.
const (
	issueLane        = contracts.DispatchIssueTopicPrefix + ">"
	roleExceptions   = bus.ExceptionTopicPrefix + contracts.RoleTopicPrefix + ">"
	legionLanes      = contracts.LegionNoticeTopicPrefix + ">"
	githubLanes      = contracts.GithubTopicPrefix + ">"
	everyTopic       = bus.NotificationTopicPrefix + ">"
	streamInfo       = jsAPI + "STREAM.INFO." + bus.Stream
	streamMsgGet     = jsAPI + "STREAM.MSG.GET." + bus.Stream
	consumerCreate   = jsAPI + "CONSUMER.CREATE." + bus.Stream + "."
	consumerInfo     = jsAPI + "CONSUMER.INFO." + bus.Stream + "."
	consumerNext     = jsAPI + "CONSUMER.MSG.NEXT." + bus.Stream + "."
	consumerAck      = jsAck + bus.Stream + "."
	durableCreate    = jsAPI + "CONSUMER.DURABLE.CREATE."
	streamMsgGetCall = jsAPI + "STREAM.MSG.GET."
	directGetCall    = jsAPI + "DIRECT.GET."
)

var projectKey = regexp.MustCompile(contracts.ProjectKeyPattern)

// WithinProjects is grant held to scope: each entry that reaches every project's subjects
// rewritten to the subjects of scope's projects, and refused, naming the entry, when it cannot be.
// The grant handed in is not changed, and a subscribe entry's queue group follows each subject
// that replaces it. Per entry:
//
//   - _INBOX.>, where every client's replies arrive, becomes InboxPrefix(scope.Identity)+".>";
//   - each family whose names carry the project as a token of their own becomes that family under
//     each project: an issue's events (notifications.dispatch.issue.<P>.>), the role lanes'
//     delivery exceptions (notifications.envoy.exceptions.notifications.role.<P>.>) and the Legion
//     daemon's lanes (notifications.legion.<p>.>, p the project key in lower case, the token
//     contracts.SubjectProject reads back as the key);
//   - every repository's GitHub events (notifications.github.>) are kept whole: a GitHub event is
//     a message of no project, which every service is granted, and a daemon drops in its own
//     process the events of a repository it does not work in;
//   - a consumer call on the notification stream that reaches every consumer (its consumer token a
//     wildcard) becomes that call on each consumer the Legion daemon opens under each project,
//     legion-go-<P>-<kind>, and nothing else: the create (its subject naming the consumer's one
//     filter, which nats-server holds the request's body to), the info, the pull
//     (CONSUMER.MSG.NEXT) and the acknowledgements. The kinds are dispatch (the project's issue
//     events), wake (its Legion wake lane), ready (the one wake lane every daemon shares,
//     bus.LegionWakeTopic) and github (every repository's events, notifications.github.>). The
//     GitHub consumer's create subject ends in its filter's wildcard, so it admits a create of
//     that consumer on any filter inside the family. Each project has one GitHub consumer, not
//     one per repository: a grant names a create subject exactly, with a wildcard only as a whole
//     token, so it could name a consumer per repository only from a list of the repositories,
//     which no scope holds, and a wildcard in place of the consumer's name would reach every
//     consumer of the stream, other projects' included.
//     The create alone names a filter: the info, the pull and the acknowledgements name the
//     consumer only, so the grant holds a daemon to its consumers by name, not by what they
//     filter. A durable of one of those names that already exists with a wider filter is read
//     whole through this grant: today's daemon opens legion-go-<P>-dispatch on every project's
//     issue events (notifications.dispatch.issue.>) and legion-go-<P>-wake on the shared wake
//     lane, so a held PUB daemon pulls PRIV's issue events through its own dispatch consumer
//     (TestWithinProjectsReadsAWideConsumerWhole). The grant holds a daemon to its projects only
//     once each of its consumers filters on the project's own lane, which the daemon that opens
//     its consumers per project does in place at boot, and only while it stays so: a user holding
//     today's daemon grant, or a listener, may widen it again.
//     The shared wake lane is a project's own only once the listener publishes its role and agent
//     wake reports on each project's lane (contracts.LegionWakeSubject) and keeps only
//     listener_ready there: until then it carries every project's role names and session ids, and
//     no daemon may take this grant before it opens these consumers;
//   - beside them, each project's Dispatch probe, legion-go-<P>-dispatch-probe, is granted its
//     create alone, on the project's issue events (notifications.dispatch.issue.<P>.>): the create
//     of a short-lived consumer whose answer counts the project's issue events stored after a
//     sequence (NumPending), from which the daemon reads that its Dispatch consumer is idle with
//     no stream-wide read. Its subject names the probe's one filter, which nats-server holds the
//     request's body to, and no name but the probe's: a wildcard there would admit another
//     project's probe. The probe is none of the durables above, so its info, pull,
//     acknowledgements and delete are not granted;
//   - the notification stream's info and stored-message get are dropped: the info lists every
//     stored subject with its count, the get reads any stored message, and a daemon held to its
//     projects reads its consumers' info and its Dispatch probe's create instead (today's daemon
//     opens the stream and reads stored messages, so it cannot run under this grant).
//
// Refused: a stored-message read of any other stream and a direct get of this one; a consumer
// create whose subject cannot name a filter; any call on a KV bucket; notifications.>; and every
// entry not named above, since an entry this function does not know is one it cannot vouch for.
//
// A grant that comes out with no publish or no subscribe entry is refused too, naming the
// direction: nats-server holds a user JWT to nothing in a direction whose allow list is empty
// (server/accounts.go, buildPermissionsFromJwt), and the auth service writes these lists into the
// JWT as they are. A daemon's grant held to no project keeps no publish entry, so it is refused
// rather than answered as a grant that may publish to every subject.
//
// The deny lists are kept as they are handed in, entry for entry: a deny only narrows, so one a
// grant carries holds within any scope, and dropping it would let through what it refuses.
func WithinProjects(grant Permissions, scope Scope) (Permissions, error) {
	held, err := newHeldScope(scope)
	if err != nil {
		return Permissions{}, err
	}
	publish, err := held.entries(grant.Publish)
	if err != nil {
		return Permissions{}, err
	}
	subscribe, err := held.entries(grant.Subscribe)
	if err != nil {
		return Permissions{}, err
	}
	narrowed := Permissions{
		Publish: publish, Subscribe: subscribe,
		PublishDeny: slices.Clone(grant.PublishDeny), SubscribeDeny: slices.Clone(grant.SubscribeDeny),
	}
	if direction, open := OpenDirection(narrowed); open {
		return Permissions{}, fmt.Errorf("natsgrant: grant held to scope keeps no %s entry, and a NATS user with no %s allow list may %s to every subject", direction, direction, direction)
	}
	return narrowed, nil
}

// heldScope is a Scope checked and laid out for the rewrite: the identity that names its reply
// prefix, its projects sorted with repeats taken out, the daemon's consumers under them, and each
// project's Dispatch probe, which is granted its create alone.
type heldScope struct {
	identity  string
	projects  []string
	consumers []daemonConsumer
	probes    []daemonConsumer
}

// daemonConsumer is one consumer the Legion daemon opens on the notification stream, a durable or
// its Dispatch probe: its name and the one filter its create names.
type daemonConsumer struct {
	name, filter string
}

func newHeldScope(scope Scope) (heldScope, error) {
	projects := slices.Clone(scope.Projects)
	for _, project := range projects {
		if !projectKey.MatchString(project) {
			return heldScope{}, fmt.Errorf("natsgrant: scope project %q is not a project key", project)
		}
	}
	slices.Sort(projects)
	projects = slices.Compact(projects)

	held := heldScope{identity: scope.Identity, projects: projects}
	for _, project := range projects {
		named := func(kind, filter string) daemonConsumer {
			return daemonConsumer{name: daemonConsumerPrefix + project + "-" + kind, filter: filter}
		}
		issues := contracts.DispatchIssueTopicPrefix + project + ".>"
		held.consumers = append(held.consumers,
			named("dispatch", issues),
			named("wake", contracts.LegionWakeSubject(strings.ToLower(project))),
			named("ready", bus.LegionWakeTopic),
			named("github", githubLanes),
		)
		held.probes = append(held.probes, named("dispatch-probe", issues))
	}
	return held, nil
}

// entries is each of entries held to the scope (WithinProjects), in order, with no entry twice.
func (h heldScope) entries(entries []string) ([]string, error) {
	var out []string
	seen := make(map[string]struct{})
	for _, entry := range entries {
		subject, queue, _ := strings.Cut(strings.TrimSpace(entry), " ")
		subjects, err := h.subjects(entry, subject)
		if err != nil {
			return nil, err
		}
		for _, held := range subjects {
			if queue != "" {
				held += " " + queue
			}
			if _, repeat := seen[held]; !repeat {
				seen[held] = struct{}{}
				out = append(out, held)
			}
		}
	}
	return out, nil
}

// subjects is subject, the subject of entry, held to the scope: the subjects that replace it,
// none when it is dropped, or the refusal naming entry.
func (h heldScope) subjects(entry, subject string) ([]string, error) {
	refuse := func(why string) ([]string, error) {
		return nil, fmt.Errorf("natsgrant: grant entry %q %s", entry, why)
	}
	switch {
	case isKVCall(subject):
		return refuse("is a KV bucket's call, which no project's grant holds")
	case subject == inboxes:
		if h.identity == "" {
			return refuse("needs the scope's identity to name its reply prefix")
		}
		return []string{InboxPrefix(h.identity) + ".>"}, nil
	case subject == streamInfo, subject == streamMsgGet:
		return nil, nil
	case strings.HasPrefix(subject, streamMsgGetCall), strings.HasPrefix(subject, directGetCall):
		return refuse("reads any stored message of a stream")
	case strings.HasPrefix(subject, durableCreate):
		return refuse("creates a consumer whose subject names no filter")
	case strings.HasPrefix(subject, consumerCreate):
		switch rest := strings.TrimPrefix(subject, consumerCreate); {
		case rest == ">" || rest == "*.>":
			create := func(c daemonConsumer) []string { return []string{consumerCreate + c.name + "." + c.filter} }
			creates := h.consumerCalls(create)
			for _, probe := range h.probes {
				creates = append(creates, create(probe)...)
			}
			return creates, nil
		case !strings.Contains(rest, "."):
			return refuse("creates a consumer whose subject names no filter")
		}
	case strings.HasPrefix(subject, consumerInfo), strings.HasPrefix(subject, consumerNext):
		call := consumerInfo
		if strings.HasPrefix(subject, consumerNext) {
			call = consumerNext
		}
		if rest := strings.TrimPrefix(subject, call); rest == ">" || rest == "*" {
			return h.consumerCalls(func(c daemonConsumer) []string { return []string{call + c.name} }), nil
		}
	case strings.HasPrefix(subject, consumerAck):
		if rest := strings.TrimPrefix(subject, consumerAck); rest == ">" || rest == "*.>" {
			return h.consumerCalls(func(c daemonConsumer) []string { return []string{consumerAck + c.name + ".>"} }), nil
		}
	case subject == issueLane:
		return h.perProject(func(project string) string { return contracts.DispatchIssueTopicPrefix + project + ".>" }), nil
	case subject == roleExceptions:
		return h.perProject(func(project string) string {
			return bus.ExceptionTopicPrefix + contracts.RoleTopicPrefix + project + ".>"
		}), nil
	case subject == legionLanes:
		return h.perProject(func(project string) string {
			return contracts.LegionNoticeTopicPrefix + strings.ToLower(project) + ".>"
		}), nil
	case subject == githubLanes:
		return []string{githubLanes}, nil
	case subject == everyTopic:
		return refuse("reaches every project's notifications")
	}
	return refuse("cannot be held to projects")
}

// isKVCall reports whether subject is a call on a KV bucket: a write or read of its keys, a
// JetStream API call on its stream (KV_<bucket>), or its watchers' flow control or
// acknowledgements.
func isKVCall(subject string) bool {
	if strings.HasPrefix(subject, kvSubjects) ||
		strings.HasPrefix(subject, jsFlow+kvStream) || strings.HasPrefix(subject, jsAck+kvStream) {
		return true
	}
	api, found := strings.CutPrefix(subject, jsAPI)
	if !found {
		return false
	}
	// The API names a call's stream as its third token (STREAM.INFO.<stream>) or, when its verb is
	// two tokens, its fourth (STREAM.MSG.GET.<stream>, CONSUMER.DURABLE.CREATE.<stream>.<consumer>),
	// so a call with any token naming a KV stream is a call on a bucket. No entry WithinProjects
	// holds has one.
	return slices.ContainsFunc(strings.Split(api, "."), func(token string) bool { return strings.HasPrefix(token, kvStream) })
}

// perProject is subject(project) for each project of the scope.
func (h heldScope) perProject(subject func(project string) string) []string {
	out := make([]string, len(h.projects))
	for i, project := range h.projects {
		out[i] = subject(project)
	}
	return out
}

// consumerCalls is calls(consumer) for each consumer the daemon opens under the scope.
func (h heldScope) consumerCalls(calls func(daemonConsumer) []string) []string {
	var out []string
	for _, consumer := range h.consumers {
		out = append(out, calls(consumer)...)
	}
	return out
}
