package api

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// rowDrive is how a drive sends one GET row of the table on the seeded store (newDriveWorld) and
// what it must see: the request it makes, a statement its handler must run, and how its answer is
// compared.
type rowDrive struct {
	// target is the path and query the drive sends, every wildcard of the row's pattern filled,
	// each $name standing for a value of the seeded store (driveWorld.fill). The table's GET rows
	// must route it to the row's own pattern, filled or not (sendsItsRow), so a drive keyed by one
	// row never sends another.
	target string
	// statement is one statement the row's handler must run: a read of the store for a read, the
	// write its WriteNote names for a GET that writes. Every read-only window of the row must
	// hold it, so a tracer that missed the handler's work cannot pass the row.
	statement *regexp.Regexp
	// fromMemory says why a read's handler runs no statement of its own, where it names none.
	fromMemory string
	// status is the status the row answers both credentials of each pair on the seeded store,
	// and 0 for 200.
	status int
	// volatile is every member of the row's answer that two answers on one state differ in,
	// each with its reason; they are left out of the comparison.
	volatile []volatileMember
	// credential marks a row whose answer describes the credential that asked.
	credential *credentialAnswer
	// stored, for a GET that writes, counts the rows its write stores: none before the drive and
	// one after it. Its $names are filled as the target's are.
	stored string
}

// statementOf is a statement a drive must see: pattern matched case-insensitively across lines.
func statementOf(pattern string) *regexp.Regexp {
	return regexp.MustCompile(`(?is)` + pattern)
}

// noBroker is why every route that relays the secrets broker reads nothing: the rig configures
// no broker (Deps.AgentSecrets), so each answers its no-broker answer from memory.
const noBroker = "it relays the secrets broker, which this rig does not configure, and answers its no-broker answer from memory"

// testHookRead is why a test hook's read reads nothing: it reads this process's memory.
const testHookRead = "a test hook, it reads this process's memory and no row"

// documentHookRead is why a document test hook's read reads nothing and answers 404: the rig's
// document service is built without its test hooks (newRegisteredTestServer), so the hook
// answers TEST_HOOKS_OFF from memory, to the read-only credential and its writer alike.
const documentHookRead = "a document test hook on a document service built without its hooks, it answers TEST_HOOKS_OFF from memory"

// readTime is why a member that names the moment of the read is volatile: each read stamps its
// own clock.
const readTime = "the moment the read was answered, which each read takes from its own clock"

// searchTook is why a search's took_ms is volatile: it is how long that search ran.
const searchTook = "how long the search ran, which each read times on its own clock"

// postHistoryStatement is the one read of a post's kept bodies (postHistory): the lateral page of
// post_body_history, in SQL the statement writes itself, which both history rows run on the
// post the seed corrected once.
const postHistoryStatement = `\bleft\s+join\s+lateral\s*\(\s*select\s+body,\s*edited_by,\s*edited_at\s+from\s+post_body_history\b`

