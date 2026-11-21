package api

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// The census of references (the rule that a reader sees only the objects of the projects it sees:
// references_seen.go has it). A
// column names another row's object when a foreign key holds it to issues(key), artifacts(id),
// asks(id), comments(id), messages(id) or projects(key), or when it is named for a session, a
// person or a route (referenceColumnNames). schemaReferenceColumns reads every such column from
// the migrated schema; referenceDispositions says, for each one, how a read answers it, and the
// census fails on a column the schema holds that it does not name, and on an entry the schema does
// not hold. A column whose disposition is filteredReference can name an object its reader does not
// see, and TestNoAnswerNamesAnObjectItsReaderCannotSee holds every read to leaving such a reference
// out: its seed writes one into every such column, and its walk scans every answer for it.

// referenceColumnNames are the names a column of no foreign key is a reference by: a session by id
// or by a `session:<id>` target, a person by login, a route, and a document or an issue a column
// stores the id or key of without a key on it.
var referenceColumnNames = regexp.MustCompile(`^(session_id|session|sessions|resolved_session_id|target|artifact_id|issue|issue_key|project|project_key|assignee|asked_of|default_assignee|route|claimed_by)$`)

// The dispositions of a reference column.
const (
	// ownColumn is the row's own owner - its issue, document, project or conversation - or its own
	// key, which the row's project clause already binds: a reader that reads the row sees it.
	ownColumn = "own"
	// sameOwnerColumn is another row that a write rule holds to the row's own owner, so a reader that
	// sees the row sees it too; the reason names the rule.
	sameOwnerColumn = "same owner"
	// callerColumn is a row of the caller's own, read only by its own person.
	callerColumn = "caller's own"
	// internalColumn is a column no read answers: a queue, a backfill's cursor, a projection's
	// bookkeeping.
	internalColumn = "no read answers it"
	// deploymentColumn is a deployment row's, read only by an open viewer (readclass.Deployment).
	deploymentColumn = "deployment row"
	// filteredReference can name an object its reader does not see, and every read that answers
	// it leaves such an object out: the reason names the reads and the filter.
	filteredReference = "reference"
)

// referenceDisposition is how reads answer one reference column.
type referenceDisposition struct{ kind, how string }

