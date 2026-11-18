package graphcensus

// The search topic's lists stand in census_search.go and not in census_windows.go: Go builds a
// file whose name ends in _windows.go for GOOS windows alone.

// searchProjections are the census's kinds of the search windows a node's text is read in
// (projections). It holds none.
var searchProjections []projection

// searchBackfills are the backfill's passes over the search windows a node's text is read in
// (backfillPasses). It holds none.
var searchBackfills []backfillPass
