package api

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

// GET /api/v2/issues/{key}/references pages each of the read's three lists in the envelope, as
// the architect ruled (dispatch://INFRA-4071/comment/7ce01db2-7ea2-4cc4-8dd7-d4b6e9e9fa05).
//
// The issue's comment links to another issue and to three documents of that issue, so links holds
// four rows and members, the rows of links whose node is a document, three: a members read that
// answered every row of links answers a total of four and the issue's row. Two comments of the
// other issue and a document of it that the comment does not link to cite the issue, so
// linked_from holds three rows, one of them a document's: a linked_from read that filled a
// document's row as links does answers that row's artifact.
//
// Each list is paged by two: the first page answers two rows and the list's whole total, the next
// page the rest, and no node is on two pages. A row of members, and a document's row of links,
// carries the document's own row; the other issue's row of links, and every row of linked_from,
// carries null. Each page answers an ETag of its own, which another page or another list never
// shares, and the page's own ETag answers 304. A new version of a document on members' first page
// moves that page's ETag and leaves the second page's: an ETag that hashed the page's place in its
// list and not its rows would keep both. With no relation the read is of members.
func TestEachV2ReferencesListPagesPastItsLimit(t *testing.T) {
	handler, _ := blockIndexHandler(t)
	createReferenceAPIProject(t, handler, "CORE")
	createReferenceAPIProject(t, handler, "OPS")
	issue := createReferenceAPIIssue(t, handler, "CORE")
	other := createReferenceAPIIssue(t, handler, "OPS")
	otherOwner := "/api/v1/issues/" + other.Key
	// want is each list's nodes, each with whether its row carries its document's row; names is
	// the name of each document the issue links to, by its id.
	want := map[string]map[[2]string]bool{
		"members":     {},
		"links":       {{"issue", other.Key}: false},
		"linked_from": {},
	}
	names := map[string]string{}
	body := "It links dispatch://" + other.Key
	for _, name := range []string{"first.md", "second.md", "third.md"} {
		document := uploadDocument(t, handler, otherOwner, name, "A document the issue links to.\n").Artifact
		body += " dispatch://" + other.Key + "/artifact/" + document.Slug
		names[document.ID] = name
		want["members"][[2]string{"artifact", document.ID}] = true
		want["links"][[2]string{"artifact", document.ID}] = true
	}
	createReferenceAPIComment(t, handler, issue.Key, body+" here.")
	for range 2 {
		comment := createReferenceAPIComment(t, handler, other.Key, "It cites dispatch://"+issue.Key+" here.")
		want["linked_from"][[2]string{"comment", comment.ID}] = false
	}
	citing := uploadDocument(t, handler, otherOwner, "cites.md", "It cites dispatch://"+issue.Key+" here.\n").Artifact
	want["linked_from"][[2]string{"artifact", citing.ID}] = false

	target := "/api/v2/issues/" + issue.Key + "/references"
	read := func(t *testing.T, query string) (page[referenceRowV2], string) {
		t.Helper()
		response := dispatchRequest(t, handler, http.MethodGet, target+query, nil, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("GET %s%s: status=%d body=%s", target, query, response.Code, response.Body.String())
		}
		return decodeBody[page[referenceRowV2]](t, response), response.Header().Get("ETag")
	}
	etags := map[string]string{}
	for _, relation := range []string{"members", "links", "linked_from"} {
		t.Run(relation, func(t *testing.T) {
			nodes := want[relation]
			first, firstTag := read(t, "?relation="+relation+"&limit=2")
			next, nextTag := read(t, "?relation="+relation+"&limit=2&offset=2")
			if first.Total != len(nodes) || len(first.Items) != 2 || first.Limit != 2 ||
				next.Total != len(nodes) || len(next.Items) != len(nodes)-2 || next.Offset != 2 {
				t.Fatalf("%s pages: first total=%d items=%d limit=%d, next total=%d items=%d offset=%d; want %d rows paged 2 and %d",
					relation, first.Total, len(first.Items), first.Limit, next.Total, len(next.Items), next.Offset, len(nodes), len(nodes)-2)
			}
			seen := map[[2]string]bool{}
			for _, row := range slices.Concat(first.Items, next.Items) {
				node := [2]string{row.Node.Kind, row.Node.ID}
				if seen[node] {
					t.Fatalf("%s answers %s %s on two pages", relation, node[0], node[1])
				}
				seen[node] = true
				document, listed := nodes[node]
				switch {
				case !listed:
					t.Fatalf("%s answers %s %s, which the fixture does not put on it", relation, node[0], node[1])
				case document && (row.Artifact == nil || row.Artifact.ID != row.Node.ID):
					t.Fatalf("%s row %s %s carries artifact %+v; want the document's own row", relation, node[0], node[1], row.Artifact)
				case !document && row.Artifact != nil:
					t.Fatalf("%s row %s %s carries artifact %s; want null", relation, node[0], node[1], row.Artifact.ID)
				}
			}
			for _, tag := range []string{firstTag, nextTag} {
				if tag == "" {
					t.Fatalf("%s answered a page with no ETag", relation)
				}
				if owner, taken := etags[tag]; taken {
					t.Fatalf("%s answered the ETag %s that %s answered too", relation, tag, owner)
				}
				etags[tag] = relation
			}
			request := newDispatchRequest(t, http.MethodGet, target+"?relation="+relation+"&limit=2", nil, "alice")
			request.Header.Set("If-None-Match", firstTag)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusNotModified {
				t.Fatalf("%s's first page sent its own ETag answered %d, want 304", relation, response.Code)
			}
		})
	}
	if plain, plainTag := read(t, "?limit=2"); plain.Total != len(want["members"]) || etags[plainTag] != "members" {
		t.Fatalf("a read naming no relation answered total=%d under the ETag of %q; want members' first page", plain.Total, etags[plainTag])
	}

	firstPage, secondPage := "?relation=members&limit=2", "?relation=members&limit=2&offset=2"
	first, firstTag := read(t, firstPage)
	_, secondTag := read(t, secondPage)
	var name string
	if len(first.Items) > 0 {
		name = names[first.Items[0].Node.ID]
	}
	if name == "" {
		t.Fatalf("members' first page answered %+v; want it to begin with a document the issue links to", first.Items)
	}
	uploadDocument(t, handler, otherOwner, name, "A document the issue links to, written again.\n")
	if _, tag := read(t, firstPage); tag == firstTag {
		t.Fatalf("members' first page kept the ETag %s after a new version of %s, a document it holds", firstTag, name)
	}
	if _, tag := read(t, secondPage); tag != secondTag {
		t.Fatalf("members' second page moved its ETag from %s to %s after a new version of %s, a document it does not hold", secondTag, tag, name)
	}
}