// referenceDispositions is every reference column of the schema, by table.column.
var referenceDispositions = map[string]referenceDisposition{
	"architecture_snapshots.project_key":        {ownColumn, "the snapshot's project"},
	"architecture_sources.project_key":          {ownColumn, "the source's project"},
	"artifact_reviews.artifact_id":              {ownColumn, "the review's document"},
	"artifact_reviews.ask_id":                   {sameOwnerColumn, "the approval request of the same document (docs/approval_review.go)"},
	"artifact_versions.artifact_id":             {ownColumn, "the version's document"},
	"artifacts.issue_key":                       {ownColumn, "the document's issue"},
	"artifacts.project_key":                     {ownColumn, "the document's project"},
	"artifacts.session_id":                      {ownColumn, "the conversation a file belongs to (access.DocumentClause's parties)"},
	"ask_block_refs.ask_id":                     {ownColumn, "the ask whose part holds the link"},
	"ask_block_state.ask_id":                    {internalColumn, "the ask's part index state"},
	"ask_block_texts.ask_id":                    {ownColumn, "the ask whose part it is"},
	"ask_blocks.ask_id":                         {ownColumn, "the ask whose part it is"},
	"ask_followers.ask_id":                      {ownColumn, "the followed ask"},
	"ask_followers.session_id":                  {filteredReference, "any session may follow any ask: leaveOutHiddenFollowers in GET /asks/{id}/followers, GET /asks/{id} and every ask thread (loadAskThreads); the follow events by eventReadableSQL's session subjects"},
	"asks.artifact_id":                          {ownColumn, "the ask's document"},
	"asks.block_artifact_id":                    {sameOwnerColumn, "the document holding the ask's block, of the ask's own owner (docs/ask_blocks.go)"},
	"asks.handed_back_reply_id":                 {sameOwnerColumn, "a reply in the ask's own thread (reviews.go)"},
	"asks.issue_key":                            {ownColumn, "the ask's issue"},
	"asks.asked_of":                             {filteredReference, "a person the writer sees: leaveOutHiddenAskPersons in every ask read (queueAskDetails, attachAskReadFields); payloads by leaveOutHiddenReferences"},
	"block_refs.artifact_id":                    {ownColumn, "the document whose block holds the link"},
	"check_result_retractions.session":          {ownColumn, "the retraction's own writer"},
	"check_results.check_artifact_id":           {sameOwnerColumn, "the Verification document of the recording issue or an ancestor (results.go)"},
	"check_results.check_issue_key":             {sameOwnerColumn, "the recording issue or an ancestor, which shares its project (results.go, issue_parent.go)"},
	"check_results.issue_key":                   {ownColumn, "the result's issue"},
	"check_results.session":                     {ownColumn, "the result's own writer"},
	"comment_block_refs.comment_id":             {ownColumn, "the comment whose block holds the link"},
	"comment_block_state.comment_id":            {internalColumn, "the comment's block index state"},
	"comment_block_texts.comment_id":            {ownColumn, "the comment whose block it is"},
	"comment_blocks.comment_id":                 {ownColumn, "the comment whose block it is"},
	"comment_deliveries.comment_id":             {ownColumn, "the delivered comment"},
	"comment_deliveries.reply_id":               {sameOwnerColumn, "the reply in the comment's own thread (comment_create.go)"},
	"comment_deliveries.session_id":             {filteredReference, "the session a mention reached: leaveOutHiddenCommentSessions in every comment read; the delivery events by eventReadableSQL's session subjects"},
	"comment_deliveries.target":                 {filteredReference, "a `session:<id>` or `role:<name>` target: leaveOutHiddenCommentSessions"},
	"comment_fallback_copies.session_id":        {internalColumn, "the outbox's copy of a mention it could not deliver (outbox/lookups.go)"},
	"comment_fallback_copies.target":            {internalColumn, "the outbox's copy of a mention it could not deliver (outbox/lookups.go)"},
	"comment_mentions.comment_id":               {ownColumn, "the mentioning comment"},
	"comment_mentions.resolved_session_id":      {filteredReference, "the session a mention resolved to: leaveOutHiddenCommentSessions"},
	"comment_mentions.target":                   {filteredReference, "a `session:<id>` or `role:<name>` target: leaveOutHiddenCommentSessions"},
	"comment_watermarks.artifact_id":            {internalColumn, "a comment list's validator counter"},
	"comment_watermarks.issue_key":              {internalColumn, "a comment list's validator counter"},
	"comments.answers":                          {sameOwnerColumn, "a comment of the same owner (comment_create.go)"},
	"comments.artifact_id":                      {ownColumn, "the comment's document"},
	"comments.ask_id":                           {sameOwnerColumn, "an ask of the same owner (comment_create.go)"},
	"comments.issue_key":                        {ownColumn, "the comment's issue"},
	"comments.reply_to":                         {sameOwnerColumn, "a comment of the same owner (loadOwnedComment)"},
	"component_depends.project_key":             {ownColumn, "the component's project"},
	"component_facts.project_key":               {ownColumn, "the component's project"},
	"components.project_key":                    {ownColumn, "the component's project"},
	"decision_keeps.artifact_id":                {ownColumn, "the kept decision's document"},
	"delivery_pull_requests.issue_key":          {filteredReference, "the issue a pull request is credited to: the Delivery timeline and measures show a pull request only to a reader who sees that issue, read beside each issue's facts under ProjectClause (deliveryIssueFacts), and one that names no issue to an open viewer alone (readDeliveryRows)"},
	"delivery_pull_requests.sessions":           {deploymentColumn, "a pull request's own sessions, which the Delivery timeline and measures (class project) name to whoever is shown that pull request: a reader who sees its issue, or an open viewer for one that names no issue (readDeliveryRows)"},
	"delivery_session_group_members.session_id": {deploymentColumn, "the sessions of a Delivery session group, which the timeline and measures (class project) answer only in a group whose every session passes SessionClause for the reader (groupsShown)"},
	"design_gates.artifact_id":                  {filteredReference, "a document id the write stores unread: issueDesignGatesStatement leaves out a gate naming a document its reader does not see"},
	"design_gates.issue":                        {ownColumn, "the gate's issue"},
	"doc_ask_block_authors.artifact_id":         {internalColumn, "settlement bookkeeping"},
	"doc_checkpoints.artifact_id":               {internalColumn, "the document's CRDT checkpoint"},
	"doc_closed_edit_sources.artifact_id":       {internalColumn, "settlement bookkeeping"},
	"doc_compaction_owed.artifact_id":           {internalColumn, "a compaction queue"},
	"doc_conditional_edit_queue.artifact_id":    {internalColumn, "an edit queue"},
	"doc_pending_authors.artifact_id":           {internalColumn, "settlement bookkeeping"},
	"doc_settlements_pending.artifact_id":       {internalColumn, "a settlement queue"},
	"doc_snapshots.artifact_id":                 {internalColumn, "the document's CRDT snapshot"},
	"doc_updates.artifact_id":                   {internalColumn, "the document's CRDT updates"},
	"document_block_texts.artifact_id":          {ownColumn, "the document whose block it is"},
	"document_block_versions.artifact_id":       {internalColumn, "the block index's version marks"},
	"document_blocks.artifact_id":               {ownColumn, "the document whose block it is"},
	"document_edit_notices.artifact_id":         {internalColumn, "the edit notice queue"},
	"document_indexed_versions.artifact_id":     {internalColumn, "the index's version marks"},
	"document_search.artifact_id":               {ownColumn, "the searched document (searchProjectVisible)"},
	"document_search_backfill.artifact_id":      {internalColumn, "a backfill's cursor"},
	"embeddings.project_key":                    {ownColumn, "the embedded item's project"},
	"events.artifact_id":                        {ownColumn, "the event's document"},
	"events.issue_key":                          {ownColumn, "the event's issue"},
	"events.project_key":                        {ownColumn, "the event's project"},
	"going_requests.project":                    {ownColumn, "the project a run was asked of, read by its asker alone (readGoingRequest, asked_unseen)"},
	"graph_nodes.project_key":                   {ownColumn, "the node's project (refs.VisibilityMarker)"},
	"handoffs.issue_key":                        {ownColumn, "the handoff's issue"},
	"issue_component_members.issue_key":         {ownColumn, "the issue whose components they are"},
	"issue_component_members.project_key":       {ownColumn, "the issue's project"},
	"issue_components.issue_key":                {ownColumn, "the issue whose components they are"},
	"issue_external_links.issue_key":            {ownColumn, "the linking issue (EXTERNAL_LINK_TAKEN names it only to who sees it)"},
	"issue_links.issue_key":                     {ownColumn, "the waiting issue"},
	"issue_links.target_key":                    {filteredReference, "the issue waited on, of any project: issueSummaryJoins and leaveOutHiddenIssueReferences in every issue answer; payloads by leaveOutHiddenReferences"},
	"issue_role_holders.issue_key":              {ownColumn, "the issue whose roles the sessions hold (readIssueRoleHolders binds its project clause)"},
	"issue_role_holders.session":                {sameOwnerColumn, "a session of the issue's own project: only that project's Legion daemon writes the set (putIssueRoleHolders), and its sessions act in that project"},
	"legion_current.issue_key":                  {ownColumn, "an issue of the row's project, which the sampler records of its project alone"},
	"legion_current.project":                    {ownColumn, "the board's project (legionProject reads it under the project clause)"},
	"legion_observations.issue_key":             {ownColumn, "an issue of the row's project, which the sampler records of its project alone"},
	"legion_observations.project":               {ownColumn, "the board's project (legionProject reads it under the project clause)"},
	"legion_reads.project":                      {ownColumn, "the board's project (legionProject reads it under the project clause)"},
	"legion_settings.project":                   {ownColumn, "the daemon setting's project (legionProject reads it under the project clause)"},
	"issues.assignee":                           {filteredReference, "a person the writer sees: leaveOutHiddenIssueReferences, leaveOutHiddenSummaryReferences; payloads by leaveOutHiddenReferences"},
	"issues.claimed_by":                         {filteredReference, "a session or a person holding the claim: leaveOutHiddenIssueReferences, leaveOutHiddenSummaryReferences, leaveOutHiddenPriorityReferences, claimHolderHidden; the claim events by eventReadableSQL's claim rule, payloads by leaveOutHiddenReferences"},
	"issues.parent_key":                         {sameOwnerColumn, "a parent must be in the same project (validateIssueParent)"},
	"issues.project_key":                        {ownColumn, "the issue's project"},
	"issues.route":                              {filteredReference, "a `session:<id>` or `role:<name>` route: leaveOutHiddenIssueReferences, leaveOutHiddenSummaryReferences, leaveOutHiddenReaches (a role of a project the reader does not see, by leaveOutRoute; the reach of one whose holder it may not see, by leaveOutHiddenRouteHolders); payloads by leaveOutHiddenReferences"},
	"message_block_refs.message_id":             {ownColumn, "the message whose block holds the link"},
	"message_block_state.message_id":            {internalColumn, "the message's block index state"},
	"message_blocks.message_id":                 {ownColumn, "the message whose block it is"},
	"message_deliveries.message_id":             {ownColumn, "the delivered message"},
	"message_deliveries.reply_id":               {sameOwnerColumn, "the reply in the message's own thread"},
	"message_deliveries.session_id":             {filteredReference, "the session a message reached: leaveOutHiddenMessageSessions; the delivery events by eventReadableSQL's session subjects"},
	"messages.in_reply_to":                      {sameOwnerColumn, "a message of the same issue or conversation (messages.go)"},
	"messages.issue_key":                        {ownColumn, "the message's issue"},
	"messages.target":                           {filteredReference, "a `session:<id>` or `role:<name>` target, on an issue any session: leaveOutHiddenMessageSessions; on no issue a party of the conversation (MessageClause)"},
	"near_copy_checks.issue_key":                {ownColumn, "the issue its create wrote, of the row's project, whose candidates are issues of that project too (duplicateGate)"},
	"near_copy_checks.project_key":              {ownColumn, "the create's project (nearCopyChecksPageQuery and getNearCopyCheck bind its project clause)"},
	"project_access.project_key":                {ownColumn, "the access list's project"},
	"projects.default_assignee":                 {filteredReference, "a person the writer sees, read as an ask's waiting_on_person: leaveOutHiddenAskPersons"},
	"pull_request_heads.issue_key":              {internalColumn, "the attribution pass's record, answered through delivery_pull_requests"},
	"reference_excerpt_backoff.artifact_id":     {internalColumn, "an excerpt queue"},
	"related_checks.project_key":                {ownColumn, "the check's project"},
	"related_checks.source_artifact_id":         {ownColumn, "the checked write's document"},
	"related_checks.source_ask_id":              {ownColumn, "the checked write's ask"},
	"related_checks.source_issue_key":           {ownColumn, "the checked write's issue"},
	"repo_projects.project":                     {ownColumn, "the mapping's project"},
	"route_roster.sessions":                     {internalColumn, "the listener's stored session list, which every read filters as its own sessions"},
	"search_term_queue.artifact_id":             {internalColumn, "a search index queue"},
	"search_term_queue.issue_key":               {internalColumn, "a search index queue"},
	"search_term_stats.project_key":             {internalColumn, "a project's search statistics"},
	"search_term_totals.project_key":            {internalColumn, "a project's search statistics"},
	"session_projects.project_key":              {internalColumn, "what SessionClause reads"},
	"session_projects.session_id":               {internalColumn, "what SessionClause reads"},
	"session_owners.session_id":                 {internalColumn, "what SessionContentClause reads"},
	"user_agent_pin.session_id":                 {callerColumn, "the caller's own pin"},
	"user_agent_read.session_id":                {callerColumn, "the caller's own read mark"},
	"user_agent_reply_read.session_id":          {callerColumn, "the caller's own read mark"},
	"user_agent_state.session_id":               {callerColumn, "the caller's own state"},
	"user_ask_snooze.ask_id":                    {callerColumn, "the caller's own snooze"},
	"user_document_reads.artifact_id":           {callerColumn, "the caller's own read mark"},
	"user_issue_state.issue_key":                {callerColumn, "the caller's own state, read under ProjectClause"},
	"user_threads.artifact_id":                  {callerColumn, "the caller's own thread state"},
	"user_threads.issue_key":                    {callerColumn, "the caller's own thread state"},
	"user_threads.project_key":                  {callerColumn, "the caller's own thread state"},
	"write_suggestions.source_artifact_id":      {ownColumn, "the write the suggestion was made to"},
	"write_suggestions.source_ask_id":           {ownColumn, "the write the suggestion was made to"},
	"write_suggestions.source_issue_key":        {ownColumn, "the write the suggestion was made to"},
}

