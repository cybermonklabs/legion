package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

type userIssueState struct {
	Pinned      bool     `json:"pinned"`
	LastReadSeq int      `json:"last_read_seq"`
	Dismissed   []string `json:"dismissed"`
	Seq         int64    `json:"seq"`
}

// userState is the body GET /api/v1/me/state answers: the caller's state for each issue it
// holds one for, by issue key.
type userState map[string]userIssueState

func (s *server) getUserState(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	rows, err := s.deps.Store.Pool.Query(r.Context(), `
		select issue_key, pinned, last_read_seq, dismissed, seq
		from user_issue_state where login = $1 order by issue_key
	`, actor.ID)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	defer rows.Close()
	state := userState{}
	for rows.Next() {
		var key string
		var value userIssueState
		var dismissed []byte
		if err := rows.Scan(&key, &value.Pinned, &value.LastReadSeq, &dismissed, &value.Seq); err != nil {
			s.writeHandlerError(w, r, err)
			return
		}
		if err := json.Unmarshal(dismissed, &value.Dismissed); err != nil {
			s.writeHandlerError(w, r, err)
			return
		}
		state[key] = value
	}
	if err := rows.Err(); err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, state)
}

// userIssueStateInput is the body of PUT /api/v1/me/issues/{key}/state.
type userIssueStateInput struct {
	Pinned      *bool     `json:"pinned"`
	LastReadSeq *int      `json:"last_read_seq"`
	Dismissed   *[]string `json:"dismissed"`
	Seq         *int64    `json:"seq"`
	// Actor names no writer of this row: the state is the calling person's, keyed on the
	// login actingPerson resolves, and the row has no column for a session. The session it
	// names is the acting agent's, which goes on the act of the event this write appends
	// (actSessionFromBody); a client that has always sent one is not refused for an unknown
	// key either (decodeJSON).
	Actor *model.Actor `json:"actor"`
}

// userStateStaleError is the 409 STATE_STALE body PUT /api/v1/me/issues/{key}/state answers a
// write whose seq is not newer than the stored row's.
type userStateStaleError struct {
	Code  string         `json:"code"`
	Error string         `json:"error"`
	State userIssueState `json:"state"`
}

// PUT /api/v1/me/issues/{key}/state writes the caller's own state for one issue: the pin, the
// read mark and the asks they dismissed, each field the body names, against the seq the body
// may give as its precondition. It runs in the two phases every write route runs in: a read
// phase holding no transaction id, which takes the project's owner lock and reads the row under
// it, and a write phase of the row's upsert and the event's append, back to back.
func (s *server) putUserState(w http.ResponseWriter, r *http.Request) {
	var input userIssueStateInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	actor, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	if actor, ok = actSessionFromBody(w, r, actor, input.Actor); !ok {
		return
	}
	if input.LastReadSeq != nil && *input.LastReadSeq < 0 {
		writeError(w, r, apicode.InvalidState, "last_read_seq must be non-negative")
		return
	}
	value, err := s.writeUserIssueState(r.Context(), actor, r.PathValue("key"), input)
	var stale *userStateStale
	if errors.As(err, &stale) {
		code, status := refusalPair(r, apicode.StateStale)
		WriteJSON(w, status, userStateStaleError{Code: code, Error: stale.Error(), State: stale.state})
		return
	}
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, value)
}

// userStateStale is writeUserIssueStateIn's refusal of a write whose seq is not newer than the
// stored row's: state is the row as it stands, which each version's 409 STATE_STALE answers.
type userStateStale struct {
	state userIssueState
}

func (stale *userStateStale) Error() string { return "user state was updated by another write" }

// writeUserIssueState is PUT /api/v1/me/issues/{key}/state's write in a transaction of its own
// (writeUserIssueStateIn), its event appended and published once it commits.
func (s *server) writeUserIssueState(ctx context.Context, actor model.Actor, key string, input userIssueStateInput) (userIssueState, error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return userIssueState{}, err
	}
	defer tx.Rollback(ctx)
	value, event, err := s.writeUserIssueStateIn(ctx, tx, actor, key, input)
	if err != nil {
		return userIssueState{}, err
	}
	if event, err = s.appendEvent(ctx, tx, event); err != nil {
		return userIssueState{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return userIssueState{}, err
	}
	s.publish(ctx, event)
	return value, nil
}

