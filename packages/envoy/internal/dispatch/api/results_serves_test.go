package api

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"
)

// Each check of the results read names the statements its block serves, each reference as the
// block writes it and in the block's order: k1 serves one requirement, k2 two, written r2 first so
// that an order the read made of its own would show, and k3 none, which carries no serves at all.
func TestCheckResultsCarryWhatEachCheckServes(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "ACME", "Serves", twoRequirements)
	r1, r2 := "dispatch://"+issue.Key+"/spec#b-r1", "dispatch://"+issue.Key+"/spec#b-r2"
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md",
		checkServing("k1", r1)+"\n"+checkServing("k2", r2+" "+r1)+"\n"+
			":::check{#k3 surface=\"cli\" run=\"go test ./...\"}\nThe check that serves nothing.\n:::\n")

	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/results", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read the results of %s: status=%d body=%s", issue.Key, response.Code, response.Body.String())
	}
	read := decodeBody[issueResults](t, response)
	if len(read.Checks) != 3 {
		t.Fatalf("checks = %+v, want k1, k2 and k3", read.Checks)
	}
	for _, want := range []struct {
		check  string
		serves []string
	}{
		{"k1", []string{r1}},
		{"k2", []string{r2, r1}},
		{"k3", nil},
	} {
		if got := checkOf(t, read, want.check).Serves; !slices.Equal(got, want.serves) {
			t.Errorf("%s's serves = %q, want %q", want.check, got, want.serves)
		}
	}
	raw := decodeBody[struct {
		Checks []map[string]json.RawMessage `json:"checks"`
	}](t, response)
	for _, check := range raw.Checks {
		if string(check["block_id"]) != `"k3"` {
			continue
		}
		if served, carried := check["serves"]; carried {
			t.Fatalf("k3 carries serves %s, want none for a check that serves nothing", served)
		}
		return
	}
	t.Fatalf("the read holds no k3: %s", response.Body.String())
}
