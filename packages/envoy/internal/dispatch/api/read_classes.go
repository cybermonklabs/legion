package api

import (
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

// asOfKind is how one GET row of the table answers a read at a past time, the cut: a request's
// as_of query parameter, or the cut a read-only token carries (model.Credential.Cut).
type asOfKind int

const (
	// asOfUnset is the zero value: an entry that decides nothing. It is never a kind, so an
	// entry that leaves AsOf unset is a row nobody has decided for as_of yet.
	asOfUnset asOfKind = iota
	// asOfAnswers is a row that answers what existed at the cut, from the records Dispatch
	// keeps (package asof): the cut of an as_of, or the cut of a read-only token, which wins
	// over any as_of the request sends (asOfGate). The read class's table of these rows is the
	// plan's table "Routes and their kinds" (INFRA-4620).
	asOfAnswers
	// asOfNoHistory is a row that holds no history: it answers the present only, refuses every
	// as_of 400 AS_OF_NOT_SERVED (asOfGate), and refuses a read-only token that carries a cut
	// 403 AS_OF_TOKEN_NOT_SERVED (refuseReadOnlyToken).
	asOfNoHistory
	// asOfCaller is GET /api/v1/whoami alone: it describes the caller now, the cut its token
	// carries included, and refuses as_of as a row that holds no history does.
	asOfCaller
)

// readEntry is the read class of one GET row of the table: what the row writes, the note that
// says so where the class alone does not, and how the row answers a read at a past time.
type readEntry struct {
	// Writes is the row's class: readWritesNothing or readWritesState, never readWritesUnset.
	Writes readWrites
	// WriteNote names what the row writes, and the statement or the function that writes it. A
	// readWritesState row always carries one. A readWritesNothing row carries one when its
	// handler makes the server write on its own context, or when its class differs from what a
	// first reading of its handler suggests, and then it says why.
	WriteNote string
	// AsOf is the row's as_of kind: asOfAnswers, asOfNoHistory or asOfCaller, never asOfUnset.
	AsOf asOfKind
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
// Each entry also names the row's as_of kind (AsOf). The rows of the plan's table "Routes and
// their kinds" (INFRA-4620) answer as_of (asOfAnswers), GET /api/v1/whoami describes the caller
// now (asOfCaller), and every other GET row holds no history (asOfNoHistory). The as_of gate
// reads the kind (asOfGate).
//
// routes() stamps each entry onto its row (apiRoute.Read), which is what the read-only gate and
// the as_of gate read (refuseReadOnlyToken, asOfGate), and register refuses to mount a table with
// a GET row this map does not class, or whose entry leaves AsOf unset, or with an entry that
// names no GET row it mounts (readClassRefusals), so a GET row added later is served to no
// read-only token and answers no as_of until someone has read its handler and classed it here.
//
// The entries are grouped by class, each group in the order of the table.
var readClasses = map[string]readEntry{
	// The GETs that write.
	"GET /api/v1/delivery/timeline":          {Writes: readWritesState, AsOf: asOfNoHistory, WriteNote: "When a deploy run has no configured production job, or its production job has no configured apply step, the read inserts a delivery_run_warnings row on the request's context (firstToWarn, on conflict doing nothing), once per process for each run and configuration."},
	"GET /api/v1/delivery/measures":          {Writes: readWritesState, AsOf: asOfNoHistory, WriteNote: "As the timeline's: an insert into delivery_run_warnings for a deploy run with no matching production job or apply step, on the request's context, once per process for each run and configuration (firstToWarn)."},
	"GET /api/v1/delivery/pipeline":          {Writes: readWritesState, AsOf: asOfNoHistory, WriteNote: "As the timeline's, for the deploy runs on main since the first day it shows: an insert into delivery_run_warnings for a run with no matching production job or apply step, on the request's context, once per process for each run and configuration (firstToWarn)."},
	"GET /api/v1/going/requests/{id}/stream": {Writes: readWritesState, AsOf: asOfNoHistory, WriteNote: "When the run is past its ends_at and holds no terminal frame, the read inserts the run's end frame, the error that its answer did not arrive, into going_events on the request's context (endGoingRun), doing nothing on conflict when another writer ended the run first."},

	// The reads whose handler makes the server write on its own context, or whose class a first
	// reading of the handler would get wrong.
	"GET /api/v1/projects/{key}/related":                 {Writes: readWritesNothing, AsOf: asOfNoHistory, WriteNote: "When the project has no search_term_totals row, the related-item check starts a refresh of the project's term statistics on the server's own context (ensureSearchTermStats, once per project and process): it inserts and updates search_term_totals, deletes and inserts search_term_stats, and deletes the project's search_term_queue rows. The statistics are a cache of what the project holds, which every related check reads and the first to find them missing rebuilds; nothing else is written."},
	"GET /api/v1/github/app-installation/{owner}/{repo}": {Writes: readWritesNothing, AsOf: asOfNoHistory, WriteNote: "Writes nothing itself. When GitHub answers the installation lookup with a rate limit, the GitHub client records that limit in github_rate_limits on a context detached from the request (ratelimit.Record), as it does for every caller that reads GitHub: the row is GitHub's own state, not a fact of Dispatch's."},
	"GET /api/v1/references":                             {Writes: readWritesNothing, AsOf: asOfNoHistory, WriteNote: "Writes nothing itself. With ?since=, the read waits on the event horizon, as the event log does, and the horizon's observation, which may run on this request's goroutine, writes event_horizon on a context of its own (writePersisted), at most once a second, after its waiters have their answer: the server's write for every reader alike. Without ?since= it does not read the horizon."},
	"GET /api/v1/issues/{key}/results":                   {Writes: readWritesNothing, AsOf: asOfAnswers, WriteNote: "A read-only caller's read reads GitHub and stores nothing. A writer's read stores what it read from GitHub, as it always has: the pull request heads of a result's commit (pull_request_heads), with the branch commits (branch_commits) and the attribution events (events) that come with them. Like every reader of GitHub, the GitHub client may record GitHub's rate limit in github_rate_limits on a detached context, and the check blocks are read through the document service, whose cold read may queue a compaction (docsCompactionNote)."},
	"GET /api/v1/issues/{key}/coverage":                  {Writes: readWritesNothing, AsOf: asOfNoHistory, WriteNote: docsCompactionNote},
	"GET /api/v1/issues/{key}/decisions":                 {Writes: readWritesNothing, AsOf: asOfNoHistory, WriteNote: docsCompactionNote},
	"GET /api/v1/asks/{id}":                              {Writes: readWritesNothing, AsOf: asOfAnswers, WriteNote: "The ask's anchor block is read through the document service. " + docsCompactionNote},
	"GET /api/v1/comments/{id}":                          {Writes: readWritesNothing, AsOf: asOfAnswers, WriteNote: "The comment's anchor block is read through the document service. " + docsCompactionNote},
	"GET /api/v1/artifacts/{id}/blocks":                  {Writes: readWritesNothing, AsOf: asOfNoHistory, WriteNote: docsCompactionNote},
	"GET /api/v1/artifacts/{id}/blocks/{block_id}":       {Writes: readWritesNothing, AsOf: asOfNoHistory, WriteNote: docsCompactionNote},
	"GET /api/v1/artifacts/{id}/text":                    {Writes: readWritesNothing, AsOf: asOfAnswers, WriteNote: docsCompactionNote},
	"GET /api/v1/issues/{key}/artifacts/{slug}/text":     {Writes: readWritesNothing, AsOf: asOfNoHistory, WriteNote: docsCompactionNote},
	"GET /api/v1/issues/{key}/artifacts/{slug}/blocks":   {Writes: readWritesNothing, AsOf: asOfAnswers, WriteNote: docsCompactionNote},
	"GET /api/v1/projects/{key}/artifacts/{slug}/text":   {Writes: readWritesNothing, AsOf: asOfNoHistory, WriteNote: docsCompactionNote},
	"GET /api/v1/projects/{key}/artifacts/{slug}/blocks": {Writes: readWritesNothing, AsOf: asOfAnswers, WriteNote: docsCompactionNote},
	"GET /api/v1/events/log":                             {Writes: readWritesNothing, AsOf: asOfAnswers, WriteNote: "Writes nothing itself. Reading the safe horizon (events.Horizon) may run the horizon's observation on this request's goroutine, and that observation writes event_horizon on a context of its own (writePersisted), at most once a second, after its waiters have their answer: the server's write for every reader of the event log alike, not this request's."},

	// The other reads.
	"GET /api/v1":                                                        {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/schema/blocks":                                          {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/limits":                                                 {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/projects":                                               {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/projects/{key}/artifacts":                               {Writes: readWritesNothing, AsOf: asOfAnswers},
	"GET /api/v1/settings/repo-projects":                                 {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/settings/architecture-sources":                          {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/projects/{key}/architecture-sources":                    {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/projects/{key}/architecture-source":                     {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/projects/{key}/architecture":                            {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/projects/{key}/plans":                                   {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/settings/project-default-assignees":                     {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/settings/new-issue":                                     {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/settings/delivery":                                      {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/projects/{key}/legion/daemon":                           {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/projects/{key}/legion/board":                            {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/delivery/runs/{id}":                                     {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/ci-reconcile/passes":                                    {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/me/agent-tokens":                                        {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/users":                                                  {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/whoami":                                                 {Writes: readWritesNothing, AsOf: asOfCaller},
	"GET /api/v1/dashboard/builds":                                       {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/priorities":                                             {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/priorities/{id}/history":                                {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/issues":                                                 {Writes: readWritesNothing, AsOf: asOfAnswers},
	"GET /api/v1/issues/resolve":                                         {Writes: readWritesNothing, AsOf: asOfAnswers},
	"GET /api/v1/issues/{key}":                                           {Writes: readWritesNothing, AsOf: asOfAnswers},
	"GET /api/v1/issues/{key}/summary":                                   {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/issues/{key}/events":                                    {Writes: readWritesNothing, AsOf: asOfAnswers},
	"GET /api/v1/issues/{key}/references":                                {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/issues/{key}/collaborators":                             {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/references/questions":                                   {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/reference-targets":                                      {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/block-links":                                            {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/issues/{key}/subscribers":                               {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/issues/{key}/artifacts":                                 {Writes: readWritesNothing, AsOf: asOfAnswers},
	"GET /api/v1/issues/{key}/messages":                                  {Writes: readWritesNothing, AsOf: asOfAnswers},
	"GET /api/v1/issues/{key}/messages/{id}":                             {Writes: readWritesNothing, AsOf: asOfAnswers},
	"GET /api/v1/issues/{key}/handoffs":                                  {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/issues/{key}/handoffs/{phase}/latest":                   {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/issues/{key}/steps":                                     {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/issues/{key}/role-holders":                              {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/handoffs/{id}":                                          {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/messages/{id}":                                          {Writes: readWritesNothing, AsOf: asOfAnswers},
	"GET /api/v1/messages/{id}/blocks":                                   {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/messages/{id}/history":                                  {Writes: readWritesNothing, AsOf: asOfAnswers},
	"GET /api/v1/inbox":                                                  {Writes: readWritesNothing, AsOf: asOfAnswers},
	"GET /api/v1/me/answers":                                             {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/me/replies":                                             {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/search":                                                 {Writes: readWritesNothing, AsOf: asOfAnswers},
	"GET /api/v1/search/results":                                         {Writes: readWritesNothing, AsOf: asOfAnswers},
	"GET /api/v1/search/coverage":                                        {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET " + goingCataloguePath:                                          {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/related-checks":                                         {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/near-copy-checks":                                       {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/near-copy-checks/{id}":                                  {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/going/capability":                                       {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/agents":                                                 {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/agents/{session_id}/answers":                            {Writes: readWritesNothing, AsOf: asOfAnswers},
	"GET /api/v1/agents/{session_id}/messages":                           {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/agents/{session_id}/artifacts/{slug}":                   {Writes: readWritesNothing, AsOf: asOfAnswers},
	"GET /api/v1/agents/{session_id}/artifacts/{slug}/versions/{number}": {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/broadcasts":                                             {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/broadcasts/{id}":                                        {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/issues/{key}/asks":                                      {Writes: readWritesNothing, AsOf: asOfAnswers},
	"GET /api/v1/asks/open":                                              {Writes: readWritesNothing, AsOf: asOfAnswers},
	"GET /api/v1/asks/{id}/followers":                                    {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/issues/{key}/comments":                                  {Writes: readWritesNothing, AsOf: asOfAnswers},
	"GET /api/v1/comments/{id}/blocks":                                   {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/comments/{id}/history":                                  {Writes: readWritesNothing, AsOf: asOfAnswers},
	"GET /api/v1/artifacts/{id}/asks":                                    {Writes: readWritesNothing, AsOf: asOfAnswers},
	"GET /api/v1/artifacts/{id}/comments":                                {Writes: readWritesNothing, AsOf: asOfAnswers},
	"GET /api/v1/artifacts/{id}/events":                                  {Writes: readWritesNothing, AsOf: asOfAnswers},
	"GET /api/v1/artifacts/{id}/references":                              {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/artifacts/{id}/subscribers":                             {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/artifacts/{id}":                                         {Writes: readWritesNothing, AsOf: asOfAnswers},
	"GET /api/v1/artifacts/{id}/reviews":                                 {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/artifacts/{id}/versions/{number}":                       {Writes: readWritesNothing, AsOf: asOfAnswers},
	"GET /api/v1/issues/{key}/artifacts/{slug}":                          {Writes: readWritesNothing, AsOf: asOfAnswers},
	"GET /api/v1/issues/{key}/artifacts/{slug}/versions/{number}":        {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/projects/{key}/artifacts/{slug}":                        {Writes: readWritesNothing, AsOf: asOfAnswers},
	"GET /api/v1/projects/{key}/artifacts/{slug}/summary":                {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/projects/{key}/artifacts/{slug}/versions/{number}":      {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/me/state":                                               {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/me/agents/state":                                        {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/me/documents/{id}/read":                                 {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/credential-requests":                                    {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/credential-grants":                                      {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/machine-logins":                                         {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/signins":                                                {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/credentials/capabilities":                               {Writes: readWritesNothing, AsOf: asOfNoHistory},
	// The test hooks' GET rows, which read this process's memory.
	"GET /api/v1/artifacts/_test/rooms":                          {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/artifacts/{id}/_test/room-counters":             {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/artifacts/{id}/_test/live-write-pauses/{point}": {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/messages/_test/queued":                          {Writes: readWritesNothing, AsOf: asOfNoHistory},
}

// readClassRefusals names every way readClasses fails to class the table, sorted, each line
// naming its row: a GET row with no entry, an entry whose Writes is unset or no class at all, a
// GET that writes (readWritesState) whose entry has no WriteNote, an entry whose AsOf is unset or
// no kind at all, and an entry that names no GET row the table mounts, a POST row's name or a
// name no row holds. The walk is over the table with its test hooks in, as routesNotMounted's
// is, since a hook's row is a row of the table wherever a deployment leaves it out. register
// refuses to mount a table it names anything for, and TestEveryGETRowHasOneReadEntry reads this
// same answer, so the two cannot drift.
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
		if !classed {
			refused = append(refused, name+": a GET row with no entry in readClasses")
			continue
		}
		switch {
		case entry.Writes == readWritesUnset:
			refused = append(refused, name+": its entry leaves Writes unset")
		case entry.Writes != readWritesNothing && entry.Writes != readWritesState:
			refused = append(refused, name+": its entry's Writes is "+strconv.Itoa(int(entry.Writes))+", which is neither readWritesNothing nor readWritesState")
		case entry.Writes == readWritesState && strings.TrimSpace(entry.WriteNote) == "":
			refused = append(refused, name+": a GET that writes (readWritesState) whose entry has no WriteNote naming what it writes")
		}
		switch entry.AsOf {
		case asOfAnswers, asOfNoHistory, asOfCaller:
		case asOfUnset:
			refused = append(refused, name+": its entry leaves AsOf unset")
		default:
			refused = append(refused, name+": its entry's AsOf is "+strconv.Itoa(int(entry.AsOf))+", which is neither asOfAnswers, asOfNoHistory nor asOfCaller")
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
