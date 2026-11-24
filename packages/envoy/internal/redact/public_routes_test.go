package redact_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/redact"
)

// routedValue is one row of testdata/public-routes.json: a value a request carries under a name,
// the request sent with Method to Target and the value in its query, its form, its JSON body (a
// string, or a list of one string for "json-list") or a header (At). On says whether the value is
// public there, its name one the public-name table lists on a route its contract names: there the
// answer's words print the value, and anywhere else the value is a credential like any other.
type routedValue struct {
	Method string `json:"method"`
	Target string `json:"target"`
	At     string `json:"at"`
	Name   string `json:"name"`
	Value  string `json:"value"`
	On     bool   `json:"on"`
}

// TestPublicRoutesReadAsTheSharedTableSays: every row of testdata/public-routes.json, each group
// a subtest named by its id, read through an answer that quotes the row's value. The TypeScript
// half, packages/contracts/src/public-request-routes.test.ts, reads the same table through
// requestCredentials, so the two halves of the route rule are held to one set of requests.
func TestPublicRoutesReadAsTheSharedTableSays(t *testing.T) {
	data, err := os.ReadFile("testdata/public-routes.json")
	if err != nil {
		t.Fatal(err)
	}
	var table struct {
		About  string `json:"about"`
		Groups []struct {
			ID    string        `json:"id"`
			About string        `json:"about"`
			Rows  []routedValue `json:"rows"`
		} `json:"groups"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&table); err != nil {
		t.Fatal(err)
	}
	if len(table.Groups) == 0 {
		t.Fatal("the table holds no group, so nothing was checked")
	}
	seen := map[string]bool{}
	for _, group := range table.Groups {
		if group.ID == "" || seen[group.ID] || len(group.Rows) == 0 {
			t.Fatalf("the group %q has no id, the id of another group, or no row", group.ID)
		}
		seen[group.ID] = true
		t.Run(group.ID, func(t *testing.T) { requireRoutes(t, group.Rows) })
	}
}

// requireRoutes reads, for each of values, an answer to its request that quotes the value
// (ReadAnswer, then Words), and requires the words to hold the value where it is public and to hold
// it redacted anywhere else.
func requireRoutes(t *testing.T, values []routedValue) {
	t.Helper()
	for _, v := range values {
		if v.Target == "" || v.Name == "" || v.Value == "" {
			t.Fatalf("the row %+v names no target, name or value, so it checks nothing", v)
		}
		route := "on"
		if !v.On {
			route = "off"
		}
		t.Run(route+" "+v.Method+" "+v.Target+" "+v.Name, func(t *testing.T) {
			words := routedWords(t, v)
			if v.On && !strings.Contains(words, v.Value) {
				t.Errorf("the words are %q; want %s's value %q as the answer wrote it, where it is public", words, v.Name, v.Value)
			}
			if !v.On && (strings.Contains(words, v.Value) || !strings.Contains(words, redact.Redacted)) {
				t.Errorf("the words are %q; want %s's value %q redacted, where it is no public value", words, v.Name, v.Value)
			}
		})
	}
}

// routedWords is the words of a refusal of v's request that quotes v's value, the request's method
// v's as written: http.NewRequest turns an empty one into GET before the rule could read it, and a
// request built by hand keeps it.
func routedWords(t *testing.T, v routedValue) string {
	t.Helper()
	target, body, headers := v.Target, "", map[string]string{}
	switch v.At {
	case "query":
		target += "?" + url.Values{v.Name: {v.Value}}.Encode()
	case "form":
		body, headers["Content-Type"] = url.Values{v.Name: {v.Value}}.Encode(), "application/x-www-form-urlencoded"
	case "json", "json-list":
		var member any = v.Value
		if v.At == "json-list" {
			member = []string{v.Value}
		}
		encoded, err := json.Marshal(map[string]any{v.Name: member})
		if err != nil {
			t.Fatal(err)
		}
		body, headers["Content-Type"] = string(encoded), "application/json"
	case "header":
		headers[v.Name] = v.Value
	default:
		t.Fatalf("no place %q", v.At)
	}
	request := newRequest(t, v.Method, target, body, headers)
	request.Method = v.Method
	said := "the request's " + v.Name + " " + v.Value + " was refused"
	answer, err := redact.ReadAnswer(&http.Response{StatusCode: http.StatusBadRequest, Body: io.NopCloser(strings.NewReader(said)), Request: request}, 1<<10)
	if err != nil {
		t.Fatal(err)
	}
	return answer.Words().String()
}