// schemaReferenceColumns is every reference column of the migrated schema, as table.column: each
// column a foreign key holds to one of the objects a read names by id, and each column named for a
// session, a person or a route (referenceColumnNames).
func schemaReferenceColumns(t *testing.T, fixture fragmentCensusFixture) []string {
	t.Helper()
	rows, err := fixture.database.Pool.Query(context.Background(), `
		select distinct t || '.' || c from (
			select conrelid::regclass::text as t, a.attname as c
			from pg_constraint k
			join lateral unnest(k.conkey) with ordinality as child (number, place) on true
			join pg_attribute a on a.attrelid = k.conrelid and a.attnum = child.number
			join lateral unnest(k.confkey) with ordinality as parent (number, place) on parent.place = child.place
			join pg_attribute b on b.attrelid = k.confrelid and b.attnum = parent.number
			where k.contype = 'f' and k.connamespace = 'public'::regnamespace
				and (k.confrelid::regclass::text, b.attname) in
					(('issues', 'key'), ('artifacts', 'id'), ('asks', 'id'), ('comments', 'id'), ('messages', 'id'), ('projects', 'key'))
			union all
			select table_name, column_name from information_schema.columns
			where table_schema = 'public' and column_name ~ $1
		) referencing
		order by 1`, referenceColumnNames.String())
	if err != nil {
		t.Fatalf("read the schema's reference columns: %v", err)
	}
	columns, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the schema's reference columns: %v", err)
	}
	if len(columns) == 0 {
		t.Fatal("the schema holds no reference column, so the census reads nothing")
	}
	return columns
}

