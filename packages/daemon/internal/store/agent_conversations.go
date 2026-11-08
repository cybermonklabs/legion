package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// ConversationSource is how the daemon learned of a conversation (agent_conversations.source).
type ConversationSource string

const (
	// ConversationRegistered is a conversation a claim's registration recorded.
	ConversationRegistered ConversationSource = "register"
	// ConversationScanned is a conversation the boot's backfill found by its title.
	ConversationScanned ConversationSource = "scan"
)

// AgentConversation is one conversation an agent of this daemon has had (agent_conversations):
// the session, the Legion project token, issue, role and role token it ran for, the harness it ran
// on, where its conversation lives (Location: the session file's path, Oh My Pi's session table's
// key under a database store, or the Claude Code transcript's path), when it started (a
// registration's time, or a scanned session header's), how the daemon learned of it, and when the
// row was last written.
type AgentConversation struct {
	Session    string
	Project    string
	Issue      string
	Role       claim.Role
	Token      claim.Token
	Harness    runtime.Backend
	Location   string
	StartedAt  time.Time
	Source     ConversationSource
	RecordedAt time.Time
}

const conversationColumns = `session, project, issue, role, token, harness, location, started_at, source, recorded_at`

// RegisterConversation records c as a registration's (its Source is ignored) and returns the row as
// written: a session recorded before by a scan becomes a registered one, with the registration's
// started_at, and one already registered keeps its first registration's started_at while
// everything else is rewritten, so a re-registration that moved the session file records where it
// is now.
func (s *Store) RegisterConversation(ctx context.Context, c AgentConversation) (AgentConversation, error) {
	rows, err := s.pool.Query(ctx, `insert into agent_conversations (`+conversationColumns+`)
		values ($1, $2, $3, $4, $5, $6, $7, $8, 'register', $9)
		on conflict (session) do update set project = excluded.project, issue = excluded.issue,
			role = excluded.role, token = excluded.token, harness = excluded.harness, location = excluded.location,
			started_at = case when agent_conversations.source = 'register' then agent_conversations.started_at else excluded.started_at end,
			source = 'register', recorded_at = excluded.recorded_at
		returning `+conversationColumns,
		c.Session, c.Project, c.Issue, string(c.Role), string(c.Token), string(c.Harness), c.Location, c.StartedAt, c.RecordedAt)
	if err != nil {
		return AgentConversation{}, fmt.Errorf("record the conversation %s of %s: %w", c.Session, c.Token, err)
	}
	written, err := scanConversations(rows)
	if err != nil {
		return AgentConversation{}, fmt.Errorf("record the conversation %s of %s: %w", c.Session, c.Token, err)
	}
	if len(written) != 1 {
		return AgentConversation{}, fmt.Errorf("record the conversation %s of %s: %d rows written, want 1", c.Session, c.Token, len(written))
	}
	return written[0], nil
}

// ScanConversation records c as one the backfill found (its Source is ignored), unless its session
// is recorded already: a scan never rewrites a row, a registration's least of all. recorded is
// whether it wrote one.
func (s *Store) ScanConversation(ctx context.Context, c AgentConversation) (recorded bool, err error) {
	tag, err := s.pool.Exec(ctx, `insert into agent_conversations (`+conversationColumns+`)
		values ($1, $2, $3, $4, $5, $6, $7, $8, 'scan', $9) on conflict (session) do nothing`,
		c.Session, c.Project, c.Issue, string(c.Role), string(c.Token), string(c.Harness), c.Location, c.StartedAt, c.RecordedAt)
	if err != nil {
		return false, fmt.Errorf("record the scanned conversation %s of %s: %w", c.Session, c.Token, err)
	}
	return tag.RowsAffected() == 1, nil
}

// AgentConversation is the recorded conversation of session, and false when there is none.
func (s *Store) AgentConversation(ctx context.Context, session string) (AgentConversation, bool, error) {
	rows, err := s.pool.Query(ctx, `select `+conversationColumns+` from agent_conversations where session = $1`, session)
	if err != nil {
		return AgentConversation{}, false, fmt.Errorf("read the conversation %s: %w", session, err)
	}
	found, err := scanConversations(rows)
	if err != nil {
		return AgentConversation{}, false, fmt.Errorf("read the conversation %s: %w", session, err)
	}
	if len(found) == 0 {
		return AgentConversation{}, false, nil
	}
	return found[0], true, nil
}

