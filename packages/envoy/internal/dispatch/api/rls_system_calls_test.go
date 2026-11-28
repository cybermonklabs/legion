package api

import (
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"maps"
	"slices"
	"testing"
)

// apiWorkClass is how a site of the API's own work runs under row security (plan D3, "The
// inventory of the API's own work").
type apiWorkClass string

const (
	// apiSystemWork is the server's own work: a context taken from the server's lifetime or from a
	// background loop's, which carries the system-work mark (store.WithSystemWork) and no viewer,
	// so the request store's pool runs its statements on the system pool; or a statement on the
	// system store itself (s.deps.System).
	apiSystemWork apiWorkClass = "system work"
	// apiRequestWork is work a request starts that keeps the request's context values (a
	// goroutine reading beside the request, a context.WithoutCancel of it, a mark of its own over
	// it), so its statements run under the viewer the request resolved.
	apiRequestWork apiWorkClass = "request work"
	// apiViewerRead is a read of D11 that runs under a viewer on a context taken from a lifetime,
	// where a forgotten viewer would read as the owner: its function must call
	// store.RequireViewer, which refuses a context with no viewer whatever its mark.
	apiViewerRead apiWorkClass = "a read under a viewer on a lifetime context (store.RequireViewer)"
)

// apiWorkSite is one entry of the reviewed list: the class of a site and why.
type apiWorkSite struct {
	class apiWorkClass
	why   string
}

// The reasons the list repeats, worded once.
const (
	whyLifetimeEnded  = "reads only whether the server's lifetime has ended, and no row"
	whyRequestCopy    = "a copy of the request's context that its cancel does not end, so it keeps the viewer the request resolved"
	whyRollbackCopy   = "the rollback of the request's own transaction, on a copy of its context its cancel does not end"
	whyRequestRead    = "a read beside the request on a connection mark of its own, over the request's context and its viewer"
	whyRequestWorker  = "a goroutine reading for the request, on the request's context and its viewer"
	whyHubLoop        = "the delivery hub's own loop on its generation's context, taken from the hub's lifetime: system work under the mark, reading the whole change log"
	whyRelayLoop      = "the event stream relay's own loop on a context taken from its lifetime: system work under the mark"
	whyMentionSender  = "the mention sender's own loop on a context taken from the server's lifetime: system work under the mark"
	whyTestHooks      = "the test hooks' fan-out on its own lifetime: system work under the mark"
	whyKeyedLayer     = "the keyed layer's bookkeeping of the request's own key (request_keys, class N), on a copy of the request's context that keeps its viewer"
	whySystemStore    = "a statement on the system store itself, which the work before the viewer and the API's own work run on"
	whyMarkedLoopOrRe = "runs on the context its caller gives: a request's copy, which keeps the request's viewer, or a background loop's, which carries the system-work mark"
)

