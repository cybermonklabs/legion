package api

import (
	"cmp"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// TestEveryVersionRowIsTheProjectionOfAVersionNode holds every insert of an artifact_versions row to
// R16 of the issue-tree spec (T2b of its plan): the row is the projection of a version node,
// `<ref_key>@v<number>`, supertag `document version`, part_of its document's node, with its number
// and its time as values, written in the row's own transaction and caused by the event of the
// write. Each site is driven through its route: an issue's spec at the create (api's createIssue),
// a markdown upload and its next version (writeDocumentVersion), an image upload and its next
// version (storeArtifact's insert of a file's version), and a named version (docs' writeVersionTx).
// Each row and its node are read in one statement after the request, so the two are one snapshot;
// each site's write refused at its commit, after every tuple is appended, leaves neither the row
// nor the node nor a tuple out of it. The issue is born converted, so its spec's node is a query
// node and the spec's first version a `query version` holding the query's definition_version (R14).
//
// A row an older build stored with no node gets one from the backfill, dated at the row's own
// created_at, and a second backfill writes no version node. A document made a query node through
// graphstore.Write reads back with supertag `query` and its `query`, `takes`, `subtree` and
// `definition_version` values, and the version written after it is a `query version` holding that
// definition_version, while the node the backfill writes after it of a version stored before it is
// a `query version` holding none. The store census reads zero differences after every write and
// after the backfill.
func TestEveryVersionRowIsTheProjectionOfAVersionNode(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	ctx := context.Background()
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "VER", "name": "Versions",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create the project: status=%d body=%s", response.Code, response.Body.String())
	}
	const key = "VER-1"
	refused := func(what string, response interface{ Result() *http.Response }) {
		t.Helper()
		if code := response.Result().StatusCode; code < http.StatusInternalServerError {
			t.Fatalf("%s refused at its commit answered %d, want a 5xx", what, code)
		}
	}

	// The issue's spec, at its create.
	refuseCommits(t, database, "artifact_versions", func() {
		refused("the create", dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
			"project": "VER", "title": "A document's versions", "spec": "The spec.\n",
		}, "alice"))
	})
	requireNoVersion(t, database, key+"/spec", 1)
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "VER", "title": "A document's versions", "spec": "The spec.\n",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create the issue: status=%d body=%s", created.Code, created.Body.String())
	}
	issue := decodeBody[struct {
		Key               string `json:"key"`
		PrimaryArtifactID string `json:"primary_artifact_id"`
	}](t, created)
	if issue.Key != key {
		t.Fatalf("the create made %s, want %s", issue.Key, key)
	}
	requireVersionNode(t, database, issue.PrimaryArtifactID, 1, graphstore.SupertagQueryVersion,
		eventOf(t, database, "issue.created", "issue_key", key))
	requireDefinitionVersion(t, database, key+"/spec", 1, strconv.Itoa(graphstore.QueryDefinitionVersion))
	requireStoreCensusZero(t, database, "after the issue's create")

	// A markdown upload, and its next version, which names the token of the version it replaces,
	// as every upload over a document does (uploadBaseNamed).
	upload := func(name, content string) *artifactUploadResponse {
		t.Helper()
		path := "/api/v1/issues/" + key + "/artifacts"
		response := dispatchRequest(t, handler, http.MethodPost, path, map[string]any{
			"name": name, "content": content, "precondition": uploadBaseNamed(t, handler, path, name),
		}, "alice")
		if response.Code != http.StatusCreated {
			t.Fatalf("upload %s: status=%d body=%s", name, response.Code, response.Body.String())
		}
		uploaded := decodeBody[artifactUploadResponse](t, response)
		return &uploaded
	}
	refuseCommits(t, database, "artifact_versions", func() {
		refused("the markdown upload", dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/artifacts",
			map[string]string{"name": "notes.md", "content": "# Notes\n"}, "alice"))
	})
	requireNoVersion(t, database, key+"/notes-md", 1)
	notes := upload("notes.md", "# Notes\n\nThe first.\n").Artifact
	requireVersionNode(t, database, notes.ID, 1, graphstore.SupertagDocumentVersion, artifactCreated(t, database, notes.ID))
	upload("notes.md", "# Notes\n\nThe second.\n")
	requireVersionNode(t, database, notes.ID, 2, graphstore.SupertagDocumentVersion, versionEvent(t, database, notes.ID, 2))
	requireStoreCensusZero(t, database, "after the markdown uploads")

	// An image upload, and its next version, which a file's own insert writes.
	image := func(content string) *httptest.ResponseRecorder {
		t.Helper()
		return multipartRequest(t, handler, "/api/v1/issues/"+key+"/artifacts", map[string]string{"name": "shot.png"},
			"shot.png", "image/png", pngBytes(content), "alice")
	}
	refuseCommits(t, database, "artifact_versions", func() { refused("the image upload", image("refused")) })
	requireNoVersion(t, database, key+"/shot-png", 1)
	first := image("the first picture")
	if first.Code != http.StatusCreated {
		t.Fatalf("upload the image: status=%d body=%s", first.Code, first.Body.String())
	}
	shot := decodeBody[artifactUploadResponse](t, first).Artifact
	requireVersionNode(t, database, shot.ID, 1, graphstore.SupertagDocumentVersion, artifactCreated(t, database, shot.ID))
	refuseCommits(t, database, "artifact_versions", func() { refused("the image's next version", image("refused again")) })
	requireNoVersion(t, database, key+"/shot-png", 2)
	if second := image("the second picture"); second.Code != http.StatusCreated {
		t.Fatalf("upload the image's second version: status=%d body=%s", second.Code, second.Body.String())
	}
	requireVersionNode(t, database, shot.ID, 2, graphstore.SupertagDocumentVersion, versionEvent(t, database, shot.ID, 2))
	requireStoreCensusZero(t, database, "after the image uploads")

	// A named version, which the document service writes (writeVersionTx).
	name := func() *http.Response {
		return dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+notes.ID+"/versions",
			map[string]string{"summary": "named"}, "alice").Result()
	}
	refuseCommits(t, database, "artifact_versions", func() {
		if code := name().StatusCode; code < http.StatusInternalServerError {
			t.Fatalf("the named version refused at its commit answered %d, want a 5xx", code)
		}
	})
	requireNoVersion(t, database, key+"/notes-md", 3)
	if code := name().StatusCode; code != http.StatusCreated {
		t.Fatalf("name the notes' version: status=%d", code)
	}
	requireVersionNode(t, database, notes.ID, 3, graphstore.SupertagDocumentVersion, versionEvent(t, database, notes.ID, 3))
	requireStoreCensusZero(t, database, "after the named version")

	// A row an older build stored, with no node: the census names it, and the backfill gives it its
	// node, dated at the row's own created_at.
	olderAt := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	if _, err := database.Pool.Exec(ctx, `
		insert into artifact_versions (artifact_id, number, markdown, authors, created_at)
		values ($1, 4, 'Written by an older build.', '[{"kind":"user","id":"bob"}]', $2)
	`, notes.ID, olderAt); err != nil {
		t.Fatalf("store version 4 as an older build stores it: %v", err)
	}
	census, err := graphcensus.StoreCensus(ctx, database.Pool.Config().ConnString(), false, docs.ReturnDeclined)
	if err != nil {
		t.Fatalf("take the store census before the backfill: %v", err)
	}
	for _, kind := range census.Kinds {
		want := int64(0)
		if kind.Kind == graphstore.KindVersion {
			want = 1
		}
		if kind.RowDifferences != want || kind.NodeDifferences != 0 {
			t.Errorf("before the backfill the census reads %d row and %d node difference(s) of kind %s, want %d and none",
				kind.RowDifferences, kind.NodeDifferences, kind.Kind, want)
		}
	}
	report, err := graphcensus.Backfill(ctx, database, io.Discard)
	if err != nil {
		t.Fatalf("run the backfill: %v", err)
	}
	if done := backfilledKind(t, report, graphstore.KindVersion); done.Rows != 1 || done.Nodes != 1 || !done.Complete {
		t.Errorf("the backfill reports %#v for versions, want the one row written and none remaining", done)
	}
	requireVersionNode(t, database, notes.ID, 4, graphstore.SupertagDocumentVersion, graphstore.NoEvent)
	var madeAt time.Time
	var madeBy model.Actor
	if err := database.Pool.QueryRow(ctx, `select made_at, made_by from graph_nodes where kind = $1 and id = $2`,
		graphstore.KindVersion, graphstore.VersionID(notes.RefKey, 4)).Scan(&madeAt, &madeBy); err != nil {
		t.Fatalf("read the backfilled version's node: %v", err)
	}
	if !madeAt.Equal(olderAt) || madeBy != (model.Actor{Kind: "user", ID: "bob"}) {
		t.Errorf("the backfilled version's node is made at %s by %+v, want the row's own %s and its first author bob", madeAt, madeBy, olderAt)
	}
	requireStoreCensusZero(t, database, "after the backfill")
	again, err := graphcensus.Backfill(ctx, database, io.Discard)
	if err != nil {
		t.Fatalf("run the backfill again: %v", err)
	}
	if done := backfilledKind(t, again, graphstore.KindVersion); done.Rows != 0 || done.Nodes != 0 || done.Tuples != 0 || !done.Complete {
		t.Errorf("the second backfill reports %#v for versions, want nothing written", done)
	}

	// A version an older build stored before the notes became a query node, with no node.
	if _, err := database.Pool.Exec(ctx, `
		insert into artifact_versions (artifact_id, number, markdown, authors)
		values ($1, 5, 'Written before the notes were a query.', '[{"kind":"user","id":"bob"}]')
	`, notes.ID); err != nil {
		t.Fatalf("store version 5 as an older build stores it: %v", err)
	}

	// A document made a query node through graphstore.Write, and the version written after it.
	query := graphstore.QueryNode(notes.RefKey, "VER", graphstore.QueryPlan, model.Actor{Kind: "user", ID: "alice"}, time.Now().UTC())
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		return graphstore.Write(ctx, tx, []graphstore.Node{query}, graphstore.AttributeFacts(nil, query))
	}); err != nil {
		t.Fatalf("make the notes a query node: %v", err)
	}
	node, held, err := graphstore.ReadNode(ctx, database.Pool, graphstore.KindArtifact, notes.RefKey)
	if err != nil || !held || node.Supertag != graphstore.SupertagQuery || node.Project != "VER" {
		t.Errorf("the query node reads back as %+v (held %t, %v), want supertag %s in project VER", node, held, err, graphstore.SupertagQuery)
	}
	for attribute, want := range map[string]string{
		graphstore.AttributeQuery:             graphstore.QueryPlan,
		graphstore.AttributeTakes:             `["plan_task","plan_role"]`,
		graphstore.AttributeSubtree:           "issue:" + key,
		graphstore.AttributeDefinitionVersion: strconv.Itoa(graphstore.QueryDefinitionVersion),
	} {
		value, held, err := graphstore.CurrentValue(ctx, database.Pool, graphstore.KindArtifact, notes.RefKey, attribute)
		if err != nil || !held || value != want {
			t.Errorf("the query node's %s is %q (held %t, %v), want %q", attribute, value, held, err, want)
		}
	}
	// The backfill after it writes the node of the version stored before it: a query version,
	// since its document is a query node as the pass runs, holding no definition_version, since
	// what the query node holds now says nothing of the definition the version was written under.
	if _, err := graphcensus.Backfill(ctx, database, io.Discard); err != nil {
		t.Fatalf("run the backfill after the notes became a query node: %v", err)
	}
	requireVersionNode(t, database, notes.ID, 5, graphstore.SupertagQueryVersion, graphstore.NoEvent)
	requireDefinitionVersion(t, database, notes.RefKey, 5, "")
	if code := name().StatusCode; code != http.StatusCreated {
		t.Fatalf("name the query's version: status=%d", code)
	}
	requireVersionNode(t, database, notes.ID, 6, graphstore.SupertagQueryVersion, versionEvent(t, database, notes.ID, 6))
	requireDefinitionVersion(t, database, notes.RefKey, 6, strconv.Itoa(graphstore.QueryDefinitionVersion))
	requireStoreCensusZero(t, database, "after the query's version")
}

