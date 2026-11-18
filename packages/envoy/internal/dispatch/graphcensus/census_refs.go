package graphcensus

// refProjections are the census's kinds of the references an artifact makes, the refs rows whose
// source is an artifact (projections). It holds none.
var refProjections []projection

// refBackfills are the backfill's passes over the references an artifact makes (backfillPasses).
// It holds none.
var refBackfills []backfillPass
