package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// linkCount is every count of links a refusal's text names.
var linkCount = regexp.MustCompile(`(\d+) links`)

// chainOf creates length issues of project as carol, each the child of the one before it, the
// first under parent, and answers their keys: keys[i] is i+1 links below parent.
func (s privateSeed) chainOf(t *testing.T, project, parent string, length int) []string {
	t.Helper()
	keys := make([]string, 0, length)
	for index := range length {
		parent = s.issueUnder(t, project, fmt.Sprintf("%s link %d under %s", project, index+1, parent), parent)
		keys = append(keys, parent)
	}
	return keys
}

// issueUnder is the key of an issue of project carol creates under parent, "" for none.
func (s privateSeed) issueUnder(t *testing.T, project, title, parent string) string {
	t.Helper()
	body := map[string]any{"project": project, "title": title, "force": true}
	if parent != "" {
		body["parent"] = parent
	}
	return decodeBody[struct {
		Key string `json:"key"`
	}](t, s.mustRequest(t, callerCarol, http.MethodPost, "/api/v1/issues", body, http.StatusCreated)).Key
}

// TestTheDepthCapRefusesThroughAHiddenChainAndNamesNoHiddenDepth (F21): the depth cap counts every
// link of a parent chain, a project's its writer does not see included (refuseParentTooDeep, whose
// walks read every row), so a write that would put an issue more than refs.IssueDepthCap links
// below its root is refused 409 PARENT_INPUT however few of those links the writer sees; and the
// refusal names the cap alone, never the depth the walks counted, which would tell the writer how
// many issues it may not see stand above the parent or below the issue it moves.
//
// Under PRIV's issue, which alice does not see, carol builds a PRIV chain 31 links deep. PUB's Q
// hangs from its tip, 32 links below the root, and PUB's P from its twentieth link, 21 below. PUB's
// M12 and M14 are roots, each over a PRIV chain 12 and 14 links deep. alice sees Q, P, M12 and M14
// and none of the PRIV issues. Her create under Q (33 links), and her moves of M12 (34) and M14
// (36) under P, are each refused, each naming no count of links but the cap, and the two moves in
// the same words but for the moved key. None is written.
//
// Control: refuseParentTooDeep naming the depth it counted, as HEAD c99245cbc6 does
// (controls/F21.patch reversed): the create's text names 33 links and the moves' 34 and 36, and the
// test fails at each row "names N links", and at the row comparing the two moves.
func TestTheDepthCapRefusesThroughAHiddenChainAndNamesNoHiddenDepth(t *testing.T) {
	seed := seedPrivateProject(t)
	hidden := seed.chainOf(t, seedPrivate, seed.issue[seedPrivate], 31) // hidden[i] is i+1 links below PRIV's issue
	q := seed.issueUnder(t, seedPublic, "Q, 32 links below a hidden root", hidden[30])
	p := seed.issueUnder(t, seedPublic, "P, 21 links below a hidden root", hidden[19])
	m12 := seed.issueUnder(t, seedPublic, "M12, over a hidden chain 12 deep", "")
	seed.chainOf(t, seedPrivate, m12, 12)
	m14 := seed.issueUnder(t, seedPublic, "M14, over a hidden chain 14 deep", "")
	seed.chainOf(t, seedPrivate, m14, 14)
	for _, key := range []string{q, p, m12, m14} {
		seed.mustRequest(t, callerAlice, http.MethodGet, "/api/v1/issues/"+key, nil, http.StatusOK)
	}
	seed.mustRequest(t, callerAlice, http.MethodGet, "/api/v1/issues/"+hidden[0], nil, http.StatusNotFound)

	issues := func() int {
		t.Helper()
		var count int
		if err := seed.database.Pool.QueryRow(context.Background(), `select count(*) from issues`).Scan(&count); err != nil {
			t.Fatalf("count the issues: %v", err)
		}
		return count
	}
	parentOf := func(key string) *string {
		t.Helper()
		var parent *string
		if err := seed.database.Pool.QueryRow(context.Background(), `select parent_key from issues where key = $1`, key).Scan(&parent); err != nil {
			t.Fatalf("read the parent of %s: %v", key, err)
		}
		return parent
	}
	refused := func(what string, write func() *httptest.ResponseRecorder) string {
		t.Helper()
		before := issues()
		response := write()
		body := response.Body.String()
		if response.Code != http.StatusConflict || !strings.Contains(body, `"code":"PARENT_INPUT"`) || !strings.Contains(body, "at most 32 links") {
			t.Errorf("%s: status=%d body=%s, want 409 PARENT_INPUT naming the 32-link cap: the cap counts the hidden links too", what, response.Code, body)
		}
		for _, named := range linkCount.FindAllStringSubmatch(body, -1) {
			if named[1] != "32" {
				t.Errorf("%s names %s links, a depth counting issues alice may not see: %s", what, named[1], body)
			}
		}
		if after := issues(); after != before {
			t.Errorf("%s took the issues from %d to %d, want nothing written", what, before, after)
		}
		return body
	}

	refused("alice's create under Q, 33 links below the hidden root", func() *httptest.ResponseRecorder {
		return seed.request(t, callerAlice, http.MethodPost, "/api/v1/issues",
			map[string]any{"project": seedPublic, "title": "Under Q", "parent": q, "force": true})
	})
	move := func(moved string) string {
		return refused("alice's move of "+moved+" under P", func() *httptest.ResponseRecorder {
			return seed.request(t, callerAlice, http.MethodPatch, "/api/v1/issues/"+moved, map[string]any{"parent": p})
		})
	}
	twelve, fourteen := move(m12), move(m14)
	if strings.ReplaceAll(fourteen, m14, m12) != twelve {
		t.Errorf("the two moves are refused in different words, which tell the hidden depths apart:\n%s\n%s", twelve, fourteen)
	}
	for _, moved := range []string{m12, m14} {
		if parent := parentOf(moved); parent != nil {
			t.Errorf("the refused move left %s under %s, want no parent", moved, *parent)
		}
	}
}