// readRowDrives is the drive of every read row of the table (readWritesNothing), keyed
// "GET <pattern>" as readClasses is: the request that shows the row writes nothing a read may
// not write and answers a read-only token as it answers that token's writer.
var readRowDrives = map[string]rowDrive{
	"GET /api/v1":               {target: "/api/v1", fromMemory: "the route index is built in memory when the table is registered"},
	"GET /api/v1/schema/blocks": {target: "/api/v1/schema/blocks", fromMemory: "the block schema is a constant of the document package"},
	"GET /api/v1/limits":        {target: "/api/v1/limits", fromMemory: "the limits are constants"},
	"GET /api/v1/projects":      {target: "/api/v1/projects", statement: statementOf(`\bcoalesce\(open_asks\.total\b`)},
	"GET /api/v1/projects/{key}/artifacts": {target: "/api/v1/projects/$project/artifacts",
		statement: statementOf(`\bfrom\s+artifacts\s+where\s+project_key\s*=\s*\$1\s+and\s+not\s+is_primary\b`)},
	"GET /api/v1/projects/{key}/related": {
		target:    "/api/v1/projects/$relatedProject/related?title=Drive+every+row&text=The+issue+page+lists+every+open+ask",
		statement: statementOf(`\btext_lexemes\s+as\s*\(`),
	},
	"GET /api/v1/settings/repo-projects": {target: "/api/v1/settings/repo-projects", statement: statementOf(`\bfrom\s+repo_projects\s+order\s+by\s+repo\b`)},
	// The handler asks GitHub; its one statement is the GitHub client's read of the rate limit
	// GitHub last answered, which it reads before it asks.
	"GET /api/v1/github/app-installation/{owner}/{repo}": {target: "/api/v1/github/app-installation/acme/widgets",
		statement: statementOf(`\bfrom\s+github_rate_limits\s+where\s+scope\b`)},
	"GET /api/v1/settings/architecture-sources":       {target: "/api/v1/settings/architecture-sources", statement: statementOf(`\bfrom\s+architecture_sources\b`)},
	"GET /api/v1/projects/{key}/architecture-sources": {target: "/api/v1/projects/$project/architecture-sources", statement: statementOf(`\bfrom\s+architecture_sources\b`)},
	"GET /api/v1/projects/{key}/architecture-source":  {target: "/api/v1/projects/$project/architecture-source", statement: statementOf(`\bfrom\s+architecture_sources\b`)},
	"GET /api/v1/projects/{key}/architecture": {target: "/api/v1/projects/$project/architecture",
		statement: statementOf(`\bfrom\s+components\s+c\s+join\s+architecture_sources\b`)},
	"GET /api/v1/projects/{key}/plans": {target: "/api/v1/projects/$project/plans",
		statement: statementOf(`\bfrom\s+issues\s+i\s+left\s+join\s+\(\s*workflow_tuples\b`)},
	"GET /api/v1/settings/project-default-assignees": {target: "/api/v1/settings/project-default-assignees",
		statement: statementOf(`\bselect\s+key,\s*default_assignee\s+from\s+projects\b`)},
	"GET /api/v1/settings/new-issue":           {target: "/api/v1/settings/new-issue", fromMemory: "the new-issue settings are the deployment's configuration, held in memory"},
	"GET /api/v1/settings/delivery":            {target: "/api/v1/settings/delivery", statement: statementOf(`\bfrom\s+delivery_settings\b`)},
	"GET /api/v1/projects/{key}/legion/daemon": {target: "/api/v1/projects/$project/legion/daemon", statement: statementOf(`\bfrom\s+legion_settings\s+where\s+project\b`)},
	"GET /api/v1/projects/{key}/legion/board": {target: "/api/v1/projects/$project/legion/board", statement: statementOf(`\bfrom\s+legion_reads\b`),
		volatile: []volatileMember{{"computed_at", readTime}}},
	"GET /api/v1/delivery/runs/{id}": {target: "/api/v1/delivery/runs/$pipelineRun",
		statement: statementOf(`\bfrom\s+delivery_run_jobs\s+where\s+repo\s*=\s*\$1\s+and\s+run_id\s*=\s*\$2\b`)},
	"GET /api/v1/ci-reconcile/passes": {target: "/api/v1/ci-reconcile/passes", statement: statementOf(`\bci_reconcile_passes\b`)},
	"GET /api/v1/me/agent-tokens":     {target: "/api/v1/me/agent-tokens", statement: statementOf(`\bfrom\s+agent_read_tokens\s+where\s+owner\s*=\s*\$1\b`)},
	"GET /api/v1/users":               {target: "/api/v1/users", statement: statementOf(`\bselect\s+email\s+from\s+people\s+order\s+by\s+email\b`)},
	// P's page lists P's reply (the seed's comments), so the route runs its list, the statement
	// that runs only once the page is shown. Every credential of the drive acts as P, so a page
	// shown to nobody would answer P, its own reader, an empty 200, which the list's absence alone
	// tells apart.
	"GET /api/v1/people/{login}/nodes": {target: "/api/v1/people/$person/nodes",
		statement: statementOf(`\bjoin\s+comments\s+c\s+on\s+r\.from_kind\b`)},
	"GET /api/v1/whoami": {target: "/api/v1/whoami", fromMemory: "it answers the credential the request resolved", credential: &credentialAnswer{
		reason:  "whoami answers the credential's own scopes: a read-only one's hold read and never approve, its writer's do not hold read",
		members: []string{"scopes"},
		holds:   holdsReadAndNotApprove,
	}},
	"GET /api/v1/dashboard/builds":        {target: "/api/v1/dashboard/builds", statement: statementOf(`\bfrom\s+dashboard_builds\b`)},
	"GET /api/v1/priorities":              {target: "/api/v1/priorities", statement: statementOf(`\bfrom\s+priority_nodes\s+where\s+kind\s*=\s*'priority'\s+and\s+state\b`)},
	"GET /api/v1/priorities/{id}/history": {target: "/api/v1/priorities/$priority/history", statement: statementOf(`\bfrom\s+priority_nodes\s+where\s+kind\s*=\s*'priority'\s+and\s+id\b`)},
	"GET /api/v1/issues":                  {target: "/api/v1/issues?project=$project", statement: statementOf(`\bwith\s+recursive\s+advances_relations\s+as\b`)},
	"GET /api/v1/issues/resolve":          {target: "/api/v1/issues/resolve?ref=$issue", statement: statementOf(`\bselect\s+key\s+from\s+issues\s+where\s+key\s*=\s*\$1\b`)},
	"GET /api/v1/issues/{key}":            {target: "/api/v1/issues/$issue", statement: statementOf(`\bfrom\s+design_gates\s+where\s+issue\b`)},
	"GET /api/v1/issues/{key}/summary":    {target: "/api/v1/issues/$issue/summary", statement: statementOf(`\bselect\s+key,\s*project_key,\s*title,\s*status\s+from\s+issues\b`)},
	"GET /api/v1/issues/{key}/events":     {target: "/api/v1/issues/$issue/events", statement: statementOf(`\bselect\s+last_seq,\s*created_at\s+from\s+issues\b`)},
	// refs.Closure tags each statement it runs (refs.ClosureTag), its one-hop reads in each direction.
	"GET /api/v1/issues/{key}/references": {target: "/api/v1/issues/$issue/references", statement: statementOf(regexp.QuoteMeta(refs.ClosureTag))},
	"GET /api/v1/issues/{key}/collaborators": {target: "/api/v1/issues/$issue/collaborators",
		statement: statementOf(`\bselect\s+assignee\s+from\s+issues\s+where\s+key\b`)},
	"GET /api/v1/references":           {target: "/api/v1/references?to=$issueRef&since=0", statement: statementOf(`\bfrom\s+graph_edges\s+g\b`)},
	"GET /api/v1/references/questions": {target: "/api/v1/references/questions?kind=artifact&id=$spec", statement: statementOf(`\bfrom\s+artifact_versions\s+where\s+artifact_id\s*=\s*\$1::uuid\s+and\s+markdown\s+is\s+not\s+null\b`)},
	"GET /api/v1/reference-targets":    {target: "/api/v1/reference-targets?ref=$issueRef&ref=$specRef", statement: statementOf(`\ba\.ref_key,\s*a\.issue_key\b`)},
	"GET /api/v1/block-links":          {target: "/api/v1/block-links?from=$linksRef&to=$specRef", statement: statementOf(`\bblock_refs\b`)},
	"GET /api/v1/issues/{key}/subscribers": {target: "/api/v1/issues/$issue/subscribers",
		fromMemory: "it lists the listener's interests and sessions, and reads no row"},
	"GET /api/v1/issues/{key}/artifacts": {target: "/api/v1/issues/$issue/artifacts", statement: statementOf(`\bfrom\s+artifacts\s+where\s+issue_key\s*=\s*\$1\s+and\s+retired_at\s+is\s+null\s+order\s+by\b`)},
	// The after_merge and default_branch checks' read of whether a commit is on their branch.
	"GET /api/v1/issues/{key}/results": {target: "/api/v1/issues/$issue/results", statement: statementOf(`\bfrom\s+branch_commits\s+where\s+repo\b`),
		volatile: []volatileMember{
			{"checks.*.compared_with.*.learned_at.at", "the moment the read learned a pull request's head from GitHub, its own clock: the read-only read stores nothing and keeps nothing after its request, so its writer's read asks GitHub again"},
			{"checks.*.results.*.commit_stands.basis.learned_at.at", "the same moment, on the basis each result stands at"},
		}},
	"GET /api/v1/issues/{key}/coverage":      {target: "/api/v1/issues/$issue/coverage", statement: statementOf(`\bt\.type\s*=\s*'requirement'`)},
	"GET /api/v1/issues/{key}/messages":      {target: "/api/v1/issues/$issue/messages", statement: statementOf(`\bfrom\s+messages\s+where\s+issue_key\s*=\s*\$1\s+and\s+in_reply_to\s+is\s+null\b`)},
	"GET /api/v1/issues/{key}/messages/{id}": {target: "/api/v1/issues/$issue/messages/$message", statement: statementOf(`\bfrom\s+messages\s+where\s+id\s*=\s*\$1\s+and\s+issue_key\s*=\s*\$2\b`)},
	"GET /api/v1/issues/{key}/handoffs":      {target: "/api/v1/issues/$issue/handoffs", statement: statementOf(`\bfrom\s+handoffs\s+where\s+issue_key\s*=\s*\$1\s+order\s+by\b`)},
	"GET /api/v1/issues/{key}/handoffs/{phase}/latest": {target: "/api/v1/issues/$issue/handoffs/$phase/latest",
		statement: statementOf(`\bfrom\s+handoffs\s+where\s+issue_key\s*=\s*\$1\s+and\s+phase\b`)},
	"GET /api/v1/issues/{key}/steps":        {target: "/api/v1/issues/$issue/steps", statement: statementOf(`\bfrom\s+workflow_tuples\s+t\s+join\s+workflow_nodes\s+n\b`)},
	"GET /api/v1/issues/{key}/role-holders": {target: "/api/v1/issues/$issue/role-holders", statement: statementOf(`\bleft\s+join\s+issue_role_holders\b`)},
	"GET /api/v1/issues/{key}/legion":       {target: "/api/v1/issues/$issue/legion", statement: statementOf(`\bfrom\s+workflow_tuples\s+t,\s*lateral\b`)},
	"GET /api/v1/handoffs/{id}":             {target: "/api/v1/handoffs/$handoff", statement: statementOf(`\bfrom\s+handoffs\s+where\s+id\s*=\s*\$1\b`)},
	"GET /api/v1/messages/{id}":             {target: "/api/v1/messages/$message", statement: statementOf(`\bwith\s+recursive\s+thread\s+as\b`)},
	"GET /api/v1/messages/{id}/blocks":      {target: "/api/v1/messages/$message/blocks", statement: statementOf(`\bfrom\s+messages\s+m\s+left\s+join\s+message_bl`)},
	"GET /api/v1/messages/{id}/history":     {target: "/api/v1/messages/$correctedMessage/history", statement: statementOf(postHistoryStatement)},
	"GET /api/v1/inbox":                     {target: "/api/v1/inbox", statement: statementOf(`\bfrom\s+ask_followers\s+where\s+ask_id\s*=\s*any\b`)},
	"GET /api/v1/me/answers":                {target: "/api/v1/me/answers", statement: statementOf(`\bwith\s+answered\s+as\b`)},
	"GET /api/v1/me/replies": {target: "/api/v1/me/replies?state=inbox", statement: statementOf(`\bfrom\s+user_threads\s+u\b`),
		volatile: []volatileMember{{"as_of", readTime}}},
	"GET /api/v1/me/inbox": {target: "/api/v1/me/inbox", statement: statementOf(`\bperson_nodes\(kind,\s*id\)\s+as\s*\(`),
		volatile: []volatileMember{{"as_of", readTime}}},
	"GET /api/v1/search": {target: "/api/v1/search?q=drive", statement: statementOf(`\bselect\s+numnode\(websearch_to_tsquery\b`),
		volatile: []volatileMember{{"took_ms", searchTook}}},
	"GET /api/v1/search/results": {target: "/api/v1/search/results?q=drive", statement: statementOf(`\bselect\s+numnode\(websearch_to_tsquery\b`),
		volatile: []volatileMember{{"took_ms", searchTook}}},
	"GET /api/v1/search/coverage": {target: "/api/v1/search/coverage", statement: statementOf(`\bwith\s+judged\s+as\b`),
		volatile: []volatileMember{
			{"kinds.*.oldest_lacking_seconds", "how long the oldest item lacking a vector has waited, counted to the moment of the read"},
			{"total.oldest_lacking_seconds", "the same wait, over every kind"},
		}},
	"GET " + goingCataloguePath:                        {target: goingCataloguePath, fromMemory: "the catalogue is built from the table when it is registered"},
	"GET /api/v1/related-checks":                       {target: "/api/v1/related-checks", statement: statementOf(`\bfrom\s+related_checks\b`)},
	"GET /api/v1/near-copy-checks":                     {target: "/api/v1/near-copy-checks?project=$project", statement: statementOf(`\bfrom\s+near_copy_checks\b`)},
	"GET /api/v1/near-copy-checks/{id}":                {target: "/api/v1/near-copy-checks/$nearCopy", statement: statementOf(`\bfrom\s+near_copy_checks\b`)},
	"GET /api/v1/going/capability":                     {target: "/api/v1/going/capability", fromMemory: "it answers whether the deployment configured a model"},
	"GET /api/v1/agents":                               {target: "/api/v1/agents", statement: statementOf(`\ba\.author->>'kind'\s*=\s*'session'`)},
	"GET /api/v1/agents/known":                         {target: "/api/v1/agents/known?id=$session", statement: statementOf(`\bfrom\s+unnest\(\$1::text\[\]\)\s+with\s+ordinality\s+as\s+asked\b`)},
	"GET /api/v1/agents/{session_id}/answers":          {target: "/api/v1/agents/$session/answers?since=$answersSince", statement: statementOf(`\bwith\s+reached\s+as\b`)},
	"GET /api/v1/agents/{session_id}/conversation":     {target: "/api/v1/agents/$session/conversation", statement: statementOf(`\bwith\s+recursive\s+direct_roots\s+as\b`)},
	"GET /api/v1/agents/{session_id}/messages":         {target: "/api/v1/agents/$session/messages", statement: statementOf(`\bwith\s+recursive\s+direct_roots\s+as\b`)},
	"GET /api/v1/agents/{session_id}/artifacts":        {target: "/api/v1/agents/$session/artifacts", statement: statementOf(`\bfrom\s+artifacts\s+where\s+session_id\s*=\s*\$1\b`)},
	"GET /api/v1/agents/{session_id}/artifacts/{slug}": {target: "/api/v1/agents/$session/artifacts/$fileSlug", statement: statementOf(`\bfrom\s+artifacts\s+where\s+ref_key\s*=\s*\$1\b`)},
	"GET /api/v1/agents/{session_id}/artifacts/{slug}/versions/{number}": {target: "/api/v1/agents/$session/artifacts/$fileSlug/versions/1",
		statement: statementOf(`\bselect\s+content,\s*mime,\s*sha256,\s*size\s+from\s+artifact_versions\b`)},
	"GET /api/v1/broadcasts":             {target: "/api/v1/broadcasts", statement: statementOf(`\bb\.id::text,\s*b\.author,\s*b\.body\b`)},
	"GET /api/v1/broadcasts/{id}":        {target: "/api/v1/broadcasts/$broadcast", statement: statementOf(`\bfrom\s+broadcasts\s+where\s+id\s*=\s*\$1\b`)},
	"GET /api/v1/issues/{key}/asks":      {target: "/api/v1/issues/$issue/asks?thread=1", statement: statementOf(`\bfrom\s+ask_followers\s+where\s+ask_id\s*=\s*any\b`)},
	"GET /api/v1/issues/{key}/decisions": {target: "/api/v1/issues/$issue/decisions", statement: statementOf(`\bfrom\s+decision_keeps\b`)},
	"GET /api/v1/asks/open": {target: "/api/v1/asks/open?project=$project", statement: statementOf(`\bselect\s+statement_timestamp\(\),\s*\$5::timestamptz\s+is\s+not\s+null\s+and\s+exists\b`),
		volatile: []volatileMember{{"as_of", readTime}, {"asks.*.age_seconds", "each ask's age, counted to the moment of the read"}}},
	"GET /api/v1/asks/{id}":             {target: "/api/v1/asks/$ask", statement: statementOf(`\bfrom\s+ask_followers\s+where\s+ask_id\s*=\s*\$1\b`)},
	"GET /api/v1/asks/{id}/followers":   {target: "/api/v1/asks/$followedAsk/followers", statement: statementOf(`\bselect\s+issue_key,\s*artifact_id::text\s+from\s+asks\s+where\s+id\b`)},
	"GET /api/v1/issues/{key}/comments": {target: "/api/v1/issues/$issue/comments", statement: statementOf(`\bfrom\s+comment_watermarks\b`)},
	"GET /api/v1/comments/{id}":         {target: "/api/v1/comments/$comment", statement: statementOf(`\bfrom\s+comment_mentions\s+where\s+comment_id\b`)},
	"GET /api/v1/comments/{id}/blocks":  {target: "/api/v1/comments/$comment/blocks", statement: statementOf(`\bleft\s+join\s+comment_block_state\b`)},
	"GET /api/v1/comments/{id}/history": {target: "/api/v1/comments/$correctedComment/history", statement: statementOf(postHistoryStatement)},
	// A document's own asks, comments and events are an unlinked project document's: the
	// guide (an issue's document answers ARTIFACT_LINKED, naming its issue's routes).
	"GET /api/v1/artifacts/{id}/asks":        {target: "/api/v1/artifacts/$guide/asks", statement: statementOf(`\bfrom\s+asks\s+a\b`)},
	"GET /api/v1/artifacts/{id}/comments":    {target: "/api/v1/artifacts/$guide/comments", statement: statementOf(`\bfrom\s+comments\b`)},
	"GET /api/v1/artifacts/{id}/events":      {target: "/api/v1/artifacts/$guide/events", statement: statementOf(`\bfrom\s+events\b`)},
	"GET /api/v1/artifacts/{id}/references":  {target: "/api/v1/artifacts/$spec/references", statement: statementOf(`\bselect\s+e\.to_kind,\s*e\.to_id\b`)},
	"GET /api/v1/artifacts/{id}/subscribers": {target: "/api/v1/artifacts/$guide/subscribers", statement: statementOf(`\bfrom\s+artifacts\s+where\s+id\s*=\s*\$1\b`)},
	"GET /api/v1/artifacts/{id}":             {target: "/api/v1/artifacts/$notes", statement: statementOf(`\bfrom\s+artifacts\s+where\s+id\s*=\s*\$1\b`)},
	"GET /api/v1/artifacts/{id}/reviews":     {target: "/api/v1/artifacts/$notes/reviews", statement: statementOf(`\bfrom\s+artifact_reviews\s+where\s+artifact_id\s*=\s*\$1\b`)},
	"GET /api/v1/artifacts/{id}/blocks":      {target: "/api/v1/artifacts/$spec/blocks", statement: statementOf(`\bselect\s+anchor_block_id,\s*count\(\*\)`)},
	"GET /api/v1/artifacts/{id}/blocks/{block_id}": {target: "/api/v1/artifacts/$spec/blocks/$block",
		statement: statementOf(`\bselect\s+h\.version,\s*coalesce\(u\.xmin::text`)},
	"GET /api/v1/artifacts/{id}/text": {target: "/api/v1/artifacts/$notes/text?blocks=true", statement: statementOf(`\bselect\s+max\(number\)\s+from\s+artifact_versions\b`)},
	"GET /api/v1/artifacts/{id}/versions/{number}": {target: "/api/v1/artifacts/$notes/versions/1",
		statement: statementOf(`\bfrom\s+artifact_versions\s+v\s+where\s+v\.artifact_id\s*=\s*\$1\s+and\s+v\.number\s*=\s*\$2\b`)},
	"GET /api/v1/artifacts/{id}/changes": {target: "/api/v1/artifacts/$notes/changes?from=1&to=2",
		statement: statementOf(`\bfrom\s+artifact_versions\s+where\s+artifact_id\s*=\s*\$1::uuid\s+and\s+number\s*=\s*any\(\$2::int\[\]\)`)},
	"GET /api/v1/issues/{key}/artifacts/{slug}": {target: "/api/v1/issues/$issue/artifacts/$notesSlug", statement: statementOf(`\bfrom\s+artifacts\s+where\s+ref_key\s*=\s*\$1\b`)},
	"GET /api/v1/issues/{key}/artifacts/{slug}/text": {target: "/api/v1/issues/$issue/artifacts/$notesSlug/text",
		statement: statementOf(`\bselect\s+max\(number\)\s+from\s+artifact_versions\b`)},
	"GET /api/v1/issues/{key}/artifacts/{slug}/blocks": {target: "/api/v1/issues/$issue/artifacts/$notesSlug/blocks",
		statement: statementOf(`\bselect\s+anchor_block_id,\s*count\(\*\)`)},
	"GET /api/v1/issues/{key}/artifacts/{slug}/versions/{number}": {target: "/api/v1/issues/$issue/artifacts/$notesSlug/versions/2",
		statement: statementOf(`\bfrom\s+artifact_versions\s+v\s+where\s+v\.artifact_id\s*=\s*\$1\s+and\s+v\.number\s*=\s*\$2\b`)},
	"GET /api/v1/projects/{key}/artifacts/{slug}": {target: "/api/v1/projects/$project/artifacts/$guideSlug", statement: statementOf(`\bfrom\s+artifacts\s+where\s+ref_key\s*=\s*\$1\b`)},
	"GET /api/v1/projects/{key}/artifacts/{slug}/summary": {target: "/api/v1/projects/$project/artifacts/$guideSlug/summary",
		statement: statementOf(`\bfrom\s+artifacts\s+a\s+where\s+a\.ref_key\b`)},
	"GET /api/v1/projects/{key}/artifacts/{slug}/text": {target: "/api/v1/projects/$project/artifacts/$guideSlug/text",
		statement: statementOf(`\bselect\s+max\(number\)\s+from\s+artifact_versions\b`)},
	"GET /api/v1/projects/{key}/artifacts/{slug}/versions/{number}": {target: "/api/v1/projects/$project/artifacts/$guideSlug/versions/1",
		statement: statementOf(`\bfrom\s+artifact_versions\s+v\s+where\s+v\.artifact_id\s*=\s*\$1\s+and\s+v\.number\s*=\s*\$2\b`)},
	"GET /api/v1/projects/{key}/artifacts/{slug}/blocks": {target: "/api/v1/projects/$project/artifacts/$guideSlug/blocks",
		statement: statementOf(`\bselect\s+anchor_block_id,\s*count\(\*\)`)},
	"GET /api/v1/me/state":                 {target: "/api/v1/me/state", statement: statementOf(`\bfrom\s+user_issue_state\s+where\s+login\b`)},
	"GET /api/v1/me/agents/state":          {target: "/api/v1/me/agents/state", statement: statementOf(`\bwith\s+recursive\s+direct_roots\s+as\b`)},
	"GET /api/v1/me/documents/{id}/read":   {target: "/api/v1/me/documents/$notes/read", statement: statementOf(`\bfrom\s+user_document_reads\b`)},
	"GET /api/v1/events/log":               {target: "/api/v1/events/log?after=0", statement: statementOf(`\bselect\s+e\.id,\s*e\.issue_key,\s*e\.artifact_id::text\b`)},
	"GET /api/v1/credential-requests":      {target: "/api/v1/credential-requests?approver=me", fromMemory: noBroker},
	"GET /api/v1/credential-grants":        {target: "/api/v1/credential-grants?approver=me", fromMemory: noBroker, status: http.StatusNotFound},
	"GET /api/v1/machine-logins":           {target: "/api/v1/machine-logins", fromMemory: noBroker, status: http.StatusNotFound},
	"GET /api/v1/signins":                  {target: "/api/v1/signins", fromMemory: noBroker, status: http.StatusNotFound},
	"GET /api/v1/credentials/capabilities": {target: "/api/v1/credentials/capabilities", fromMemory: noBroker},
	"GET /api/v1/artifacts/_test/rooms":    {target: "/api/v1/artifacts/_test/rooms", fromMemory: testHookRead},
	"GET /api/v1/artifacts/{id}/_test/ask-attributes": {target: "/api/v1/artifacts/$spec/_test/ask-attributes",
		fromMemory: documentHookRead, status: http.StatusNotFound},
	"GET /api/v1/artifacts/{id}/_test/room-counters": {target: "/api/v1/artifacts/$spec/_test/room-counters",
		fromMemory: documentHookRead, status: http.StatusNotFound},
	"GET /api/v1/artifacts/{id}/_test/live-write-pauses/{point}": {target: "/api/v1/artifacts/$spec/_test/live-write-pauses/after-commit?wait=100ms",
		fromMemory: documentHookRead, status: http.StatusNotFound},
	"GET /api/v1/messages/_test/queued": {target: "/api/v1/messages/_test/queued", fromMemory: testHookRead},
}

