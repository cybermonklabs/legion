// Package legionagents is the Legion agent registry the listener keeps in the legion_agents KV
// bucket (bus.LegionAgentBucket): one entry per Legion session the daemon ran, which outlives the
// session, so a finished issue's agents can still be named and asked. The daemon writes it through
// the listener's PUT routes; Dispatch reads it through the GET routes (cmd/listener/api.go).
//
// Key layout. session.<session_id> holds the entry, and two index keys hold the same entry JSON:
// issue.<ISSUE>.<session_id> and role.<role_token>.<session_id>, the role token being the claim
// token of the session's agent, which names the agent and not the role its step is on.
// project.<legion_project> is reserved for the daemon's own record (INFRA-204), and no read here
// reaches it: every read is a session key or an issue or role token prefix. A write claims the
// session key first, by compare-and-swap, and writes the index keys after it, so a session two
// writes race for is listed under the winner's issue alone. Nothing is ever deleted, so a closed
// issue's agents keep their entries and no read replays delete markers.
package legionagents

import (
	"cmp"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/legionclaim"
	"github.com/sjawhar/envoy/listenerwire"
)

// Entry is one Legion session as the registry stores it, which the listener's wire package
// declares (listenerwire.LegionAgentEntry). Its role is a role of an issue (legionclaim.IsRole: one
// of Legion's six or a role the deployment defines), the role of the step its agent runs as
// of the entry's latest write. Its role token is the claim token of that agent, which ends in the
// agent's name, the role of its first step. Readers ignore fields they do not know, so a later
// writer may add some.
type Entry = listenerwire.LegionAgentEntry

// Agent is an entry with what a read derives from the other sessions of its role token, ordered by
// started_at: the session that replaced it and when, both nil for the newest. Neither is stored.
// It is the listener's wire type (listenerwire.LegionAgent).
type Agent = listenerwire.LegionAgent

// Harnesses are the agent harnesses a Legion session runs under.
var Harnesses = []string{"omp", "claude"}

// MaxBatch is the most entries one batch write takes.
const MaxBatch = 256

// The longest each keyed field may be. They keep every key this package writes far inside the
// subject bound bus.KeyValue holds a key to, so a validated entry is never refused by NATS halfway
// through its writes: the longest key, role.<role_token>.<session_id>, stays under 280 bytes.
const (
	maxSessionIDBytes = 128
	maxProjectBytes   = 64
	maxIssueBytes     = 32
	// maxRoleBytes is the longest role or agent name the grammar takes (legionclaim.RolePattern).
	maxRoleBytes  = legionclaim.MaxRoleBytes
	maxTitleBytes = 1024
)

var (
	// sessionIDPattern is a session id that is one KV key token: nats.go's key alphabet without
	// the dot, which separates tokens, and the slash and equals sign, which no session id holds and
	// a slash would split the route's path.
	sessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	// projectTokenPattern is a Legion project token (claim.ProjectToken).
	projectTokenPattern = regexp.MustCompile(`^[a-z0-9]+$`)
	// issueKeyPattern is a Dispatch issue key (claim.IsIssueKey, DISPATCH_KEY_PATTERN).
	issueKeyPattern = regexp.MustCompile(`^[A-Z][A-Z0-9]*-[0-9]+$`)
)

// ErrInvalid is a session id, issue key, claim token or entry the registry refuses. Each refusal
// wraps it with the field and the rule it breaks.
var ErrInvalid = errors.New("invalid legion agent")

// ErrNotFound is a session the registry holds no entry for.
var ErrNotFound = errors.New("unknown legion agent")

// ErrTooMany is a batch of more than MaxBatch entries.
var ErrTooMany = fmt.Errorf("a batch takes at most %d legion agents", MaxBatch)

// ConflictError is a write that would move a recorded session to another issue or another claim:
// a session belongs to one claim of one issue for good, so no index key is ever orphaned. Its role
// is not held: an agent keeps its claim and its session across the roles of its steps, and a write
// of the session on another role's step keeps every key where it was. Every write claims the
// session key before it writes an index key (Store.write), so the refusal holds against a
// concurrent write and a write that ended partway alike.
type ConflictError struct {
	SessionID, Issue, RoleToken string
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("legion agent %s is recorded for %s as %s", e.SessionID, e.Issue, e.RoleToken)
}

// BatchError is the refusal of one entry of a batch, which refuses the whole batch.
type BatchError struct {
	Index int
	Err   error
}

func (e *BatchError) Error() string { return fmt.Sprintf("entries[%d]: %v", e.Index, e.Err) }

func (e *BatchError) Unwrap() error { return e.Err }

// PartialWriteError is a batch that failed after its checks: Written of its Total entries were
// written whole, in order, before Err, the failed entry's *BatchError. Written is 0 when the
// failure came before the first write.
type PartialWriteError struct {
	Written, Total int
	Err            error
}

func (e *PartialWriteError) Error() string {
	return fmt.Sprintf("wrote %d of %d legion agents, then %v", e.Written, e.Total, e.Err)
}

func (e *PartialWriteError) Unwrap() error { return e.Err }

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// ValidateSessionID refuses a session id that is not one key token of at most 128 bytes.
func ValidateSessionID(id string) error {
	if len(id) > maxSessionIDBytes {
		return invalid("session_id is %d bytes, past %d", len(id), maxSessionIDBytes)
	}
	if !sessionIDPattern.MatchString(id) {
		return invalid("session_id %q must be letters, digits, '-' and '_'", id)
	}
	return nil
}

