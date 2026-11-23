package api

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
)

// A change request is compared against the approved spec of its repository, which another issue's
// approval, or an import of an approved level, writes while the request's own documents stand. So
// the read names the approved spec it compared against (approved_spec_mark), and each act that
// writes the approved spec appends spec.approved_written on its own issue, naming what it wrote,
// for a reader of the other requests to hear (R22, R25). These tests drive the approval and import
// routes and read the requests and the events back.

// approvedWrittenPayload is the payload of spec.approved_written, as the contract writes it.
type approvedWrittenPayload struct {
	Issue string `json:"issue"`
	Repo  string `json:"repo"`
	Act   string `json:"act"`
	Nodes []struct {
		ID      string `json:"id"`
		Kind    string `json:"kind"`
		Version int    `json:"version"`
		Removed bool   `json:"removed"`
	} `json:"nodes"`
}

// approvedWritten is each spec.approved_written payload appended on the issue key, oldest first.
func approvedWritten(t *testing.T, rig *specRig, key string) []approvedWrittenPayload {
	t.Helper()
	rows, err := rig.database.Pool.Query(context.Background(),
		`select payload::text from events where issue_key = $1 and type = 'spec.approved_written' order by id`, key)
	if err != nil {
		t.Fatalf("read the spec.approved_written events of %s: %v", key, err)
	}
	defer rows.Close()
	var payloads []approvedWrittenPayload
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			t.Fatalf("read a spec.approved_written event of %s: %v", key, err)
		}
		var payload approvedWrittenPayload
		if err := json.Unmarshal([]byte(raw), &payload); err != nil {
			t.Fatalf("decode the spec.approved_written event %s: %v", raw, err)
		}
		payloads = append(payloads, payload)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the spec.approved_written events of %s: %v", key, err)
	}
	return payloads
}

// writtenIDs is the id of each node the payload names, sorted.
func writtenIDs(payload approvedWrittenPayload) []string {
	ids := make([]string, 0, len(payload.Nodes))
	for _, node := range payload.Nodes {
		ids = append(ids, node.ID)
	}
	slices.Sort(ids)
	return ids
}

