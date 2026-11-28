package store

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
)

// tableRowSecurity is what the catalog holds of one table's row security: whether it is enabled
// and forced, whether a policy named viewer is on it, and whether that policy is for all commands,
// to public and permissive, as plan D4 writes every policy.
type tableRowSecurity struct {
	enabled, forced, viewer, forAllToPublic bool
}

// TestEveryTableIsClassedForRowSecurity (plan D4, R2): every table of schema public in a freshly
// migrated database is in exactly one class of RowSecurityClasses and every name there is a table;
// each table of the classes P to R-repo has row security enabled and not forced (the owner, the
// old build and the read login meet no policy), with its policy named viewer, for all commands and
// to public, so it can be made before dispatch_app exists; each table of class N has row security
// off. An entry whose reason is still UNREVIEWED fails until it is reviewed. graph_edges, the one
// view, reads its base tables under its caller's row security (security_invoker).
func TestEveryTableIsClassedForRowSecurity(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openMigratedTestStore(t, latestVersion)
	rows, err := store.Pool.Query(ctx, `
		select c.relname::text, c.relrowsecurity, c.relforcerowsecurity,
			exists (select from pg_policy p where p.polrelid = c.oid and p.polname = 'viewer'),
			exists (select from pg_policy p where p.polrelid = c.oid and p.polname = 'viewer'
				and p.polcmd = '*' and p.polroles = '{0}'::oid[] and p.polpermissive)
		from pg_class c
		where c.relnamespace = 'public'::regnamespace and c.relkind in ('r', 'p')`)
	if err != nil {
		t.Fatalf("read the tables of schema public: %v", err)
	}
	catalog := map[string]tableRowSecurity{}
	for rows.Next() {
		var name string
		var held tableRowSecurity
		if err := rows.Scan(&name, &held.enabled, &held.forced, &held.viewer, &held.forAllToPublic); err != nil {
			t.Fatalf("read a table of schema public: %v", err)
		}
		catalog[name] = held
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the tables of schema public: %v", err)
	}
	if len(catalog) == 0 {
		t.Fatal("the migrated database holds no table of schema public, so this test compares nothing")
	}

	var unclassed, missing []string
	for _, name := range slices.Sorted(maps.Keys(catalog)) {
		if _, ok := RowSecurityClasses[name]; !ok {
			unclassed = append(unclassed, name)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(RowSecurityClasses)) {
		if _, ok := catalog[name]; !ok {
			missing = append(missing, name)
		}
	}
	if len(unclassed) > 0 {
		t.Errorf("schema public holds %d tables that store.RowSecurityClasses does not class; give each its class of plan D4 and its reason: %s",
			len(unclassed), strings.Join(unclassed, ", "))
	}
	if len(missing) > 0 {
		t.Errorf("store.RowSecurityClasses classes %d names that are no table of schema public after the migrations: %s",
			len(missing), strings.Join(missing, ", "))
	}

	known := map[RowSecurityClass]bool{}
	for _, class := range []RowSecurityClass{
		RowSecurityProject, RowSecurityIssue, RowSecurityTwoIssues, RowSecurityIssueOrDocument, RowSecurityDocuments,
		RowSecurityThroughDocument, RowSecurityThroughComment, RowSecurityThroughAsk, RowSecurityMessages,
		RowSecurityThroughMessage, RowSecurityCommentOrMessage, RowSecurityEvents, RowSecurityThroughEvent,
		RowSecurityGraphNodes, RowSecurityGraphTuples, RowSecurityReferences, RowSecurityWorkflow, RowSecurityPriorities,
		RowSecurityGoingRequests, RowSecurityCheckResults, RowSecurityEmbeddings, RowSecurityBroadcasts, RowSecuritySessions,
		RowSecurityListenerAnswer, RowSecurityRepositories, RowSecurityNone,
	} {
		known[class] = true
	}
	var unreviewed, offWithAPolicyClass, forced, noViewerPolicy, notForAllToPublic, onWithoutAPolicyClass []string
	for _, name := range slices.Sorted(maps.Keys(RowSecurityClasses)) {
		classed := RowSecurityClasses[name]
		switch {
		case !known[classed.Class]:
			t.Errorf("store.RowSecurityClasses puts %s in class %q, which is no class of plan D4", name, classed.Class)
			continue
		case strings.TrimSpace(classed.Reason) == "":
			t.Errorf("store.RowSecurityClasses puts %s in class %s with no reason", name, classed.Class)
		case strings.HasPrefix(classed.Reason, "UNREVIEWED"):
			unreviewed = append(unreviewed, fmt.Sprintf("%s (class %s)", name, classed.Class))
		}
		held, ok := catalog[name]
		if !ok {
			continue
		}
		if classed.Class == RowSecurityNone {
			if held.enabled {
				onWithoutAPolicyClass = append(onWithoutAPolicyClass, name)
			}
			continue
		}
		label := fmt.Sprintf("%s (class %s)", name, classed.Class)
		if !held.enabled {
			offWithAPolicyClass = append(offWithAPolicyClass, label)
		}
		if held.forced {
			forced = append(forced, label)
		}
		if !held.viewer {
			noViewerPolicy = append(noViewerPolicy, label)
		} else if !held.forAllToPublic {
			notForAllToPublic = append(notForAllToPublic, label)
		}
	}
	for _, problem := range []struct {
		names []string
		says  string
	}{
		{unreviewed, "store.RowSecurityClasses holds tables plan D4 does not class, each put in the class whose definition fits it best and still to be reviewed (a comment on the plan's block a names the table and the reason)"},
		{offWithAPolicyClass, "tables of the classes P to R-repo have no row security enabled (relrowsecurity false)"},
		{forced, "tables of the classes P to R-repo force row security (relforcerowsecurity true), so the owner, the old build and the read login meet the policy"},
		{noViewerPolicy, "tables of the classes P to R-repo hold no policy named viewer"},
		{notForAllToPublic, "tables hold a viewer policy that is not a permissive policy for all commands to public"},
		{onWithoutAPolicyClass, "tables of class N have row security enabled, where plan D4 keeps it off"},
	} {
		if len(problem.names) > 0 {
			t.Errorf("%d %s: %s", len(problem.names), problem.says, strings.Join(problem.names, ", "))
		}
	}

	t.Run("graph_edges_is_security_invoker", func(t *testing.T) {
		var kind string
		var invoker bool
		if err := store.Pool.QueryRow(ctx, `
			select c.relkind::text, coalesce((select o.option_value::boolean from pg_options_to_table(c.reloptions) o
				where o.option_name = 'security_invoker'), false)
			from pg_class c where c.oid = 'public.graph_edges'::regclass`).Scan(&kind, &invoker); err != nil {
			t.Fatalf("read graph_edges: %v", err)
		}
		if kind != "v" {
			t.Fatalf("graph_edges is a relation of relkind %q, want the view (v)", kind)
		}
		if !invoker {
			t.Errorf("graph_edges does not set security_invoker: its arms read their base tables as the view's owner, past the caller's row security (plan D4: alter view public.graph_edges set (security_invoker = on))")
		}
	})
}

// functionSecurity is how a function of schema public runs under the request role: as its owner
// (security definer, with the reason it must see what the caller cannot), or as its invoker.
type functionSecurity struct {
	definer bool
	reason  string
}

// invoker is a function that runs as its caller: its reads and writes meet the caller's row
// security, and a trigger's writes stay within the row the request wrote (plan D5).
var invoker = functionSecurity{}

// definer is a function that runs as its owner, for reason, with search_path pinned (plan D5).
func definer(reason string) functionSecurity { return functionSecurity{definer: true, reason: reason} }

// reviewedFunctionSecurity is each function of schema public, by name, and how it runs under the
// request role, as a person reviewed it (plan D5): definer with the reason it must read or write
// what the caller cannot, or invoker. A function a migration adds fails
// TestEveryTriggerAndFunctionDeclaresHowItRunsUnderTheRequestRole until it is reviewed here.
var reviewedFunctionSecurity = map[string]functionSecurity{
	// Plan D5's owner-run functions: each is called by a policy, or is a trigger function that
	// reads or writes rows of another scope.
	"session_clause_visible":             definer("SessionClause, which policies call, asks it: it reads a session's pairs, its listener record and the private projects, which the caller reads under row security, and no policy may recurse"),
	"session_content_visible":            definer("SessionContentClause and PartyClause, which policies call, ask it: it reads a session's pairs, ties, reached projects and listener record, which the caller reads under row security"),
	"dispatch_sees_project":              definer("every policy but projects' asks it: it probes projects by its primary key as the owner, so no policy reads projects under row security and none recurses (D4)"),
	"dispatch_repo_visible":              definer("the repository rule of class R-repo and the GitHub proxy: it reads which issues of every project resolve to a repository, and answers only whether the viewer may read it (D11)"),
	"record_project_made_private":        definer("the trigger of a switch to private: it writes session_reached_projects for every session whose listener list names the project, rows of sessions the writer does not see"),
	"sync_route_roster_session_projects": definer("route_roster's trigger: it rewrites the projection of every session's listed projects and records reached projects, rows of sessions the writer does not see"),
	"projects_session_projects_swept":    definer("projects' trigger (projects_session_projects_swept): it sweeps the session_projects pairs of every session for the project, rows of sessions the writer does not see"),
	"user_threads_record":                definer("the user_threads bump (0095_user_threads.up.sql:73-77): a reply moves replied_at on every other participant's row of the thread, rows of other logins"),
	"dispatch_node_visible":              definer("graph_nodes' policy, and graph_tuples' through dispatch_tuple_end_visible: a node's rule by its kind reads its item's node, a role's held_by tuples and a session's pairs, so no policy reads graph_tuples or graph_nodes under row security and none recurses"),
	"dispatch_tuple_end_visible":         definer("graph_tuples' policy: it reads the node an end of the tuple names as the owner, then judges it by dispatch_node_visible"),
	"dispatch_broadcast_visible":         definer("broadcasts' policy: access.BroadcastClause reads the broadcast and every copy of it, and a copy to a session the viewer may not read must refuse the broadcast, not be skipped under row security; a policy of broadcasts may not read broadcasts again"),
	"dispatch_workflow_node_visible":     definer("workflow_nodes' policy: it reads the node's part_of tuple, whose policy reads workflow_nodes, so the node's issue is resolved as the owner"),
	"dispatch_message_visible":           definer("messages' policy for a message on no issue: it reads the message's node and its thread root, rows the caller reads under row security, to find its conversation's parties"),

	// Plan D2's readers of the viewer settings: each reads only its own setting.
	"dispatch_viewer_sees":    invoker,
	"dispatch_viewer_open":    invoker,
	"dispatch_viewer_person":  invoker,
	"dispatch_viewer_session": invoker,
	"dispatch_viewer_private": invoker,
	"dispatch_request_role":   invoker,
	"dispatch_viewer":         invoker,

	// Every other function on main, each run as its invoker (plan D5): a trigger function's writes
	// stay within the row the request wrote, and a helper reads what its caller may read.
	"artifact_review_content_sha256_insert":     invoker,
	"artifact_review_content_sha256_update":     invoker,
	"artifact_reviews_event_seq":                invoker,
	"artifact_reviews_own_row":                  invoker,
	"artifact_version_text_sha256":              invoker,
	"artifact_versions_document_search":         invoker,
	"artifact_versions_issues_search":           invoker,
	"artifact_versions_search":                  invoker,
	"artifacts_ref_key":                         invoker,
	"ask_search_suffix":                         invoker,
	"asks_default_approval_requested_version":   invoker,
	"asks_normalize_approval_documents":         invoker,
	"asks_search":                               invoker,
	"asks_search_term_queue":                    invoker,
	"asks_written_version":                      invoker,
	"block_grammar_owe":                         invoker,
	"block_id_key":                              invoker,
	"block_pointer_targets":                     invoker,
	"block_ref_listed":                          invoker,
	"block_schema_changed":                      invoker,
	"broker_revocations_notify":                 invoker,
	"comment_side_table_watermark":              invoker,
	"comment_watermark_bump":                    invoker,
	"comments_search":                           invoker,
	"comments_search_term_queue":                invoker,
	"comments_watermark":                        invoker,
	"comments_written_version":                  invoker,
	"delivery_change_log":                       invoker,
	"delivery_pull_requests_changed":            invoker,
	"delivery_run_jobs_changed":                 invoker,
	"delivery_runs_changed":                     invoker,
	"delivery_session_groups_changed":           invoker,
	"delivery_settings_changed":                 invoker,
	"document_block_versions_owe":               invoker,
	"document_edit_notices_notify":              invoker,
	"document_search_term_queue":                invoker,
	"embeddings_enqueue":                        invoker,
	"embeddings_enqueue_issue":                  invoker,
	"embeddings_enqueue_issue_spec":             invoker,
	"embeddings_enqueue_window":                 invoker,
	"embeddings_label_project_key":              invoker,
	"embeddings_owner_project_key":              invoker,
	"events_notify":                             invoker,
	"events_session_title":                      invoker,
	"github_read_attempts_notify":               invoker,
	"issue_embedding_spec":                      invoker,
	"issue_embedding_text":                      invoker,
	"issue_search_vector":                       invoker,
	"issues_open_notify":                        invoker,
	"issues_search":                             invoker,
	"issues_search_term_queue":                  invoker,
	"item_blocks_owe":                           invoker,
	"legion_observations_current":               invoker,
	"messages_search":                           invoker,
	"messages_search_term_queue":                invoker,
	"owner_lock":                                invoker,
	"owner_lock_fleet":                          invoker,
	"owner_lock_fleet_read":                     invoker,
	"owner_lock_legacy_arrival":                 invoker,
	"owner_lock_legacy_registration":            invoker,
	"projects_owner_from_default":               invoker,
	"projects_owner_kept":                       invoker,
	"refs_excerpts_owed_notify":                 invoker,
	"refs_forms_owe":                            invoker,
	"refs_forms_owe_deleted":                    invoker,
	"refs_forms_owe_deleted_ask_block_refs":     invoker,
	"refs_forms_owe_deleted_block_refs":         invoker,
	"refs_forms_owe_deleted_comment_block_refs": invoker,
	"refs_forms_owe_deleted_message_block_refs": invoker,
	"refs_forms_owe_version":                    invoker,
	"refs_grammar_owe":                          invoker,
	"refs_grammar_owe_ask_scope":                invoker,
	"refs_grammar_owe_version":                  invoker,
	"search_document_text":                      invoker,
	"search_text":                               invoker,
	"search_vector":                             invoker,
	"title_lexemes":                             invoker,
	"user_agent_thread_dismissed_clear":         invoker,
	"user_threads_ask":                          invoker,
	"user_threads_comment":                      invoker,
	"user_threads_login":                        invoker,
	"user_threads_message":                      invoker,
}

// heldFunction is one function of schema public as the catalog holds it.
type heldFunction struct {
	signature string
	definer   bool
	settings  []string
}

// TestEveryTriggerAndFunctionDeclaresHowItRunsUnderTheRequestRole (plan D5): every function of
// schema public a freshly migrated database holds (pg_proc, no extension's member) is on the
// reviewed list, runs as the list says (prosecdef), and, where it runs as its owner, pins
// search_path to pg_catalog, public (proconfig), so a caller's search_path cannot hand it another
// table; every name on the list is a function the database holds; and every trigger of a table of
// schema public runs a function of schema public the list reviews, or one of pg_catalog.
func TestEveryTriggerAndFunctionDeclaresHowItRunsUnderTheRequestRole(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openMigratedTestStore(t, latestVersion)
	rows, err := store.Pool.Query(ctx, `
		select p.proname::text, p.oid::regprocedure::text, p.prosecdef, coalesce(p.proconfig, '{}')
		from pg_proc p
		where p.pronamespace = 'public'::regnamespace and p.prokind in ('f', 'p')
			and not exists (select from pg_depend d
				where d.classid = 'pg_proc'::regclass and d.objid = p.oid and d.deptype = 'e')
		order by 2`)
	if err != nil {
		t.Fatalf("read the functions of schema public: %v", err)
	}
	held := map[string][]heldFunction{}
	for rows.Next() {
		var name string
		var function heldFunction
		if err := rows.Scan(&name, &function.signature, &function.definer, &function.settings); err != nil {
			t.Fatalf("read a function of schema public: %v", err)
		}
		held[name] = append(held[name], function)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the functions of schema public: %v", err)
	}
	if len(held) == 0 {
		t.Fatal("the migrated database holds no function of schema public, so this test compares nothing")
	}

	triggers := map[string][]string{}
	var foreignTriggers []string
	rows, err = store.Pool.Query(ctx, `
		select t.tgname::text, c.relname::text, n.nspname::text, p.proname::text
		from pg_trigger t
		join pg_class c on c.oid = t.tgrelid
		join pg_proc p on p.oid = t.tgfoid
		join pg_namespace n on n.oid = p.pronamespace
		where not t.tgisinternal and c.relnamespace = 'public'::regnamespace
		order by 2, 1`)
	if err != nil {
		t.Fatalf("read the triggers of schema public: %v", err)
	}
	for rows.Next() {
		var trigger, table, schema, function string
		if err := rows.Scan(&trigger, &table, &schema, &function); err != nil {
			t.Fatalf("read a trigger of schema public: %v", err)
		}
		switch schema {
		case "public":
			triggers[function] = append(triggers[function], trigger+" on "+table)
		case "pg_catalog":
		default:
			foreignTriggers = append(foreignTriggers, fmt.Sprintf("%s on %s runs %s.%s", trigger, table, schema, function))
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the triggers of schema public: %v", err)
	}
	if len(foreignTriggers) > 0 {
		t.Errorf("triggers of schema public run a function of a schema the review does not read: %s", strings.Join(foreignTriggers, "; "))
	}
	runBy := func(name string) string {
		if on := triggers[name]; len(on) > 0 {
			return " (the trigger function of " + strings.Join(on, ", ") + ")"
		}
		return ""
	}

	var unreviewed, absent []string
	for _, name := range slices.Sorted(maps.Keys(held)) {
		reviewed, ok := reviewedFunctionSecurity[name]
		if !ok {
			unreviewed = append(unreviewed, held[name][0].signature+runBy(name))
			continue
		}
		for _, function := range held[name] {
			switch {
			case reviewed.definer && !function.definer:
				t.Errorf("%s%s runs as its invoker, and the review has it run as its owner (security definer): %s", function.signature, runBy(name), reviewed.reason)
			case !reviewed.definer && function.definer:
				t.Errorf("%s%s runs as its owner (security definer), and the review has it run as its invoker: a definer is reviewed with the reason it must see what its caller cannot", function.signature, runBy(name))
			}
			if reviewed.definer && function.definer && !slices.ContainsFunc(function.settings, func(setting string) bool {
				return strings.ReplaceAll(setting, " ", "") == "search_path=pg_catalog,public"
			}) {
				t.Errorf("%s runs as its owner and sets %q for its calls: it must pin search_path = pg_catalog, public, so a caller's search_path cannot hand it a table of another schema", function.signature, function.settings)
			}
		}
	}
	for _, name := range slices.Sorted(maps.Keys(reviewedFunctionSecurity)) {
		if _, ok := held[name]; !ok {
			absent = append(absent, name)
		}
	}
	if len(unreviewed) > 0 {
		t.Errorf("%d functions of schema public are not on the reviewed list (reviewedFunctionSecurity): review each as definer, with its reason, or invoker:\n  %s",
			len(unreviewed), strings.Join(unreviewed, "\n  "))
	}
	if len(absent) > 0 {
		t.Errorf("the reviewed list names %d functions the migrated database does not hold: %s", len(absent), strings.Join(absent, ", "))
	}
}