// writeUserIssueStateIn writes, in tx, the fields input names onto actor's state for the issue key
// names, each replacing the stored one, and answers the state as it now stands and the
// user_state.updated the write announces, for its caller to append: the work of PUT
// /api/v1/me/issues/{key}/state and of an issue mark's write under /api/v2 once each has read its
// body and its caller. A seq that is not above the stored one is a *userStateStale.
func (s *server) writeUserIssueStateIn(ctx context.Context, tx pgx.Tx, actor model.Actor, key string, input userIssueStateInput) (userIssueState, model.Event, error) {
	// The read phase holds no transaction id (INFRA-636). The issue's project is read first and
	// plainly: it names the owner whose lock the event's append would otherwise be the first to
	// take (projectOwner, events' lockEventOwner), and a route that waited for that lock holding
	// an id - which the row's `select ... for update` assigned before anything was judged - holds
	// back every reader's snapshot for as long as it waits. A key no issue row holds is the error
	// the insert's foreign key raised when that insert came first, and is answered as it was.
	var project string
	if err := tx.QueryRow(ctx, `select project_key from issues where key = $1`, key).Scan(&project); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return userIssueState{}, model.Event{}, fmt.Errorf("user state names %s, which no issue row holds", key)
		}
		return userIssueState{}, model.Event{}, err
	}
	// The owner's lock: advisory, so it assigns no transaction id (store.LockOwner), and the one
	// every writer of this project's events takes. It serialises two writes of one row, which the
	// row's `for update` serialised before: this route is the only writer of user_issue_state,
	// and two writes of one row take this same lock, since a row names one issue and an issue
	// names one project.
	//
	// The row as it stands under that lock, read in the statement after it
	// (store.LockOwnerAndRead), so it holds whatever the lock's previous holder committed. No row
	// is the row this write creates, read as the column defaults it is inserted with (migrations
	// 0001 and 0042): unpinned, nothing read, nothing dismissed, seq 0.
	value := userIssueState{Dismissed: []string{}}
	var encodedDismissed []byte
	err := store.LockOwnerAndRead(ctx, tx, store.ProjectOwner(project), `
		select pinned, last_read_seq, dismissed, seq
		from user_issue_state where login = $1 and issue_key = $2
	`, []any{actor.ID, key}, &value.Pinned, &value.LastReadSeq, &encodedDismissed, &value.Seq)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return userIssueState{}, model.Event{}, err
	default:
		if err := json.Unmarshal(encodedDismissed, &value.Dismissed); err != nil {
			return userIssueState{}, model.Event{}, err
		}
	}
	if input.Seq != nil && *input.Seq <= value.Seq {
		return userIssueState{}, model.Event{}, &userStateStale{state: value}
	}
	previous := value
	if input.Pinned != nil {
		value.Pinned = *input.Pinned
	}
	if input.LastReadSeq != nil {
		value.LastReadSeq = *input.LastReadSeq
	}
	if input.Dismissed != nil {
		value.Dismissed = *input.Dismissed
	}
	if input.Seq != nil {
		value.Seq = *input.Seq
	}
	encodedDismissed, err = encodeJSON(value.Dismissed)
	if err != nil {
		return userIssueState{}, model.Event{}, err
	}
	// changed names which of pinned, last_read_seq and dismissed this write moved from the row it
	// read (seq, the write's own precondition, is not one), so another tab refreshes its pinned
	// list for a pin and not for a read mark. Nothing it moved leaves it [].
	changed := []string{}
	if previous.Pinned != value.Pinned {
		changed = append(changed, "pinned")
	}
	if previous.LastReadSeq != value.LastReadSeq {
		changed = append(changed, "last_read_seq")
	}
	if !slices.Equal(previous.Dismissed, value.Dismissed) {
		changed = append(changed, "dismissed")
	}
	// The write phase: the row, and then the event its caller appends, back to back, and the
	// COMMIT behind them. One upsert does what the insert of the defaults and the update over
	// them did, each of which stood on its own side of the whole read phase.
	if _, err := tx.Exec(ctx, `
		insert into user_issue_state (login, issue_key, pinned, last_read_seq, dismissed, seq)
		values ($1, $2, $3, $4, $5, $6)
		on conflict (login, issue_key) do update set
			pinned = excluded.pinned, last_read_seq = excluded.last_read_seq,
			dismissed = excluded.dismissed, seq = excluded.seq
	`, actor.ID, key, value.Pinned, value.LastReadSeq, encodedDismissed, value.Seq); err != nil {
		return userIssueState{}, model.Event{}, err
	}
	return value, projectOwner(project).event(
		"user_state.updated", actor, map[string]any{"login": actor.ID, "state": value, "changed": changed},
	), nil
}

