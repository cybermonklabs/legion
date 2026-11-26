package api

// The reading of one statement's parse tree (m19_statement_binding_test.go holds the rule this
// serves): which rows each table of the statement answers, the walk that records its uses, its
// equalities and its constants, and the node helpers the walk reads the tree with. The scanner of
// its fragments is m19_statement_spans_test.go, and which of the rows a clause binds is
// m19_statement_bind_test.go.