// getWriteDrives is the drive of every GET row of the table that writes (readWritesState), keyed
// as readRowDrives is: the request that makes the write the row's WriteNote names, on the seeded
// store. Each delivery read is sent at a window holding a deploy run with no production job of
// its own, the newest run the timeline's and the oldest the pipeline's, and they are sent in the
// table's order, the pipeline last: the timeline and the measures each read only the window
// around their own run, and the pipeline reads seven days, which hold all three, and firstToWarn
// writes once per process for each run (delivery.go), so a pipeline read sent first would warn
// of the other two rows' runs before their own reads.
var getWriteDrives = map[string]rowDrive{
	"GET /api/v1/delivery/timeline": {
		target:    "/api/v1/delivery/timeline?from=$timelineFrom&to=$timelineTo",
		statement: statementOf(`\binsert\s+into\s+delivery_run_warnings\b`),
		stored:    "select count(*) from delivery_run_warnings where run_id = $timelineRun",
	},
	"GET /api/v1/delivery/measures": {
		target:    "/api/v1/delivery/measures?from=$measuresFrom&to=$measuresTo",
		statement: statementOf(`\binsert\s+into\s+delivery_run_warnings\b`),
		stored:    "select count(*) from delivery_run_warnings where run_id = $measuresRun",
	},
	"GET /api/v1/delivery/pipeline": {
		target:    "/api/v1/delivery/pipeline?at=$pipelineAt",
		statement: statementOf(`\binsert\s+into\s+delivery_run_warnings\b`),
		stored:    "select count(*) from delivery_run_warnings where run_id = $pipelineRun",
	},
	"GET /api/v1/going/requests/{id}/stream": {
		target:    "/api/v1/going/requests/$going/stream",
		statement: statementOf(`\binsert\s+into\s+going_events\b`),
		stored:    "select count(*) from going_events where request_id = '$going'::uuid",
	},
}

