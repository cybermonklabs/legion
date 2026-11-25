package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/text"
	"github.com/sjawhar/envoy/internal/legionclaim"
)

// An issue's role holders are the sessions that hold its Legion roles, each with the claim it holds
// its role by (issue_role_holders, migrations 20261116163813_issue_role_holders and
// 20261124213825_issue_role_holders_by_session). Two claims of one issue can hold one role (the
// Legion daemon's record.WorkerClaim), so a role can have more than one session, and each session
// holds one role.
// The Legion daemon is the one party that gives a session a role. It publishes the live set of each
// issue here: when a claim readies, when a claim ends and at each boot. The results route of a
// later release reads the set in the write transaction of a result (heldRole), so that the stored
// role of a result is the role that its session holds.
//
// A write must name the actor session legion-daemon:<project> (legionDaemonSessionOf), the session
// the daemon of the issue's project writes as. Before the airlock, Dispatch takes the session that
// a bearer caller names as the caller names it, so any caller whose Dispatch credential proves no
// session of its own can name that session: the deployment's shared token, and a person's own
// agent token whatever its scopes. The airlock closes that case, and this route does not.
//
// A claim ends when the daemon retires it, fails it or suspends it. The session of an ended claim
// leaves the set only when the daemon's next publication lands, and until then its row stays. The
// process of a failed or a suspended claim can still run during that window: the daemon lets go of
// a process whose stop the runtime refused. Such a process keeps the pane's bearer, and its session
// keeps its role, until the publication lands. The daemon queues the publication in the
// transaction of each claim write that changes the session holding a role, so an end whose claim
// write failed with an error that the daemon owes is published when the daemon makes that write
// again at the claim's next decision. An end whose claim write failed and is not owed leaves the
// daemon's memory ended and its store live, and the set follows the store (dispatch://INFRA-4099).
// A later decision of that claim, such as a second stop, makes that write again only while the
// claim still holds a pending task that the decision retires, such as the task of a turn that is
// over, because the retirement writes the claim with it; otherwise the session stays listed until
// another write of that claim lands, and the daemon's next start takes the claim up from its row as
// live. Such ends include a suspension, a stop or a failure whose claim write the database
// refused, and a stop whose retirement of its round's ask failed.
//
// Each write replaces the issue's whole set under the issue's owner lock (store.LockOwner), which
// a second write of the same issue waits for, so the stored set is always one whole set, and each
// write answers the set that it stored. Two writes of one issue at once store the set of the one
// that finishes last. The daemon sends one write of an issue at a time: its outbox holds one
// role_holders_publish row per issue, and a lane runs its issue's rows one at a time
// (packages/daemon's internal/daemon/outbox_lanes.go, the lanes' comment).
//
// The write appends no issue event and publishes nothing, whether the set changed or not, for the
// reasons a design-gate publication gives (issue_design_gates.go): the record is the daemon's own
// fact, and an event would feed the daemon's intake with facts that it caused. The write takes a
// closed issue, because the tester's production check runs after the close.

// issueRoleHolder is one holder of a publication: a role of the issue, the session that holds it,
// the claim it holds the role by and whether that claim works the role's step. Role and session
// are required: the route refuses a holder that leaves either out. The role is one of Legion's
// six, a role the deployment defines or a role the issue's plan defines, a name of the role grammar
// but the controller's (legionclaim.IsRole), so the shape spells the grammar and lists no roles.
// The claim is the Legion claim token of the issue the session runs (legionclaim.CutClaimToken),
// and works_step is true for the claim the daemon's record gives the role's step: two claims of
// one issue can hold one role, and the way back of 20261124213825 keeps that claim's session where
// it folds a role back to one session. A daemon before that release names neither, which reads as
// no claim and no step.
type issueRoleHolder struct {
	Role      string `json:"role" required:"true" pattern:"^[a-z][a-z0-9-]{0,31}$"`
	Session   string `json:"session" required:"true"`
	Claim     string `json:"claim,omitempty"`
	WorksStep bool   `json:"works_step,omitempty"`
}

