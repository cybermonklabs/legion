package api

// writeProbe is how a driver calls one write row whose pattern names an object ({key}, {id} or
// {session_id}): the object each wildcard is filled with (a Probe, as a GET row's), and the JSON
// body sent with every fill, "" for none. The body is one the row takes as far as the read of the
// object its path names, from a person at a browser and from a personal token alike, so a call
// naming an object nobody holds is answered as missing rather than refused for its body
// (TestNoWriteReachesAHiddenObject holds every row to that).
type writeProbe struct {
	Probe readProbe
	Body  string
}

// probeActor is the writer a probe's body names where its row takes one: a bearer caller that
// proves no session must name its own (400 ACTOR_KIND otherwise), and a browser's write records
// its person whatever the body says.
const probeActor = `"actor":{"kind":"session","id":"s-probe"}`

// The kinds of object only a write's path names.
const (
	probeResult            probeObject = "result"             // a check result's uuid
	probeThread            probeObject = "thread"             // the uuid of the message a thread starts from
	probeAttempt           probeObject = "attempt"            // a delivery attempt of the message the path names
	probeEmail             probeObject = "email"              // the email of a person the project's access list names
	probeGroup             probeObject = "group"              // the name of an access group the project's access list names
	probeAgentToken        probeObject = "agent-token"        // a personal agent token's id
	probeSessionGroup      probeObject = "session-group"      // a Delivery session group's id
	probeCredentialRequest probeObject = "credential-request" // a credential request's id at the secrets broker
	probeCredentialGrant   probeObject = "credential-grant"   // a credential grant's id at the secrets broker
	probeMachineLogin      probeObject = "machine-login"      // a machine login's id at the secrets broker
	probeSignin            probeObject = "signin"             // a sign-in's id at the secrets broker
	probeStep              probeObject = "step"               // a step record's id: the uuid the Legion daemon derives for a run
	// probeMappedRepository is the name of a repository a project's mapping holds, which an
	// issue's external reference names (bodyReferences). Its missing side is a name no mapping
	// holds.
	probeMappedRepository probeObject = "mapped-repository"
)

// Probes the write rows share, beside the read rows' (byIssue, byProject, byDocument, bySession).
var (
	byAsk          = readProbe{"id": probeAsk}
	byComment      = readProbe{"id": probeComment}
	byMessage      = readProbe{"id": probeMessage}
	byIssueSlug    = readProbe{"key": probeIssue, "slug": probeDocumentSlug}
	byProjectSlug  = readProbe{"key": probeProject, "slug": probeDocumentSlug}
	byArchitecture = readProbe{"key": probeProject, "owner": probeRepositoryOwner, "repo": probeRepositoryName}
)