// versionDefinition reads the node of version number of the document at refKey: its supertag, and
// the definition_version it holds, nil where it holds none.
func versionDefinition(t *testing.T, database *store.Store, refKey string, number int) (string, *string) {
	t.Helper()
	var supertag string
	var held *string
	if err := database.Pool.QueryRow(context.Background(), `
		select n.supertag,
		       (select case when t.asserted then t.to_id end from graph_tuples t
		        where t.from_kind = n.kind and t.from_id = n.id and t.kind = $3 and t.to_kind = $4
		        order by t.seq desc limit 1)
		from graph_nodes n where n.kind = $1 and n.id = $2
	`, graphstore.KindVersion, graphstore.VersionID(refKey, number), graphstore.AttributeDefinitionVersion,
		graphstore.KindValue).Scan(&supertag, &held); err != nil {
		t.Fatalf("read the node of version %d of %s: %v", number, refKey, err)
	}
	return supertag, held
}

// definitionHeld is a definition_version as a failure prints it: none where the node holds none.
func definitionHeld(held *string) string {
	if held == nil {
		return "none"
	}
	return *held
}

// requireDefinitionVersion fails unless the node of version number of the document at refKey holds
// the definition_version want, none where want is empty.
func requireDefinitionVersion(t *testing.T, database *store.Store, refKey string, number int, want string) {
	t.Helper()
	_, held := versionDefinition(t, database, refKey, number)
	if got, wanted := definitionHeld(held), cmp.Or(want, "none"); got != wanted {
		t.Errorf("the version %s holds definition_version %s, want %s", graphstore.VersionID(refKey, number), got, wanted)
	}
}