// crossReferences are the persons the cross seed (seedCrossReferences) names from each project's
// rows, beside fragmentCensusFixture's own objects.
type crossReferences struct {
	// publicPerson is a person no list of PUB's names: bob, PRIV's restricted member. Every walker
	// that sees PUB is open and reads every person, so he is hidden from none of them.
	publicPerson string
	// privatePerson is a person whom a reader that sees PRIV alone does not see: dana.
	privatePerson string
}

// seedCrossReferences writes, as carol, who sees both projects, a reference from each project's
// objects to the other's in every column filteredReference names and in every event payload those
// writes append: each issue waits on an issue of the other project and is routed to a session that
// acted in the other alone; each issue's new ask is followed by that session; each issue's comment
// mentions that session and cites the other's issue and document; each issue messages that
// session; and each issue's design gate names the other's document. PUB's issue is assigned to bob,
// whom its new ask then waits on; PRIV's issue is assigned to dana, PRIV's default assignee, whom a
// reader that sees PRIV alone does not see, and its ask, which waits on dana as the assignee (an
// ask asked of its issue's assignee stores no asked_of), is handed to alice, whom that reader does
// not see either. PUB's pull request is credited to PRIV's issue, as a mapping moved between the two
// would leave it.
func seedCrossReferences(t *testing.T, fixture fragmentCensusFixture) crossReferences {
	t.Helper()
	seedPeople(t, fixture.database, hiddenPerson)
	carol := func(method, path string, body any) {
		t.Helper()
		if response := fixture.request(t, callerCarol, method, path, body); response.Code < 200 || response.Code > 299 {
			t.Fatalf("carol: %s %s = %d %s", method, path, response.Code, response.Body.String())
		}
	}
	cross := crossReferences{publicPerson: seedBob, privatePerson: hiddenPerson}
	carol(http.MethodPut, "/api/v1/projects/"+seedPrivate+"/default-assignee", map[string]any{"assignee": cross.privatePerson})
	// session-priv, which acted in PRIV, holds the claim on PUB's issue, so that issue's reads and
	// the payloads of its later writes name a holder the walkers that do not see PRIV may not see.
	fixture.asSession(t, sessionOfPrivate, seedPublic, http.MethodPost, "/api/v1/issues/"+fixture.public.issue+"/claim", map[string]any{}, http.StatusOK)
	for _, pair := range []struct {
		from, to         censusObjects
		session          string
		assignee, handed string
	}{
		{fixture.public, fixture.private, sessionOfPrivate, cross.publicPerson, ""},
		{fixture.private, fixture.public, sessionOfPublic, cross.privatePerson, seedAlice},
	} {
		issue := "/api/v1/issues/" + pair.from.issue
		// The issue waits on an issue of the other project of its own, so the two waits close no
		// ring (assertNoWaitPath).
		waitedOn := decodeBody[struct {
			Key string `json:"key"`
		}](t, fixture.mustRequest(t, callerCarol, http.MethodPost, "/api/v1/issues",
			map[string]string{"project": pair.to.project, "title": "Waited on by " + pair.from.project}, http.StatusCreated)).Key
		carol(http.MethodPatch, issue, map[string]any{
			"blocked_by": []string{waitedOn}, "assignee": pair.assignee, "route": "session:" + pair.session,
		})
		// An open ask of its own: the census seed's ask of PUB is answered, and only an open ask is
		// handed on.
		ask := createdID(t, fixture.request(t, callerCarol, http.MethodPost, issue+"/asks",
			map[string]any{"question": "Who takes the other project's half?"}))
		carol(http.MethodPut, "/api/v1/asks/"+ask+"/followers/"+pair.session, map[string]any{})
		if pair.handed != "" {
			carol(http.MethodPut, "/api/v1/asks/"+ask+"/asked-of", map[string]any{"asked_of": pair.handed})
		}
		carol(http.MethodPost, issue+"/comments", map[string]any{
			"body":     "Waits on dispatch://" + pair.to.issue + " and dispatch://" + pair.to.issue + "/artifact/" + pair.to.issueDocumentSlug + ".",
			"mentions": []map[string]string{{"target": "session:" + pair.session}},
		})
		carol(http.MethodPost, issue+"/messages", map[string]any{
			"body": "A note for the other project's session.", "target": "session:" + pair.session, "delivery": "btw",
		})
		carol(http.MethodPut, issue+"/design-gates", map[string]any{
			"gates": []map[string]string{{"step": "spec", "artifact_id": pair.to.document}},
		})
	}
	// The attribution pass writes a pull request's issue (delivery.attribution.go); no route does.
	if _, err := fixture.database.Pool.Exec(context.Background(), `update delivery_pull_requests set issue_key = $1 where repo = $2`,
		fixture.private.issue, "acme/"+strings.ToLower(seedPublic)+"-app"); err != nil {
		t.Fatalf("credit PUB's pull request to %s: %v", fixture.private.issue, err)
	}
	awaitBackgroundWrites(t, fixture.server)
	return cross
}