// ValidateIssue refuses a key that is not a Dispatch issue key.
func ValidateIssue(issue string) error {
	if len(issue) > maxIssueBytes {
		return invalid("issue is %d bytes, past %d", len(issue), maxIssueBytes)
	}
	if !issueKeyPattern.MatchString(issue) {
		return invalid("issue %q is not a Dispatch issue key (PROJECT-123)", issue)
	}
	return nil
}

// ValidateRoleToken refuses a token that is not the claim token of an agent of an issue
// (legionclaim.CutClaimToken).
func ValidateRoleToken(token string) error {
	if limit := len("legion-") + maxProjectBytes + 1 + maxIssueBytes + 1 + maxRoleBytes; len(token) > limit {
		return invalid("role_token is %d bytes, past %d", len(token), limit)
	}
	if _, _, _, ok := legionclaim.CutClaimToken(token); !ok {
		return invalid("role_token %q is not legion-<legion_project>-<issue>-<agent>", token)
	}
	return nil
}

// Validate refuses an entry the registry does not store. Both writes, one entry and a batch, refuse
// through it, so they refuse the same entries.
func Validate(entry Entry) error {
	if err := ValidateSessionID(entry.SessionID); err != nil {
		return err
	}
	if len(entry.LegionProject) > maxProjectBytes {
		return invalid("legion_project is %d bytes, past %d", len(entry.LegionProject), maxProjectBytes)
	}
	if !projectTokenPattern.MatchString(entry.LegionProject) {
		return invalid("legion_project %q is not a Legion project token (lower-case letters and digits)", entry.LegionProject)
	}
	if err := ValidateIssue(entry.Issue); err != nil {
		return err
	}
	if !legionclaim.IsRole(entry.Role) {
		return invalid("role %q is not one of %s or a role of the grammar %s but controller", entry.Role, strings.Join(legionclaim.Roles, ", "), legionclaim.RolePattern)
	}
	if project, issue, _, ok := legionclaim.CutClaimToken(entry.RoleToken); !ok || project != entry.LegionProject || issue != entry.Issue {
		return invalid("role_token %q is not legion-<legion_project>-<issue>-<agent>, the claim token of an agent of %s under %s",
			entry.RoleToken, entry.Issue, entry.LegionProject)
	}
	if len(entry.Title) > maxTitleBytes {
		return invalid("title is %d bytes, past %d", len(entry.Title), maxTitleBytes)
	}
	if !slices.Contains(Harnesses, entry.Harness) {
		return invalid("harness %q is not one of %s", entry.Harness, strings.Join(Harnesses, ", "))
	}
	if !isUTCTimestamp(entry.StartedAt) {
		return invalid("started_at %q is not an RFC 3339 UTC time (2026-10-26T03:15:00Z)", entry.StartedAt)
	}
	return nil
}

func isUTCTimestamp(value string) bool {
	_, err := time.Parse(time.RFC3339, value)
	return err == nil && strings.HasSuffix(value, "Z")
}

// sameSession reports whether stored and next name the same Legion project, issue and claim
// token, the fields a session keeps for good. The role is not one of them: it is the role of the
// step the session's agent runs, which changes as the agent's steps do.
func sameSession(stored, next Entry) bool {
	return stored.LegionProject == next.LegionProject && stored.Issue == next.Issue &&
		stored.RoleToken == next.RoleToken
}

// startedAt is the entry's start, the order a role token's sessions replace each other in. A stored
// entry was validated, so it parses; one that does not sorts first.
func startedAt(e Entry) time.Time {
	at, _ := time.Parse(time.RFC3339, e.StartedAt)
	return at
}

// oldestFirst orders one role token's sessions by when they started, then by when they were
// recorded, then by id, so two reads order the same sessions the same way.
func oldestFirst(a, b Entry) int {
	return cmp.Or(startedAt(a).Compare(startedAt(b)), cmp.Compare(a.RecordedAt, b.RecordedAt), cmp.Compare(a.SessionID, b.SessionID))
}

// succession derives each session's replacement among entries, every one of one role token, one
// agent's claim: the next session to start replaced it, at that session's start. It returns them
// newest first, the first being the claim's current session.
func succession(entries []Entry) []Agent {
	slices.SortFunc(entries, oldestFirst)
	agents := make([]Agent, len(entries))
	for i, entry := range entries {
		agents[i] = Agent{LegionAgentEntry: entry}
		if i+1 < len(entries) {
			next := entries[i+1]
			agents[i].ReplacedBy, agents[i].ReplacedAt = &next.SessionID, &next.StartedAt
		}
	}
	slices.Reverse(agents)
	return agents
}

// issueOrder lists an issue's agents by role, the role of each entry's latest write, the six in
// the order of the default steps and then every other role by name, then by role token, then
// newest first, each role token's replacements derived among its own sessions.
func issueOrder(entries []Entry) []Agent {
	byToken := map[string][]Entry{}
	for _, entry := range entries {
		byToken[entry.RoleToken] = append(byToken[entry.RoleToken], entry)
	}
	agents := make([]Agent, 0, len(entries))
	for _, sessions := range byToken {
		agents = append(agents, succession(sessions)...)
	}
	// rank is a role's place in legionclaim.Roles, and every role outside the six comes after them.
	rank := func(role string) int {
		if i := slices.Index(legionclaim.Roles, role); i >= 0 {
			return i
		}
		return len(legionclaim.Roles)
	}
	slices.SortStableFunc(agents, func(a, b Agent) int {
		return cmp.Or(
			cmp.Compare(rank(a.Role), rank(b.Role)),
			cmp.Compare(a.Role, b.Role),
			cmp.Compare(a.RoleToken, b.RoleToken),
			-oldestFirst(a.LegionAgentEntry, b.LegionAgentEntry),
		)
	})
	return agents
}
