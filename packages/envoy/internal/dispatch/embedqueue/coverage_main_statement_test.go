package embedqueue

import _ "embed"

// mainRouteCoverageStatement is the statement main's search coverage route ran, byte for byte as
// the Go of df18d9be built it (coverageQuery in embedqueue/coverage.go there): it counted every
// item of every covered kind and every row of the not-indexed tables on each call, so its reads
// grew with the corpus. main ran it under mainRouteCoverageSettings, in the transaction Coverage
// bounds. The route never sends it now (coverageRead, TestTheCoverageRouteSendsNoStatementOfMains).
// It is one side of every comparison of the report's answers, beside mainCoverageQuery, the
// statement before it, and the statement the buffer reads set beside the kept counts' read
// (TestTheCoverageReadsOfMainAndTheHead on the seeded database, and the production check). The
// file testdata/main_route_coverage_statement.sql is the statement: this package embeds it and
// cmd/dispatch's process tests read it (readCoverageOracles), so both hold the route to one text.
// The file must never be edited to follow a schema change: a copy edited to fit answers for
// nothing that was ever served.
//
//go:embed testdata/main_route_coverage_statement.sql
var mainRouteCoverageStatement string

// mainRouteCoverageSettings are the settings main ran mainRouteCoverageStatement under, byte for
// byte as df18d9be's planCoverage set them (coverageSettings there), each set local in the
// statement's transaction. The file testdata/main_route_coverage_settings.sql is the settings,
// which cmd/dispatch's process tests read too (readCoverageOracles); it must never be edited
// either.
//
//go:embed testdata/main_route_coverage_settings.sql
var mainRouteCoverageSettings string