// hiddenObjects is what one walker of the census does not see: identifiers it must find in no
// answer, each with what it is.
type hiddenObjects map[string]string

// hiddenObjectsOf is every object of project's rows - its key, its issues, its documents, the asks,
// comments and messages of its issues - with sessions and persons beside them, each named by what
// it is.
func hiddenObjectsOf(t *testing.T, fixture fragmentCensusFixture, project string, sessions, persons []string) hiddenObjects {
	t.Helper()
	hidden := hiddenObjects{project: "project " + project}
	rows, err := fixture.database.Pool.Query(context.Background(), `
		select 'issue', key from issues where project_key = $1
		union all select 'document', id::text from artifacts where project_key = $1
			or issue_key in (select key from issues where project_key = $1)
		union all select 'ask', id::text from asks where issue_key in (select key from issues where project_key = $1)
			or artifact_id in (select id from artifacts where project_key = $1)
		union all select 'comment', id::text from comments where issue_key in (select key from issues where project_key = $1)
		union all select 'message', id::text from messages where issue_key in (select key from issues where project_key = $1)
		union all select 'handoff', id::text from handoffs where issue_key in (select key from issues where project_key = $1)`, project)
	if err != nil {
		t.Fatalf("read %s's objects: %v", project, err)
	}
	var kind, id string
	if _, err := pgx.ForEachRow(rows, []any{&kind, &id}, func() error {
		hidden[id] = kind + " " + id + " of " + project
		return nil
	}); err != nil {
		t.Fatalf("read %s's objects: %v", project, err)
	}
	for _, session := range sessions {
		hidden[session] = "session " + session
	}
	for _, person := range persons {
		hidden[person] = "person " + person
	}
	return hidden
}

