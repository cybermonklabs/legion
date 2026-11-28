package store

// RowSecurityClass is one class of plan D4 (INFRA-6168): how a table's policy decides whether the
// request role (dispatch_app) sees a row. Every class but RowSecurityNone has row security
// enabled, not forced, and one policy named viewer, for all commands and to public, which passes
// every role but dispatch_app (`not dispatch_request_role() or <predicate>`), so the owner, the
// old build while a release rolls and the read login keep the reads they have.
// RowSecurityNone has no policy and row security off: machinery or identity that holds no
// content of a project.
type RowSecurityClass string

// The classes of plan D4, each named by its letters there. Each predicate starts with the R5
// fast path (an open viewer while no project is private sees every row).
const (
	// RowSecurityProject (P): the table's own project column, through dispatch_sees_project;
	// projects reads its own key and visibility.
	RowSecurityProject RowSecurityClass = "P"
	// RowSecurityIssue (I): through the issue the row names, under issues' row security.
	RowSecurityIssue RowSecurityClass = "I"
	// RowSecurityTwoIssues (L): both issues the row names, each through issues.
	RowSecurityTwoIssues RowSecurityClass = "L"
	// RowSecurityIssueOrDocument (O): through the issue when issue_key is set, else through the
	// document (artifacts).
	RowSecurityIssueOrDocument RowSecurityClass = "O"
	// RowSecurityDocuments (A): access.DocumentClause over dispatch_viewer().
	RowSecurityDocuments RowSecurityClass = "A"
	// RowSecurityThroughDocument (D): through the document (artifacts) the row names.
	RowSecurityThroughDocument RowSecurityClass = "D"
	// RowSecurityThroughComment (C): through the comment the row names.
	RowSecurityThroughComment RowSecurityClass = "C"
	// RowSecurityThroughAsk (K): through the ask the row names.
	RowSecurityThroughAsk RowSecurityClass = "K"
	// RowSecurityMessages (M): access.MessageClause over dispatch_viewer().
	RowSecurityMessages RowSecurityClass = "M"
	// RowSecurityThroughMessage (MC): through the message the row names.
	RowSecurityThroughMessage RowSecurityClass = "MC"
	// RowSecurityCommentOrMessage (PB): the comment arm or the message arm, whichever is set.
	RowSecurityCommentOrMessage RowSecurityClass = "PB"
	// RowSecurityEvents (E): the event's owner (project, issue or document), and for an event of
	// no owner its key: agent:<session> through SessionContentClause, priority: and
	// access_group: to an open viewer.
	RowSecurityEvents RowSecurityClass = "E"
	// RowSecurityThroughEvent (EC): through the event the row names.
	RowSecurityThroughEvent RowSecurityClass = "EC"
	// RowSecurityGraphNodes (G): the node's project, else its parties (PartyClause), else an open
	// viewer while no project is private.
	RowSecurityGraphNodes RowSecurityClass = "G"
	// RowSecurityGraphTuples (GT): both nodes, through graph_nodes.
	RowSecurityGraphTuples RowSecurityClass = "GT"
	// RowSecurityReferences (R): the from item and the to item, each through its own table by
	// kind, or open where the kind holds no project.
	RowSecurityReferences RowSecurityClass = "R"
	// RowSecurityWorkflow (WF): the step node through workflow_nodes, a node through the issue it
	// is part_of.
	RowSecurityWorkflow RowSecurityClass = "WF"
	// RowSecurityPriorities (PR): a priority node passes an open viewer; a priority tuple both its
	// ends.
	RowSecurityPriorities RowSecurityClass = "PR"
	// RowSecurityGoingRequests (GR): through going_requests.
	RowSecurityGoingRequests RowSecurityClass = "GR"
	// RowSecurityCheckResults (CR): through check_results.
	RowSecurityCheckResults RowSecurityClass = "CR"
	// RowSecurityEmbeddings (EM): the project column, else the owning row by kind and id.
	RowSecurityEmbeddings RowSecurityClass = "EM"
	// RowSecurityBroadcasts (B): access.BroadcastClause.
	RowSecurityBroadcasts RowSecurityClass = "B"
	// RowSecuritySessions (S): access.SessionClause over the row's session and dispatch_viewer().
	RowSecuritySessions RowSecurityClass = "S"
	// RowSecurityListenerAnswer (RR): an open viewer; the rule functions read it as the owner.
	RowSecurityListenerAnswer RowSecurityClass = "RR"
	// RowSecurityRepositories (R-repo): the repository rule, dispatch_repo_visible(repo); a row
	// with an issue goes through issues.
	RowSecurityRepositories RowSecurityClass = "R-repo"
	// RowSecurityNone (N): no policy; row security stays off.
	RowSecurityNone RowSecurityClass = "N"
)