// requireVersionNode reads one version's row and its node in one statement and fails unless the
// node is the row's projection: its address, supertag, made at the row's created_at, part_of the
// row's document and nothing else, its number and created_at values the row's, and every tuple out
// of it caused by event.
func requireVersionNode(t *testing.T, database *store.Store, artifactID string, number int, supertag string, event int64) {
	t.Helper()
	var refKey string
	var createdAt time.Time
	var nodeSupertag, numberValue, createdValue *string
	var madeAt *time.Time
	var partOf []string
	var causes []int64
	if err := database.Pool.QueryRow(context.Background(), `
		select a.ref_key, v.created_at, n.supertag, n.made_at,
		       coalesce((select array_agg(t.to_kind || ':' || t.to_id order by t.seq) from graph_tuples t
		                 where t.from_kind = $3 and t.from_id = n.id and t.kind = $4), '{}'),
		       (select t.to_id from graph_tuples t where t.from_kind = $3 and t.from_id = n.id and t.kind = $5
		        and t.to_kind = 'value' order by t.seq desc limit 1),
		       (select t.to_id from graph_tuples t where t.from_kind = $3 and t.from_id = n.id and t.kind = $6
		        and t.to_kind = 'value' order by t.seq desc limit 1),
		       coalesce((select array_agg(distinct t.source_event) from graph_tuples t
		                 where t.from_kind = $3 and t.from_id = n.id), '{}')
		from artifact_versions v
		join artifacts a on a.id = v.artifact_id
		left join graph_nodes n on n.kind = $3 and n.id = a.ref_key || '@v' || v.number
		where v.artifact_id = $1::uuid and v.number = $2
	`, artifactID, number, graphstore.KindVersion, graphstore.TuplePartOf, graphstore.AttributeNumber,
		graphstore.AttributeCreatedAt).Scan(&refKey, &createdAt, &nodeSupertag, &madeAt, &partOf, &numberValue,
		&createdValue, &causes); err != nil {
		t.Fatalf("read version %d of %s and its node: %v", number, artifactID, err)
	}
	id := graphstore.VersionID(refKey, number)
	if nodeSupertag == nil {
		t.Fatalf("version %d of %s has no node %s/%s", number, refKey, graphstore.KindVersion, id)
	}
	if *nodeSupertag != supertag || madeAt == nil || !madeAt.Equal(createdAt) {
		t.Errorf("node %s is %q made at %v, want %q made at the row's created_at %s", id, *nodeSupertag, madeAt, supertag, createdAt)
	}
	if want := []string{graphstore.KindArtifact + ":" + refKey}; !slices.Equal(partOf, want) {
		t.Errorf("node %s is part_of %v, want %v", id, partOf, want)
	}
	if numberValue == nil || *numberValue != strconv.Itoa(number) {
		t.Errorf("node %s states number %v, want %d", id, numberValue, number)
	}
	if createdValue == nil {
		t.Errorf("node %s states no created_at, want the row's %s", id, createdAt)
	} else if stated, err := time.Parse(time.RFC3339Nano, *createdValue); err != nil || !stated.Equal(createdAt) {
		t.Errorf("node %s states created_at %q (%v), want the row's %s", id, *createdValue, err, createdAt)
	}
	if !slices.Equal(causes, []int64{event}) {
		t.Errorf("node %s's tuples carry causes %v, want the event %d of the write that stored its row alone", id, causes, event)
	}
}