// proseKeys are the keys whose values are text a person or a session wrote, which may spell an
// identifier as words without referring to it: a reference is a field of its own, never prose.
var proseKeys = map[string]bool{
	"body": true, "question": true, "title": true, "text": true, "excerpt": true, "excerpt_text": true,
	"markdown": true, "content": true, "summary": true, "words": true, "name": true, "label": true,
	"description": true, "reason": true, "note": true, "snippet": true, "preview": true, "reply_body": true,
	"ask_question": true, "error": true, "headline": true,
}

// identifierToken matches one identifier-shaped run inside a value with no whitespace: a reference
// ("dispatch://PRIV-1/artifact/runbook-md#b-3"), a topic, a block id.
var identifierToken = regexp.MustCompile(`[A-Za-z0-9_@.\-]+`)

// hiddenIn names each hidden object value names: the whole value, or one identifier-shaped token of
// a value that holds no whitespace.
func (h hiddenObjects) hiddenIn(value string) []string {
	if what, held := h[value]; held {
		return []string{what}
	}
	if target := routeSession(value); target != "" {
		if what, held := h[target]; held {
			return []string{what}
		}
	}
	if strings.ContainsAny(value, " \t\n") {
		return nil
	}
	var named []string
	for _, token := range identifierToken.FindAllString(value, -1) {
		if what, held := h[token]; held && !slices.Contains(named, what) {
			named = append(named, what)
		}
	}
	return named
}

// leak is one place an answer names a hidden object.
type leak struct{ path, value, object string }

// leaksOf walks a decoded JSON answer and answers every string value naming a hidden object, by
// its path.
func (h hiddenObjects) leaksOf(value any, path string) []leak {
	var found []leak
	switch value := value.(type) {
	case map[string]any:
		for _, key := range slices.Sorted(maps.Keys(value)) {
			if proseKeys[key] {
				continue
			}
			if what, held := h[key]; held {
				found = append(found, leak{path + "." + key, "(a key)", what})
			}
			found = append(found, h.leaksOf(value[key], path+"."+key)...)
		}
	case []any:
		for index, element := range value {
			found = append(found, h.leaksOf(element, fmt.Sprintf("%s[%d]", path, index))...)
		}
	case string:
		for _, what := range h.hiddenIn(value) {
			found = append(found, leak{path, value, what})
		}
	}
	return found
}

