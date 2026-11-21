package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/sjawhar/envoy/internal/cipher/config"
	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/proxyrules"
	"github.com/sjawhar/envoy/internal/cipher/record"
	"github.com/sjawhar/envoy/internal/cipher/secrets"
	"github.com/sjawhar/envoy/internal/metrics"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The group, its member, an outsider and the reader account of the wiring test: example names
// alone, since the repository is public. readerBearer is the reader account's token the IAM
// stand-in mints, and podBearer the pod's own credential; neither is a credential.
const (
	wiredGroup    = "approvers@example.com"
	wiredMember   = "member@example.com"
	wiredOutsider = "outsider@example.com"
	wiredReader   = "group-reader@example-project.iam.gserviceaccount.com"
	readerBearer  = "reader-" + "minted"
	podBearer     = "pod-" + "own"
)

// groupStands are stand-ins of IAM's generateAccessToken, answering the reader account's token,
// and of the Directory's members.list, answering wiredMember as the one member of wiredGroup when
// it is asked with that token.
func groupStands(t *testing.T) (iam, directory *httptest.Server) {
	t.Helper()
	iam = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/projects/-/serviceAccounts/"+wiredReader+":generateAccessToken" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"accessToken": readerBearer, "expireTime": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)})
	}))
	t.Cleanup(iam.Close)
	directory = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+readerBearer || r.URL.Path != "/admin/directory/v1/groups/"+wiredGroup+"/members" {
			http.Error(w, `{"error":{"code":403,"errors":[{"reason":"forbidden"}]}}`, http.StatusForbidden)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"members": []map[string]string{{"email": wiredMember, "type": "USER", "role": "MEMBER"}}})
	}))
	t.Cleanup(directory.Close)
	return iam, directory
}

// TestEveryDecisionSiteAdmitsAMember builds the broker's decision sites as main wires them — the
// group reader (newGroupReader), the policy's loader (policyLoader) and the request machine with
// its chain check (newRequestMachine), which the approval page reads its group reader from — with
// the reader at stand-ins that list one member, and asks each site about that member and an
// outsider: the policy gives the member the group's agent-tier secret at once and asks the group
// for the outsider's; record.MayDecide on the policy's groups admits the member alone; the
// machine lists the group for the member alone; its chain check admits the member alone.
func TestEveryDecisionSiteAdmitsAMember(t *testing.T) {
	iam, directory := groupStands(t)
	cfg := config.Config{SecretsPrefix: policytest.Prefix, DirectoryURL: directory.URL, DirectoryAccount: wiredReader}
	reader := newGroupReader(cfg, iam.URL, oauth2.StaticTokenSource(&oauth2.Token{AccessToken: podBearer}))
	reader.Start(t.Context())

	local := secrets.NewLocal(policytest.Secret("TEAM_TOKEN", record.GroupPrefix+wiredGroup, policy.TierAgent, "team-v1"))
	current, err := policy.NewCurrent(t.Context(), policyLoader(cfg, policytest.Store(local), proxyrules.Rules{}, metrics.New(), reader), time.Hour)
	if err != nil {
		t.Fatalf("NewCurrent: %v", err)
	}
	testwait.Eventually(t, "the group read the first load started", func() bool { return reader.Fresh(wiredGroup) })

	set := current.Get()
	for _, c := range []struct {
		login string
		want  string
	}{{wiredMember, policy.Automatic}, {wiredOutsider, policy.Approval}} {
		if d, err := set.Evaluate("TEAM_TOKEN", policy.Requester{Operator: c.login}); err != nil || d.Outcome != c.want {
			t.Errorf("the policy's Evaluate(TEAM_TOKEN, %s) = %+v, %v; want %s", c.login, d, err, c.want)
		}
	}
	approver := record.GroupPrefix + wiredGroup
	machine := newRequestMachine(cfg, nil, current, nil, nil, proxyrules.Rules{}, "", nil, nil, reader)
	for _, c := range []struct {
		login string
		want  bool
	}{{wiredMember, true}, {strings.ToUpper(wiredMember), true}, {wiredOutsider, false}} {
		sites := map[string]bool{
			"the policy's groups (record.MayDecide)": record.MayDecide(record.KindAgentSecret, approver, "", c.login, set.Groups()),
			"the request machine (GroupsOf)":         machine.Groups != nil && slices.Equal(machine.Groups.GroupsOf(c.login), []string{wiredGroup}),
			"the machine's chain check":              record.MayDecide(record.KindAgentSecret, approver, "", c.login, machine.Chain.Groups),
		}
		for site, got := range sites {
			if got != c.want {
				t.Errorf("%s admits %s: %v, want %v", site, c.login, got, c.want)
			}
		}
	}
}