// requireNoVersion fails if the store or artifact_versions holds anything of the version number
// of the document at refKey: its row, its node, or a tuple out of it.
func requireNoVersion(t *testing.T, database *store.Store, refKey string, number int) {
	t.Helper()
	id := graphstore.VersionID(refKey, number)
	var left int
	if err := database.Pool.QueryRow(context.Background(), `
		select (select count(*) from artifact_versions v join artifacts a on a.id = v.artifact_id
		        where a.ref_key = $3 and v.number = $4)
		     + (select count(*) from graph_nodes where kind = $1 and id = $2)
		     + (select count(*) from graph_tuples where from_kind = $1 and from_id = $2)
	`, graphstore.KindVersion, id, refKey, number).Scan(&left); err != nil {
		t.Fatalf("read what a refused write left of %s: %v", id, err)
	}
	if left != 0 {
		t.Errorf("a refused write left %d rows, nodes or tuples of %s, want none", left, id)
	}
}

// backfilledKind is one kind's line of a backfill report.
func backfilledKind(t *testing.T, report graphcensus.BackfillReport, kind string) graphcensus.KindBackfill {
	t.Helper()
	for _, done := range report.Kinds {
		if done.Kind == kind {
			return done
		}
	}
	t.Fatalf("the backfill reports no %s kind: %#v", kind, report)
	return graphcensus.KindBackfill{}
}