// issueRoleHolderRecord is one stored holder, with the time Dispatch first stored it. A
// publication that names the same role, session and claim again keeps that time.
type issueRoleHolderRecord struct {
	Role       string    `json:"role" pattern:"^[a-z][a-z0-9-]{0,31}$"`
	Session    string    `json:"session"`
	Claim      string    `json:"claim"`
	WorksStep  bool      `json:"works_step"`
	RecordedAt time.Time `json:"recorded_at"`
}

// issueRoleHolders is the stored set both routes answer: the issue and its holders, Legion's six in
// the order of legionclaim.Roles, the order of the default steps, then every other role by name,
// `acceptance` and each role the deployment or the issue's plan defines. An issue where no session
// holds a role answers [].
type issueRoleHolders struct {
	Issue   string                  `json:"issue"`
	Holders []issueRoleHolderRecord `json:"holders"`
}

// issueRoleHoldersInput is the body PUT /api/v1/issues/{key}/role-holders takes. Holders is the
// issue's whole set. It is required, and a pointer, so a body that names no list is refused rather
// than read as the empty one: a malformed publication cannot clear the roles of an issue, while an
// explicit [] is the publication of an issue where no session holds a role.
type issueRoleHoldersInput struct {
	Holders *[]issueRoleHolder `json:"holders" required:"true"`
	Actor   *model.Actor       `json:"actor"`
}

// legionProjectOf is the project of the issue key: the key up to its first -, the daemon's own rule
// (packages/daemon's dispatch.DaemonSession). No route moves an issue to another project, so it is
// the issue row's project_key too.
func legionProjectOf(key string) string {
	project, _, _ := strings.Cut(key, "-")
	return project
}

// legionDaemonSessionOf is the session the Legion daemon writes the issue key's project as:
// legion-daemon:<legionProjectOf(key)>, the daemon's own rule (packages/daemon's
// dispatch.DaemonSession).
func legionDaemonSessionOf(key string) string {
	return legionDaemonSessionPrefix + legionProjectOf(key)
}

// heldRole is the role that session holds on issueKey, and "" when it holds none. An empty session
// holds no role.
func heldRole(ctx context.Context, q queryer, issueKey, session string) (string, error) {
	if session == "" {
		return "", nil
	}
	var role string
	err := q.QueryRow(ctx, `
		select role from issue_role_holders where issue_key = $1 and session = $2
	`, issueKey, session).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return role, err
}

// issueRoleHoldersStatement reads an issue and its stored set, $1 the issue's key and $2 Legion's
// roles in the order of the default steps, which is the order the set is answered in, every other
// role after them by name, and the sessions of one role by session. The left join answers one row
// with no holder for an issue where no session holds a role, and no row for a key that no issue
// carries, so one statement says both.
const issueRoleHoldersStatement = `
	select i.key, h.role, h.session, h.claim, h.works_step, h.recorded_at
	from issues i
	left join issue_role_holders h on h.issue_key = i.key
	where i.key = $1
	order by array_position($2::text[], h.role) nulls last, h.role, h.session
`

// readIssueRoleHolders answers the stored set of issueKey, and false when no issue carries the key.
func readIssueRoleHolders(ctx context.Context, q queryer, issueKey string) (issueRoleHolders, bool, error) {
	rows, err := q.Query(ctx, issueRoleHoldersStatement, issueKey, legionclaim.Roles)
	if err != nil {
		return issueRoleHolders{}, false, err
	}
	defer rows.Close()
	read := issueRoleHolders{Holders: []issueRoleHolderRecord{}}
	found := false
	for rows.Next() {
		var role, session, claim *string
		var worksStep *bool
		var recordedAt *time.Time
		if err := rows.Scan(&read.Issue, &role, &session, &claim, &worksStep, &recordedAt); err != nil {
			return issueRoleHolders{}, false, err
		}
		found = true
		if role != nil {
			read.Holders = append(read.Holders, issueRoleHolderRecord{Role: *role, Session: *session, Claim: *claim,
				WorksStep: *worksStep, RecordedAt: *recordedAt})
		}
	}
	if err := rows.Err(); err != nil {
		return issueRoleHolders{}, false, err
	}
	return read, found, nil
}

