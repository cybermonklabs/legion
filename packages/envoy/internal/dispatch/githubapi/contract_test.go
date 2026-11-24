package githubapi

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"reflect"
	"strings"
	"testing"
)

// updateContract is this package's -update flag, as internal/dispatch/api's: it rewrites the
// contract file from the proxy's declarations rather than comparing them.
var updateContract = flag.Bool("update", false, "rewrite testdata/proxy-contract.json from the proxy's declarations")

// contractPath is the proxy's contract, which the dashboard's recording test reads
// (packages/dispatch/e2e/__tests__/stub-recordings.test.ts) to hold every committed GitHub answer
// to what this proxy forwards and what it trims, rather than a copy of this file it would keep by hand.
const contractPath = "testdata/proxy-contract.json"

// contractQueries are the workflow-runs queries whose verdicts the contract carries, so a reader
// of the contract that ports workflowRunsOfOneCommit can hold its port to this one.
var contractQueries = []string{
	"head_sha=" + strings.Repeat("a", 40),
	"head_sha=" + strings.Repeat("a", 40) + "&per_page=100&page=2",
	"per_page=100&page=1",
	"head_sha=" + strings.Repeat("a", 40) + "&head_sha=" + strings.Repeat("b", 40),
	"head_sha=" + strings.Repeat("a", 40) + "&branch=main",
	"head_sha=" + strings.Repeat("a", 39),
	"",
}

// proxyContract is what the proxy forwards and answers, from its own declarations.
type proxyContract struct {
	Prefix             string          `json:"prefix"`
	Readable           string          `json:"readable"`
	WorkflowRunsGroup  string          `json:"workflowRunsGroup"`
	WorkflowRunsQuery  string          `json:"workflowRunsQuery"`
	WorkflowRunsCommit string          `json:"workflowRunsCommit"`
	WorkflowRunsChecks map[string]bool `json:"workflowRunsQueries"`
	ErrorCodes         []string        `json:"errorCodes"`
	WorkflowRuns       any             `json:"workflowRuns"`
}

// fieldsOf writes a struct type as its JSON fields: "integer" or "string" for a field, a one-entry
// list holding the fields of an element for a list of structs.
func fieldsOf(t reflect.Type) any {
	switch t.Kind() {
	case reflect.Struct:
		fields := map[string]any{}
		for i := range t.NumField() {
			field := t.Field(i)
			name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
			fields[name] = fieldsOf(field.Type)
		}
		return fields
	case reflect.Slice:
		return []any{fieldsOf(t.Elem())}
	case reflect.Int, reflect.Int64:
		return "integer"
	case reflect.String:
		return "string"
	}
	panic("fieldsOf: no JSON type for " + t.String())
}

func renderContract() []byte {
	checks := map[string]bool{}
	for _, query := range contractQueries {
		checks[query] = workflowRunsOfOneCommit(query)
	}
	contract := proxyContract{
		Prefix:             Prefix,
		Readable:           readable.String(),
		WorkflowRunsGroup:  readable.SubexpNames()[runsGroup],
		WorkflowRunsQuery:  workflowRunsQuery.String(),
		WorkflowRunsCommit: workflowRunsCommit,
		WorkflowRunsChecks: checks,
		ErrorCodes:         []string{codeMethodNotAllowed, codePathRefused, codeTokenUnavailable, codeUpstream, codeRateLimited},
		WorkflowRuns:       fieldsOf(reflect.TypeFor[workflowRuns]()),
	}
	var rendered bytes.Buffer
	encoder := json.NewEncoder(&rendered)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(contract); err != nil {
		panic(err)
	}
	return rendered.Bytes()
}

// TestProxyContractFile holds the committed contract to a fresh render of the proxy's own
// declarations. -update writes the render instead.
func TestProxyContractFile(t *testing.T) {
	rendered := renderContract()
	if *updateContract {
		if err := os.WriteFile(contractPath, rendered, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	committed, err := os.ReadFile(contractPath)
	if err != nil {
		t.Fatalf("read %s: %v; write it with go test ./internal/dispatch/githubapi/ -run TestProxyContractFile -update", contractPath, err)
	}
	if !bytes.Equal(committed, rendered) {
		t.Fatalf("%s differs from the proxy's declarations; rewrite it with go test ./internal/dispatch/githubapi/ -run TestProxyContractFile -update\nrendered:\n%s", contractPath, rendered)
	}
}
