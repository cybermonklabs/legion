package api

import (
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// A batch's fact names each request with the end it reports: the same batch sent again, in
// another order or with its ends written in another zone, is the fact already applied, and a batch
// that reports a later end of a request is a fact of its own, so the store moves the request to
// that end (record.Store.KeepModelRequests) rather than taking the report as a duplicate. The
// Claude Code bridge sends such a batch where a block of a request came after its report.
func TestAModelRequestsFactNamesTheEndOfEachRequest(t *testing.T) {
	const token = claim.Token("legion-acme-acme-7-implementer")
	end := time.Date(2026, time.November, 26, 10, 0, 58, 0, time.UTC)
	request := func(id string, ended time.Time) claim.ModelRequest {
		return claim.ModelRequest{ID: id, Session: "ses_implementer", Model: "acme-model", EndedAt: ended}
	}
	batch := []claim.ModelRequest{request("msg_a", end), request("msg_b", end.Add(time.Second))}
	id := modelRequestsFactID(token, 3, batch)
	for name, again := range map[string][]claim.ModelRequest{
		"the batch again":                  batch,
		"the batch in another order":       {batch[1], batch[0]},
		"the batch's ends in another zone": {request("msg_a", end.In(time.FixedZone("UTC+2", 2*60*60))), batch[1]},
	} {
		if got := modelRequestsFactID(token, 3, again); got != id {
			t.Errorf("%s is the fact %s, want %s, the fact already applied", name, got, id)
		}
	}
	for name, other := range map[string]struct {
		generation uint64
		requests   []claim.ModelRequest
	}{
		"a later end of a request": {3, []claim.ModelRequest{request("msg_a", end.Add(12*time.Second)), batch[1]}},
		"another id":               {3, []claim.ModelRequest{request("msg_a", end), request("msg_c", end.Add(time.Second))}},
		"another launch":           {4, batch},
	} {
		if got := modelRequestsFactID(token, other.generation, other.requests); got == id {
			t.Errorf("%s is the fact %s of the first batch, want a fact of its own", name, got)
		}
	}
}
