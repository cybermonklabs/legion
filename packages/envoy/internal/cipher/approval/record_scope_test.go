package approval

import (
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/proxyrules"
	"github.com/sjawhar/envoy/internal/cipher/record"
	"github.com/sjawhar/envoy/internal/cipher/requests"
	"github.com/sjawhar/envoy/internal/cipher/secrets"
)

// TestThePagePrintsTheKindAsTheRecordReadIt: the page prints each name's kind, hosts and allow
// rows from the record it read (requests.RecordProfile), which carries the kind's name and the
// fingerprint beside them from one policy set, and reads the policy no second time. So when the
// live policy moves after the record was read, here to serve DEEL_TOKEN and DEEL_READ behind
// another kind, the page still prints one reading of each name, the record's. Each allow row is
// printed as policy.RowWords words it, a row of the profile's own host included.
func TestThePagePrintsTheKindAsTheRecordReadIt(t *testing.T) {
	const live = `[{"name": "open", "hosts": ["api.open.example"], "ways": ["swap"]}]`
	local := secrets.NewLocal(
		policytest.Proxied(policytest.Secret("DEEL_TOKEN", "ada@example.com", policy.TierHuman, "deel-v1"), "open"),
		policytest.Secret("DEEL_SOURCE", "ada@example.com", policy.TierHuman, "source-v1"),
	)
	cur := policytest.ProfiledCurrent(t, local, policytest.KindsFileOf(live, `{"name": "DEEL_READ", "source": "DEEL_SOURCE", "proxy": "open"}`))
	if got := cur.Get().ProxyRules().HostsOf("DEEL_TOKEN"); len(got) != 1 || got[0] != "api.open.example" {
		t.Fatalf("the live policy binds DEEL_TOKEN to %v; want the open kind's api.open.example, which this row is about", got)
	}
	p := &Page{cfg: Config{Machine: &requests.Machine{Policy: cur}}}
	now := time.Now()
	read := []proxyrules.Allow{{Method: "GET", PathPrefix: "/v1/payroll/"}, {Method: "POST", PathPrefix: "/v1/payroll/exports", Host: "api.deel.example"}}
	detail := requests.RecordDetail{
		RecordID: strings.Repeat("a", 64), Kind: record.KindAgentSecret, State: "pending", Approver: "ada@example.com",
		Identifiers: []string{"DEEL_TOKEN", "DEEL_READ"}, ExpiresAt: now.Add(time.Hour), RequestedAt: now,
		Profiles: []requests.RecordProfile{
			{Name: "DEEL_TOKEN", Kind: "deel", Hosts: []string{"api.deel.example"}, Allow: read, Digest: strings.Repeat("d", 64)},
			{Name: "DEEL_READ", Profile: cur.Get().Secrets["DEEL_READ"].Profile, Kind: "deel", Hosts: []string{"api.deel.example", "files.deel.example"}, Digest: strings.Repeat("e", 64)},
		},
	}
	v := p.recordView(detail, "ada@example.com", now)
	want := proxiedView{Name: "DEEL_TOKEN", Kind: "deel", ProxyHosts: "api.deel.example", Allow: "GET /v1/payroll/, POST /v1/payroll/exports on api.deel.example"}
	if len(v.Proxied) != 1 || v.Proxied[0] != want {
		t.Errorf("the page prints DEEL_TOKEN as %+v; want the record's reading %+v", v.Proxied, want)
	}
	if len(v.Profiles) != 1 || v.Profiles[0].ProxyHosts != "api.deel.example, files.deel.example" {
		t.Errorf("the page prints DEEL_READ as %+v; want the record's hosts api.deel.example, files.deel.example", v.Profiles)
	}
}