// TestAnApprovalElsewhereMovesTheApprovedSpecARequestIsReadAgainst: an issue that holds the approved
// check as context is read against the approved spec the seed wrote. An approval elsewhere of a
// statement it does not hold moves the approved spec it is read against, and changes nothing of its
// read; an approval that writes nothing moves nothing; an approval that changes the check it holds
// moves the approved spec again, and its read now changes a check no person approved, at the same
// two document versions. Each approval that wrote a node appended one spec.approved_written on its
// own issue naming each node it wrote, and no other approval appended one. An issue of a repository
// that is no spec source is read against no approved spec.
func TestAnApprovalElsewhereMovesTheApprovedSpecARequestIsReadAgainst(t *testing.T) {
	rig := newSpecRig(t)
	seed := rig.seed(t)
	if events := approvedWritten(t, rig, seed.Key); len(events) != 1 || events[0].Act != "approval" || events[0].Repo != specRepo ||
		!slices.Equal(writtenIDs(events[0]), []string{"a-bill-names-its-order", "a-second-payment-is-refused", "an-order-is-paid-once", "bank", "billing", "billing-to-bank"}) {
		t.Fatalf("the seed's approval appended %+v; want one spec.approved_written of the approval, naming each node it wrote", events)
	}
	holder := rig.issue(t, "A bill shows its number", specRepo, "Why the bill changes.\n", paidOnceCheck("1"))
	first, _ := changeRequestOf(t, rig, holder.Key, "")
	if first.ApprovedSpecMark <= 0 || first.ChangesChecks || first.Proposes {
		t.Fatalf("the change request that holds the approved check as context reads %+v; want it read against the seed's approved spec, changing nothing", first)
	}

	unrelated := rig.issue(t, "A refund names its order", specRepo, ":::requirement{#a-refund-names-its-order kind=\"behavior\"}\nA refund names its order.\n:::\n", "")
	rig.approve(t, rig.request(t, unrelated, false))
	second, _ := changeRequestOf(t, rig, holder.Key, "")
	if second.ApprovedSpecMark <= first.ApprovedSpecMark || second.ChangesChecks || second.Version != first.Version || second.VerificationVersion != first.VerificationVersion {
		t.Fatalf("after an approval elsewhere of a statement the request does not hold it reads %+v (before: %+v); want a newer approved spec and the same read", second, first)
	}
	if events := approvedWritten(t, rig, unrelated.Key); len(events) != 1 || !slices.Equal(writtenIDs(events[0]), []string{"a-refund-names-its-order"}) {
		t.Fatalf("the approval of a new statement appended %+v; want one spec.approved_written naming that statement", events)
	}

	quiet := rig.issue(t, "Prose about the receipt", specRepo, "Prose about the receipt alone.\n", "")
	rig.approve(t, rig.request(t, quiet, false))
	if third, _ := changeRequestOf(t, rig, holder.Key, ""); third.ApprovedSpecMark != second.ApprovedSpecMark {
		t.Fatalf("after an approval that writes nothing the request is read against approved spec %d, want %d still", third.ApprovedSpecMark, second.ApprovedSpecMark)
	}
	if events := approvedWritten(t, rig, quiet.Key); len(events) != 0 {
		t.Fatalf("an approval that wrote nothing appended %+v; want no spec.approved_written", events)
	}

	changer := rig.issue(t, "The refusal gets a day", specRepo, "Why the check changes.\n",
		paidOnceCheckClaiming("A second payment of one order is refused within a day.", "1"))
	rig.approve(t, rig.request(t, changer, true))
	fourth, _ := changeRequestOf(t, rig, holder.Key, "")
	if fourth.ApprovedSpecMark <= second.ApprovedSpecMark || !fourth.ChangesChecks || !fourth.ChangesChecksUnapproved ||
		fourth.Version != first.Version || fourth.VerificationVersion != first.VerificationVersion {
		t.Fatalf("after an approval elsewhere that changes the check the request holds it reads %+v; want a newer approved spec and a changed check no person approved, "+
			"at versions %d and %d", fourth, first.Version, first.VerificationVersion)
	}
	if events := approvedWritten(t, rig, changer.Key); len(events) != 1 || !slices.Equal(writtenIDs(events[0]), []string{"a-second-payment-is-refused"}) ||
		events[0].Nodes[0].Version != 2 || events[0].Nodes[0].Kind != "check" {
		t.Fatalf("the approval of a changed check appended %+v; want one spec.approved_written naming version 2 of the check", events)
	}

	// A refused approval writes nothing: the holder's check is stale now, so its own approval is
	// refused, appends no spec.approved_written, and moves no approved spec.
	rig.refused(t, rig.request(t, holder, true), "SPEC_REQUEST_STALE")
	if events := approvedWritten(t, rig, holder.Key); len(events) != 0 {
		t.Fatalf("a refused approval appended %+v; want no spec.approved_written", events)
	}
	if fifth, _ := changeRequestOf(t, rig, holder.Key, ""); fifth.ApprovedSpecMark != fourth.ApprovedSpecMark {
		t.Fatalf("after a refused approval the request is read against approved spec %d, want %d still", fifth.ApprovedSpecMark, fourth.ApprovedSpecMark)
	}

	plain := rig.issue(t, "Tools runner prose", "acme/tools", "Prose about the tools.\n", "")
	if read, _ := changeRequestOf(t, rig, plain.Key, ""); read.SpecSource || read.ApprovedSpecMark != 0 {
		t.Fatalf("the change request of an issue of no spec source reads %+v; want it read against no approved spec", read)
	}
}

// TestAnImportAppendsWhatItWroteIntoTheApprovedSpec: the import of an approved level writes the
// approved spec of its repository as an approval does, so it appends one spec.approved_written on
// the level's issue naming each node it wrote, and an import run again that writes nothing appends
// none.
func TestAnImportAppendsWhatItWroteIntoTheApprovedSpec(t *testing.T) {
	rig := newLevelRig(t, "Level one of the fixture", levelFixture)
	notes := rig.upload(t, "notes.md", levelNotesFixture)
	rig.approveLevel(t)
	rig.approveDocument(t, notes.ID)
	if events := approvedWritten(t, rig.specRig, rig.level.Key); len(events) != 0 {
		t.Fatalf("the level's approval, given before its repository was a spec source, appended %+v; want nothing", events)
	}
	imported := rig.imported(t, "notes.md")
	events := approvedWritten(t, rig.specRig, rig.level.Key)
	if len(events) != 1 || events[0].Act != "import" || events[0].Repo != specRepo ||
		!slices.Equal(writtenIDs(events[0]), slices.Sorted(slices.Values(imported.Written))) {
		t.Fatalf("the import that wrote %v appended %+v; want one spec.approved_written of the import naming each node it wrote", imported.Written, events)
	}
	if again := rig.imported(t, "notes.md"); len(again.Written) != 0 {
		t.Fatalf("the import run again wrote %v, want nothing", again.Written)
	}
	if events := approvedWritten(t, rig.specRig, rig.level.Key); len(events) != 1 {
		t.Fatalf("after an import that wrote nothing the level holds %d spec.approved_written events, want 1", len(events))
	}
}