// userAgentState is one viewer's state for one agent's conversation. The Agents page hides every
// exchange whose newest message is at or before cleared_before (the viewer's Clear); read_through
// is how far the viewer has read; unread_replies counts the session's replies to messages this
// viewer sent that are newer than both and that the viewer has not read by id (read_replies),
// which is how the dashboard says an agent answered; pinned_at is when the viewer pinned the
// session, which is what puts it in the sidebar's Agents list and the order it is drawn in.
type userAgentState struct {
	ClearedBefore *string `json:"cleared_before,omitempty"`
	ReadThrough   *string `json:"read_through,omitempty"`
	UnreadReplies int     `json:"unread_replies"`
	PinnedAt      *string `json:"pinned_at,omitempty"`
}

// userAgentStates is the body GET /api/v1/me/agents/state answers: the caller's state for each
// agent conversation it holds one for, by session id.
type userAgentStates map[string]userAgentState

// agentStateCutoffSkew is how far ahead of the server clock a Clear or a read mark may land. The
// client stamps the cutoff with its own clock, and a browser a few seconds fast must not be
// refused.
const agentStateCutoffSkew = time.Minute

// userAgentStatesQuery reads a viewer's per-session state from three sources, so a session named
// by any one of them is in the answer: the shared unreadDirectRepliesCTE gives `direct_marks`,
// every session with a Clear (user_agent_state) or a read mark (user_agent_read), and
// `unread_direct_replies` counted per session, every session holding a reply the viewer has not
// read; `pinned` is every session the viewer pinned. A pinned session with neither a mark nor an
// unread reply is named by `pinned` alone, which is why the join is three ways and not two.
// Parameters are the fragment's own (unreadDirectRepliesArgs): the pin
// is keyed on the canonical login, $1, as the read mark is.
const userAgentStatesQuery = `
	with recursive` + unreadDirectRepliesCTE + `,
	unread as (
		select session_id, count(*)::int as unread from unread_direct_replies group by session_id
	),
	pinned as (
		select session_id, pinned_at from user_agent_pin
		where login = $1 and ($3::text is null or session_id = $3::text)
	)
	select coalesce(direct_marks.session_id, unread.session_id, pinned.session_id),
	       direct_marks.cleared_before, direct_marks.read_through,
	       coalesce(unread.unread, 0), pinned.pinned_at
	from direct_marks
	full join unread on unread.session_id = direct_marks.session_id
	full join pinned on pinned.session_id = coalesce(direct_marks.session_id, unread.session_id)
	order by 1
`

// loadUserAgentStates runs userAgentStatesQuery for login, narrowed to sessionID when it is
// not nil. The read mark, the pin and the viewer's own direct messages are matched on the
// canonical login, so one person is one viewer however their identity source spells them; the
// Clear (user_agent_state, migration 0033) is keyed on the raw actor id.
func (s *server) loadUserAgentStates(ctx context.Context, login string, sessionID *string) (userAgentStates, error) {
	rows, err := s.deps.Store.Pool.Query(ctx, userAgentStatesQuery, unreadDirectRepliesArgs(login, sessionID)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	states := userAgentStates{}
	for rows.Next() {
		var session string
		var clearedBefore, readThrough, pinnedAt *time.Time
		var state userAgentState
		if err := rows.Scan(&session, &clearedBefore, &readThrough, &state.UnreadReplies, &pinnedAt); err != nil {
			return nil, err
		}
		state.ClearedBefore = timestampPtr(clearedBefore)
		state.ReadThrough = timestampPtr(readThrough)
		state.PinnedAt = timestampPtr(pinnedAt)
		states[session] = state
	}
	return states, rows.Err()
}

func (s *server) getUserAgentState(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	states, err := s.loadUserAgentStates(r.Context(), actor.ID, nil)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, states)
}

