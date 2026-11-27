package store

// TypeMigrations are the migrations that write the store's type nodes, by file name in the
// migrations directory, in the order the runner applies them: the seed, then each migration that
// has changed a type since. Each appends the schema's own supertag and attribute nodes to
// graph_nodes and their tuples to graph_tuples, and no other migration writes the tuple table.
// A new type migration joins this list. The tests that hold the type migrations read them from
// here: cmd/dispatch's block schema bump test, which tells the issue-tree spec's from the ones
// before them and reads the schema version a store migrated through each side carries, and refs'
// plan test, which reads their statements against the tuple table as inserts alone.
// cmd/dispatch's typeMigrations names the ones a second run proves, those numbered before the
// store's backfill whose rows a second run writes again.
var TypeMigrations = []string{
	"20261109051232_graph_supertags.up.sql",
	"20261113230000_graph_supertags_check_ci_check.up.sql",
	"20261115130000_graph_supertags_element_versions.up.sql",
	// The interaction type's `source` attribute node, as literal rows.
	"20261122230406_interaction_source.up.sql",
	// plan_task's `steps` at block schema version 5 (#4611), as literal rows written after the
	// store's backfill.
	"20261123080000_graph_supertags_plan_task_steps.up.sql",
	// The lens_kind supertag and the attribute nodes of the types that declare no block type, as
	// literal rows, each held to its migration by cmd/dispatch's
	// requireStoreOnlyAttributesMatchTheirMigrations.
	"20261123174710_lens_catalogue.up.sql",
	// No type node, but it appends the tuples of the catalogue nodes it writes beside
	// 20261123174710's, under that migration's writer.
	"20261123174720_lens_catalogue_further_kinds.up.sql",
	// The issue-tree spec's `query` and `query version` supertags, as literal rows written after
	// the store's backfill.
	"20261123174750_issue_tree_query_supertags.up.sql",
	// check's `subject` and `program`, read from block schema version 6; the decision and reading
	// block types at version 7, creatable from version 8; the bump to version 8 that makes them
	// creatable; and the issue-tree spec's block schema at version 9; each as literal rows written
	// after the store's backfill.
	"20261125125701_graph_supertags_check_subject.up.sql",
	"20261125163555_graph_supertags_decision_reading.up.sql",
	"20261126040700_graph_supertags_version_8.up.sql",
	"20261126050010_issue_tree_block_schema.up.sql",
}