// TestNoAnswerNamesAnObjectItsReaderCannotSee: no read answers a reader a reference to an object it
// does not see, wherever the reference sits - a column, a listing, a count's subject, a stored
// event's payload (the issue's events and the event log) - and the census of references
// (referenceDispositions) holds every reference column the schema holds.
//
// The census seed (seedFragmentCensus) is joined by seedCrossReferences, which writes a reference
// from each project into the other in every column filteredReference names; the test reads back,
// from the stored rows, that each such column holds one. Four readers then walk every GET row of
// the route table whose read classes include project, graph, people or agents, filled with the
// objects of the project each sees, and every answer is scanned for an object hidden from that
// reader: alice, an open viewer who sees PUB and not PRIV's session, which has acted in PRIV
// (access.SessionClause); alice's token that lists PUB, a full member's and so an open viewer that
// sees what alice sees; bob, a restricted member who sees PRIV alone; and bob's token that lists
// PRIV, which sees what bob sees. A found
// one fails naming the route, the JSON path of the answer, and what the object is. The stored
// payloads are read too: every leaf of a payload a reader may read (eventReadableSQL) that names an
// object hidden from that reader must sit under a key leaveOutHiddenReferences leaves out
// (payloadReferenceKeys), and the seed must have written one under each key that names an object of
// another project.
func TestNoAnswerNamesAnObjectItsReaderCannotSee(t *testing.T) {
	census := &fragmentCensus{}
	fixture := seedFragmentCensus(t, census)
	mintListingToken(t, fixture.privateSeed, callerAlicePubToken, seedAlice, []string{seedPublic})
	cross := seedCrossReferences(t, fixture)

	// The census against the schema: every reference column has a disposition, every disposition
	// a column, and every column that can name an object of another project holds one in the seed.
	columns := schemaReferenceColumns(t, fixture)
	for _, column := range columns {
		if _, named := referenceDispositions[column]; !named {
			t.Errorf("the schema holds %s, a reference column, and referenceDispositions says nothing of how a read answers it", column)
		}
	}
	for _, column := range slices.Sorted(maps.Keys(referenceDispositions)) {
		if !slices.Contains(columns, column) {
			t.Errorf("referenceDispositions names %s, which is no reference column of the schema: take it off", column)
		}
	}

	walkers := []struct {
		caller  privateCaller
		objects censusObjects
		hidden  hiddenObjects
	}{
		{callerAlice, fixture.public, hiddenObjectsOf(t, fixture, seedPrivate, []string{sessionOfPrivate}, nil)},
		{callerAlicePubToken, fixture.public, hiddenObjectsOf(t, fixture, seedPrivate, []string{sessionOfPrivate}, nil)},
		{callerBob, fixture.private, hiddenObjectsOf(t, fixture, seedPublic, []string{sessionOfPublic}, []string{cross.privatePerson, seedAlice})},
		{callerBobPrivToken, fixture.private, hiddenObjectsOf(t, fixture, seedPublic, []string{sessionOfPublic}, []string{cross.privatePerson, seedAlice})},
	}
	for _, column := range columns {
		if referenceDispositions[column].kind != filteredReference {
			continue
		}
		table, name, _ := strings.Cut(column, ".")
		var values []string
		rows, err := fixture.database.Pool.Query(context.Background(), fmt.Sprintf(`select distinct %s::text from %s where %s is not null`,
			pgx.Identifier{name}.Sanitize(), pgx.Identifier{table}.Sanitize(), pgx.Identifier{name}.Sanitize()))
		if err == nil {
			values, err = pgx.CollectRows(rows, pgx.RowTo[string])
		}
		if err != nil {
			t.Fatalf("read %s: %v", column, err)
		}
		// A jsonb column holds an actor; the object it names is that actor's id.
		for index, value := range values {
			var actor struct {
				ID string `json:"id"`
			}
			if json.Unmarshal([]byte(value), &actor) == nil && actor.ID != "" {
				values[index] = actor.ID
			}
		}
		held := slices.ContainsFunc(values, func(value string) bool {
			return slices.ContainsFunc(walkers, func(walker struct {
				caller  privateCaller
				objects censusObjects
				hidden  hiddenObjects
			}) bool {
				return len(walker.hidden.hiddenIn(strings.Trim(value, "{}"))) > 0
			})
		})
		if !held {
			t.Errorf("the seed holds no row whose %s names an object a walker does not see, so no read of it is walked: write one in seedCrossReferences", column)
		}
	}

	routes := (&server{deps: Deps{TestHooksEnabled: true}}).routes()
	walked := 0
	for _, walker := range walkers {
		// read counts the walker's answers the census looked into: a 2xx JSON body. A walker whose
		// every read was refused, or answered something else, looked at nothing, and fails below.
		read := 0
		for _, route := range routes {
			name := route.Method + " " + route.Pattern
			if route.Method != http.MethodGet || !slices.ContainsFunc(route.Reads, readsProjectRows) {
				continue
			}
			target := walker.objects.fill(t, route.Pattern, readClasses[name].Probe)
			if query, takes := censusQueries[name]; takes {
				target += "?" + query(walker.objects).Encode()
			}
			walked++
			answer := fixture.request(t, walker.caller, http.MethodGet, target, nil)
			jsonBody := strings.HasPrefix(answer.Header().Get("Content-Type"), "application/json")
			var decoded any
			if err := json.Unmarshal(answer.Body.Bytes(), &decoded); err != nil {
				if jsonBody {
					t.Errorf("%s: GET %s answered %d with a JSON body that does not decode: %v", walker.caller, target, answer.Code, err)
				}
				continue
			}
			if answer.Code >= http.StatusOK && answer.Code < http.StatusMultipleChoices {
				read++
			}
			for _, found := range walker.hidden.leaksOf(decoded, "$") {
				t.Errorf("%s: GET %s answers %s at %s %q, naming %s, which it does not see", walker.caller, target,
					name, found.path, found.value, found.object)
			}
		}
		if read == 0 {
			t.Errorf("%s: no GET row answered it a 2xx JSON body, so the census read nothing of what it sees", walker.caller)
		}
		// The event log and the issue's events, read whole: every event the walker may read, once
		// the horizon is proven above every seeded event, so no event goes unread (awaitHorizonRead).
		safeHorizon(t, fixture.server)
		for _, target := range []string{"/api/v1/events/log?" + url.Values{"limit": {"1000"}}.Encode(),
			"/api/v1/issues/" + walker.objects.issue + "/events"} {
			answer := fixture.request(t, walker.caller, http.MethodGet, target, nil)
			var decoded any
			if err := json.Unmarshal(answer.Body.Bytes(), &decoded); err != nil {
				t.Fatalf("%s: GET %s answered %d %s", walker.caller, target, answer.Code, answer.Body.String())
			}
			for _, found := range walker.hidden.leaksOf(decoded, "$") {
				t.Errorf("%s: GET %s answers at %s %q, naming %s, which it does not see", walker.caller, target,
					found.path, found.value, found.object)
			}
		}
	}
	if walked == 0 {
		t.Fatal("the route table holds no GET row of a project, the graph, a person or a session, so the census walked nothing")
	}

	// The payload census, read from the stored payloads: each leaf of an event a walker may read
	// that names an object hidden from it sits under a key the payload filter leaves out.
	payloadKeys := map[string]bool{}
	for _, walker := range walkers {
		rows, err := fixture.database.Pool.Query(context.Background(), `
			select e.type, e.payload from events e where `+eventReadableSQL("e", "$1")+` order by e.id`,
			readerVisibilityArg(t, fixture, walker.caller))
		if err != nil {
			t.Fatalf("read the events %s may read: %v", walker.caller, err)
		}
		var eventType string
		var payload []byte
		if _, err := pgx.ForEachRow(rows, []any{&eventType, &payload}, func() error {
			var decoded map[string]any
			if json.Unmarshal(payload, &decoded) != nil {
				return nil
			}
			for _, found := range walker.hidden.leaksOf(decoded, "") {
				key, _, _ := strings.Cut(strings.TrimPrefix(found.path, "."), ".")
				key, _, _ = strings.Cut(key, "[")
				payloadKeys[key] = true
				if _, filtered := payloadReferenceKeys[key]; !filtered {
					t.Errorf("%s may read a stored %s whose payload names %s at %s (%q), and payloadReferenceKeys does not name %q: "+
						"leaveOutHiddenReferences would answer it", walker.caller, eventType, found.object, found.path, found.value, key)
				}
			}
			return nil
		}); err != nil {
			t.Fatalf("read the events %s may read: %v", walker.caller, err)
		}
	}
	for _, key := range []string{"blocked_by", "references_changed", "assignee", "asked_of", "route", "target", "mentions", "thread_logins", "claim"} {
		if !payloadKeys[key] {
			t.Errorf("no stored payload a walker may read names a hidden object under %q, so its filter is walked by nothing: write one in seedCrossReferences", key)
		}
	}
	t.Logf("%d reads walked over %d reference columns; payload keys naming hidden objects: %v", walked, len(columns), slices.Sorted(maps.Keys(payloadKeys)))
}