// apiOwnWork is the reviewed list of every site TestTheAPIsSystemCallsAreTheListedOnes finds in the
// api package's non-test files, keyed "<file> <enclosing function> <kind>#<ordinal>": the kind is
// System (a reference to the system store, s.deps.System or NewDeps' input.System), go, a call of
// goReadWorker, ForConcurrentWorker, ForConcurrentRead or readContext, lifetime (a context taken
// from a lifetime field), context.Background or context.WithoutCancel; the ordinal counts the
// sites of that kind in that function in source order. The walker, not this list, is the
// authority: a site it finds that the list does not hold fails, and so does an entry it does not
// find. The System entries the plan lists (D3, "Work before the viewer, and system work of the
// API") are written here before the code that makes them, so the list fails until it does.
var apiOwnWork = map[string]apiWorkSite{
	// The system store's sites of plan D3: credential resolution and access.Resolve before any
	// request-pool statement, the relay's reads of the whole log and its horizon, and the
	// listener dials. Each is written before the code that makes it (INFRA-6168 work step 5).
	"server.go NewDeps System#1":                                  {apiSystemWork, "NewDeps hands the system store on to Deps.System"},
	"server.go server.withViewer System#1":                        {apiSystemWork, "access.Resolve of the request's caller, before any request-pool statement"},
	"agent_tokens.go server.personalTokenActor System#1":          {apiSystemWork, "credential resolution: the personal token's row, before the viewer is resolved"},
	"agent_tokens.go server.personalTokenActor System#2":          {apiSystemWork, "credential resolution: the token's last use (update agent_tokens), before the viewer is resolved"},
	"event_stream_relay.go eventStreamRelay.startLocked System#1": {apiSystemWork, "the relay's Listener dial"},
	"event_stream_relay.go eventStreamRelay.page System#1":        {apiSystemWork, "the relay's one page of the event log for every subscriber at once"},
	"event_stream_relay.go eventStreamRelay.resetCursor System#1": {apiSystemWork, "the relay's reset read of the whole log's shape"},
	"event_stream_relay.go server.firstRelayCursor System#1":      {apiSystemWork, "the relay's start read of the whole log's shape, on the first subscriber's context"},
	"event_stream_relay.go server.horizon System#1":               {apiSystemWork, "the horizon's fallback reader"},
	"event_stream_relay.go server.horizon System#2":               {apiSystemWork, "the Horizon side pool, which also answers /auth/whoami's event_head"},
	"delivery_live_hub.go newDeliveryHub System#1":                {apiSystemWork, "the delivery hub's Listener dial"},
	"test_hooks_fanout.go testHookFanout.dial System#1":           {apiSystemWork, "the test hooks' Listener dial"},

	// The reads of D11 that run under a viewer on a context taken from a lifetime.
	"delivery_live_hub_worker.go deliveryHub.read readContext#1":     {apiViewerRead, "the delivery hub's view recompute, on the hub's read context, under the view's viewer (row V2)"},
	"live_subscriptions.go server.subscribeLive ForConcurrentRead#1": {apiViewerRead, "a live subscription's start, under the socket's viewer (row L1)"},

	"agents.go server.listAgents lifetime#1":                                        {apiSystemWork, whyLifetimeEnded},
	"artifact_removal.go server.removeArtifact context.WithoutCancel#1":             {apiRequestWork, "the document service's eviction of the removed document's room, on " + whyRequestCopy},
	"broadcasts.go server.createBroadcast context.WithoutCancel#1":                  {apiRequestWork, "the broadcast's sends, detached from the request: " + whyRequestCopy},
	"broadcasts.go server.deliverBroadcast ForConcurrentRead#1":                     {apiRequestWork, "each delivery worker's mark, over the context the broadcast's send runs on: the request's copy (createBroadcast, replayBroadcast)"},
	"broadcasts.go server.deliverBroadcast ForConcurrentWorker#1":                   {apiRequestWork, "the delivery's own hold mark, over the request's copy it runs on"},
	"broadcasts.go server.deliverBroadcast go#1":                                    {apiRequestWork, "a delivery worker, on the request's copy the send runs on"},
	"broadcasts.go server.sendOwedBroadcastRecipients context.WithoutCancel#1":      {apiRequestWork, "the owed sends a keyed replay of the broadcast puts on their way: " + whyRequestCopy},
	"comment_create.go server.createCommentFor context.WithoutCancel#1":             {apiRequestWork, "the comment's mention sends: " + whyRequestCopy},
	"comment_create.go server.replayComment context.WithoutCancel#1":                {apiRequestWork, "the owed mention sends of a keyed replay: " + whyRequestCopy},
	"comment_delivery.go server.createCommentDelivery context.WithoutCancel#1":      {apiRequestWork, "a comment delivery's send: " + whyRequestCopy},
	"comment_delivery.go server.createCommentDelivery context.WithoutCancel#2":      {apiRequestWork, "a held mention's enqueue: " + whyRequestCopy},
	"comment_mention_claims.go mentionSender.keepClaims lifetime#1":                 {apiSystemWork, whyMentionSender},
	"comment_mention_sender.go mentionSender.queueLocked go#1":                      {apiSystemWork, whyMentionSender},
	"comment_mention_sender.go mentionSender.stoppedLocked lifetime#1":              {apiSystemWork, whyLifetimeEnded},
	"comment_mention_sender.go mentionSender.unpark go#1":                           {apiSystemWork, whyMentionSender},
	"comment_mention_sender.go mentionSender.wait context.WithoutCancel#1":          {apiSystemWork, "the release of the sender's claims at shutdown, on a copy of its lifetime: system work under the mark"},
	"comment_mention_sender.go mentionSender.wait lifetime#1":                       {apiSystemWork, "the release of the sender's claims at shutdown, on a copy of its lifetime: system work under the mark"},
	"comment_mention_sender.go newMentionSender context.WithoutCancel#1":            {apiSystemWork, whyMentionSender},
	"comment_mention_sender.go newMentionSender go#1":                               {apiSystemWork, whyMentionSender},
	"comment_mention_sender.go server.startMentionSender lifetime#1":                {apiSystemWork, whyMentionSender},
	"delivery_live_hub.go deliveryHub.Subscribe lifetime#1":                         {apiSystemWork, "the oldest change of the whole delivery change log, on the hub's lifetime: system work under the mark"},
	"delivery_live_hub.go deliveryHub.Subscribe lifetime#2":                         {apiSystemWork, whyLifetimeEnded},
	"delivery_live_hub.go deliveryHub.startLocked go#1":                             {apiSystemWork, whyHubLoop},
	"delivery_live_hub.go deliveryHub.startLocked go#2":                             {apiSystemWork, whyHubLoop},
	"delivery_live_hub.go deliveryHub.startLocked go#3":                             {apiSystemWork, whyHubLoop},
	"delivery_live_hub.go deliveryHub.startLocked lifetime#1":                       {apiSystemWork, whyHubLoop},
	"delivery_live_hub_intake.go deliveryHub.catchUpChanges readContext#1":          {apiSystemWork, whyHubLoop},
	"delivery_live_hub_intake.go deliveryHub.readChangeCursor readContext#1":        {apiSystemWork, whyHubLoop},
	"delivery_live_hub_intake.go deliveryHub.seedEvents go#1":                       {apiSystemWork, whyHubLoop},
	"delivery_live_hub_intake.go deliveryHub.seedEvents readContext#1":              {apiSystemWork, whyHubLoop},
	"delivery_live_hub_intake.go deliveryHub.seedEvents readContext#2":              {apiSystemWork, whyHubLoop},
	"delivery_sweep.go server.resumeOwedDeliveries ForConcurrentRead#1":             {apiSystemWork, "the maintenance sweep's resumed sends, on the sweep's context, which cmd/dispatch marks as system work"},
	"delivery_sweep.go server.resumeOwedDeliveries context.WithoutCancel#1":         {apiSystemWork, "the maintenance sweep's resumed sends, on the sweep's context, which cmd/dispatch marks as system work"},
	"display_trees.go buildDisplayTrees go#1":                                       {apiRequestWork, whyRequestWorker},
	"duplicates.go server.readDuplicateCandidates context.WithoutCancel#1":          {apiRequestWork, whyRollbackCopy},
	"event_stream_relay.go eventStreamRelay.startLocked go#1":                       {apiSystemWork, whyRelayLoop},
	"event_stream_relay.go eventStreamRelay.startLocked go#2":                       {apiSystemWork, whyRelayLoop},
	"event_stream_relay.go eventStreamRelay.startLocked lifetime#1":                 {apiSystemWork, whyRelayLoop},
	"event_stream_relay.go eventStreamRelay.subscribe lifetime#1":                   {apiSystemWork, whyLifetimeEnded},
	"event_stream_relay.go eventStreamRelay.subscribe lifetime#2":                   {apiSystemWork, whyLifetimeEnded},
	"event_stream_relay.go server.takeBootMark go#1":                                {apiSystemWork, "the event log's boot mark, on the server's lifetime: system work under the mark"},
	"event_stream_relay.go server.takeBootMark lifetime#1":                          {apiSystemWork, "the event log's boot mark, on the server's lifetime: system work under the mark"},
	"event_stream_relay.go server.takeBootMark lifetime#2":                          {apiSystemWork, whyLifetimeEnded},
	"going_routes.go server.createGoingRequest ForConcurrentRead#1":                 {apiRequestWork, "the going run's answer, on a mark of its own over " + whyRequestCopy},
	"going_routes.go server.createGoingRequest context.WithoutCancel#1":             {apiRequestWork, "the going run's answer: " + whyRequestCopy},
	"issue_claim.go claimLegionManagedStatus context.WithoutCancel#1":               {apiRequestWork, "the savepoint of the request's own transaction, on " + whyRequestCopy},
	"issue_route_status.go server.readRouteRoster goReadWorker#1":                   {apiSystemWork, "the listener answer's fetch (fetchRouteRoster) on the server's lifetime, which stores route_roster: system work under the mark"},
	"issue_route_status.go server.readRouteRoster lifetime#1":                       {apiSystemWork, "the listener answer's fetch (fetchRouteRoster) on the server's lifetime, which stores route_roster: system work under the mark"},
	"live_socket.go server.serveLiveSocket go#1":                                    {apiRequestWork, "the socket's writer, on the upgrade request's context and the viewer the upgrade resolved"},
	"live_socket.go server.serveLiveSocket lifetime#1":                              {apiSystemWork, whyLifetimeEnded},
	"live_subscriptions.go runSubscription go#1":                                    {apiRequestWork, "a subscription's run, on the socket's context and its viewer"},
	"live_subscriptions.go server.endLive go#1":                                     {apiRequestWork, "the stop of one subscription's run, which reads nothing"},
	"message_delivery.go server.createDelivery context.WithoutCancel#1":             {apiRequestWork, "a message delivery's send: " + whyRequestCopy},
	"messages.go server.createStoredMessage context.WithoutCancel#1":                {apiRequestWork, "the message's send: " + whyRequestCopy + "; Background.PostSystemMessage marks its own context as system work"},
	"messages.go server.sendHeldMessageOn context.WithoutCancel#1":                  {apiRequestWork, "a keyed replay's held send: " + whyRequestCopy},
	"near_copy_checks.go server.recordNearCopyCheck context.WithoutCancel#1":        {apiRequestWork, "the near-copy check's record: " + whyRequestCopy},
	"near_copy_checks.go server.recordNearCopyRefusal context.WithoutCancel#1":      {apiRequestWork, "the near-copy refusal's record: " + whyRequestCopy},
	"references.go server.boundedClosure context.WithoutCancel#1":                   {apiRequestWork, whyRollbackCopy},
	"related_other_projects.go server.relatedOtherProjects context.WithoutCancel#1": {apiRequestWork, whyRollbackCopy},
	"related_terms.go refreshSearchTermStats context.WithoutCancel#1":               {apiSystemWork, "the rollback of the term statistics refresh, which runs on the server's lifetime (ensureSearchTermStats)"},
	"related_terms.go server.ensureSearchTermStats lifetime#1":                      {apiSystemWork, "the term statistics refresh of a whole project, on the server's lifetime: system work under the mark"},
	"request_keys.go outlivingTheClient context.WithoutCancel#1":                    {apiRequestWork, whyKeyedLayer},
	"request_keys.go server.answerDrainedRepeat context.WithoutCancel#1":            {apiRequestWork, whyKeyedLayer},
	"request_keys.go server.claimRequestKey context.WithoutCancel#1":                {apiRequestWork, whyKeyedLayer},
	"request_keys.go server.keyedWrites context.WithoutCancel#1":                    {apiRequestWork, "the keyed write's handler, run detached from its client on a copy of the request's context that keeps the viewer withViewer attached outside the keyed layer"},
	"request_keys.go server.releaseRequestKey context.WithoutCancel#1":              {apiRequestWork, whyKeyedLayer},
	"request_keys.go server.renewRequestKey context.Background#1":                   {apiSystemWork, "the keyed write's lease renewer, which renews request_keys (class N) on a context of its own: system work, marked as such"},
	"request_keys.go server.renewRequestKeyLease go#1":                              {apiSystemWork, "the keyed write's lease renewer: system work, marked as such"},
	"request_keys.go server.settleRequestKey context.WithoutCancel#1":               {apiRequestWork, whyKeyedLayer},
	"request_keys.go server.writeUnjudgedKeyInFlight context.WithoutCancel#1":       {apiRequestWork, whyKeyedLayer},
	"search.go server.beginMeaningScan context.WithoutCancel#1":                     {apiRequestWork, whyRollbackCopy},
	"search.go server.beginMeaningScan context.WithoutCancel#2":                     {apiRequestWork, whyRollbackCopy},
	"search.go server.runMeaningSearch context.WithoutCancel#1":                     {apiRequestWork, whyRollbackCopy},
	"server.go backgroundWork.goDetached go#1":                                      {apiRequestWork, "the detached work a request leaves running: " + whyMarkedLoopOrRe},
	"server.go server.goReadWorker go#1":                                            {apiRequestWork, "a read stage that can outlive its request: " + whyMarkedLoopOrRe},
	"server.go server.whileCallerResolves go#1":                                     {apiSystemWork, "a socket's heartbeat, which resolves the admitting credential again: credential resolution, on the system store"},
	"suggestion_outcomes.go judgeSuggestionPageInTransaction context.Background#1":  {apiSystemWork, "the rollback of the suggestion outcome sweep's transaction, a background loop cmd/dispatch marks as system work"},
	"suggestions.go server.decisionBlockChecks ForConcurrentRead#1":                 {apiRequestWork, whyRequestRead},
	"suggestions.go server.insertRelatedChecks context.WithoutCancel#1":             {apiRequestWork, "the rollback of the related checks' record: on the request's copy (recordRelatedChecks), or on the server's lifetime when retried (retryRecord), which carries the mark"},
	"suggestions.go server.recordRelatedChecks context.WithoutCancel#1":             {apiRequestWork, "the related checks' record: " + whyRequestCopy},
	"suggestions.go server.relatedMeaningSearch context.WithoutCancel#1":            {apiRequestWork, whyRollbackCopy},
	"suggestions.go server.relatedMeaningStage goReadWorker#1":                      {apiRequestWork, whyRequestWorker},
	"suggestions.go server.retryRecord lifetime#1":                                  {apiSystemWork, "a record's retry, run detached on the server's lifetime: system work under the mark"},
	"suggestions.go server.runRelatedCheck ForConcurrentRead#1":                     {apiRequestWork, whyRequestRead},
	"suggestions.go server.runRelatedCheck ForConcurrentRead#2":                     {apiRequestWork, whyRequestRead},
	"suggestions.go server.runRelatedCheck ForConcurrentRead#3":                     {apiRequestWork, whyRequestRead},
	"suggestions.go server.runRelatedCheck goReadWorker#1":                          {apiRequestWork, whyRequestWorker},
	"suggestions.go server.runRelatedCheck goReadWorker#2":                          {apiRequestWork, whyRequestWorker},
	"test_hooks_fanout.go startTestHookFanout go#1":                                 {apiSystemWork, whyTestHooks},
	"test_hooks_fanout.go testHookFanout.acknowledge lifetime#1":                    {apiSystemWork, whyTestHooks},
	"test_hooks_fanout.go testHookFanout.dial context.Background#1":                 {apiSystemWork, "the close of the test hooks' listening connection: " + whyTestHooks},
	"test_hooks_fanout.go testHookFanout.heard lifetime#1":                          {apiSystemWork, whyTestHooks},
	"text_trees.go displayTreeReader.storeOf ForConcurrentWorker#1":                 {apiRequestWork, "the store of a display tree (text_trees, class N), detached from the read: " + whyRequestCopy},
	"text_trees.go displayTreeReader.storeOf context.WithoutCancel#1":               {apiRequestWork, "the store of a display tree (text_trees, class N), detached from the read: " + whyRequestCopy},
}

