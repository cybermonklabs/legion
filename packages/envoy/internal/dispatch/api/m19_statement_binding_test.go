package api

// What one statement of a project read binds, read as Postgres reads it (libpg_query, through
// github.com/wasilibs/go-pgquery, the parser the migration census already reads SQL with).
//
// The rule the census holds a statement to: every project-owned table the statement reads - in a
// FROM list, a comma join, a JOIN, a subselect or the body of a function it calls - is bound by a
// clause on that table's own alias, or joined by its own owner column to a table that is. A clause
// is bound to an alias by the columns it reads: a fragment that reads no column of a row decides
// nothing about that row, and a fragment that reads a reference the row holds to another project's
// object (issue_links.target_key) decides the object it names, not the row.
//
// The project-owned tables are read off the migrated schema and the reference census
// (referenceDispositions), never listed here: a table is project-owned when it holds a column that
// names the object whose visibility decides its rows - one of the six objects a read names by id,
// or a node of the graph - and the census fails on a column of that shape the reference census
// does not class, and on a class of its own the schema does not hold.
//
// What the census does not judge: the reads a clause makes of its own, inside the fragment it
// renders (the rule cannot be asked to bind itself), and the body of a function such a clause
// calls (session_clause_visible). It reads those bodies all the same, and
// TestEveryProjectReadBindsTheFragmentPerStatement fails if it read none of them.