// TestWithNoPrivateProjectAReferenceReadsAsToday (R9): with no private project every reader sees
// every object, so no reference is left out and each read answers what it answered before the rule
// of references: on the census seed and its cross references, with PRIV made public, alice reads
// each read that names another project's objects byte for byte as carol, who sees every project,
// reads it, and alice's answer still names the PRIV objects PUB's rows name. A reader that sees
// every object takes the filters' short path (seesEveryObject), which reads nothing more.
func TestWithNoPrivateProjectAReferenceReadsAsToday(t *testing.T) {
	census := &fragmentCensus{}
	fixture := seedFragmentCensus(t, census)
	seedCrossReferences(t, fixture)
	fixture.setVisibility(t, seedPrivate, model.ProjectPublic)
	if alice := readerOfCaller(t, fixture, callerAlice); !seesEveryObject(alice) {
		t.Fatalf("with PRIV public alice reads as %+v, want an open viewer of a deployment with no private project", *alice.Viewer)
	}
	public := fixture.public
	reads := []string{
		"/api/v1/issues/" + public.issue,
		"/api/v1/issues?" + url.Values{"project": {seedPublic}}.Encode(),
		"/api/v1/issues/" + public.issue + "/events",
		"/api/v1/events/log?" + url.Values{"limit": {"1000"}}.Encode(),
		"/api/v1/issues/" + public.issue + "/comments",
		"/api/v1/issues/" + public.issue + "/messages",
		"/api/v1/issues/" + public.issue + "/asks",
		"/api/v1/asks/open?" + url.Values{"project": {seedPublic}}.Encode(),
		"/api/v1/priorities/" + public.priority + "/history",
		"/api/v1/settings/project-default-assignees",
		"/api/v1/delivery/timeline",
	}
	// Both callers read the event log at a horizon proven above every seeded event, so a held
	// horizon can neither shorten both reads alike nor make the two differ (awaitHorizonRead).
	safeHorizon(t, fixture.server)
	namesPrivate := false
	for _, read := range reads {
		// Two answers stamp the moment they were read (as_of, a window ending now), and the open
		// asks count each ask's age in whole seconds at that moment (age_seconds), which two reads
		// a second boundary falls between answer apart: each time and each age is written as one
		// placeholder, and every other byte compared.
		want := fixture.request(t, callerCarol, http.MethodGet, read, nil)
		got := fixture.request(t, callerAlice, http.MethodGet, read, nil)
		if got.Code != want.Code || maskAges(maskTimes(got.Body.String())) != maskAges(maskTimes(want.Body.String())) {
			t.Errorf("PRIV public: alice reads %s = %d %s, want carol's answer byte for byte: %d %s",
				read, got.Code, got.Body.String(), want.Code, want.Body.String())
		}
		namesPrivate = namesPrivate || strings.Contains(got.Body.String(), `"`+seedPrivate+"-")
	}
	if !namesPrivate {
		t.Error("PRIV public: no read of alice's names an issue of PRIV, so the comparison holds nothing the rule could leave out")
	}
}

// readerOfCaller is the reader caller's requests resolve to on the seeded server (readerOf).
func readerOfCaller(t *testing.T, fixture fragmentCensusFixture, caller privateCaller) model.Actor {
	t.Helper()
	recorder := httptest.NewRecorder()
	reader, ok := fixture.server.readerOf(recorder, fixture.readerRequest(t, caller))
	if !ok {
		t.Fatalf("resolve %s: %d %s", caller, recorder.Code, recorder.Body.String())
	}
	return reader
}

// readerVisibilityArg is the visibility argument caller's requests bind (readerOfCaller).
func readerVisibilityArg(t *testing.T, fixture fragmentCensusFixture, caller privateCaller) any {
	t.Helper()
	return refs.VisibilityArg(readerOfCaller(t, fixture, caller))
}