// apiSite is one site the walker found.
type apiSite struct {
	file, function, kind string
	ordinal, line        int
}

func (site apiSite) key() string {
	return fmt.Sprintf("%s %s %s#%d", site.file, site.function, site.kind, site.ordinal)
}

// TestTheAPIsSystemCallsAreTheListedOnes (plan D3, R3): a go/ast walk of the api package's
// non-test files, as go/build's default context selects them, lists every reference to the
// system store (s.deps.System, and NewDeps' input.System), every goroutine start (a go statement,
// goReadWorker, store.ForConcurrentWorker, store.ForConcurrentRead), every readContext() and
// every context taken from a lifetime field, context.Background() or context.WithoutCancel. Each
// must be in apiOwnWork with its class; an unlisted site fails, and so does a listed one the walk
// does not find. Each site classed as a read under a viewer on a lifetime context must sit in a
// function that calls store.RequireViewer.
func TestTheAPIsSystemCallsAreTheListedOnes(t *testing.T) {
	t.Parallel()
	sites, requiring := apiWorkSites(t, ".")
	found := map[string]apiSite{}
	for _, site := range sites {
		found[site.key()] = site
		listed, ok := apiOwnWork[site.key()]
		if !ok {
			t.Errorf("%s (line %d) is in no reviewed list: class it in apiOwnWork as %s, %s or %s, with why",
				site.key(), site.line, apiSystemWork, apiRequestWork, apiViewerRead)
			continue
		}
		if listed.class == apiViewerRead && !requiring[site.file+" "+site.function] {
			t.Errorf("%s (line %d) is %s, and %s does not call store.RequireViewer: %s",
				site.key(), site.line, apiViewerRead, site.function, listed.why)
		}
	}
	for _, key := range slices.Sorted(maps.Keys(apiOwnWork)) {
		if _, ok := found[key]; !ok {
			t.Errorf("apiOwnWork lists %s (%s: %s), which the walk does not find", key, apiOwnWork[key].class, apiOwnWork[key].why)
		}
	}
	t.Logf("the walk found %d sites of the API's own work in the api package's non-test files", len(sites))
}