// parseAgentStateCutoff reads one optional cutoff of a PUT: absent is nil, anything else must
// be an RFC3339 timestamp no further ahead of the server clock than agentStateCutoffSkew.
func parseAgentStateCutoff(name string, value *string) (*time.Time, error) {
	if value == nil {
		return nil, nil
	}
	parsed, err := time.Parse(time.RFC3339, *value)
	if err != nil {
		return nil, errorf(apicode.InvalidState, "%s must be an RFC3339 timestamp", name)
	}
	if parsed.After(time.Now().Add(agentStateCutoffSkew)) {
		return nil, errorf(apicode.InvalidState, "%s must not be in the future", name)
	}
	return &parsed, nil
}

// userAgentStateInput is the body of PUT /api/v1/me/agents/{session_id}/state.
type userAgentStateInput struct {
	ClearedBefore *string  `json:"cleared_before"`
	ReadThrough   *string  `json:"read_through"`
	ReadReplies   []string `json:"read_replies"`
	Pinned        *bool    `json:"pinned"`
}

// userAgentStateEvent announces that the viewer's state on one session moved. It is owned by the
// session, like its messages, so the outbox publishes it nowhere and it reaches only the viewer's
// other tabs and devices, which refresh their badge from it rather than waiting for a focus or
// the next message. Both writers of that state build it here: putUserAgentState below, and the
// Inbox's Mark all read (api/inbox_read.go).
func userAgentStateEvent(actor model.Actor, sessionID string) model.Event {
	return model.Event{
		Type:    "user_agent_state.updated",
		Actor:   actor,
		Payload: model.UserAgentStateEventPayload{Login: actor.ID, SessionID: sessionID},
	}
}

// putUserAgentState records a Clear (cleared_before, which replaces the previous one), a read
// mark (read_through, which only ever moves forward, so a tab that read less a moment ago cannot
// make a reply unread again), replies read one by one (read_replies, the session's own
// messages by id, which a view that shows only some of a session's replies writes so it marks
// nothing it did not show), and/or the pin that puts the session in the sidebar's Agents list
// (pinned), announces the write to the viewer's other tabs unless it was only replies already
// read by id or passed by the read mark (one the Clear hides is neither), and answers with the
// session's whole state.
func (s *server) putUserAgentState(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	var input userAgentStateInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	write, err := parseUserAgentStateInput(r.PathValue("session_id"), input)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	state, err := s.writeUserAgentState(r.Context(), actor, r.PathValue("session_id"), write)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, state)
}

// writeUserAgentState is PUT /api/v1/me/agents/{session_id}/state's write in a transaction of its
// own (writeUserAgentStateIn), committed with its event where the write is announced and rolled
// back where it wrote nothing, and the session's whole state read once it has ended. It writes
// several of the person's mark rows - the Clear, the read mark, the pin, the replies read by id
// and the prune - in an order of its own, so it takes the person's marks lock first
// (lockPersonMarks).
func (s *server) writeUserAgentState(ctx context.Context, actor model.Actor, sessionID string, write userAgentStateWrite) (userAgentState, error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return userAgentState{}, err
	}
	defer tx.Rollback(ctx)
	if err := lockPersonMarks(ctx, tx, model.CanonicalLogin(actor.ID), marksAlone); err != nil {
		return userAgentState{}, err
	}
	announce, err := s.writeUserAgentStateIn(ctx, tx, actor, sessionID, write)
	if err != nil {
		return userAgentState{}, err
	}
	// Replies read by id that all had a row or sat at or before the read mark change nothing, so
	// nothing is announced: a broadcast page sends its replies again on every visit while the
	// session has an unread reply elsewhere, and each event would refetch the badge in every tab
	// the viewer has open.
	if !announce {
		// Nothing was written; end the transaction before the state is read through the pool.
		if err := tx.Rollback(ctx); err != nil {
			return userAgentState{}, err
		}
	} else {
		event, err := s.appendEvent(ctx, tx, userAgentStateEvent(actor, sessionID))
		if err != nil {
			return userAgentState{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return userAgentState{}, err
		}
		s.publish(ctx, event)
	}
	states, err := s.loadUserAgentStates(ctx, actor.ID, &sessionID)
	if err != nil {
		return userAgentState{}, err
	}
	return states[sessionID], nil
}