// holdsReadAndNotApprove holds a read-only credential's whoami to its own scopes: read among
// them, approve never.
func holdsReadAndNotApprove(answer map[string]any) string {
	scopes, _ := answer["scopes"].([]any)
	if !slices.Contains(scopes, any(model.ScopeRead)) || slices.Contains(scopes, any(model.ScopeApprove)) {
		return fmt.Sprintf("its scopes are %v, want read among them and never approve", scopes)
	}
	return ""
}

// TestEveryGETRowHasADrive holds the two registries of drives to the read class: every GET row of
// the table with its test hooks in (readOnlyTable), read off the table, so a GET row added later
// fails here until it is driven. It fails for each read row with no drive in readRowDrives, each
// GET that writes with no drive in getWriteDrives, each GET row readClasses has not classed that
// has a drive in neither, each drive that names no GET row of its class or whose target its
// row's pattern does not match, each read's drive that names neither a statement its handler runs
// nor why it reads from memory, or names both, and each drive of a GET that writes that names no
// write. Every problem is collected first and the test fails once, naming each of them, so one
// run lists all that is missing.
func TestEveryGETRowHasADrive(t *testing.T) {
	t.Parallel()
	patterns := map[string]string{}
	var problems []string
	for _, route := range readOnlyTable() {
		if route.Method != http.MethodGet {
			continue
		}
		name := route.Method + " " + route.Pattern
		patterns[name] = route.Pattern
		_, read := readRowDrives[name]
		_, write := getWriteDrives[name]
		switch readClasses[name].Writes {
		case readWritesNothing:
			if !read {
				problems = append(problems, name+": a read (readWritesNothing) with no drive in readRowDrives")
			}
		case readWritesState:
			if !write {
				problems = append(problems, name+": a GET that writes (readWritesState) with no drive in getWriteDrives")
			}
		default:
			if !read && !write {
				problems = append(problems, name+": a GET row readClasses has not classed, with no drive in readRowDrives or getWriteDrives")
			}
		}
	}
	if len(patterns) == 0 {
		t.Fatal("the table with its test hooks in holds no GET row, so the walk read nothing")
	}
	for _, registry := range []struct {
		name   string
		class  readWrites
		rows   string
		drives map[string]rowDrive
	}{
		{"readRowDrives", readWritesNothing, "read row (readWritesNothing)", readRowDrives},
		{"getWriteDrives", readWritesState, "GET that writes (readWritesState)", getWriteDrives},
	} {
		for name, drive := range registry.drives {
			pattern, mounted := patterns[name]
			switch {
			case !mounted || readClasses[name].Writes != registry.class:
				problems = append(problems, name+": "+registry.name+" drives it, and it is no "+registry.rows+" of the table")
			case !sendsItsRow(pattern, drive.target):
				problems = append(problems, name+": "+registry.name+" drives it at "+drive.target+", which its pattern does not match")
			case registry.class == readWritesNothing && (drive.statement == nil) == (drive.fromMemory == ""):
				problems = append(problems, name+": its drive must name either a statement its handler runs or why it reads from memory, and not both")
			case registry.class == readWritesState && (drive.statement == nil || drive.stored == ""):
				problems = append(problems, name+": its drive names no write its WriteNote names, or no count of what that write stores")
			}
		}
	}
	sort.Strings(problems)
	t.Logf("walked %d GET rows of the table (test hooks in); readRowDrives holds %d drives and getWriteDrives %d",
		len(patterns), len(readRowDrives), len(getWriteDrives))
	if len(problems) > 0 {
		t.Errorf("the drives (read_class_drive_test.go) have %d problems:\n%s", len(problems), strings.Join(problems, "\n"))
	}
}