// writeRoleHoldersIssueNotFound answers 404 ISSUE_NOT_FOUND for a key that no issue carries. The
// daemon ends a publication on this code alone, as it ends a design-gate publication
// (issue_design_gates.go): a Dispatch that does not serve the route answers its router's 404 under
// the code NOT_FOUND, which the daemon retries until the release that has the route.
func writeRoleHoldersIssueNotFound(w http.ResponseWriter, key string) {
	writeError(w, "ISSUE_NOT_FOUND", http.StatusNotFound, fmt.Sprintf("no issue %s", key))
}

// maxRoleHolderSessionBytes is the longest session the route stores. A Legion session id is far
// shorter: the Legion agent registry holds one to 128 bytes (legionagents.ValidateSessionID). The
// bound keeps every row inside the unique index on (issue_key, session), whose B-tree entry holds
// at most 2704 bytes, so no body reaches the index with a session it cannot hold.
const maxRoleHolderSessionBytes = 256

// putIssueRoleHolders publishes the session of each role of an issue. The daemon sends the issue's
// whole set, so the transaction deletes each stored holder whose role, session and claim the body
// does not name together, then writes each holder by its session, which keeps one row a session:
// a holder whose role, session and claim did not change keeps its row, recorded_at included, and
// is written again only where whether its claim works the role's step changed, so a publication
// that changes nothing writes no row. A holder that changed is deleted before the write, so the
// write meets the session key alone. The body is checked whole before the transaction opens: each
// role is a role of an issue (legionclaim.IsRole: one of Legion's six, a role the deployment
// defines or a role the issue's plan defines), and may be named for several sessions, since two
// claims of one issue can hold one role; each session is a session id (text.IsSessionID) of at
// most maxRoleHolderSessionBytes, once; and each claim named is a claim token of this issue
// (legionclaim.CutClaimToken), once. A body that fails is 400 INVALID_ROLE_HOLDER and stores
// nothing, so no body gives a 500 from a check or an index of the table, whose check takes the
// same roles.
func (s *server) putIssueRoleHolders(w http.ResponseWriter, r *http.Request) {
	var input issueRoleHoldersInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	actor, ok := s.requireActor(w, r, input.Actor)
	if !ok {
		return
	}
	key := r.PathValue("key")
	if daemon := legionDaemonSessionOf(key); actor.Kind != "session" || actor.ID != daemon {
		writeError(w, "ROLE_HOLDER_FORBIDDEN", http.StatusForbidden, fmt.Sprintf(
			"only the Legion daemon of project %s names the sessions that hold the roles of %s. That daemon writes as session %s.",
			legionProjectOf(key), key, daemon))
		return
	}
	if input.Holders == nil {
		writeError(w, "INVALID_ROLE_HOLDER", http.StatusBadRequest, fmt.Sprintf(
			"holders is required: the session of each role of %s, or [] when no session holds a role.", key))
		return
	}
	holders := *input.Holders
	roles := make([]string, len(holders))
	sessions := make([]string, len(holders))
	claims := make([]string, len(holders))
	worksStep := make([]bool, len(holders))
	for index, holder := range holders {
		if !legionclaim.IsRole(holder.Role) {
			writeError(w, "INVALID_ROLE_HOLDER", http.StatusBadRequest, fmt.Sprintf(
				"%s is no role of an issue. A role is a name of %s other than controller: one of %s, a role the deployment defines, or a role the issue's plan defines.",
				holder.Role, legionclaim.RolePattern, strings.Join(legionclaim.Roles, ", ")))
			return
		}
		if holder.Claim != "" {
			if _, claimed, _, ok := legionclaim.CutClaimToken(holder.Claim); !ok || claimed != key {
				writeError(w, "INVALID_ROLE_HOLDER", http.StatusBadRequest, fmt.Sprintf(
					"%s is no claim of %s. A claim is the Legion claim token of an agent of the issue.", holder.Claim, key))
				return
			}
			if slices.Contains(claims[:index], holder.Claim) {
				writeError(w, "INVALID_ROLE_HOLDER", http.StatusBadRequest, fmt.Sprintf(
					"claim %s is named twice. A claim holds one role of an issue.", holder.Claim))
				return
			}
		}
		if strings.TrimSpace(holder.Session) == "" {
			writeError(w, "INVALID_ROLE_HOLDER", http.StatusBadRequest, fmt.Sprintf(
				"the session of %s is empty.", holder.Role))
			return
		}
		if !text.IsSessionID(holder.Session) || len(holder.Session) > maxRoleHolderSessionBytes {
			writeError(w, "INVALID_ROLE_HOLDER", http.StatusBadRequest, fmt.Sprintf(
				"the session of %s is not a session id. A session id is at most %d bytes, and it holds no whitespace, no control character and none of / ? # < > \" ' ` [ ] |.",
				holder.Role, maxRoleHolderSessionBytes))
			return
		}
		if slices.Contains(sessions[:index], holder.Session) {
			writeError(w, "INVALID_ROLE_HOLDER", http.StatusBadRequest, fmt.Sprintf(
				"session %s is named twice. A session holds one role of an issue.", holder.Session))
			return
		}
		roles[index] = holder.Role
		sessions[index] = holder.Session
		claims[index] = holder.Claim
		worksStep[index] = holder.WorksStep
	}

	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	// The issue row is read in the write's own transaction, under the issue's owner lock, which a
	// second write of the same issue waits for (store.LockOwner, the advisory lock every writer of
	// the issue takes), so the two replaces run one after the other: the second reads the rows that
	// the first committed, and each answers the set that it stored. The lock and the read are one
	// batch, and the read is a plain select, so while no earlier build is in the fleet the
	// transaction takes no row lock of an owner table and holds no transaction id until its first
	// write (INFRA-636; beside an earlier build LockOwnerAndRead also takes the issue's row, as that
	// build's writers do); it does not stop a reader.
	var issue string
	if err := store.LockOwnerAndRead(r.Context(), tx, store.IssueOwner(key), `
		select key from issues where key = $1
	`, []any{key}, &issue); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeRoleHoldersIssueNotFound(w, key)
			return
		}
		s.writeHandlerError(w, err)
		return
	}
	if _, err := tx.Exec(r.Context(), `
		delete from issue_role_holders
		where issue_key = $1
		  and (role, session, claim) not in (select * from unnest($2::text[], $3::text[], $4::text[]))
	`, issue, roles, sessions, claims); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// The delete above is the whole of the publication that names no holder, so the write runs
	// only when there is a holder to write.
	if len(holders) > 0 {
		if _, err := tx.Exec(r.Context(), `
			insert into issue_role_holders (issue_key, role, session, claim, works_step)
			select $1::text, published.role, published.session, published.claim, published.works_step
			from unnest($2::text[], $3::text[], $4::text[], $5::boolean[]) as published(role, session, claim, works_step)
			on conflict (issue_key, session) do update set works_step = excluded.works_step
			where issue_role_holders.works_step is distinct from excluded.works_step
		`, issue, roles, sessions, claims, worksStep); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	stored, _, err := readIssueRoleHolders(r.Context(), tx, issue)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, stored)
}

// getIssueRoleHolders answers the stored set of an issue: GET /api/v1/issues/{key}/role-holders. A
// closed issue reads as an open one does.
func (s *server) getIssueRoleHolders(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	key := r.PathValue("key")
	read, found, err := readIssueRoleHolders(r.Context(), s.deps.Store.Pool, key)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if !found {
		writeRoleHoldersIssueNotFound(w, key)
		return
	}
	WriteJSON(w, http.StatusOK, read)
}