// userAgentStateWrite is a write of one viewer's state on one conversation as
// parseUserAgentStateInput reads it off its body: the two cutoffs, the replies read by id in their
// canonical spelling and sorted, and the pin, each nil where the body leaves it out.
type userAgentStateWrite struct {
	clearedBefore, readThrough *time.Time
	readReplies                []string
	pinned                     *bool
}

// parseUserAgentStateInput reads input as a write onto the conversation of sessionID, refusing
// one that names nothing, a cutoff that is no RFC3339 time or lies ahead of the server, a reply id
// that is no message id, and a session id no route can name, each 400 INVALID_STATE: what PUT
// /api/v1/me/agents/{session_id}/state and an agent mark's PATCH under /api/v2 refuse before they
// write.
func parseUserAgentStateInput(sessionID string, input userAgentStateInput) (userAgentStateWrite, error) {
	if input.ClearedBefore == nil && input.ReadThrough == nil && len(input.ReadReplies) == 0 && input.Pinned == nil {
		return userAgentStateWrite{}, errorf(apicode.InvalidState,
			"cleared_before, read_through, read_replies or pinned is required")
	}
	clearedBefore, err := parseAgentStateCutoff("cleared_before", input.ClearedBefore)
	if err != nil {
		return userAgentStateWrite{}, err
	}
	readThrough, err := parseAgentStateCutoff("read_through", input.ReadThrough)
	if err != nil {
		return userAgentStateWrite{}, err
	}
	// An id is taken in any form uuid.Parse reads and sent to Postgres in the canonical one, which
	// Postgres takes; a urn:uuid: id, which Postgres refuses, would otherwise fail the write.
	readReplies := make([]string, 0, len(input.ReadReplies))
	for _, id := range input.ReadReplies {
		parsed, err := uuid.Parse(id)
		if err != nil {
			return userAgentStateWrite{}, errorf(apicode.InvalidState,
				"read_replies: %q is not a message id", id)
		}
		readReplies = append(readReplies, parsed.String())
	}
	// The insert below locks each new row in the order it is given, so two writes naming the same
	// new replies in opposite orders would wait on each other until Postgres killed one (40P01).
	// One order for every write, the ids' own, leaves no cycle to wait in.
	slices.Sort(readReplies)
	// The state is announced on an event the session owns, and only a session id a route can
	// name can own one, so any other is refused here rather than failing the write.
	if _, err := model.ParseRoute("session:" + sessionID); err != nil {
		return userAgentStateWrite{}, errorf(apicode.InvalidState,
			"session id %q is not one Dispatch can route", sessionID)
	}
	return userAgentStateWrite{clearedBefore: clearedBefore, readThrough: readThrough, readReplies: readReplies, pinned: input.Pinned}, nil
}