// apiWorkSites is every site of the API's own work in the non-test Go files of the package in dir,
// and the functions ("<file> <function>") that call store.RequireViewer.
func apiWorkSites(t *testing.T, dir string) ([]apiSite, map[string]bool) {
	t.Helper()
	pkg, err := build.Default.ImportDir(dir, 0)
	if err != nil {
		t.Fatalf("read the package in %s: %v", dir, err)
	}
	fset := token.NewFileSet()
	var sites []apiSite
	requiring := map[string]bool{}
	for _, name := range pkg.GoFiles {
		file, err := parser.ParseFile(fset, dir+"/"+name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			function := "package scope"
			if fn, ok := decl.(*ast.FuncDecl); ok {
				function = declaredFunctionName(fn)
			}
			counts := map[string]int{}
			add := func(kind string, node ast.Node) {
				counts[kind]++
				sites = append(sites, apiSite{file: name, function: function, kind: kind, ordinal: counts[kind], line: fset.Position(node.Pos()).Line})
			}
			ast.Inspect(decl, func(node ast.Node) bool {
				switch node := node.(type) {
				case *ast.GoStmt:
					add("go", node)
				case *ast.SelectorExpr:
					switch node.Sel.Name {
					case "System":
						if systemStoreOwner(node.X) {
							add("System", node)
						}
					case "lifetime":
						add("lifetime", node)
					}
				case *ast.CallExpr:
					fun, ok := node.Fun.(*ast.SelectorExpr)
					if !ok {
						break
					}
					switch fun.Sel.Name {
					case "goReadWorker", "ForConcurrentWorker", "ForConcurrentRead", "readContext":
						add(fun.Sel.Name, node)
					case "Background", "WithoutCancel":
						if pkg, ok := fun.X.(*ast.Ident); ok && pkg.Name == "context" {
							add("context."+fun.Sel.Name, node)
						}
					case "RequireViewer":
						if pkg, ok := fun.X.(*ast.Ident); ok && pkg.Name == "store" {
							requiring[name+" "+function] = true
						}
					}
				}
				return true
			})
		}
	}
	return sites, requiring
}

// declaredFunctionName is a function declaration's name, a method's as "<receiver type>.<name>".
func declaredFunctionName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	receiver := fn.Recv.List[0].Type
	if star, ok := receiver.(*ast.StarExpr); ok {
		receiver = star.X
	}
	switch typed := receiver.(type) {
	case *ast.IndexExpr:
		receiver = typed.X
	case *ast.IndexListExpr:
		receiver = typed.X
	}
	if ident, ok := receiver.(*ast.Ident); ok {
		return ident.Name + "." + fn.Name.Name
	}
	return fn.Name.Name
}

// systemStoreOwner reports whether x, the operand of a .System selector, is what holds the system
// store: a deps field (s.deps, h.server.deps) or NewDeps' own input.
func systemStoreOwner(x ast.Expr) bool {
	switch x := x.(type) {
	case *ast.SelectorExpr:
		return x.Sel.Name == "deps"
	case *ast.Ident:
		return x.Name == "deps" || x.Name == "input"
	}
	return false
}