// getRows is a mux holding every GET row of the table with its test hooks in (readOnlyTable),
// each answering nothing, made once: what a drive's target is routed by, as the server's own mux
// routes it.
var getRows = sync.OnceValue(func() *http.ServeMux {
	mux := http.NewServeMux()
	for _, route := range readOnlyTable() {
		if route.Method == http.MethodGet {
			mux.HandleFunc(route.Method+" "+route.Pattern, func(http.ResponseWriter, *http.Request) {})
		}
	}
	return mux
})

// sendsItsRow reports whether a GET of target reaches the row of pattern: the table's GET rows
// (getRows) route the request to that row and to no other.
func sendsItsRow(pattern, target string) bool {
	request, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		return false
	}
	_, matched := getRows().Handler(request)
	return matched == http.MethodGet+" "+pattern
}

// driveRowsVariable names the read rows a run of the read drive sends, for a run that needs only
// some of them: the rows' names ("GET <pattern>", as readClasses keys them), separated by commas.
// Unset or empty, every read row runs.
const driveRowsVariable = "DISPATCH_READ_DRIVE_ROWS"

// drivenRows is every GET row of class in the table's order, with its drive. For the read rows
// (readWritesNothing) it is narrowed to the rows driveRowsVariable names, where the variable names
// any. A row's drive is read from either registry, so a row classed otherwise than its drive's
// registry says is still driven as its class says: that is what a class changed without its drive
// looks like, and the drive is what catches it. It fails for a row of the class with no drive, and
// for a name the variable holds that is no row of the class.
func drivenRows(t *testing.T, class readWrites) ([]string, map[string]rowDrive) {
	t.Helper()
	only := map[string]bool{}
	if class == readWritesNothing {
		for name := range strings.SplitSeq(os.Getenv(driveRowsVariable), ",") {
			if name = strings.TrimSpace(name); name != "" {
				only[name] = true
			}
		}
	}
	var rows []string
	drives := map[string]rowDrive{}
	for _, route := range readOnlyTable() {
		name := route.Method + " " + route.Pattern
		if route.Method != http.MethodGet || readClasses[name].Writes != class {
			continue
		}
		if len(only) > 0 && !only[name] {
			continue
		}
		drive, ok := readRowDrives[name]
		if !ok {
			drive, ok = getWriteDrives[name]
		}
		if !ok {
			t.Fatalf("%s: a row of its class with no drive in either registry", name)
		}
		rows = append(rows, name)
		drives[name] = drive
	}
	var unknown []string
	for name := range only {
		if _, driven := drives[name]; !driven {
			unknown = append(unknown, name)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		t.Fatalf("%s names %v, which are no rows of the class this test drives", driveRowsVariable, unknown)
	}
	if len(rows) == 0 {
		t.Fatal("no row to drive")
	}
	return rows, drives
}

// driveSend is one request of a drive: who sends it and the request.
type driveSend struct {
	who      string
	personal bool
	request  func(t *testing.T, target string) *http.Request
}

// drivenRequest is request on a context the test marks (countedRead), so the tracer tells its
// own statements and the work it detaches from those the server runs on a context of its own;
// and carrying Dispatch-Trees: 1, so every row that shows a text's tree shows it, and stores it
// the first time.
func drivenRequest(request *http.Request) *http.Request {
	request = request.WithContext(context.WithValue(request.Context(), countedRead{}, true))
	request.Header.Set(displayTreesHeader, "1")
	return request
}

// brokerSend is the send of a broker token: presented under DPoP with a fresh proof of the
// caller's key.
func (w *driveWorld) brokerSend(who, token string) driveSend {
	return driveSend{who: who, request: func(t *testing.T, target string) *http.Request {
		return drivenRequest(w.caller.presenting(t, http.MethodGet, target, token, nil))
	}}
}

// personalSend is the send of a personal token: presented as a bearer, as its agent presents it.
func personalSend(who, token string) driveSend {
	return driveSend{who: who, personal: true, request: func(t *testing.T, target string) *http.Request {
		return drivenRequest(brokerRequest(t, http.MethodGet, target, nil, "Bearer "+token, ""))
	}}
}

// pairs is every pair of credentials a read row is sent with: a read-only credential and its
// writer, the same identity holding person and not read, each pair sent on one state.
func (w *driveWorld) pairs() [][2]driveSend {
	return [][2]driveSend{
		{w.brokerSend("the machine login's narrowed token", w.readOnly), w.brokerSend("its writer", w.writer)},
		{personalSend("P's personal token holding read and person", w.personalReadOnly), personalSend("P's personal token holding person", w.personalWriter)},
	}
}

// driveTally counts what the read drive saw, for its log line.
type driveTally struct {
	rows, windows, statements int
	writes                    map[string]int
	treesStored               []string
}

// proofInsert is the insert that spends a broker token's proof.
var proofInsert = statementOf(`^\s*insert\s+into\s+dpop_proofs\b`)

// TestEveryReadRowWritesOnlyItsProofAndCachesAndAnswersAsItsWriter: every read row of the table
// (readWritesNothing), sent on one seeded store (newDriveWorld) by two pairs of credentials, each
// a read-only credential and its writer: the golden file's answer of a machine login's narrowed
// token (read-person, P, session S) and the same answer with the project scope person and no
// read; and P's personal token holding read and person, and P's holding person. In each pair the
// read-only credential is sent first, so nothing its writer writes is what the read-only request
// read. Before the personal pair, P's read-only token is left with no last use and P's writer with
// one now (startPersonalPair): the read-only request makes its last-use write, under both answers,
// and the writer's request, sent within agentTokenUse's minute, makes none.
//
// A tracer on the whole pool the server and the broker-token verifier share records every
// statement on any context (poolStatements), and each read-only request is sent in a window: it
// opens before the request and closes no sooner than 2 s after the answer and once the pool has
// run no write for 500 ms, and a window still open at 20 s fails the row. The window must hold no
// write a read may not make (readMayWrite), wherever it ran; it must hold the request's own
// credential check, the insert that spends the broker token's proof or the lookup of the personal
// token; and it must hold the statement the row's drive names, unless the drive says why the
// handler reads from memory. Each read-only answer must equal its writer's, status and body, but
// for the members the row's drive lists as volatile, each with its reason, and but for a row
// whose answer describes the credential (whoami), whose credential's members are held to the
// read-only credential's own answer.
//
// Before the first row a calibration window, GET /api/v1/limits under the deployment's shared
// token, which writes nothing, must hold no write at all: no duty of the rig writes while a row
// is driven. Then the pre-check must see, each in its own window, one insert into text_trees
// (the first read of a version no other test has shown), one upsert of route_roster on the
// server's own context (a read of a stale roster), and the term statistics refresh a related
// read starts on the server's own context, so a tracer that sees nothing, or sees the request's
// own context alone, fails before any row.
//
// DISPATCH_READ_DRIVE_ROWS narrows the run to the rows it names, separated by commas (each
// "GET <pattern>", as readClasses keys it); unset, every read row runs, which takes minutes,
// about 4.5 s a row, and so runs in a Job and in CI rather than in a shared pod. The calibration
// and the pre-check run either way.
func TestEveryReadRowWritesOnlyItsProofAndCachesAndAnswersAsItsWriter(t *testing.T) {
	// Not parallel: it holds every window and every personal pair to a wall-clock bound
	// (driveWindowLimit, agentTokenUse's minute) for minutes, and run among this package's parallel
	// tests on CI's runner it took 592 s, while TestTheEventFeedDoesNotResendAnEventAtOrBelowItsSince
	// missed its 3 s beside it (envoy-go-race-api job 137929719893).
	rows, drives := drivenRows(t, readWritesNothing)
	world := newDriveWorld(t)
	world.calibrate(t)
	world.precheck(t)
	tally := driveTally{writes: map[string]int{}}
	for _, name := range rows {
		t.Run(name, func(t *testing.T) {
			world.driveRead(t, name, drives[name], &tally)
		})
	}
	writes := make([]string, 0, len(tally.writes))
	for _, write := range slices.Sorted(maps.Keys(tally.writes)) {
		writes = append(writes, fmt.Sprintf("%s ×%d", write, tally.writes[write]))
	}
	t.Logf("drove %d read rows in %d read-only windows holding %d statements; their writes: %s; the rows whose read-only request stored a tree: %v",
		tally.rows, tally.windows, tally.statements, strings.Join(writes, ", "), tally.treesStored)
}

// calibrate is the calibration row: GET /api/v1/limits under the deployment's shared token, whose
// request writes nothing, in a window that must hold no write. A write in it is a duty of the rig
// writing while nothing asked it to, which would land in any row's window.
func (w *driveWorld) calibrate(t *testing.T) {
	t.Helper()
	w.tracer.quiet(t, "before the calibration row")
	var response *httptest.ResponseRecorder
	statements := w.tracer.window(t, "the calibration row", func() {
		request := drivenRequest(httptest.NewRequest(http.MethodGet, "/api/v1/limits", nil))
		request.Header.Set("Authorization", "Bearer agent-token")
		response = w.rig.serve(request)
	})
	if response.Code != http.StatusOK {
		t.Fatalf("the calibration row: GET /api/v1/limits answered %d %s", response.Code, response.Body.String())
	}
	if writes := writesIn(statements); len(writes) > 0 {
		t.Fatalf("the calibration window of GET /api/v1/limits under the shared token holds writes %v: a duty of the rig writes while a row is driven, and must be stopped for the drive", writes)
	}
	t.Logf("the calibration window held %d statements and no write", len(statements))
}

// precheck shows the tracer sees the three writes a read makes beyond its proof, each in its own
// window, two of them on the server's own context, and leaves the roster stale again for the rows.
func (w *driveWorld) precheck(t *testing.T) {
	t.Helper()
	readOnly := w.brokerSend("the machine login's narrowed token", w.readOnly)
	send := func(what, target string) []drivenStatement {
		t.Helper()
		w.tracer.quiet(t, "before "+what)
		var response *httptest.ResponseRecorder
		statements := w.tracer.window(t, what, func() { response = w.rig.serve(readOnly.request(t, w.fill(t, target))) })
		if response.Code != http.StatusOK {
			t.Fatalf("%s: GET %s answered %d %s", what, target, response.Code, response.Body.String())
		}
		requireReadsWrites(t, what, statements)
		return statements
	}
	count := func(statements []drivenStatement, pattern *regexp.Regexp) (all, unmarked int) {
		for _, statement := range statements {
			if pattern.MatchString(statement.sql) {
				all++
				if !statement.marked {
					unmarked++
				}
			}
		}
		return all, unmarked
	}

	trees := send("the pre-check's read of a version no other test has shown", "/api/v1/artifacts/$prose/versions/1")
	if stored, _ := count(trees, statementOf(`^\s*insert\s+into\s+text_trees\b`)); stored != 1 {
		t.Fatalf("the pre-check's read of a version no other test has shown made %d inserts into text_trees, want one: %v", stored, writesIn(trees))
	}

	w.staleRoster(t)
	roster := send("the pre-check's read of a stale roster", "/api/v1/agents")
	upserts, unmarked := count(roster, statementOf(`^\s*insert\s+into\s+route_roster\b.*\bon\s+conflict\b`))
	if upserts != 1 || unmarked != 1 {
		t.Fatalf("the pre-check's read of a stale roster made %d upserts of route_roster, %d of them on the server's own context, want one there: %v",
			upserts, unmarked, writesIn(roster))
	}

	related := send("the pre-check's related read of a project with no term statistics",
		"/api/v1/projects/$precheckProject/related?title=Drive+every+row&text=The+issue+page+lists+every+open+ask")
	totals, unmarked := count(related, statementOf(`\b(insert\s+into|update)\s+search_term_totals\b`))
	if totals == 0 || unmarked != totals {
		t.Fatalf("the pre-check's related read made %d writes of search_term_totals, %d of them on the server's own context, want the refresh's, all there: %v",
			totals, unmarked, writesIn(related))
	}
	t.Logf("the pre-check saw one insert into text_trees, one upsert of route_roster on the server's own context, and %d writes of search_term_totals there", totals)
	w.staleRoster(t)
}

// driveRead sends one read row with each pair (pairs), the read-only credential first, each in a
// window, and holds each window and each answer to what the test's comment says.
func (w *driveWorld) driveRead(t *testing.T, name string, drive rowDrive, tally *driveTally) {
	t.Helper()
	target := w.fill(t, drive.target)
	pattern := strings.TrimPrefix(name, http.MethodGet+" ")
	if !sendsItsRow(pattern, target) {
		t.Fatalf("the filled target %s is no request of %s", target, name)
	}
	status := drive.status
	if status == 0 {
		status = http.StatusOK
	}
	for _, pair := range w.pairs() {
		readOnly, writer := pair[0], pair[1]
		what := readOnly.who + " on " + target
		w.tracer.quiet(t, "before "+what)
		if readOnly.personal {
			w.startPersonalPair(t)
		}
		var answer *httptest.ResponseRecorder
		statements := w.tracer.window(t, what, func() { answer = w.rig.serve(readOnly.request(t, target)) })
		tally.windows++
		tally.statements += len(statements)
		for _, write := range requireReadsWrites(t, what, statements) {
			tally.writes[write]++
			if write == "insert text_trees" && !slices.Contains(tally.treesStored, name) {
				tally.treesStored = append(tally.treesStored, name)
			}
		}
		check := proofInsert
		if readOnly.personal {
			check = agentTokenLookup
		}
		if !holdsStatement(statements, check) {
			t.Errorf("%s: the window holds no statement matching %s among %d: the tracer did not see the request's own credential check:\n%s",
				what, check, len(statements), statementList(statements))
		}
		if drive.statement != nil && !holdsStatement(statements, drive.statement) {
			t.Errorf("%s: the window holds no statement matching %s among %d: the tracer did not see the handler's read:\n%s",
				what, drive.statement, len(statements), statementList(statements))
		}
		answered := w.rig.serve(writer.request(t, target))
		w.tracer.quiet(t, "after "+writer.who+" on "+target)
		if answer.Code != status {
			t.Errorf("%s: answered %d %s, want %d", what, answer.Code, cutBody(answer.Body.String()), status)
		}
		if refusal := gateRefusalOf(answer).Code; refusal == contracts.DispatchReadOnlyToken || refusal == contracts.DispatchAsOfTokenNotServed {
			t.Errorf("%s: the gate refused a read: %s", what, answer.Body.String())
		}
		if difference := answerDifference(answer, answered, drive.volatile, drive.credential); difference != "" {
			t.Errorf("%s: %s", what, difference)
		}
	}
	tally.rows++
}

// statementList is statements, one a line, for a failure to show.
func statementList(statements []drivenStatement) string {
	lines := make([]string, 0, len(statements))
	for _, statement := range statements {
		context := "server"
		if statement.marked {
			context = "request"
		}
		lines = append(lines, "  ["+context+"] "+oneLine(statement.sql))
	}
	return strings.Join(lines, "\n")
}

// TestEveryGETThatWritesMakesItsWrite: every GET row of the table that writes (readWritesState),
// sent with a writing token, the golden machine-login answer with the project scope person and no
// read, makes the write its WriteNote names: the going stream stores the end frame of a run past
// its deadline in going_events, and each delivery read inserts delivery_run_warnings for the
// deploy run with no production job that is its own. It runs on the drive rig (newDriveRig) with
// only the state those writes read (driveWorld.seedWrites), not the whole seeded store. Each is
// sent in a window as the reads are, which must hold that write, and the store must hold none of
// the row's rows before it and one after. The rows run in the table's order, which is the order
// getWriteDrives's runs need.
func TestEveryGETThatWritesMakesItsWrite(t *testing.T) {
	t.Parallel()
	rows, drives := drivenRows(t, readWritesState)
	world := newDriveRig(t)
	world.seedWrites(t)
	writer := world.brokerSend("the writing token", world.writer)
	stored := func(t *testing.T, drive rowDrive) int {
		t.Helper()
		var count int
		if err := world.raw.Pool.QueryRow(context.Background(), world.fill(t, drive.stored)).Scan(&count); err != nil {
			t.Fatalf("count what %s stores: %v", drive.stored, err)
		}
		return count
	}
	for _, name := range rows {
		t.Run(name, func(t *testing.T) {
			drive := drives[name]
			target := world.fill(t, drive.target)
			if before := stored(t, drive); before != 0 {
				t.Fatalf("before the drive the store holds %d of what %s writes, want none", before, name)
			}
			world.tracer.quiet(t, "before "+target)
			var answer *httptest.ResponseRecorder
			statements := world.tracer.window(t, target, func() { answer = world.rig.serve(writer.request(t, target)) })
			if answer.Code != http.StatusOK {
				t.Fatalf("GET %s answered %d %s", target, answer.Code, cutBody(answer.Body.String()))
			}
			if !holdsStatement(statements, drive.statement) {
				t.Errorf("GET %s: the window holds no statement matching %s among %d:\n%s",
					target, drive.statement, len(statements), statementList(statements))
			}
			if after := stored(t, drive); after != 1 {
				t.Errorf("GET %s: the store holds %d of what it writes (%s), want one", target, after, drive.stored)
			}
			t.Logf("GET %s made its write %s, the window's writes %v", target, drive.statement, writesIn(statements))
		})
	}
}