// writeUserAgentStateIn makes write, in tx, onto actor's state for the conversation of sessionID,
// and answers whether the write is announced (userAgentStateEvent), which is whether it changed
// anything: the work of PUT /api/v1/me/agents/{session_id}/state and of an agent mark's write under
// /api/v2 once each has read its body (parseUserAgentStateInput) and its caller. A write that is
// not announced has written nothing.
func (s *server) writeUserAgentStateIn(ctx context.Context, tx pgx.Tx, actor model.Actor, sessionID string, write userAgentStateWrite) (bool, error) {
	clearedBefore, readThrough, readReplies := write.clearedBefore, write.readThrough, write.readReplies
	// A Clear, a read mark or a pin is announced whenever it is given; replies read by id only
	// when one of them gets a row (below).
	announce := clearedBefore != nil || readThrough != nil || write.pinned != nil
	// A cutoff up to agentStateCutoffSkew ahead of the server clock is accepted, so a browser a few
	// seconds fast is not refused, but it is stored as no later than now: a reply that lands in
	// the gap is still after the viewer's Clear and read mark, so it shows and it counts.
	if clearedBefore != nil {
		// The Clear is keyed on the raw actor id on purpose: user_agent_state is migration 0033's
		// table and its rows predate the canonical convention 0048 and 0051 follow. This is not
		// the missing model.CanonicalLogin a grep for one would take it for.
		if _, err := tx.Exec(ctx, `
			insert into user_agent_state (login, session_id, cleared_before)
			values ($1, $2, least($3::timestamptz, now()))
			on conflict (login, session_id) do update set cleared_before = excluded.cleared_before
		`, actor.ID, sessionID, *clearedBefore); err != nil {
			return false, err
		}
	}
	if readThrough != nil {
		if _, err := tx.Exec(ctx, `
			insert into user_agent_read (login, session_id, read_through)
			values ($1, $2, least($3::timestamptz, now()))
			on conflict (login, session_id) do update set
				read_through = greatest(user_agent_read.read_through, excluded.read_through)
		`, model.CanonicalLogin(actor.ID), sessionID, *readThrough); err != nil {
			return false, err
		}
	}
	if write.pinned != nil {
		// The pin is keyed on the canonical login, like the read mark and unlike the Clear: it is
		// a table of this change's own (user_agent_pin), so it follows 0048's and 0051's
		// convention and no row of it predates them. It is stamped with the server's clock and
		// not the browser's, because nothing hides behind it: it is the order the sidebar draws
		// the person's pinned agents in, oldest first. Unpinning deletes the row, so the table
		// holds what is pinned now; either way the Clear is left exactly as it was, since
		// pinning a conversation is not reading it.
		pin := `delete from user_agent_pin where login = $1 and session_id = $2`
		if *write.pinned {
			pin = `
				insert into user_agent_pin (login, session_id, pinned_at)
				values ($1, $2, now())
				on conflict (login, session_id) do update set pinned_at = excluded.pinned_at
			`
		}
		if _, err := tx.Exec(ctx, pin, model.CanonicalLogin(actor.ID), sessionID); err != nil {
			return false, err
		}
	}
	if len(readReplies) > 0 {
		// The check proves each id is a message the path's session wrote, and no more: the unread
		// count reads a row only for that session's reply under a direct message this viewer sent
		// (unreadDirectRepliesCTE), so a row naming any other message the session wrote is inert.
		var foreign string
		err := tx.QueryRow(ctx, `
			select wanted.id::text from unnest($1::uuid[]) as wanted(id)
			where not exists (
				select 1 from messages
				where messages.id = wanted.id
				  and messages.author->>'kind' = 'session' and messages.author->>'id' = $2
			)
			limit 1
		`, readReplies, sessionID).Scan(&foreign)
		if err == nil {
			return false, errorf(apicode.InvalidState,
				"read_replies: %s is not a message session %s wrote", foreign, sessionID)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return false, err
		}
		// A reply the session's read mark has already passed is read without a row, and the
		// prune below would delete one again, so only the others are written; a revisit of
		// replies the mark covers writes nothing and announces nothing. The rows go in the ids'
		// sorted order (ordinality), so two writes lock new rows in one order.
		inserted, err := tx.Exec(ctx, `
			insert into user_agent_reply_read (login, session_id, reply_id)
			select $1, $2, wanted.id
			from unnest($3::uuid[]) with ordinality as wanted(id, position)
			where not exists (
				select 1 from messages
				join user_agent_read on user_agent_read.login = $1 and user_agent_read.session_id = $2
				where messages.id = wanted.id and messages.created_at <= user_agent_read.read_through
			)
			order by wanted.position
			on conflict do nothing
		`, model.CanonicalLogin(actor.ID), sessionID, readReplies)
		if err != nil {
			return false, err
		}
		announce = announce || inserted.RowsAffected() > 0
	}
	if readThrough != nil {
		// A reply at or before the session's read mark is read whatever user_agent_reply_read
		// holds, and the mark only moves forward, so the rows of the replies it now passes are dead
		// and are deleted here; without this a row would outlive every reason for it. The rows are
		// the ones read under this session (user_agent_reply_read_session serves the lookup), all
		// of them its own replies, and a session answers only what was delivered to it, so its
		// replies sit under roots targeted at it, where this mark is the one the unread count
		// reads. A Clear plays no part: it can move back.
		//
		// The dead rows are locked in one order before the delete reaches them, for the reason
		// written on pruneAgentReplies below, which this route and the Inbox's mark-all share
		// (markInboxReadThreads names the lock order every writer here takes).
		if _, err := tx.Exec(ctx, pruneAgentReplies,
			model.CanonicalLogin(actor.ID), []string{sessionID},
		); err != nil {
			return false, err
		}
	}
	return announce, nil
}

// personMarksLock is how a transaction holds a person's marks lock (lockPersonMarks).
type personMarksLock int

