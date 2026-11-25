package api

import (
	"maps"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// readWrites is what one GET row of the table writes when it serves a request: whether it is a
// read, which a read-only token may call, or a GET that writes, which no read-only token may
// call. A read-only token is any credential that holds read: a token the secrets broker minted
// from a read entry or narrowed on request, and a personal token minted with read.
type readWrites int

const (
	// readWritesUnset is the zero value: an entry that decides nothing. It is never a class, so
	// an entry that leaves Writes unset is a row nobody has read yet.
	readWritesUnset readWrites = iota
	// readWritesNothing is a read. Wherever its writes run, on the request's own context or on
	// the server's context in work the request starts or waits on, the row writes nothing but
	// what readClasses lists: the proof's spent row, a personal token's last use, the caches
	// Dispatch rebuilds from what it holds, and the shared work the server does for every reader
	// alike. A read that writes any of it on the server's context carries a WriteNote saying so.
	readWritesNothing
	// readWritesState is a GET that writes: anything a read may not, wherever it runs. Its
	// WriteNote names what it writes.
	readWritesState
)

// readEntry is the read class of one GET row of the table: what the row writes, and the note
// that says so where the class alone does not.
type readEntry struct {
	// Writes is the row's class: readWritesNothing or readWritesState, never readWritesUnset.
	Writes readWrites
	// WriteNote names what the row writes, and the statement or the function that writes it. A
	// readWritesState row always carries one. A readWritesNothing row carries one when its
	// handler makes the server write on its own context, or when its class differs from what a
	// first reading of its handler suggests, and then it says why.
	WriteNote string
}

// docsCompactionNote is the WriteNote of every read that reads a document's live text through
// the document service (docs.Service's Text, TextWithBlocks, TextWithToken,
// TextWithTokenAndBlocks, Blocks and BlockPath, each through its readDocument).
const docsCompactionNote = "Writes nothing itself. A cold read of a document whose stored history holds more than one doc_updates row queues the document on the document service's compaction queue (noteStoredUpdates), and the queue's worker compacts it later on its own context: it updates and deletes doc_updates and deletes doc_compaction_owed, as the hourly sweep does for every due document, whoever reads it. A compaction changes nothing a reader reads."

// readClasses is the read class of every GET row of the table with its test hooks in, keyed
// "GET <pattern>" as routeScopes is: exactly one entry for each GET row and none for anything
// else. A read (readWritesNothing) writes nothing, on the request's own context or on the
// server's context in work the request starts or waits on, but these:
//
//   - the insert into dpop_proofs that spends a broker token's proof;
//   - the update of a personal token's last_used_at;
//   - the insert into text_trees that a read with Dispatch-Trees: 1 makes;
//   - the upsert of a stale route_roster;
//   - the event horizon's observer persisting event_horizon;
//   - a project's term statistics refresh, in search_term_stats, search_term_totals and
//     search_term_queue;
//   - the GitHub client recording GitHub's rate limit in github_rate_limits;
//   - a compaction's rewrite and delete in doc_updates and doc_compaction_owed.
//
// Every other GET row is a GET that writes (readWritesState), which no read-only token may call.
//
// routes() stamps each entry onto its row (apiRoute.Read), which is what the read-only gate reads
// (refuseReadOnlyToken), and register refuses to mount a table with a GET row this map does not
// class, or with an entry that names no GET row it mounts (readClassRefusals), so a GET row added
// later is served to no read-only token until someone has read its handler and classed it here.
//
// v1ReadClasses holds the /api/v1 GETs, grouped by class and in table order: v1Rows, then the
// /api/v1 paths of the rows both versions serve (v2Infra979Rows).
var v1ReadClasses = map[string]readEntry{
	// The GETs that write.
	"GET /api/v1/going/requests/{id}/stream": {Writes: readWritesState, WriteNote: "When the run is past its ends_at and holds no terminal frame, the read inserts the run's end frame, the error that its answer did not arrive, into going_events on the request's context (endGoingRun), doing nothing on conflict when another writer ended the run first."},
	"GET /api/v1/delivery/timeline":          {Writes: readWritesState, WriteNote: "When a deploy run has no configured production job, or its production job has no configured apply step, the read inserts a delivery_run_warnings row on the request's context (firstToWarn, on conflict doing nothing), once per process for each run and configuration."},
	"GET /api/v1/delivery/measures":          {Writes: readWritesState, WriteNote: "As the timeline's: an insert into delivery_run_warnings for a deploy run with no matching production job or apply step, on the request's context, once per process for each run and configuration (firstToWarn)."},
	"GET /api/v1/delivery/pipeline":          {Writes: readWritesState, WriteNote: "As the timeline's, for the deploy runs on main since the first day it shows: an insert into delivery_run_warnings for a run with no matching production job or apply step, on the request's context, once per process for each run and configuration (firstToWarn)."},

	// The reads whose handler makes the server write on its own context, or whose class a first
	// reading of the handler would get wrong.
	"GET /api/v1/projects/{key}/related":                 {Writes: readWritesNothing, WriteNote: "When the project has no search_term_totals row, the related-item check starts a refresh of the project's term statistics on the server's own context (ensureSearchTermStats, once per project and process): it inserts and updates search_term_totals, deletes and inserts search_term_stats, and deletes the project's search_term_queue rows. The statistics are a cache of what the project holds, which every related check reads and the first to find them missing rebuilds; nothing else is written."},
	"GET /api/v1/github/app-installation/{owner}/{repo}": {Writes: readWritesNothing, WriteNote: "Writes nothing itself. When GitHub answers the installation lookup with a rate limit, the GitHub client records that limit in github_rate_limits on a context detached from the request (ratelimit.Record), as it does for every caller that reads GitHub: the row is GitHub's own state, not a fact of Dispatch's."},
	"GET /api/v1/references":                             {Writes: readWritesNothing, WriteNote: "Writes nothing itself. With ?since=, the read waits on the event horizon, as the event log does, and the horizon's observation, which may run on this request's goroutine, writes event_horizon on a context of its own (writePersisted), at most once a second, after its waiters have their answer: the server's write for every reader alike. Without ?since= it does not read the horizon."},
	"GET /api/v1/issues/{key}/results":                   {Writes: readWritesNothing, WriteNote: "A read-only caller's read reads GitHub and stores nothing. A writer's read stores what it read from GitHub, as it always has: the pull request heads of a result's commit (pull_request_heads), with the branch commits (branch_commits) and the attribution events (events) that come with them. Like every reader of GitHub, the GitHub client may record GitHub's rate limit in github_rate_limits on a detached context, and the check blocks are read through the document service, whose cold read may queue a compaction (docsCompactionNote)."},
	"GET /api/v1/issues/{key}/coverage":                  {Writes: readWritesNothing, WriteNote: docsCompactionNote},
	"GET /api/v1/issues/{key}/decisions":                 {Writes: readWritesNothing, WriteNote: docsCompactionNote},
	"GET /api/v1/asks/{id}":                              {Writes: readWritesNothing, WriteNote: "The ask's anchor block is read through the document service. " + docsCompactionNote},
	"GET /api/v1/comments/{id}":                          {Writes: readWritesNothing, WriteNote: "The comment's anchor block is read through the document service. " + docsCompactionNote},
	"GET /api/v1/artifacts/{id}/blocks":                  {Writes: readWritesNothing, WriteNote: docsCompactionNote},
	"GET /api/v1/artifacts/{id}/blocks/{block_id}":       {Writes: readWritesNothing, WriteNote: docsCompactionNote},
	"GET /api/v1/artifacts/{id}/text":                    {Writes: readWritesNothing, WriteNote: docsCompactionNote},
	"GET /api/v1/issues/{key}/artifacts/{slug}/text":     {Writes: readWritesNothing, WriteNote: docsCompactionNote},
	"GET /api/v1/issues/{key}/artifacts/{slug}/blocks":   {Writes: readWritesNothing, WriteNote: docsCompactionNote},
	"GET /api/v1/projects/{key}/artifacts/{slug}/text":   {Writes: readWritesNothing, WriteNote: docsCompactionNote},
	"GET /api/v1/projects/{key}/artifacts/{slug}/blocks": {Writes: readWritesNothing, WriteNote: docsCompactionNote},
	"GET /api/v1/events/log":                             {Writes: readWritesNothing, WriteNote: "Writes nothing itself. Reading the safe horizon (events.Horizon) may run the horizon's observation on this request's goroutine, and that observation writes event_horizon on a context of its own (writePersisted), at most once a second, after its waiters have their answer: the server's write for every reader of the event log alike, not this request's."},

	// The other reads.
	"GET /api/v1":                                                        {Writes: readWritesNothing},
	"GET /api/v1/schema/blocks":                                          {Writes: readWritesNothing},
	"GET /api/v1/limits":                                                 {Writes: readWritesNothing},
	"GET /api/v1/projects":                                               {Writes: readWritesNothing},
	"GET /api/v1/projects/{key}/artifacts":                               {Writes: readWritesNothing},
	"GET /api/v1/settings/repo-projects":                                 {Writes: readWritesNothing},
	"GET /api/v1/settings/architecture-sources":                          {Writes: readWritesNothing},
	"GET /api/v1/projects/{key}/architecture-sources":                    {Writes: readWritesNothing},
	"GET /api/v1/projects/{key}/architecture-source":                     {Writes: readWritesNothing},
	"GET /api/v1/projects/{key}/architecture":                            {Writes: readWritesNothing},
	"GET /api/v1/projects/{key}/plans":                                   {Writes: readWritesNothing},
	"GET /api/v1/settings/project-default-assignees":                     {Writes: readWritesNothing},
	"GET /api/v1/settings/new-issue":                                     {Writes: readWritesNothing},
	"GET /api/v1/delivery/runs/{id}":                                     {Writes: readWritesNothing},
	"GET /api/v1/ci-reconcile/passes":                                    {Writes: readWritesNothing},
	"GET /api/v1/me/agent-tokens":                                        {Writes: readWritesNothing},
	"GET /api/v1/users":                                                  {Writes: readWritesNothing},
	"GET /api/v1/people/{login}/nodes":                                   {Writes: readWritesNothing},
	"GET /api/v1/whoami":                                                 {Writes: readWritesNothing},
	"GET /api/v1/dashboard/builds":                                       {Writes: readWritesNothing},
	"GET /api/v1/priorities":                                             {Writes: readWritesNothing},
	"GET /api/v1/priorities/{id}/history":                                {Writes: readWritesNothing},
	"GET /api/v1/issues":                                                 {Writes: readWritesNothing},
	"GET /api/v1/issues/resolve":                                         {Writes: readWritesNothing},
	"GET /api/v1/issues/{key}":                                           {Writes: readWritesNothing},
	"GET /api/v1/issues/{key}/summary":                                   {Writes: readWritesNothing},
	"GET /api/v1/issues/{key}/events":                                    {Writes: readWritesNothing},
	"GET /api/v1/issues/{key}/references":                                {Writes: readWritesNothing},
	"GET /api/v1/issues/{key}/collaborators":                             {Writes: readWritesNothing},
	"GET /api/v1/references/questions":                                   {Writes: readWritesNothing},
	"GET /api/v1/block-links":                                            {Writes: readWritesNothing},
	"GET /api/v1/issues/{key}/subscribers":                               {Writes: readWritesNothing},
	"GET /api/v1/issues/{key}/artifacts":                                 {Writes: readWritesNothing},
	"GET /api/v1/reference-targets":                                      {Writes: readWritesNothing},
	"GET /api/v1/issues/{key}/messages":                                  {Writes: readWritesNothing},
	"GET /api/v1/issues/{key}/messages/{id}":                             {Writes: readWritesNothing},
	"GET /api/v1/issues/{key}/handoffs":                                  {Writes: readWritesNothing},
	"GET /api/v1/issues/{key}/handoffs/{phase}/latest":                   {Writes: readWritesNothing},
	"GET /api/v1/issues/{key}/steps":                                     {Writes: readWritesNothing},
	"GET /api/v1/issues/{key}/role-holders":                              {Writes: readWritesNothing},
	"GET /api/v1/issues/{key}/legion":                                    {Writes: readWritesNothing},
	"GET /api/v1/handoffs/{id}":                                          {Writes: readWritesNothing},
	"GET /api/v1/messages/{id}":                                          {Writes: readWritesNothing},
	"GET /api/v1/messages/{id}/blocks":                                   {Writes: readWritesNothing},
	"GET /api/v1/messages/{id}/history":                                  {Writes: readWritesNothing},
	"GET /api/v1/inbox":                                                  {Writes: readWritesNothing},
	"GET /api/v1/me/answers":                                             {Writes: readWritesNothing},
	"GET /api/v1/me/replies":                                             {Writes: readWritesNothing},
	"GET /api/v1/search":                                                 {Writes: readWritesNothing},
	"GET /api/v1/search/results":                                         {Writes: readWritesNothing},
	"GET /api/v1/search/coverage":                                        {Writes: readWritesNothing},
	"GET " + goingCataloguePath:                                          {Writes: readWritesNothing},
	"GET /api/v1/related-checks":                                         {Writes: readWritesNothing},
	"GET /api/v1/near-copy-checks":                                       {Writes: readWritesNothing},
	"GET /api/v1/near-copy-checks/{id}":                                  {Writes: readWritesNothing},
	"GET /api/v1/going/capability":                                       {Writes: readWritesNothing},
	"GET /api/v1/agents":                                                 {Writes: readWritesNothing},
	"GET /api/v1/agents/known":                                           {Writes: readWritesNothing},
	"GET /api/v1/agents/{session_id}/answers":                            {Writes: readWritesNothing},
	"GET /api/v1/agents/{session_id}/messages":                           {Writes: readWritesNothing},
	"GET /api/v1/agents/{session_id}/artifacts":                          {Writes: readWritesNothing},
	"GET /api/v1/agents/{session_id}/artifacts/{slug}":                   {Writes: readWritesNothing},
	"GET /api/v1/agents/{session_id}/artifacts/{slug}/versions/{number}": {Writes: readWritesNothing},
	"GET /api/v1/broadcasts":                                             {Writes: readWritesNothing},
	"GET /api/v1/broadcasts/{id}":                                        {Writes: readWritesNothing},
	"GET /api/v1/issues/{key}/asks":                                      {Writes: readWritesNothing},
	"GET /api/v1/asks/open":                                              {Writes: readWritesNothing},
	"GET /api/v1/asks/{id}/followers":                                    {Writes: readWritesNothing},
	"GET /api/v1/issues/{key}/comments":                                  {Writes: readWritesNothing},
	"GET /api/v1/comments/{id}/blocks":                                   {Writes: readWritesNothing},
	"GET /api/v1/comments/{id}/history":                                  {Writes: readWritesNothing},
	"GET /api/v1/artifacts/{id}/asks":                                    {Writes: readWritesNothing},
	"GET /api/v1/artifacts/{id}/comments":                                {Writes: readWritesNothing},
	"GET /api/v1/artifacts/{id}/events":                                  {Writes: readWritesNothing},
	"GET /api/v1/artifacts/{id}/references":                              {Writes: readWritesNothing},
	"GET /api/v1/artifacts/{id}/subscribers":                             {Writes: readWritesNothing},
	"GET /api/v1/artifacts/{id}":                                         {Writes: readWritesNothing},
	"GET /api/v1/artifacts/{id}/reviews":                                 {Writes: readWritesNothing},
	"GET /api/v1/artifacts/{id}/versions/{number}":                       {Writes: readWritesNothing},
	"GET /api/v1/artifacts/{id}/changes":                                 {Writes: readWritesNothing},
	"GET /api/v1/issues/{key}/artifacts/{slug}":                          {Writes: readWritesNothing},
	"GET /api/v1/issues/{key}/artifacts/{slug}/versions/{number}":        {Writes: readWritesNothing},
	"GET /api/v1/projects/{key}/artifacts/{slug}":                        {Writes: readWritesNothing},
	"GET /api/v1/projects/{key}/artifacts/{slug}/summary":                {Writes: readWritesNothing},
	"GET /api/v1/projects/{key}/artifacts/{slug}/versions/{number}":      {Writes: readWritesNothing},
	"GET /api/v1/me/state":                                               {Writes: readWritesNothing},
	"GET /api/v1/me/agents/state":                                        {Writes: readWritesNothing},
	"GET /api/v1/me/documents/{id}/read":                                 {Writes: readWritesNothing},
	"GET /api/v1/credential-requests":                                    {Writes: readWritesNothing},
	"GET /api/v1/credential-grants":                                      {Writes: readWritesNothing},
	"GET /api/v1/machine-logins":                                         {Writes: readWritesNothing},
	"GET /api/v1/signins":                                                {Writes: readWritesNothing},
	"GET /api/v1/credentials/capabilities":                               {Writes: readWritesNothing},
	"GET /api/v1/settings/delivery":                                      {Writes: readWritesNothing},
	"GET /api/v1/projects/{key}/legion/daemon":                           {Writes: readWritesNothing},
	"GET /api/v1/projects/{key}/legion/board":                            {Writes: readWritesNothing},
	// The test hooks' GET rows, which read this process's memory.
	"GET /api/v1/artifacts/_test/rooms":                          {Writes: readWritesNothing},
	"GET /api/v1/artifacts/{id}/_test/room-counters":             {Writes: readWritesNothing},
	"GET /api/v1/artifacts/{id}/_test/live-write-pauses/{point}": {Writes: readWritesNothing},
	"GET /api/v1/messages/_test/queued":                          {Writes: readWritesNothing},
}

var readClasses = func() map[string]readEntry {
	classes := maps.Clone(v1ReadClasses)
	maps.Copy(classes, v2ReadClasses())
	return classes
}()

// v2ReadClasses records each mounted GET explicitly; a new GET needs a review even when it
// resembles an existing read. Rows sharing a core copy its class and note.
func v2ReadClasses() map[string]readEntry {
	read := readEntry{Writes: readWritesNothing}
	withNote := func(note string) readEntry { return readEntry{Writes: readWritesNothing, WriteNote: note} }
	return map[string]readEntry{
		"GET /api/v2":        v1ReadClasses["GET /api/v1"],
		"GET /api/v2/agents": v1ReadClasses["GET /api/v1/agents"],
		"GET /api/v2/agents/{session_id}/answers":                    v1ReadClasses["GET /api/v1/agents/{session_id}/answers"],
		"GET /api/v2/agents/{session_id}/artifacts":                  read, // Reads the conversation's artifact rows and stored versions.
		"GET /api/v2/agents/{session_id}/messages":                   v1ReadClasses["GET /api/v1/agents/{session_id}/messages"],
		"GET /api/v2/artifacts/{id}":                                 withNote("include=text or blocks reads live document text or blocks. " + docsCompactionNote),
		"GET /api/v2/artifacts/{id}/asks":                            v1ReadClasses["GET /api/v1/artifacts/{id}/asks"],
		"GET /api/v2/artifacts/{id}/blocks/{block_id}":               v1ReadClasses["GET /api/v1/artifacts/{id}/blocks/{block_id}"],
		"GET /api/v2/artifacts/{id}/changes":                         v1ReadClasses["GET /api/v1/artifacts/{id}/changes"],
		"GET /api/v2/artifacts/{id}/comments":                        v1ReadClasses["GET /api/v1/artifacts/{id}/comments"],
		"GET /api/v2/artifacts/{id}/events":                          v1ReadClasses["GET /api/v1/artifacts/{id}/events"],
		"GET /api/v2/artifacts/{id}/references":                      v1ReadClasses["GET /api/v1/artifacts/{id}/references"],
		"GET /api/v2/artifacts/{id}/reviews":                         v1ReadClasses["GET /api/v1/artifacts/{id}/reviews"],
		"GET /api/v2/artifacts/{id}/subscribers":                     v1ReadClasses["GET /api/v1/artifacts/{id}/subscribers"],
		"GET /api/v2/artifacts/{id}/versions":                        read, // Reads the artifact's stored version rows.
		"GET /api/v2/artifacts/{id}/versions/{number}":               v1ReadClasses["GET /api/v1/artifacts/{id}/versions/{number}"],
		"GET /api/v2/asks":                                           read, // Reads ask rows, optional source mapping and caller marks.
		"GET /api/v2/asks/{id}":                                      v1ReadClasses["GET /api/v1/asks/{id}"],
		"GET /api/v2/block-links":                                    v1ReadClasses["GET /api/v1/block-links"],
		"GET /api/v2/block-schema":                                   v1ReadClasses["GET /api/v1/schema/blocks"],
		"GET /api/v2/broadcasts":                                     v1ReadClasses["GET /api/v1/broadcasts"],
		"GET /api/v2/broadcasts/{id}":                                v1ReadClasses["GET /api/v1/broadcasts/{id}"],
		"GET /api/v2/ci-reconcile-passes":                            v1ReadClasses["GET /api/v1/ci-reconcile/passes"],
		"GET /api/v2/comments/{id}":                                  v1ReadClasses["GET /api/v1/comments/{id}"],
		"GET /api/v2/comments/{id}/corrections":                      v1ReadClasses["GET /api/v1/comments/{id}/history"],
		"GET /api/v2/credential-grants":                              v1ReadClasses["GET /api/v1/credential-grants"],
		"GET /api/v2/credential-requests":                            v1ReadClasses["GET /api/v1/credential-requests"],
		"GET /api/v2/dashboard-builds":                               v1ReadClasses["GET /api/v1/dashboard/builds"],
		"GET /api/v2/delivery/measures":                              v1ReadClasses["GET /api/v1/delivery/measures"],
		"GET /api/v2/delivery/pipeline":                              v1ReadClasses["GET /api/v1/delivery/pipeline"],
		"GET /api/v2/delivery/timeline":                              v1ReadClasses["GET /api/v1/delivery/timeline"],
		"GET /api/v2/deploy-runs/{id}":                               v1ReadClasses["GET /api/v1/delivery/runs/{id}"],
		"GET /api/v2/events":                                         v1ReadClasses["GET /api/v1/events/log"],
		"GET /api/v2/going-actions":                                  v1ReadClasses["GET "+goingCataloguePath],
		"GET /api/v2/going-requests/{id}/stream":                     v1ReadClasses["GET /api/v1/going/requests/{id}/stream"],
		"GET /api/v2/handoffs/{id}":                                  v1ReadClasses["GET /api/v1/handoffs/{id}"],
		"GET /api/v2/issues":                                         v1ReadClasses["GET /api/v1/issues"],
		"GET /api/v2/issues/{key}":                                   read, // The includes read issue facts, reach, design gates and sampled Legion state.
		"GET /api/v2/issues/{key}/artifacts":                         withNote("include=text or blocks reads live document text or blocks. " + docsCompactionNote),
		"GET /api/v2/issues/{key}/asks":                              v1ReadClasses["GET /api/v1/issues/{key}/asks"],
		"GET /api/v2/issues/{key}/checks":                            v1ReadClasses["GET /api/v1/issues/{key}/results"],
		"GET /api/v2/issues/{key}/collaborators":                     v1ReadClasses["GET /api/v1/issues/{key}/collaborators"],
		"GET /api/v2/issues/{key}/comments":                          v1ReadClasses["GET /api/v1/issues/{key}/comments"],
		"GET /api/v2/issues/{key}/coverage":                          v1ReadClasses["GET /api/v1/issues/{key}/coverage"],
		"GET /api/v2/issues/{key}/decisions":                         v1ReadClasses["GET /api/v1/issues/{key}/decisions"],
		"GET /api/v2/issues/{key}/events":                            v1ReadClasses["GET /api/v1/issues/{key}/events"],
		"GET /api/v2/issues/{key}/handoffs":                          v1ReadClasses["GET /api/v1/issues/{key}/handoffs"],
		"GET /api/v2/issues/{key}/messages":                          v1ReadClasses["GET /api/v1/issues/{key}/messages"],
		"GET /api/v2/issues/{key}/references":                        v1ReadClasses["GET /api/v1/issues/{key}/references"],
		"GET /api/v2/issues/{key}/role-holder-set":                   v1ReadClasses["GET /api/v1/issues/{key}/role-holders"],
		"GET /api/v2/issues/{key}/steps":                             v1ReadClasses["GET /api/v1/issues/{key}/steps"],
		"GET /api/v2/issues/{key}/subscribers":                       v1ReadClasses["GET /api/v1/issues/{key}/subscribers"],
		"GET /api/v2/known-sessions":                                 v1ReadClasses["GET /api/v1/agents/known"],
		"GET /api/v2/limit-set":                                      v1ReadClasses["GET /api/v1/limits"],
		"GET /api/v2/machine-logins":                                 v1ReadClasses["GET /api/v1/machine-logins"],
		"GET /api/v2/me":                                             v1ReadClasses[whoamiRoute],
		"GET /api/v2/me/agent-tokens":                                v1ReadClasses["GET /api/v1/me/agent-tokens"],
		"GET /api/v2/me/answers":                                     v1ReadClasses["GET /api/v1/me/answers"],
		"GET /api/v2/me/marks":                                       read, // Pages stored issue, agent, document, thread, message and ask marks.
		"GET /api/v2/me/marks/{kind}/{id}":                           read, // Reads one stored mark without setting it.
		"GET /api/v2/me/signins":                                     v1ReadClasses["GET /api/v1/signins"],
		"GET /api/v2/me/threads":                                     v1ReadClasses["GET /api/v1/me/replies"],
		"GET /api/v2/messages/{id}":                                  v1ReadClasses["GET /api/v1/messages/{id}"],
		"GET /api/v2/messages/{id}/corrections":                      v1ReadClasses["GET /api/v1/messages/{id}/history"],
		"GET /api/v2/near-copy-checks/{id}":                          v1ReadClasses["GET /api/v1/near-copy-checks/{id}"],
		"GET /api/v2/nodes":                                          v1ReadClasses["GET /api/v1/people/{login}/nodes"],
		"GET /api/v2/priorities":                                     v1ReadClasses["GET /api/v1/priorities"],
		"GET /api/v2/priorities/{id}/acts":                           v1ReadClasses["GET /api/v1/priorities/{id}/history"],
		"GET /api/v2/projects":                                       read, // The optional default assignee comes from stored project settings.
		"GET /api/v2/projects/{key}/architecture":                    v1ReadClasses["GET /api/v1/projects/{key}/architecture"],
		"GET /api/v2/projects/{key}/architecture-sources":            v1ReadClasses["GET /api/v1/projects/{key}/architecture-sources"],
		"GET /api/v2/projects/{key}/artifacts":                       withNote("include=text or blocks reads live document text or blocks. " + docsCompactionNote),
		"GET /api/v2/projects/{key}/issues":                          v1ReadClasses["GET /api/v1/issues"],
		"GET /api/v2/projects/{key}/legion/board":                    v1ReadClasses["GET /api/v1/projects/{key}/legion/board"],
		"GET /api/v2/projects/{key}/legion/daemon":                   v1ReadClasses["GET /api/v1/projects/{key}/legion/daemon"],
		"GET /api/v2/projects/{key}/near-copy-checks":                v1ReadClasses["GET /api/v1/near-copy-checks"],
		"GET /api/v2/projects/{key}/plans":                           v1ReadClasses["GET /api/v1/projects/{key}/plans"],
		"GET /api/v2/reference-targets":                              v1ReadClasses["GET /api/v1/reference-targets"],
		"GET /api/v2/references":                                     withNote("Writes nothing itself. With an after position, readReferences waits on the event horizon; the observation may persist event_horizon through writePersisted on its own context, as with since on /api/v1."),
		"GET /api/v2/related-check-stats":                            v1ReadClasses["GET /api/v1/related-checks"],
		"GET /api/v2/repositories":                                   v1ReadClasses["GET /api/v1/settings/repo-projects"],
		"GET /api/v2/repositories/{owner}/{repo}/app-installation":   v1ReadClasses["GET /api/v1/github/app-installation/{owner}/{repo}"],
		"GET /api/v2/search-coverage":                                v1ReadClasses["GET /api/v1/search/coverage"],
		"GET /api/v2/search-results":                                 v1ReadClasses["GET /api/v1/search/results"],
		"GET /api/v2/settings/credential-broker":                     v1ReadClasses["GET /api/v1/credentials/capabilities"],
		"GET /api/v2/settings/delivery":                              v1ReadClasses["GET /api/v1/settings/delivery"],
		"GET /api/v2/settings/new-issue":                             v1ReadClasses["GET /api/v1/settings/new-issue"],
		"GET /api/v2/settings/text-model":                            v1ReadClasses["GET /api/v1/going/capability"],
		"GET /api/v2/users":                                          v1ReadClasses["GET /api/v1/users"],
		"GET /api/v2/_test/artifacts/rooms":                          v1ReadClasses["GET /api/v1/artifacts/_test/rooms"],
		"GET /api/v2/_test/artifacts/{id}/room-counters":             v1ReadClasses["GET /api/v1/artifacts/{id}/_test/room-counters"],
		"GET /api/v2/_test/artifacts/{id}/live-write-pauses/{point}": v1ReadClasses["GET /api/v1/artifacts/{id}/_test/live-write-pauses/{point}"],
		"GET /api/v2/_test/messages/queued":                          v1ReadClasses["GET /api/v1/messages/_test/queued"],
	}
}

// readClassRefusals names every way readClasses fails to class the table, sorted, each line
// naming its row: a GET row with no entry, an entry whose Writes is unset or no class at all, a
// GET that writes (readWritesState) whose entry has no WriteNote, and an entry that names no GET
// row the table mounts, a POST row's name or a name no row holds. The walk is over the table with
// its test hooks in, as routesNotMounted's is, since a hook's row is a row of the table wherever
// a deployment leaves it out. register refuses to mount a table it names anything for, and
// TestEveryGETRowHasOneReadEntry reads this same answer, so the two cannot drift.
func readClassRefusals() []string {
	gets := map[string]bool{}
	var refused []string
	for _, route := range (&server{deps: Deps{TestHooksEnabled: true}}).routes() {
		if route.Method != http.MethodGet {
			continue
		}
		name := route.Method + " " + route.Pattern
		gets[name] = true
		entry, classed := readClasses[name]
		switch {
		case !classed:
			refused = append(refused, name+": a GET row with no entry in readClasses")
		case entry.Writes == readWritesUnset:
			refused = append(refused, name+": its entry leaves Writes unset")
		case entry.Writes != readWritesNothing && entry.Writes != readWritesState:
			refused = append(refused, name+": its entry's Writes is "+strconv.Itoa(int(entry.Writes))+", which is neither readWritesNothing nor readWritesState")
		case entry.Writes == readWritesState && strings.TrimSpace(entry.WriteNote) == "":
			refused = append(refused, name+": a GET that writes (readWritesState) whose entry has no WriteNote naming what it writes")
		}
	}
	for name := range readClasses {
		if !gets[name] {
			refused = append(refused, name+": readClasses names it, and it is no GET row the table mounts (test hooks in)")
		}
	}
	sort.Strings(refused)
	return refused
}
