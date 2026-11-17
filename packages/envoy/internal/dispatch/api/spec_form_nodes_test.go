package api

import (
	"net/http"
	"reflect"
	"testing"
)

// The read route names each text that its judgment read, by its kind and its node, in the order of
// the document, and each term that the document itself defines, from the one reading that the
// judgment made. So a reader of the answer, such as the reviewer of a change request, takes the
// texts from Dispatch and reads no second copy of its rules. The document holds a statement in
// each form that the reader reads: a requirement block, one in a block quote and an entry line. It
// holds a check, and a term under `## **Terms**`, a heading that the page shows as Terms. Two lines
// are no text: a line of the term-line form outside Terms, which adds no term, and a requirement
// block in a code fence. The count of each kind (texts) stays, and it is the number of texts of that
// kind, with the definitions of the document as the count of definitions.
func TestSpecFormReadNamesEachTextAndEachDefinition(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	spec := "# Doors\n\n" +
		"## **Terms**\n\n- **door alarm**: A sound that the door controller makes when a door stays open.\n\n" +
		"## Statements\n\n" +
		":::requirement{#door-close}\nThe door controller closes an open door after 60 seconds.\n:::\n\n" +
		"> :::requirement{#quoted-calm}\n> The door controller sounds the door alarm after 2 minutes.\n> :::\n\n" +
		"- **The kiosk beeps once for each key.** `kiosk-beep` · under: `kiosk` · check: none\n\n" +
		"- **chime**: A sound that the kiosk makes for each key.\n\n" +
		"```text\n:::requirement{#fenced}\nThe kiosk beeps twice.\n:::\n```\n\n" +
		":::check{#v-door-close run=\"A test opens a door and reads its state after 61 seconds.\" negative_control=\"A build that leaves the door open fails.\"}\n" +
		"The door controller closes an open door after 60 seconds.\n:::\n"
	issue := createFormIssue(t, tree.handler, tree.root, "The doors", spec)

	response := dispatchRequest(t, tree.handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/spec-form", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read: status=%d body=%s", response.Code, response.Body.String())
	}
	type node struct{ Kind, Node string }
	type definition struct{ Term, Definition string }
	read := decodeBody[struct {
		Texts       map[string]int `json:"texts"`
		Nodes       []node         `json:"nodes"`
		Definitions []definition   `json:"definitions"`
		Terms       []any          `json:"terms"`
	}](t, response)

	wantNodes := []node{
		{"statement", "door-close"},
		{"statement", "quoted-calm"},
		{"statement", "kiosk-beep"},
		{"claim", "v-door-close"},
		{"procedure", "v-door-close"},
		{"failing_example", "v-door-close"},
	}
	if !reflect.DeepEqual(read.Nodes, wantNodes) {
		t.Fatalf("nodes = %#v, want %#v", read.Nodes, wantNodes)
	}
	wantDefinitions := []definition{{"door alarm", "A sound that the door controller makes when a door stays open."}}
	if !reflect.DeepEqual(read.Definitions, wantDefinitions) {
		t.Fatalf("definitions = %#v, want %#v", read.Definitions, wantDefinitions)
	}
	listed := map[string]int{"definition": len(read.Definitions)}
	for _, one := range read.Nodes {
		listed[one.Kind]++
	}
	for kind, count := range read.Texts {
		if listed[kind] != count {
			t.Errorf("texts counts %d of %s, and the answer lists %d", count, kind, listed[kind])
		}
	}
	// The document's own terms are no part of the root's list, which holds only approved terms.
	if len(read.Terms) != 0 {
		t.Fatalf("terms = %#v, want the root's list, which holds no term", read.Terms)
	}
}

// A document with no text of The Spec lists no text and no definition: each is an empty list,
// never null.
func TestSpecFormReadListsNoTextOfADocumentWithNone(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	issue := createFormIssue(t, tree.handler, tree.root, "Prose", "# Prose\n\nA paragraph with no text of The Spec.\n")
	response := dispatchRequest(t, tree.handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/spec-form", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read: status=%d body=%s", response.Code, response.Body.String())
	}
	read := decodeBody[map[string]any](t, response)
	for _, member := range []string{"nodes", "definitions"} {
		if list, ok := read[member].([]any); !ok || len(list) != 0 {
			t.Errorf("%s = %#v, want an empty list", member, read[member])
		}
	}
}
