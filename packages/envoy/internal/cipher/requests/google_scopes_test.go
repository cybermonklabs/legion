package requests

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/record"
	"github.com/sjawhar/envoy/internal/cipher/secrets"
)

// TestAScopesTagChangeEndsTheGrantsDecidedUnderTheOldScopes: a google-service-account secret's
// scopes tag is what its minted token may do, so a grant decided under one scopes tag does not
// release a token minted under another. The secret by its own name, and a profile of it with no
// google block of its own, which mints under its source's tag, are each granted under
// devstorage.read_only and released once through the real minter against a stand-in of IAM's
// generateAccessToken, which records the scopes each mint asked for; the source's tag is then
// widened to cloud-platform, and the next release of the same grant ends it (ErrGrantNotLive)
// with no mint at all. The controls: a profile with a google block of its own, whose scopes are
// its entry's and not the tag's, mints under the widened tag at its own scope, the grant live;
// and the first release of each grant mints at the scope it was decided under.
func TestAScopesTagChangeEndsTheGrantsDecidedUnderTheOldScopes(t *testing.T) {
	t.Parallel()
	const (
		account  = "robot-1@acme-deployment.iam.gserviceaccount.com"
		readOnly = "https://www.googleapis.com/auth/devstorage.read_only"
		bigquery = "https://www.googleapis.com/auth/bigquery"
	)
	for _, c := range []struct {
		name, profiles, asked, sourceScopes string
		// firstScopes is the scope the first release mints at; live is whether the grant
		// outlives the tag's change, and then secondScopes is the scope its next release mints at.
		firstScopes  []string
		live         bool
		secondScopes []string
	}{
		{"the source by its own name", "", "GCP_FLEET", "devstorage.read_only", []string{readOnly}, false, nil},
		{"a profile with no google block", `{"name": "GCP_FLEET_READ", "source": "GCP_FLEET"}`, "GCP_FLEET_READ", "devstorage.read_only", []string{readOnly}, false, nil},
		{"the control: a profile with a google block", `{"name": "GCP_FLEET_BIGQUERY", "source": "GCP_FLEET", "google": {"scopes": ["bigquery"]}}`, "GCP_FLEET_BIGQUERY", "bigquery devstorage.read_only", []string{bigquery}, true, []string{bigquery}},
	} {
		t.Run(c.name, func(t *testing.T) {
			machine, enrollmentID, key, _ := newFixture(t)
			source := policytest.TypeSecret("GCP_FLEET", fixtureOperator, policy.TierAgent, policy.TypeGoogleServiceAccount, policytest.TypeValue(policy.TypeGoogleServiceAccount))
			source.Tags[policy.TagScopes] = c.sourceScopes
			fixtureStore(machine).Put(source)
			machine.Policy = policytest.ProfiledCurrent(t, fixtureStore(machine), policytest.ProfilesFile(c.profiles), fixtureService)
			var mu sync.Mutex
			var asked [][]string
			iam := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var sent secrets.GenerateAccessTokenRequest
				if err := json.NewDecoder(r.Body).Decode(&sent); err != nil {
					t.Errorf("the minter's request to IAM is no JSON: %v", err)
				}
				mu.Lock()
				asked = append(asked, sent.Scope)
				n := len(asked)
				mu.Unlock()
				_ = json.NewEncoder(w).Encode(map[string]string{"accessToken": "minted-" + strconv.Itoa(n)})
			}))
			t.Cleanup(iam.Close)
			minter := &secrets.GoogleServiceAccount{
				BaseURL: iam.URL, HTTP: iam.Client(), Source: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "broker-own-not-a-secret"}),
				Accounts: map[string]string{"GCP_FLEET": account},
			}
			machine.Minters = map[string]Minter{policy.TypeGoogleServiceAccount: minter.Mint}
			hour := 3600
			compact, err := record.Sign(key, machine.Audience, []record.AuthorizationDetail{{Type: record.KindAgentSecret, Identifier: c.asked, Actions: []string{"inject"}}}, "read the bucket", "", &hour, time.Now())
			if err != nil {
				t.Fatalf("record.Sign: %v", err)
			}
			request, err := machine.Create(t.Context(), enrollmentID, compact, "")
			if err != nil || request.GrantID == nil {
				t.Fatalf("Create(%s) = %+v, %v; want a grant", c.asked, request, err)
			}
			if release, err := machine.Values(t.Context(), *request.GrantID, enrollmentID, ReleaseCall{}); err != nil || release.Values[c.asked] != "minted-1" {
				t.Fatalf("the first Values = %v, %v; want %s's token", release.Values, err, c.asked)
			}
			if want := [][]string{c.firstScopes}; !reflect.DeepEqual(asked, want) {
				t.Fatalf("the first release asked IAM for %v, want %v", asked, want)
			}

			source.Tags[policy.TagScopes] = "bigquery cloud-platform"
			retag(t, machine, source)
			release, err := machine.Values(t.Context(), *request.GrantID, enrollmentID, ReleaseCall{})
			mu.Lock()
			defer mu.Unlock()
			if !c.live {
				if !errors.Is(err, ErrGrantNotLive) || release.Values != nil || len(asked) != 1 {
					t.Errorf("after GCP_FLEET's scopes tag widened to cloud-platform, Values = %v, %v, and IAM was asked for %v; want the grant ended (ErrGrantNotLive) and no second mint", release.Values, err, asked)
				}
				if err != nil && !strings.Contains(err.Error(), policy.TagScopes+" tag") {
					t.Errorf("the refusal %q does not name the %s tag, which is what moved", err, policy.TagScopes)
				}
				return
			}
			if err != nil || release.Values[c.asked] != "minted-2" || len(asked) != 2 || !reflect.DeepEqual(asked[1], c.secondScopes) {
				t.Errorf("after the tag widened, the control's Values = %v, %v, and IAM was asked for %v; want the grant live, minting at %v", release.Values, err, asked, c.secondScopes)
			}
		})
	}
}