// RoleConversations is every recorded conversation of the role token, newest first, with every
// registered one before every scanned one: a conversation found by its title never outranks one a
// registration recorded as the role's newest. Equal times are ordered by session.
func (s *Store) RoleConversations(ctx context.Context, token claim.Token) ([]AgentConversation, error) {
	rows, err := s.pool.Query(ctx, `select `+conversationColumns+` from agent_conversations where token = $1
		order by source = 'register' desc, started_at desc, session`, string(token))
	if err != nil {
		return nil, fmt.Errorf("read the conversations of %s: %w", token, err)
	}
	found, err := scanConversations(rows)
	if err != nil {
		return nil, fmt.Errorf("read the conversations of %s: %w", token, err)
	}
	return found, nil
}

// AgentConversations is every recorded conversation of the Legion project token, oldest first,
// equal times ordered by session.
func (s *Store) AgentConversations(ctx context.Context, project string) ([]AgentConversation, error) {
	rows, err := s.pool.Query(ctx, `select `+conversationColumns+` from agent_conversations where project = $1
		order by started_at, session`, project)
	if err != nil {
		return nil, fmt.Errorf("read the conversations of project %s: %w", project, err)
	}
	found, err := scanConversations(rows)
	if err != nil {
		return nil, fmt.Errorf("read the conversations of project %s: %w", project, err)
	}
	return found, nil
}

// IssueConversations is every recorded conversation of one issue of the Legion project token,
// oldest first, equal times ordered by session: the rows whose registry entries are written
// again when that issue's standing changes (agenthistory.Mirror).
func (s *Store) IssueConversations(ctx context.Context, project, issue string) ([]AgentConversation, error) {
	rows, err := s.pool.Query(ctx, `select `+conversationColumns+` from agent_conversations
		where project = $1 and issue = $2 order by started_at, session`, project, issue)
	if err != nil {
		return nil, fmt.Errorf("read the conversations of %s: %w", issue, err)
	}
	found, err := scanConversations(rows)
	if err != nil {
		return nil, fmt.Errorf("read the conversations of %s: %w", issue, err)
	}
	return found, nil
}

// FinishedIssues says, of every issue the project's recorded conversations name, whether it has
// left the workflow: neither it nor its tree root is the workflow's to run any more
// (pods.InWorkflowNotLingering, the one gate every path that acts on a released pod reads), so no
// later worker of any of its roles is coming. An issue closed or done, one parked, and a member
// of a tree that was retired or still lingers past its close are each finished; one still running
// is not. An issue the record no longer holds is finished too: nothing will run it again.
//
// outOfWorkflow is record.OutOfWorkflow, passed in because this package sits below record, as
// every other reader of that rule here passes it (resumeStanding).
func (s *Store) FinishedIssues(ctx context.Context, project string, outOfWorkflow func(status string) bool) (map[string]bool, error) {
	rows, err := s.pool.Query(ctx, `select distinct c.issue, i.phase, i.status, i.linger_until is not null,
		root.phase, root.status, root.linger_until is not null
		from agent_conversations c
		left join issues i on i.key = c.issue
		left join issues root on root.key = i.tree
		where c.project = $1`, project)
	if err != nil {
		return nil, fmt.Errorf("read the standing of the issues of project %s: %w", project, err)
	}
	defer rows.Close()
	finished := map[string]bool{}
	for rows.Next() {
		var issue string
		var issuePhase, status, rootPhase, rootStatus *string
		var lingering, rootLingering *bool
		if err := rows.Scan(&issue, &issuePhase, &status, &lingering, &rootPhase, &rootStatus, &rootLingering); err != nil {
			return nil, fmt.Errorf("read the standing of the issues of project %s: %w", project, err)
		}
		if issuePhase == nil || rootPhase == nil {
			finished[issue] = true
			continue
		}
		finished[issue] = !pods.InWorkflowNotLingering(
			pods.Standing{Phase: phase.Phase(*issuePhase), Status: *status, Lingers: *lingering},
			pods.Standing{Phase: phase.Phase(*rootPhase), Status: *rootStatus, Lingers: *rootLingering},
			outOfWorkflow)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the standing of the issues of project %s: %w", project, err)
	}
	return finished, nil
}

func scanConversations(rows pgx.Rows) ([]AgentConversation, error) {
	defer rows.Close()
	var found []AgentConversation
	for rows.Next() {
		var c AgentConversation
		var role, token, harness, source string
		if err := rows.Scan(&c.Session, &c.Project, &c.Issue, &role, &token, &harness, &c.Location, &c.StartedAt, &source, &c.RecordedAt); err != nil {
			return nil, err
		}
		c.Role, c.Token, c.Harness, c.Source = claim.Role(role), claim.Token(token), runtime.Backend(harness), ConversationSource(source)
		found = append(found, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return found, nil
}
