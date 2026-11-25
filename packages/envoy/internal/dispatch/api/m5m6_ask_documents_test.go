package api

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// askDocumentSeed is verification v16's seed (private_seed_test.go) with asks on PUB that name a
// document of PRIV beside them - the shape a stored anchor or approval keeps when the document it
// names is in a project its reader does not see.
type askDocumentSeed struct {
	privateSeed
	// anchoredIssue is a PUB issue assigned to alice, so its asks wait on her; spec is its spec;
	// document is a PUB project document.
	anchoredIssue, spec, document string
	// privateDocument is a document of PRIV's issue, as the artifacts row holds it.
	privateDocument   model.AskAnchorArtifact
	privateDocumentID string
	// anchored is a question on issue anchored on privateDocument and blocked in spec, so the
	// decisions read lists it; approval is an approval request on issue whose approval names
	// privateDocument; documentAsk is a question on document anchored on privateDocument.
	anchored, approval, documentAsk string
}

// seedAskDocuments writes the seed: every ask through its route as carol (the approval request as
// the shared token for a session, the only actor that route takes), and then, where no route
// writes it, the stored anchor's and approval's artifact_id rewritten to name PRIV's document, and
// the anchored question's block set in issue's spec - rows a release could keep after the
// document they name went to a project its reader does not see.
func seedAskDocuments(t *testing.T) askDocumentSeed {
	t.Helper()
	seed := askDocumentSeed{privateSeed: seedPrivateProject(t)}
	ctx := context.Background()
	created := decodeBody[struct {
		Key               string `json:"key"`
		PrimaryArtifactID string `json:"primary_artifact_id"`
	}](t, seed.mustRequest(t, callerCarol, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": seedPublic, "title": "Anchored work", "spec": "Ship the build tonight.", "force": true,
	}, http.StatusCreated))
	seed.anchoredIssue, seed.spec = created.Key, created.PrimaryArtifactID
	seed.mustRequest(t, callerCarol, http.MethodPatch, "/api/v1/issues/"+seed.anchoredIssue, map[string]any{"assignee": seedAlice}, http.StatusOK)
	upload := func(target, name string) string {
		t.Helper()
		return decodeBody[struct {
			Artifact model.Artifact `json:"artifact"`
		}](t, seed.mustRequest(t, callerCarol, http.MethodPost, target, map[string]any{
			"content": "Notes.\n", "name": name,
		}, http.StatusCreated)).Artifact.ID
	}
	seed.privateDocumentID = upload("/api/v1/issues/"+seed.issue[seedPrivate]+"/artifacts", "hidden-notes.md")
	seed.document = upload("/api/v1/projects/"+seedPublic+"/artifacts", "public-notes.md")
	if err := seed.database.Pool.QueryRow(ctx, `select project_key, slug, name, is_primary from artifacts where id = $1`,
		seed.privateDocumentID).Scan(&seed.privateDocument.Project, &seed.privateDocument.Slug, &seed.privateDocument.Name, &seed.privateDocument.Primary); err != nil {
		t.Fatalf("read PRIV's document: %v", err)
	}

	question := map[string]any{"question": "Why tonight?", "options": []map[string]string{{"label": "Yes"}, {"label": "No"}}}
	seed.anchored = createdID(t, seed.request(t, callerCarol, http.MethodPost, "/api/v1/issues/"+seed.anchoredIssue+"/asks", map[string]any{
		"question": question["question"], "options": question["options"], "anchor": map[string]any{"artifact": "spec", "quote": "build"},
	}))
	seed.documentAsk = createdID(t, seed.request(t, callerCarol, http.MethodPost, "/api/v1/artifacts/"+seed.document+"/asks", question))
	seed.approval = decodeBody[struct {
		Ask struct {
			ID string `json:"id"`
		} `json:"ask"`
	}](t, seed.mustRequest(t, callerSharedToken, http.MethodPost, "/api/v1/artifacts/"+seed.spec+"/approval-requests", map[string]any{
		"actor": map[string]any{"kind": "session", "id": "s-architect"},
	}, http.StatusCreated)).Ask.ID
	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`update asks set anchor = jsonb_set(anchor, '{artifact_id}', to_jsonb($2::text)), block_artifact_id = $3::uuid where id = $1`,
			[]any{seed.anchored, seed.privateDocumentID, seed.spec}},
		{`update asks set anchor = (select anchor from asks where id = $2) where id = $1`, []any{seed.documentAsk, seed.anchored}},
		{`update asks set approval = jsonb_set(jsonb_set(approval, '{artifact_id}', to_jsonb($2::text)), '{documents,0,artifact_id}', to_jsonb($2::text)) where id = $1`,
			[]any{seed.approval, seed.privateDocumentID}},
	} {
		if tag, err := seed.database.Pool.Exec(ctx, statement.sql, statement.args...); err != nil || tag.RowsAffected() != 1 {
			t.Fatalf("store %s: %v (%d rows)", statement.sql, err, tag.RowsAffected())
		}
	}
	return seed
}

// askDocumentRead is one read of one route family, as caller, and the asks it answers.
type askDocumentRead struct {
	family, target string
	asks           []string
}

// reads is every route family that answers an ask with the documents its row joins, and the asks
// each answers here.
func (s askDocumentSeed) reads() []askDocumentRead {
	return []askDocumentRead{
		{"one ask", "/api/v1/asks/" + s.anchored, []string{s.anchored}},
		{"one ask", "/api/v1/asks/" + s.approval, []string{s.approval}},
		{"one ask", "/api/v1/asks/" + s.documentAsk, []string{s.documentAsk}},
		{"the issue read", "/api/v1/issues/" + s.anchoredIssue, []string{s.anchored, s.approval}},
		{"the issue's asks", "/api/v1/issues/" + s.anchoredIssue + "/asks", []string{s.anchored, s.approval}},
		{"a document's asks", "/api/v1/artifacts/" + s.document + "/asks", []string{s.documentAsk}},
		{"the Inbox", "/api/v1/inbox?assignee=" + seedAlice, []string{s.anchored, s.approval}},
		{"the decisions", "/api/v1/issues/" + s.anchoredIssue + "/decisions", []string{s.anchored}},
	}
}

