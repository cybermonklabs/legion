package api

import (
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// approvalMovedTwoPaths is approvalMoved as it was written before it tested the documents key
// first: both path queries on every approval row, or-ed. It is the oracle the rule is held to.
const approvalMovedTwoPaths = `(a.kind = 'approval'
		and (jsonb_path_exists(a.approval, '$.documents[*] ? (@.requested_version < @.version)')
			or jsonb_path_exists(a.approval, '$ ? (!exists(@.documents) && @.requested_version < @.version)')))`

// The moved-approval rule reads the documents key first and then runs one path query, where the
// rule before it ran both. It answers as that rule did for every shape an approval column or an
// older payload can hold: SQL null, JSON null, an array, an object with no documents key (moved
// and not moved), an object whose documents is null, and documents of which one is moved or none
// is, and for an ask that is not an approval.
func TestTheMovedApprovalRuleAnswersAsTheTwoPathRuleDid(t *testing.T) {
	database := storetest.Open(t)
	rows, err := database.Pool.Query(t.Context(), `
		select a.label, `+approvalMoved+` as moved, `+approvalMovedTwoPaths+` as was
		from (values
			('SQL null', 'approval', null::jsonb),
			('JSON null', 'approval', 'null'::jsonb),
			('an empty array', 'approval', '[]'::jsonb),
			('an array that holds a moved pair', 'approval', '[{"version": 2, "requested_version": 1}]'::jsonb),
			('an array that holds the word documents', 'approval', '["documents"]'::jsonb),
			('no documents key, moved', 'approval', '{"version": 2, "requested_version": 1}'::jsonb),
			('no documents key, not moved', 'approval', '{"version": 2, "requested_version": 2}'::jsonb),
			('no documents key and no requested version', 'approval', '{"version": 2}'::jsonb),
			('documents null', 'approval', '{"documents": null, "version": 2, "requested_version": 1}'::jsonb),
			('documents empty', 'approval', '{"documents": [], "version": 2, "requested_version": 1}'::jsonb),
			('the second document moved', 'approval', '{"documents": [{"version": 1, "requested_version": 1}, {"version": 3, "requested_version": 2}], "version": 1, "requested_version": 1}'::jsonb),
			('no document moved', 'approval', '{"documents": [{"version": 1, "requested_version": 1}], "version": 2, "requested_version": 1}'::jsonb),
			('a question', 'question', '{"version": 2, "requested_version": 1}'::jsonb)
		) as a(label, kind, approval)`)
	if err != nil {
		t.Fatalf("evaluate the two rules: %v", err)
	}
	defer rows.Close()
	read := 0
	for rows.Next() {
		var label string
		var moved, was *bool
		if err := rows.Scan(&label, &moved, &was); err != nil {
			t.Fatalf("scan the two rules: %v", err)
		}
		read++
		if (moved == nil) != (was == nil) || moved != nil && *moved != *was {
			t.Errorf("%s: the rule answers %v, the two-path rule %v", label, describeBool(moved), describeBool(was))
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("evaluate the two rules: %v", err)
	}
	if read != 13 {
		t.Fatalf("the two rules answered %d rows, want 13", read)
	}
}

// describeBool is a nullable boolean as SQL prints it.
func describeBool(value *bool) string {
	switch {
	case value == nil:
		return "null"
	case *value:
		return "true"
	}
	return "false"
}