// RowSecurityTable is one table's class and why it is in that class.
type RowSecurityTable struct {
	Class  RowSecurityClass
	Reason string
}

// The reasons the class list of plan D4 gives, shared by every table of a class whose row it
// states no more of.
const (
	reasonOwnProject      = "its own project column, through dispatch_sees_project"
	reasonThroughIssue    = "the issue it names, under the row security of issues"
	reasonIssueOrDocument = "the issue when issue_key is set, else the document it names"
	reasonThroughDocument = "the document (artifacts) it names, under the row security of artifacts"
	reasonThroughComment  = "the comment it names, under the row security of comments"
	reasonThroughAsk      = "the ask it names, under the row security of asks"
	reasonThroughMessage  = "the message it names, under the row security of messages"
	reasonThroughEvent    = "the event it names, under the row security of events"
	reasonSession         = "access.SessionClause over the session it names and dispatch_viewer()"
	reasonListenerAnswer  = "the listener's answer, one row of every session's projects: an open viewer reads it, and the rule functions read it as the owner"
	reasonRepository      = "the repository rule (dispatch_repo_visible); a row with an issue goes through issues"
	reasonMachinery       = "machinery or identity, no content of a project"
)

// RowSecurityClasses is plan D4's class list: every table of schema public, each in exactly one
// class, with its reason. TestEveryTableIsClassedForRowSecurity holds it against the catalog of a
// freshly migrated database: each table the catalog holds is here and each name here is a table,
// each class but RowSecurityNone has row security on, not forced, with its viewer policy, and
// RowSecurityNone has row security off. A table a later migration adds fails that test until it
// is classed here; a table moves to another class only with a comment on the plan's block a that
// names the table and the reason. An entry whose reason starts with UNREVIEWED is a table the
// catalog holds that D4 does not class, put under the class whose definition fits it best, and
// the test fails on it until it is reviewed.
var RowSecurityClasses = map[string]RowSecurityTable{
	// P: own project column.
	"projects":                  {RowSecurityProject, "its own key and visibility: the key is one the viewer sees, or the viewer is open and the project is public"},
	"issues":                    {RowSecurityProject, reasonOwnProject},
	"repo_projects":             {RowSecurityProject, reasonOwnProject},
	"architecture_sources":      {RowSecurityProject, reasonOwnProject},
	"architecture_snapshots":    {RowSecurityProject, reasonOwnProject},
	"components":                {RowSecurityProject, reasonOwnProject},
	"component_depends":         {RowSecurityProject, reasonOwnProject},
	"issue_component_members":   {RowSecurityProject, reasonOwnProject},
	"component_facts":           {RowSecurityProject, reasonOwnProject},
	"related_checks":            {RowSecurityProject, reasonOwnProject},
	"search_term_stats":         {RowSecurityProject, reasonOwnProject},
	"search_term_totals":        {RowSecurityProject, reasonOwnProject},
	"near_copy_checks":          {RowSecurityProject, reasonOwnProject},
	"spec_check_results":        {RowSecurityProject, reasonOwnProject},
	"project_access":            {RowSecurityProject, reasonOwnProject},
	"project_role_holder_marks": {RowSecurityProject, reasonOwnProject},
	"legion_settings":           {RowSecurityProject, reasonOwnProject},
	"legion_reads":              {RowSecurityProject, reasonOwnProject},
	"legion_observations":       {RowSecurityProject, reasonOwnProject},
	"legion_current":            {RowSecurityProject, reasonOwnProject},
	"legion_event_cursors":      {RowSecurityProject, reasonOwnProject},
	"legion_event_parts":        {RowSecurityProject, reasonOwnProject},
	"going_requests":            {RowSecurityProject, reasonOwnProject},
	"search_text_versions":      {RowSecurityProject, reasonOwnProject},
	"user_threads":              {RowSecurityProject, reasonOwnProject},

	// I: through an issue.
	"issue_external_links":    {RowSecurityIssue, reasonThroughIssue},
	"issue_components":        {RowSecurityIssue, reasonThroughIssue},
	"handoffs":                {RowSecurityIssue, reasonThroughIssue},
	"design_gates":            {RowSecurityIssue, reasonThroughIssue},
	"issue_role_holders":      {RowSecurityIssue, reasonThroughIssue},
	"check_results":           {RowSecurityIssue, "the issue it names (issue_key), under the row security of issues"},
	"write_suggestions":       {RowSecurityIssue, "its source issue, artifact or ask, whichever is set, each under its own row security"},
	"embedding_issue_sources": {RowSecurityIssue, reasonThroughIssue},
	"user_issue_state":        {RowSecurityIssue, reasonThroughIssue},
	"pull_request_heads":      {RowSecurityIssue, "the issue it names (issue_key), else the repository rule (dispatch_repo_visible)"},

	// L: two issues.
	"issue_links": {RowSecurityTwoIssues, "both issues it names (issue_key and target_key), each under the row security of issues"},

	// O: issue or document.
	"asks":               {RowSecurityIssueOrDocument, reasonIssueOrDocument},
	"comments":           {RowSecurityIssueOrDocument, reasonIssueOrDocument},
	"comment_watermarks": {RowSecurityIssueOrDocument, reasonIssueOrDocument},
	"search_term_queue":  {RowSecurityIssueOrDocument, "the issue when issue_key is set, else the document it names, else, for a message on no issue, the message (item_id)"},

	// A: documents.
	"artifacts": {RowSecurityDocuments, "access.DocumentClause over dispatch_viewer(): a project's document through dispatch_sees_project, an issue's through issues, a conversation file through PartyClause over its one session party"},

	// D: through a document.
	"artifact_versions":          {RowSecurityThroughDocument, reasonThroughDocument},
	"doc_updates":                {RowSecurityThroughDocument, reasonThroughDocument},
	"doc_snapshots":              {RowSecurityThroughDocument, reasonThroughDocument},
	"doc_checkpoints":            {RowSecurityThroughDocument, reasonThroughDocument},
	"artifact_reviews":           {RowSecurityThroughDocument, reasonThroughDocument},
	"doc_pending_authors":        {RowSecurityThroughDocument, reasonThroughDocument},
	"document_blocks":            {RowSecurityThroughDocument, reasonThroughDocument},
	"block_refs":                 {RowSecurityThroughDocument, reasonThroughDocument},
	"document_block_versions":    {RowSecurityThroughDocument, reasonThroughDocument},
	"document_block_texts":       {RowSecurityThroughDocument, reasonThroughDocument},
	"document_indexed_versions":  {RowSecurityThroughDocument, reasonThroughDocument},
	"document_edit_notices":      {RowSecurityThroughDocument, reasonThroughDocument},
	"decision_keeps":             {RowSecurityThroughDocument, reasonThroughDocument},
	"doc_ask_block_authors":      {RowSecurityThroughDocument, reasonThroughDocument},
	"doc_conditional_edit_queue": {RowSecurityThroughDocument, reasonThroughDocument},
	"document_search":            {RowSecurityThroughDocument, reasonThroughDocument},
	"doc_closed_edit_sources":    {RowSecurityThroughDocument, reasonThroughDocument},
	"user_document_reads":        {RowSecurityThroughDocument, reasonThroughDocument},

	// C: through a comment.
	"comment_mentions":        {RowSecurityThroughComment, reasonThroughComment},
	"comment_deliveries":      {RowSecurityThroughComment, reasonThroughComment},
	"comment_blocks":          {RowSecurityThroughComment, reasonThroughComment},
	"comment_block_texts":     {RowSecurityThroughComment, reasonThroughComment},
	"comment_block_state":     {RowSecurityThroughComment, reasonThroughComment},
	"comment_block_refs":      {RowSecurityThroughComment, reasonThroughComment},
	"comment_fallback_copies": {RowSecurityThroughComment, reasonThroughComment},

	// K: through an ask.
	"ask_followers":   {RowSecurityThroughAsk, reasonThroughAsk},
	"ask_blocks":      {RowSecurityThroughAsk, reasonThroughAsk},
	"ask_block_texts": {RowSecurityThroughAsk, reasonThroughAsk},
	"ask_block_state": {RowSecurityThroughAsk, reasonThroughAsk},
	"ask_block_refs":  {RowSecurityThroughAsk, reasonThroughAsk},
	"user_ask_snooze": {RowSecurityThroughAsk, reasonThroughAsk},

	// M: messages.
	"messages": {RowSecurityMessages, "access.MessageClause over dispatch_viewer(): an issue's message through issues, a message on no issue through its conversation's parties (dispatch_message_visible: its node's, else those its thread root's author and target give it)"},

	// MC: through a message.
	"message_deliveries":          {RowSecurityThroughMessage, reasonThroughMessage},
	"message_blocks":              {RowSecurityThroughMessage, reasonThroughMessage},
	"message_block_state":         {RowSecurityThroughMessage, reasonThroughMessage},
	"message_block_refs":          {RowSecurityThroughMessage, reasonThroughMessage},
	"user_agent_thread_dismissed": {RowSecurityThroughMessage, reasonThroughMessage},

	// PB: a comment or a message.
	"post_body_history": {RowSecurityCommentOrMessage, "the comment or the message it names, whichever is set, under its own row security"},

	// E: events.
	"events": {RowSecurityEvents, "its owner: project_key through the project rule, issue_key through issues, artifact_id through artifacts; an event of no owner by its key (agent:<session> through SessionContentClause, priority: and access_group: to an open viewer)"},

	// EC: through an event.
	"project_access_changes":        {RowSecurityThroughEvent, reasonThroughEvent},
	"outbox_role_copy_expiries":     {RowSecurityThroughEvent, reasonThroughEvent},
	"outbox_role_undelivered_notes": {RowSecurityThroughEvent, reasonThroughEvent},

	// G: graph nodes.
	"graph_nodes": {RowSecurityGraphNodes, "dispatch_node_visible, the rule of the node's kind: a project kind's node by its project, else its parties, else no viewer; an item's block by its item; a component by its address's project; a person, a session and a role by access.PersonClause, SessionClause and RoleClause; a page by its session's or role's; the block schema's nodes to every viewer"},

	// GT: graph tuples.
	"graph_tuples": {RowSecurityGraphTuples, "both nodes it joins, each by dispatch_tuple_end_visible (the node read as the owner, judged by dispatch_node_visible); an attribute's value (to_kind value) is no node"},

	// R: references.
	"refs": {RowSecurityReferences, "the from item and the to item, each through its own table by kind, or open where the kind holds no project"},

	// WF: workflow.
	"workflow_nodes":  {RowSecurityWorkflow, "the issue the node is part_of, its issue attribute or else its part_of tuple, read as the owner (dispatch_workflow_node_visible); a node that names no issue yet passes only the viewer that made it"},
	"workflow_tuples": {RowSecurityWorkflow, "the step node, through workflow_nodes"},

	// PR: priorities.
	"priority_nodes":  {RowSecurityPriorities, "an open viewer: a priority belongs to no project"},
	"priority_tuples": {RowSecurityPriorities, "both ends: a priority end passes an open viewer, an issue end goes through issues, and an attribute's value is no end"},

	// GR: going requests.
	"going_events":        {RowSecurityGoingRequests, "the going request it names, under the row security of going_requests"},
	"going_request_posts": {RowSecurityGoingRequests, "the going request it names, under the row security of going_requests"},

	// CR: check results.
	"check_result_retractions": {RowSecurityCheckResults, "the check result it names, under the row security of check_results"},

	// EM: embeddings.
	"embeddings":              {RowSecurityEmbeddings, "project_key through the project rule; a null project_key through the owning row by kind and id"},
	"embedding_window_states": {RowSecurityEmbeddings, "the owning row by owner_key, through its own table"},

	// B: broadcasts.
	"broadcasts": {RowSecurityBroadcasts, "access.BroadcastClause as the owner reads it (dispatch_broadcast_visible); a broadcast its own insert writes passes an open viewer and its sender"},

	// S: a session's rows.
	"session_projects":               {RowSecuritySessions, reasonSession},
	"session_reached_projects":       {RowSecuritySessions, reasonSession},
	"session_owners":                 {RowSecuritySessions, reasonSession},
	"session_titles":                 {RowSecuritySessions, reasonSession},
	"user_agent_state":               {RowSecuritySessions, reasonSession},
	"user_agent_read":                {RowSecuritySessions, reasonSession},
	"user_agent_reply_read":          {RowSecuritySessions, reasonSession},
	"user_agent_pin":                 {RowSecuritySessions, reasonSession},
	"delivery_session_group_members": {RowSecuritySessions, reasonSession},

	// RR: the listener's answer.
	"route_roster":                  {RowSecurityListenerAnswer, reasonListenerAnswer},
	"route_roster_session_projects": {RowSecurityListenerAnswer, reasonListenerAnswer},

	// R-repo: repository rows.
	"delivery_pull_requests": {RowSecurityRepositories, "the issue it names (issue_key), else the repository rule on repo (dispatch_repo_visible)"},
	"delivery_runs":          {RowSecurityRepositories, reasonRepository},
	"delivery_run_jobs":      {RowSecurityRepositories, reasonRepository},
	"delivery_run_warnings":  {RowSecurityRepositories, reasonRepository},
	"delivery_changes":       {RowSecurityRepositories, "the repository rule on repo (dispatch_repo_visible); a change of no repository passes an open viewer"},
	"branch_commits":         {RowSecurityRepositories, reasonRepository},
	"commit_holds":           {RowSecurityRepositories, reasonRepository},
	"deploys":                {RowSecurityRepositories, reasonRepository},

	// N: no policy.
	"people":                          {RowSecurityNone, reasonMachinery},
	"access_groups":                   {RowSecurityNone, reasonMachinery},
	"access_group_members":            {RowSecurityNone, reasonMachinery},
	"agent_tokens":                    {RowSecurityNone, reasonMachinery},
	"agent_read_tokens":               {RowSecurityNone, reasonMachinery},
	"user_sessions":                   {RowSecurityNone, reasonMachinery},
	"request_keys":                    {RowSecurityNone, "an accepted residual (plan departure 9, D13): a replay needs the same (login, idempotency_key) and the same request_digest, so it answers only the login that sent that exact request, within 24 hours"},
	"request_key_steps":               {RowSecurityNone, reasonMachinery},
	"dpop_proofs":                     {RowSecurityNone, reasonMachinery},
	"broker_revocations":              {RowSecurityNone, reasonMachinery},
	"machine_login_revocations":       {RowSecurityNone, reasonMachinery},
	"duty_holds":                      {RowSecurityNone, reasonMachinery},
	"duty_passes":                     {RowSecurityNone, reasonMachinery},
	"server_processes":                {RowSecurityNone, reasonMachinery},
	"server_build_announcements":      {RowSecurityNone, reasonMachinery},
	"dashboard_builds":                {RowSecurityNone, reasonMachinery},
	"dashboard_assets":                {RowSecurityNone, reasonMachinery},
	"dashboard_build_assets":          {RowSecurityNone, reasonMachinery},
	"embeddings_rate_limit":           {RowSecurityNone, reasonMachinery},
	"embedder_active":                 {RowSecurityNone, reasonMachinery},
	"delivery_settings":               {RowSecurityNone, reasonMachinery},
	"delivery_reconcile_progress":     {RowSecurityNone, reasonMachinery},
	"delivery_session_groups":         {RowSecurityNone, reasonMachinery},
	"ci_reconcile_passes":             {RowSecurityNone, reasonMachinery},
	"github_rate_limits":              {RowSecurityNone, reasonMachinery},
	"github_read_attempts":            {RowSecurityNone, reasonMachinery},
	"event_horizon":                   {RowSecurityNone, reasonMachinery},
	"suggestion_outcome_cursor":       {RowSecurityNone, reasonMachinery},
	"block_index_event_cursors":       {RowSecurityNone, reasonMachinery},
	"graph_backfill_horizon":          {RowSecurityNone, reasonMachinery},
	"block_grammar_owed":              {RowSecurityNone, reasonMachinery},
	"refs_grammar_owed":               {RowSecurityNone, reasonMachinery},
	"refs_forms_owed":                 {RowSecurityNone, reasonMachinery},
	"doc_compaction_owed":             {RowSecurityNone, reasonMachinery},
	"doc_settlements_pending":         {RowSecurityNone, reasonMachinery},
	"reference_excerpt_backoff":       {RowSecurityNone, reasonMachinery},
	"outbox_kept_expiry_cursor":       {RowSecurityNone, reasonMachinery},
	"session_projects_filled":         {RowSecurityNone, reasonMachinery},
	"access_events_scrubbed":          {RowSecurityNone, reasonMachinery},
	"owner_lock_legacy_mark":          {RowSecurityNone, reasonMachinery},
	"parent_status_fix":               {RowSecurityNone, reasonMachinery},
	"document_search_backfill":        {RowSecurityNone, reasonMachinery},
	"search_text_versions_fill":       {RowSecurityNone, reasonMachinery},
	"session_titles_backfill":         {RowSecurityNone, reasonMachinery},
	"embeddings_project_key_backfill": {RowSecurityNone, reasonMachinery},
	"text_tree_rebuild":               {RowSecurityNone, reasonMachinery},
	"text_trees":                      {RowSecurityNone, "an accepted residual (plan departure 9, D13): a row is found only by the sha256 of a text its reader has already read under row security"},
	"schema_migrations":               {RowSecurityNone, reasonMachinery},
}