// writeProbes is how every write row whose pattern names an object is called, by "<method>
// <pattern>", beside writeClasses. TestNoWriteReachesAHiddenObject holds the two in exact
// correspondence and calls every entry, and the acceptance driver reads it through RouteProbes.
var writeProbes = map[string]writeProbe{
	// An issue, and what it holds.
	"PATCH /api/v1/issues/{key}":                           {byIssue, `{` + probeActor + `,"labels":["walked"]}`},
	"POST /api/v1/issues/{key}/claim":                      {byIssue, `{` + probeActor + `}`},
	"DELETE /api/v1/issues/{key}/claim":                    {byIssue, `{` + probeActor + `}`},
	"DELETE /api/v1/issues/{key}/subscribers/{session_id}": {readProbe{"key": probeIssue, "session_id": probeSession}, ""},
	"POST /api/v1/issues/{key}/artifacts":                  {byIssue, `{` + probeActor + `,"name":"walked.md","content":"# Walked\n"}`},
	"POST /api/v1/issues/{key}/artifacts/{slug}/versions":  {byIssueSlug, `{` + probeActor + `,"summary":"A walked version."}`},
	"POST /api/v1/issues/{key}/artifacts/{slug}/edits":     {byIssueSlug, `{` + probeActor + `,"summary":"A walked edit.","ops":[]}`},
	"POST /api/v1/issues/{key}/messages":                   {byIssue, `{` + probeActor + `,"body":"A walked message."}`},
	"POST /api/v1/issues/{key}/asks":                       {byIssue, `{` + probeActor + `,"question":"A walked question?"}`},
	"POST /api/v1/issues/{key}/comments":                   {byIssue, `{` + probeActor + `,"body":"A walked comment."}`},
	"POST /api/v1/issues/{key}/results": {byIssue, `{` + probeActor +
		`,"check":"k1","role":"implementer","verdict":"pass","commit":"1111111111111111111111111111111111111111"}`},
	// The record's dry run reads its issue and check as the record does (readResultRequest), and
	// writes nothing.
	"POST /api/v1/issues/{key}/results/dry-run": {byIssue, `{` + probeActor +
		`,"check":"k1","role":"implementer","verdict":"pass","commit":"1111111111111111111111111111111111111111"}`},
	"POST /api/v1/issues/{key}/handoffs/{phase}": {readProbe{"key": probeIssue, "phase": probeHandoffPhase},
		`{` + probeActor + `,"body":"{\"next\":\"walked\"}"}`},
	"PUT /api/v1/issues/{key}/design-gates": {byIssue, `{` + probeActor + `,"gates":[]}`},
	"PUT /api/v1/issues/{key}/role-holders": {byIssue, `{` + probeActor + `,"holders":[]}`},
	// Each record write reads its issue as the caller sees it before it judges the writer
	// (lockRecordedIssue), so a key the caller cannot see is a key no issue holds whatever session
	// the body names.
	"PUT /api/v1/issues/{key}/steps/{id}":        {readProbe{"key": probeIssue, "id": probeStep}, `{` + probeActor + `}`},
	"PUT /api/v1/issues/{key}/steps/{id}/rating": {readProbe{"key": probeIssue, "id": probeStep}, `{` + probeActor + `}`},
	"PUT /api/v1/issues/{key}/plans/{id}":        {readProbe{"key": probeIssue, "id": probeStep}, `{` + probeActor + `}`},
	"POST /api/v1/results/{id}/retract":          {readProbe{"id": probeResult}, `{` + probeActor + `,"reason":"A walked retraction."}`},

	// A project, and what it holds.
	"POST /api/v1/projects/{key}/artifacts":                                {byProject, `{` + probeActor + `,"name":"walked.md","content":"# Walked\n"}`},
	"POST /api/v1/projects/{key}/artifacts/{slug}/versions":                {byProjectSlug, `{` + probeActor + `,"summary":"A walked version."}`},
	"POST /api/v1/projects/{key}/artifacts/{slug}/edits":                   {byProjectSlug, `{` + probeActor + `,"summary":"A walked edit.","ops":[]}`},
	"PUT /api/v1/projects/{key}/default-assignee":                          {byProject, `{"assignee":"alice"}`},
	"PATCH /api/v1/projects/{key}/access":                                  {byProject, `{"visibility":"public"}`},
	"PUT /api/v1/projects/{key}/access/people/{email}":                     {readProbe{"key": probeProject, "email": probeEmail}, ""},
	"DELETE /api/v1/projects/{key}/access/people/{email}":                  {readProbe{"key": probeProject, "email": probeEmail}, ""},
	"PUT /api/v1/projects/{key}/access/groups/{name}":                      {readProbe{"key": probeProject, "name": probeGroup}, ""},
	"DELETE /api/v1/projects/{key}/access/groups/{name}":                   {readProbe{"key": probeProject, "name": probeGroup}, ""},
	"PUT /api/v1/projects/{key}/gated-steps":                               {byProject, `{` + probeActor + `,"steps":[]}`},
	"PUT /api/v1/projects/{key}/legion/daemon":                             {byProject, `{` + probeActor + `,"state_url":"http://legion-walked.internal"}`},
	"DELETE /api/v1/projects/{key}/legion/daemon":                          {byProject, ""},
	"PUT /api/v1/projects/{key}/architecture-sources/{owner}/{repo}":       {byArchitecture, `{` + probeActor + `,"branch":"main"}`},
	"DELETE /api/v1/projects/{key}/architecture-sources/{owner}/{repo}":    {byArchitecture, `{` + probeActor + `}`},
	"POST /api/v1/projects/{key}/architecture-sources/{owner}/{repo}/sync": {byArchitecture, ""},
	"POST /api/v1/projects/{key}/architecture-sources/sync":                {byProject, ""},
	"POST /api/v1/projects/{key}/architecture-source/sync":                 {byProject, ""},
	"POST /api/v1/projects/{key}/related":                                  {byProject, `{"text":"A walked related check."}`},

	// A document by id.
	"POST /api/v1/artifacts/{id}/blocks/{block_id}/keep": {readProbe{"id": probeDocument, "block_id": probeBlock},
		`{"text_sha256":"0000000000000000000000000000000000000000000000000000000000000000"}`},
	"POST /api/v1/artifacts/{id}/asks":                       {byDocument, `{` + probeActor + `,"question":"A walked question?"}`},
	"POST /api/v1/artifacts/{id}/comments":                   {byDocument, `{` + probeActor + `,"body":"A walked comment."}`},
	"DELETE /api/v1/artifacts/{id}/subscribers/{session_id}": {readProbe{"id": probeDocument, "session_id": probeSession}, ""},
	"POST /api/v1/artifacts/{id}/rebuild":                    {byDocument, `{}`},
	"PATCH /api/v1/artifacts/{id}":                           {byDocument, `{"name":"renamed.md"}`},
	"DELETE /api/v1/artifacts/{id}":                          {byDocument, ""},
	"POST /api/v1/artifacts/{id}/reviews":                    {byDocument, `{"state":"approved"}`},
	"POST /api/v1/artifacts/{id}/versions/{number}/reviews": {readProbe{"id": probeDocument, "number": probeDocumentVersion},
		`{"state":"approved"}`},
	"POST /api/v1/artifacts/{id}/plan-reviews":                      {byDocument, `{` + probeActor + `,"version":1}`},
	"POST /api/v1/artifacts/{id}/approval-requests":                 {byDocument, `{` + probeActor + `}`},
	"POST /api/v1/artifacts/{id}/versions":                          {byDocument, `{` + probeActor + `,"summary":"A walked version."}`},
	"POST /api/v1/artifacts/{id}/edits":                             {byDocument, `{` + probeActor + `,"summary":"A walked edit.","ops":[]}`},
	"POST /api/v1/artifacts/{id}/_test/outside-schema":              {byDocument, ""},
	"POST /api/v1/artifacts/{id}/_test/newer-content":               {byDocument, ""},
	"POST /api/v1/artifacts/{id}/_test/hold-persistence":            {byDocument, `{"duration":"1ms"}`},
	"PUT /api/v1/artifacts/{id}/_test/live-write-pauses/{point}":    {readProbe{"id": probeDocument, "point": probeLivePause}, ""},
	"DELETE /api/v1/artifacts/{id}/_test/live-write-pauses/{point}": {readProbe{"id": probeDocument, "point": probeLivePause}, ""},

	// An ask, a comment and a message by id.
	"PUT /api/v1/asks/{id}/followers/{session_id}":           {readProbe{"id": probeAsk, "session_id": probeSession}, `{` + probeActor + `}`},
	"DELETE /api/v1/asks/{id}/followers/{session_id}":        {readProbe{"id": probeAsk, "session_id": probeSession}, `{` + probeActor + `}`},
	"PATCH /api/v1/asks/{id}":                                {byAsk, `{` + probeActor + `,"urgency":"high"}`},
	"PUT /api/v1/asks/{id}/asked-of":                         {byAsk, `{"asked_of":"alice"}`},
	"POST /api/v1/asks/{id}/answer":                          {byAsk, `{` + probeActor + `,"text":"A walked answer.","expected_edited_at":null}`},
	"POST /api/v1/asks/{id}/resolve":                         {byAsk, `{` + probeActor + `,"kind":"resolved","reason":"A walked resolution."}`},
	"POST /api/v1/asks/{id}/decline":                         {byAsk, `{` + probeActor + `,"note":"A walked decline."}`},
	"POST /api/v1/comments/{id}/resolve":                     {byComment, `{` + probeActor + `}`},
	"POST /api/v1/comments/{id}/reopen":                      {byComment, ""},
	"POST /api/v1/comments/{id}/deliveries":                  {byComment, `{` + probeActor + `,"delivery":"btw"}`},
	"POST /api/v1/comments/{id}/reply":                       {byComment, `{` + probeActor + `,"attempt":1,"body":"A walked reply."}`},
	"POST /api/v1/comments/{id}/accept":                      {byComment, `{` + probeActor + `}`},
	"PATCH /api/v1/comments/{id}":                            {byComment, `{"body":"A walked edit."}`},
	"POST /api/v1/comments/{id}/reject":                      {byComment, `{` + probeActor + `}`},
	"POST /api/v1/messages/{id}/deliveries":                  {byMessage, `{` + probeActor + `,"delivery":"btw"}`},
	"POST /api/v1/messages/{id}/deliveries/{attempt}/accept": {readProbe{"id": probeMessage, "attempt": probeAttempt}, `{` + probeActor + `}`},
	"POST /api/v1/messages/{id}/reply":                       {byMessage, `{` + probeActor + `,"attempt":1,"body":"A walked reply."}`},

	// An agent session.
	"POST /api/v1/agents/{session_id}/messages":             {bySession, `{"body":"A walked message.","delivery":"btw"}`},
	"POST /api/v1/agents/{session_id}/artifacts":            {bySession, `{` + probeActor + `,"name":"walked.md","content":"# Walked\n"}`},
	"POST /api/v1/agents/{session_id}/stream/_test/publish": {bySession, `{}`},

	// The caller's own rows, each about an object.
	"PUT /api/v1/me/documents/{id}/read":       {byDocument, `{"version":1}`},
	"PATCH /api/v1/me/agent-tokens/{id}":       {readProbe{"id": probeAgentToken}, `{"scopes":[]}`},
	"DELETE /api/v1/me/agent-tokens/{id}":      {readProbe{"id": probeAgentToken}, ""},
	"PUT /api/v1/me/threads/{id}/read":         {readProbe{"id": probeThread}, `{"read_through":"2020-01-01T00:00:00Z"}`},
	"PUT /api/v1/me/issues/{key}/state":        {byIssue, `{` + probeActor + `,"pinned":true}`},
	"PUT /api/v1/me/agents/{session_id}/state": {bySession, `{"pinned":true}`},
	"PUT /api/v1/me/asks/{id}/snooze":          {byAsk, `{"snoozed_until":"2099-01-01T00:00:00Z"}`},
	"DELETE /api/v1/me/asks/{id}/snooze":       {byAsk, ""},

	// The deployment's rows.
	"PATCH /api/v1/priorities/{id}":                 {readProbe{"id": probePriority}, `{` + probeActor + `,"title":"Walked"}`},
	"DELETE /api/v1/priorities/{id}":                {readProbe{"id": probePriority}, `{` + probeActor + `}`},
	"PATCH /api/v1/delivery/session-groups/{id}":    {readProbe{"id": probeSessionGroup}, `{` + probeActor + `,"name":"walked"}`},
	"DELETE /api/v1/delivery/session-groups/{id}":   {readProbe{"id": probeSessionGroup}, `{` + probeActor + `}`},
	"POST /api/v1/credential-requests/{id}/approve": {readProbe{"id": probeCredentialRequest}, `{}`},
	"POST /api/v1/credential-requests/{id}/deny":    {readProbe{"id": probeCredentialRequest}, `{}`},
	"POST /api/v1/credential-requests/{id}/dismiss": {readProbe{"id": probeCredentialRequest}, ""},
	"POST /api/v1/credential-grants/{id}/revoke":    {readProbe{"id": probeCredentialGrant}, ""},
	"POST /api/v1/machine-logins/{id}/revoke":       {readProbe{"id": probeMachineLogin}, ""},
	"POST /api/v1/signins/{id}/revoke":              {readProbe{"id": probeSignin}, ""},
}

// noProjectWrite is a write row whose pattern names an object that belongs to no project: why it
// belongs to none, and the JSON body a driver sends when it holds the row to its write class's
// refusal, "" for none.
type noProjectWrite struct {
	Reason string
	Body   string
}

// noProjectWrites is every write row whose pattern names an object no project owns, so there is
// no object of a private project for it to reach and writeProbes holds no entry for it. Its class
// is scopeDeployment, which refuses every caller that is not open, and the acceptance driver holds
// each row to that refusal for every such caller (RouteProbe.NoProject).
var noProjectWrites = map[string]noProjectWrite{
	"PUT /api/v1/access-groups/{name}":                     {"an access group is the deployment's, which no project owns", `{"members":[]}`},
	"DELETE /api/v1/access-groups/{name}":                  {"an access group is the deployment's, which no project owns", ""},
	"PUT /api/v1/settings/repo-projects/{owner}/{repo}":    {"a repository mapping is the deployment's, keyed by a repository no project owns", `{"project":"PUB"}`},
	"DELETE /api/v1/settings/repo-projects/{owner}/{repo}": {"a repository mapping is the deployment's, keyed by a repository no project owns", ""},
}
