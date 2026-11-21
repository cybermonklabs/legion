package requests

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/proxyrules"
)

// grantUsedDetails reads the whole detail of each grant.used audit row of grantID, oldest first.
func grantUsedDetails(t *testing.T, m *Machine, grantID string) []map[string]any {
	t.Helper()
	rows, err := m.Store.Pool.Query(t.Context(), `select detail from audit where kind='grant.used' and grant_id=$1 order by id`, grantID)
	if err != nil {
		t.Fatalf("read grant.used audit: %v", err)
	}
	raws, err := pgx.CollectRows(rows, pgx.RowTo[[]byte])
	if err != nil {
		t.Fatalf("read grant.used audit: %v", err)
	}
	details := make([]map[string]any, len(raws))
	for i, raw := range raws {
		if err := json.Unmarshal(raw, &details[i]); err != nil {
			t.Fatalf("decode grant.used detail %s: %v", raw, err)
		}
	}
	return details
}

// TestEachReleaseWritesOneGrantUsedRowWithItsOwnDetail: both releases of a grant run one loop and
// write that row through one helper (release.go), so the trail an operator reads cannot gain a
// row on one path and lose it on the other. Each row names its grant's reason, and the rest is the
// writer's own: the session's release names every name it handed over, the command it starts
// (none here, as a client from before the field sends) and the narrowing it applied, the proxy's
// names the names behind the airlock alone and marks them as the airlock's.
func TestEachReleaseWritesOneGrantUsedRowWithItsOwnDetail(t *testing.T) {
	m, enr, key, _ := newPlaceholderFixture(t, proxyrules.Rules{Secrets: []proxyrules.SecretRule{rule("AUTO_TOKEN", "")}})
	retag(t, m, policytest.Secret("SECOND_TOKEN", fixtureOperator, policy.TierAgent, "second-v1"))
	_, grantID := grantOf(t, m, enr, key, "read both", "AUTO_TOKEN", "SECOND_TOKEN")

	if _, err := m.Values(t.Context(), grantID, enr, nil, ReleaseCall{}); err != nil {
		t.Fatalf("Values: %v", err)
	}
	if _, _, err := m.AirlockValues(t.Context(), grantID); err != nil {
		t.Fatalf("AirlockValues: %v", err)
	}

	want := []map[string]any{
		{"names": []any{"AUTO_TOKEN", "SECOND_TOKEN"}, "reason": "read both", "command": []any{}},
		{"names": []any{"AUTO_TOKEN"}, "airlock": true, "reason": "read both"},
	}
	if details := grantUsedDetails(t, m, grantID); !reflect.DeepEqual(details, want) {
		t.Fatalf("grant.used details = %v; want %v: one row per release, each with its own detail", details, want)
	}
}
