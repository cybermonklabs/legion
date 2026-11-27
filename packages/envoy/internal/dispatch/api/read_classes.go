package api

import (
	"net/http"
	"sort"
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
	// readWritesNothing is a read: a route a read-only credential may call. Wherever its writes
	// run, on the request's own context or on the server's context in work the request starts or
	// waits on, such a route writes nothing but these eight:
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
	// A read that writes any of them on the server's context carries a WriteNote saying so. Every
	// other GET route is a GET that writes (readWritesState), which no read-only credential may
	// call.
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

// read_classes.go holds what a read class is: the two classes, what each means, and the note a
// class carries. Which class a route has is the route's own declaration (routeDecl.Read, one file
// per route), which routes() answers as apiRoute.Read and the read-only gate reads
// (refuseReadOnlyToken). register refuses to mount a table whose GET routes are not all classed,
// or whose write states a class (readClassRefusals, routes_table.go), so a GET route added later
// is served to no read-only credential until someone has read its handler and classed it.

// v2ReadClasses is the read class of every GET routeRow, keyed "GET /api/v2<pattern>", for each
// version the row is served in: a routeRow states no class of its own, and one served in both
// versions has one class at both paths (stampV2ReadClasses). It records each mounted GET
// explicitly, so a new GET needs a review even when it resembles an existing read. A row that
// shares a core with a /api/v1 route copies that route's class and note from its declaration
// (declaredRead), and the rows no route file declares (the INFRA-979 rows served in both
// versions) state their own.
func v2ReadClasses() map[string]readEntry {
	read := readEntry{Writes: readWritesNothing}
	withNote := func(note string) readEntry { return readEntry{Writes: readWritesNothing, WriteNote: note} }
	return map[string]readEntry{
		"GET /api/v2":        declaredRead("GET /api/v1"),
		"GET /api/v2/agents": declaredRead("GET /api/v1/agents"),
		"GET /api/v2/agents/{session_id}/answers":                  declaredRead("GET /api/v1/agents/{session_id}/answers"),
		"GET /api/v2/agents/{session_id}/artifacts":                read, // Reads the conversation's artifact rows and stored versions.
		"GET /api/v2/agents/{session_id}/messages":                 declaredRead("GET /api/v1/agents/{session_id}/messages"),
		"GET /api/v2/artifacts/{id}":                               withNote("include=text or blocks reads live document text or blocks. " + docsCompactionNote),
		"GET /api/v2/artifacts/{id}/asks":                          declaredRead("GET /api/v1/artifacts/{id}/asks"),
		"GET /api/v2/artifacts/{id}/blocks/{block_id}":             declaredRead("GET /api/v1/artifacts/{id}/blocks/{block_id}"),
		"GET /api/v2/artifacts/{id}/changes":                       declaredRead("GET /api/v1/artifacts/{id}/changes"),
		"GET /api/v2/artifacts/{id}/comments":                      declaredRead("GET /api/v1/artifacts/{id}/comments"),
		"GET /api/v2/artifacts/{id}/events":                        declaredRead("GET /api/v1/artifacts/{id}/events"),
		"GET /api/v2/artifacts/{id}/references":                    declaredRead("GET /api/v1/artifacts/{id}/references"),
		"GET /api/v2/artifacts/{id}/reviews":                       declaredRead("GET /api/v1/artifacts/{id}/reviews"),
		"GET /api/v2/artifacts/{id}/subscribers":                   declaredRead("GET /api/v1/artifacts/{id}/subscribers"),
		"GET /api/v2/artifacts/{id}/versions":                      read, // Reads the artifact's stored version rows.
		"GET /api/v2/artifacts/{id}/versions/{number}":             declaredRead("GET /api/v1/artifacts/{id}/versions/{number}"),
		"GET /api/v2/asks":                                         read, // Reads ask rows, optional source mapping and caller marks.
		"GET /api/v2/asks/{id}":                                    declaredRead("GET /api/v1/asks/{id}"),
		"GET /api/v2/block-links":                                  declaredRead("GET /api/v1/block-links"),
		"GET /api/v2/block-schema":                                 declaredRead("GET /api/v1/schema/blocks"),
		"GET /api/v2/broadcasts":                                   declaredRead("GET /api/v1/broadcasts"),
		"GET /api/v2/broadcasts/{id}":                              declaredRead("GET /api/v1/broadcasts/{id}"),
		"GET /api/v2/ci-reconcile-passes":                          declaredRead("GET /api/v1/ci-reconcile/passes"),
		"GET /api/v2/comments/{id}":                                declaredRead("GET /api/v1/comments/{id}"),
		"GET /api/v2/comments/{id}/corrections":                    declaredRead("GET /api/v1/comments/{id}/history"),
		"GET /api/v2/credential-grants":                            declaredRead("GET /api/v1/credential-grants"),
		"GET /api/v2/credential-requests":                          declaredRead("GET /api/v1/credential-requests"),
		"GET /api/v2/dashboard-builds":                             declaredRead("GET /api/v1/dashboard/builds"),
		"GET /api/v2/delivery/measures":                            {Writes: readWritesState, WriteNote: "As the timeline's: an insert into delivery_run_warnings for a deploy run with no matching production job or apply step, on the request's context, once per process for each run and configuration (firstToWarn)."},
		"GET /api/v2/delivery/pipeline":                            {Writes: readWritesState, WriteNote: "As the timeline's, for the deploy runs on main since the first day it shows: an insert into delivery_run_warnings for a run with no matching production job or apply step, on the request's context, once per process for each run and configuration (firstToWarn)."},
		"GET /api/v2/delivery/timeline":                            {Writes: readWritesState, WriteNote: "When a deploy run has no configured production job, or its production job has no configured apply step, the read inserts a delivery_run_warnings row on the request's context (firstToWarn, on conflict doing nothing), once per process for each run and configuration."},
		"GET /api/v2/deploy-runs/{id}":                             declaredRead("GET /api/v1/delivery/runs/{id}"),
		"GET /api/v2/deploys":                                      declaredRead("GET /api/v1/deploys"),
		"GET /api/v2/events":                                       declaredRead("GET /api/v1/events/log"),
		"GET /api/v2/going-actions":                                declaredRead("GET " + goingCataloguePath),
		"GET /api/v2/going-requests/{id}/stream":                   declaredRead("GET /api/v1/going/requests/{id}/stream"),
		"GET /api/v2/handoffs/{id}":                                declaredRead("GET /api/v1/handoffs/{id}"),
		"GET /api/v2/issues":                                       declaredRead("GET /api/v1/issues"),
		"GET /api/v2/issues/{key}":                                 read, // The includes read issue facts, reach, design gates and sampled Legion state.
		"GET /api/v2/issues/{key}/artifacts":                       withNote("include=text or blocks reads live document text or blocks. " + docsCompactionNote),
		"GET /api/v2/issues/{key}/asks":                            declaredRead("GET /api/v1/issues/{key}/asks"),
		"GET /api/v2/issues/{key}/checks":                          declaredRead("GET /api/v1/issues/{key}/results"),
		"GET /api/v2/issues/{key}/collaborators":                   declaredRead("GET /api/v1/issues/{key}/collaborators"),
		"GET /api/v2/issues/{key}/comments":                        declaredRead("GET /api/v1/issues/{key}/comments"),
		"GET /api/v2/issues/{key}/coverage":                        declaredRead("GET /api/v1/issues/{key}/coverage"),
		"GET /api/v2/issues/{key}/decisions":                       declaredRead("GET /api/v1/issues/{key}/decisions"),
		"GET /api/v2/issues/{key}/events":                          declaredRead("GET /api/v1/issues/{key}/events"),
		"GET /api/v2/issues/{key}/handoffs":                        declaredRead("GET /api/v1/issues/{key}/handoffs"),
		"GET /api/v2/issues/{key}/messages":                        declaredRead("GET /api/v1/issues/{key}/messages"),
		"GET /api/v2/issues/{key}/references":                      declaredRead("GET /api/v1/issues/{key}/references"),
		"GET /api/v2/issues/{key}/role-holder-set":                 declaredRead("GET /api/v1/issues/{key}/role-holders"),
		"GET /api/v2/issues/{key}/steps":                           declaredRead("GET /api/v1/issues/{key}/steps"),
		"GET /api/v2/issues/{key}/subscribers":                     declaredRead("GET /api/v1/issues/{key}/subscribers"),
		"GET /api/v2/known-sessions":                               declaredRead("GET /api/v1/agents/known"),
		"GET /api/v2/limit-set":                                    declaredRead("GET /api/v1/limits"),
		"GET /api/v2/machine-logins":                               declaredRead("GET /api/v1/machine-logins"),
		"GET /api/v2/me":                                           declaredRead(whoamiRoute),
		"GET /api/v2/me/agent-tokens":                              declaredRead("GET /api/v1/me/agent-tokens"),
		"GET /api/v2/me/answers":                                   declaredRead("GET /api/v1/me/answers"),
		"GET /api/v2/me/marks":                                     read, // Pages stored issue, agent, document, thread, message and ask marks.
		"GET /api/v2/me/marks/{kind}/{id}":                         read, // Reads one stored mark without setting it.
		"GET /api/v2/me/signins":                                   declaredRead("GET /api/v1/signins"),
		"GET /api/v2/me/threads":                                   declaredRead("GET /api/v1/me/replies"),
		"GET /api/v2/messages/{id}":                                declaredRead("GET /api/v1/messages/{id}"),
		"GET /api/v2/messages/{id}/corrections":                    declaredRead("GET /api/v1/messages/{id}/history"),
		"GET /api/v2/near-copy-checks/{id}":                        declaredRead("GET /api/v1/near-copy-checks/{id}"),
		"GET /api/v2/nodes":                                        declaredRead("GET /api/v1/people/{login}/nodes"),
		"GET /api/v2/priorities":                                   declaredRead("GET /api/v1/priorities"),
		"GET /api/v2/priorities/{id}/acts":                         declaredRead("GET /api/v1/priorities/{id}/history"),
		"GET /api/v2/projects":                                     read, // The optional default assignee comes from stored project settings.
		"GET /api/v2/projects/{key}/architecture":                  declaredRead("GET /api/v1/projects/{key}/architecture"),
		"GET /api/v2/projects/{key}/architecture-sources":          declaredRead("GET /api/v1/projects/{key}/architecture-sources"),
		"GET /api/v2/projects/{key}/artifacts":                     withNote("include=text or blocks reads live document text or blocks. " + docsCompactionNote),
		"GET /api/v2/projects/{key}/issues":                        declaredRead("GET /api/v1/issues"),
		"GET /api/v2/projects/{key}/legion/board":                  {Writes: readWritesNothing},
		"GET /api/v2/projects/{key}/legion/daemon":                 {Writes: readWritesNothing},
		"GET /api/v2/projects/{key}/near-copy-checks":              declaredRead("GET /api/v1/near-copy-checks"),
		"GET /api/v2/projects/{key}/plans":                         declaredRead("GET /api/v1/projects/{key}/plans"),
		"GET /api/v2/reference-targets":                            declaredRead("GET /api/v1/reference-targets"),
		"GET /api/v2/references":                                   withNote("Writes nothing itself. With an after position, readReferences waits on the event horizon; the observation may persist event_horizon through writePersisted on its own context, as with since on /api/v1."),
		"GET /api/v2/related-check-stats":                          declaredRead("GET /api/v1/related-checks"),
		"GET /api/v2/repositories":                                 declaredRead("GET /api/v1/settings/repo-projects"),
		"GET /api/v2/repositories/{owner}/{repo}/app-installation": declaredRead("GET /api/v1/github/app-installation/{owner}/{repo}"),
		"GET /api/v2/search-coverage":                              declaredRead("GET /api/v1/search/coverage"),
		"GET /api/v2/search-results":                               declaredRead("GET /api/v1/search/results"),
		"GET /api/v2/settings/credential-broker":                   declaredRead("GET /api/v1/credentials/capabilities"),
		"GET /api/v2/settings/delivery":                            {Writes: readWritesNothing},
		"GET /api/v2/settings/new-issue":                           declaredRead("GET /api/v1/settings/new-issue"),
		"GET /api/v2/settings/text-model":                          declaredRead("GET /api/v1/going/capability"),
		"GET /api/v2/users":                                        declaredRead("GET /api/v1/users"),
	}
}

// declaredRead is the read class the route file of name declares, by "<method> <pattern>", and
// the zero entry, which classes nothing, where no route file declares it: a routeRow that names a
// route nobody declares is then a GET with no class, which register refuses (readClassRefusals).
func declaredRead(name string) readEntry {
	route, _ := declaredRoute(name)
	return route.Read
}

// stampV2ReadClasses stamps each GET routeRow's class from v2ReadClasses onto its mounts, at
// /api/v2 and, for a row served in both versions, at /api/v1. A route file's declaration carries
// its own class (v1Route).
func stampV2ReadClasses(routes []apiRoute) {
	classes := v2ReadClasses()
	for index, route := range routes {
		if route.columns && route.Method == http.MethodGet {
			routes[index].Read = classes[v2ReadClassKey(route)]
		}
	}
}

// v2ReadClassKey is the key v2ReadClasses holds a routeRow mount's class under: GET and the row's
// /api/v2 pattern, whichever version the mount serves.
func v2ReadClassKey(route apiRoute) string {
	return route.Method + " /api/v2" + strings.TrimPrefix(route.Pattern, versionPrefix(route.Version))
}

// v2ReadClassesUnmounted names, sorted, each entry of v2ReadClasses that classes no GET routeRow
// of routes: a row renamed or deleted and its entry left behind reads as though it classed a
// route that is not there (readClassRefusals).
func v2ReadClassesUnmounted(routes []apiRoute) []string {
	mounted := map[string]bool{}
	for _, route := range routes {
		if route.columns && route.Method == http.MethodGet {
			mounted[v2ReadClassKey(route)] = true
		}
	}
	var unmounted []string
	for name := range v2ReadClasses() {
		if !mounted[name] {
			unmounted = append(unmounted, name+": v2ReadClasses names it, and it is no GET routeRow the table mounts")
		}
	}
	sort.Strings(unmounted)
	return unmounted
}