// askDocuments walks an answer for every object whose id is one of asks and answers, by ask id,
// the documents it carries beside the ask: "anchor <project>/<slug>" and "approval
// <project>/<slug>", empty for an ask carrying neither. An ask the answer does not hold has no
// entry.
func askDocuments(t *testing.T, body []byte, asks []string) map[string][]string {
	t.Helper()
	var decoded any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	found := map[string][]string{}
	var walk func(any)
	walk = func(value any) {
		switch value := value.(type) {
		case map[string]any:
			if id, _ := value["id"].(string); slices.Contains(asks, id) {
				if _, kept := found[id]; !kept {
					found[id] = []string{}
				}
				for _, field := range []string{"anchor_artifact", "approval_artifact"} {
					if document, ok := value[field].(map[string]any); ok {
						found[id] = append(found[id], strings.TrimSuffix(field, "_artifact")+" "+document["project"].(string)+"/"+document["slug"].(string))
					}
				}
			}
			for _, inner := range value {
				walk(inner)
			}
		case []any:
			for _, inner := range value {
				walk(inner)
			}
		}
	}
	walk(decoded)
	return found
}

// wantDocuments is what a reader who sees PRIV reads beside each ask: the approval request names
// PRIV's document as its approval, the others as their anchor.
func (s askDocumentSeed) wantDocuments(ask string) []string {
	field := "anchor"
	if ask == s.approval {
		field = "approval"
	}
	return []string{field + " " + s.privateDocument.Project + "/" + s.privateDocument.Slug}
}

// TestM5M6AnAskNamesNoDocumentItsReaderMayNotSee: the documents an ask's row joins beside it - the
// one its anchor quotes and the one its approval request names - are joined only where the reader
// sees their project (access.ProjectClause), as an ask event's payload already joins them
// (attachAskDocumentArtifacts). alice, who does not see PRIV, reads each ask of the seed with no
// anchor_artifact and no approval_artifact - as an ask whose document no row holds - and no answer
// names PRIV's document's project, slug or name, in every route family: one ask, the issue read,
// the issue's asks, a document's asks, the Inbox and the decisions. carol, who sees PRIV, reads the
// document beside each ask in each.
//
// Control: one of askRowFromSeenBy's two clauses dropped; the families then name PRIV's document
// beside the asks that join it, and the row fails naming each.
func TestM5M6AnAskNamesNoDocumentItsReaderMayNotSee(t *testing.T) {
	seed := seedAskDocuments(t)
	for _, read := range seed.reads() {
		t.Run(read.family, func(t *testing.T) {
			body := seed.mustRequest(t, callerAlice, http.MethodGet, read.target, nil, http.StatusOK).Body.Bytes()
			documents := askDocuments(t, body, read.asks)
			for _, ask := range read.asks {
				got, held := documents[ask]
				if !held {
					t.Errorf("alice: GET %s holds no ask %s, so this read measures nothing", read.target, ask)
				} else if len(got) != 0 {
					t.Errorf("alice: GET %s answers ask %s beside %v, want no document: PRIV's is a document she may not see", read.target, ask, got)
				}
			}
			for _, named := range []string{`"project":"` + seedPrivate + `"`, `"slug":"` + seed.privateDocument.Slug + `"`, `"name":"` + seed.privateDocument.Name + `"`} {
				if strings.Contains(string(body), named) {
					t.Errorf("alice: GET %s names %s of PRIV's document", read.target, named)
				}
			}
			documents = askDocuments(t, seed.mustRequest(t, callerCarol, http.MethodGet, read.target, nil, http.StatusOK).Body.Bytes(), read.asks)
			for _, ask := range read.asks {
				if got := documents[ask]; !slices.Equal(got, seed.wantDocuments(ask)) {
					t.Errorf("carol: GET %s answers ask %s beside %v, want %v", read.target, ask, got, seed.wantDocuments(ask))
				}
			}
		})
	}
}

// TestM5M6WithNoPrivateProjectAnAskNamesItsDocumentsAsBefore (R9): with PRIV public alice, a full
// member, reads every family as the base answered it: the document beside each ask. The sha256 of
// each answer, its times and ids blanked, is logged for the report's comparison of the two commits.
//
// Control: askRowFromSeenBy's anchor clause made false for every reader; alice then reads no
// anchor document and the row fails.
func TestM5M6WithNoPrivateProjectAnAskNamesItsDocumentsAsBefore(t *testing.T) {
	seed := seedAskDocuments(t)
	seed.setVisibility(t, seedPrivate, model.ProjectPublic)
	for _, read := range seed.reads() {
		body := seed.mustRequest(t, callerAlice, http.MethodGet, read.target, nil, http.StatusOK).Body.Bytes()
		blanked := timestampsAndIDs.ReplaceAllString(string(body), "<varies>")
		t.Logf("R9 %s %s sha256 %x", read.family, timestampsAndIDs.ReplaceAllString(read.target, "<id>"), sha256.Sum256([]byte(blanked)))
		documents := askDocuments(t, body, read.asks)
		for _, ask := range read.asks {
			if got := documents[ask]; !slices.Equal(got, seed.wantDocuments(ask)) {
				t.Errorf("PRIV public: alice's GET %s answers ask %s beside %v, want %v, as before", read.target, ask, got, seed.wantDocuments(ask))
			}
		}
	}
}