const (
	// marksAlone holds it alone: a write whose mark rows are taken in an order of its own.
	marksAlone personMarksLock = iota
	// marksBesideTheInbox holds it beside the other writes that take it so: the Inbox's mark-all
	// and its dismissal, which take a person's mark rows in the one order markInboxReadThreads
	// writes down, and so are already ordered against each other by it.
	marksBesideTheInbox
)

// personMarksLockNamespace is the first key of a person's marks lock ("MARK"), its second the
// hashtext of the person's canonical login (lockPersonMarks), as an owner lock's are
// (store.OwnerLockNamespace and the owner's key). It is the two-key form, which Postgres keeps
// apart from the one-key locks - a document's room (docs' lockDocumentRoom) among them - and a
// first key no other two-key lock of the server takes, so a person's mark writes wait on no lock
// but another mark write of that person's. The lists of the server's two-key namespaces name it
// beside the others (store.OwnerLockNamespace, docs.copiedAsksLockNamespace and lockProjectCopies,
// duty.leaseNamespace, testHookLockNamespace), so the next namespace is chosen apart from it.
const personMarksLockNamespace int32 = 0x4D41524B

// lockPersonMarks takes, in tx, the advisory lock of the marks of the person login names, the
// canonical login, as the first lock of every transaction that writes more than one of one
// person's mark rows: the Inbox's mark-all (markInboxReadThreads) and its dismissal
// (dismissInboxThreads) take it beside each other (marksBesideTheInbox); a conversation's state
// (writeUserAgentState) and every named write of /api/v2/me/marks (writeMarksIn) take it alone
// (marksAlone). Every such row is keyed on that person's login, and the orders differ: the
// mark-all and the dismissal take a person's threads, replies and prune across several sessions
// in the Inbox's lock order, which orders the two against each other and against themselves
// (TestInboxReadRacingAnotherMarkAll, TestDismissRacingAnotherDismissAndAMarkAll); a
// conversation's state takes one session's Clear, read mark and pin before its replies and its
// prune; a write of several marks takes each item's rows in item order. A write that holds the
// lock alone therefore runs before or after every other of the same person's, waiting here
// holding nothing, so no two of one person's mark writes can each hold a row the other waits on;
// two people's never share such a row. It is held to the commit, as every transaction-scoped
// advisory lock is.
func lockPersonMarks(ctx context.Context, tx pgx.Tx, login string, mode personMarksLock) error {
	var err error
	if mode == marksBesideTheInbox {
		_, err = tx.Exec(ctx, `select pg_advisory_xact_lock_shared($1, hashtext($2::text))`, personMarksLockNamespace, login)
	} else {
		_, err = tx.Exec(ctx, `select pg_advisory_xact_lock($1, hashtext($2::text))`, personMarksLockNamespace, login)
	}
	if err != nil {
		return fmt.Errorf("take the person's marks lock: %w", err)
	}
	return nil
}

// pruneAgentReplies is the one prune of user_agent_reply_read, shared by the two writers that
// make rows dead: this route, which moves one session's read mark, and the Inbox's mark-all,
// which writes replies by id across several sessions at once. A reply at or before its session's
// read mark is read whatever user_agent_reply_read holds, so its row is dead.
//
// The locked subquery takes exactly those rows, ascending by reply id, and the delete removes the
// rows it locked and no others: a delete left to itself locks rows in whatever order its plan
// finds them, and the plans differ with the session list's size, so two writers that crossed here
// would each hold a row the other waits on. One statement rather than a lock and a delete of its
// own, because two statements read two snapshots, and a row that died between them would be
// deleted without ever being locked in that order (markInboxReadThreads states the whole lock
// order this file's transactions take).
const pruneAgentReplies = `
	with locked as materialized (
		select user_agent_reply_read.reply_id
		from user_agent_reply_read, messages, user_agent_read
		where user_agent_reply_read.login = $1 and user_agent_reply_read.session_id = any($2::text[])
		  and user_agent_read.login = $1 and user_agent_read.session_id = user_agent_reply_read.session_id
		  and messages.id = user_agent_reply_read.reply_id
		  and messages.created_at <= user_agent_read.read_through
		order by user_agent_reply_read.reply_id
		for no key update of user_agent_reply_read
	)
	delete from user_agent_reply_read
	using locked
	where user_agent_reply_read.login = $1 and user_agent_reply_read.reply_id = locked.reply_id`
