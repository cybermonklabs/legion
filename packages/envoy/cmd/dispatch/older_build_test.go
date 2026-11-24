package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// olderBuildSigningKey is the one DISPATCH_SIGNING_KEY both builds of the run carry, as every
// process of one deployment does: the registry refuses a process whose key set a live one does
// not share.
const olderBuildSigningKey = "older-build-signing-key"

// TestAnOlderBuildServesOverANewerStoreSchema runs two builds over one database, as a release
// does while it rolls (c-schema-older-build). The store is migrated to the schema version one
// above this tree's, holding one element that version introduces: the typed block newer, whose
// since and creatable_since are that version. This tree is the build one version below: its
// process boots over that store, its /healthz answers 200, and it installs exactly the store's
// elements whose since is at or below its own version, at its own version. It serves a document of
// elements it knows, answers 409 DOC_NEWER_CONTENT for a document holding newer, and refuses a
// write that adds it. The build of the store's version - this tree with blocks.json raised to that
// version and declaring newer, built with a Go overlay - serves beside it on the same database,
// installs the whole store and writes newer.
func TestAnOlderBuildServesOverANewerStoreSchema(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	databaseURL := database.Pool.Config().ConnString()
	built := pmdoc.BuiltSchemaVersion()
	newer := built + 1
	migrateStoreToANewerVersion(t, ctx, database.Pool, newer)

	older := startDispatchProcessWith(t, databaseURL, dispatchOptions{SigningKey: olderBuildSigningKey})
	older.waitHealthy(t)
	if status, health := older.schemaHealth(t); status != http.StatusOK || !health.OK {
		t.Fatalf("the older build's /healthz over a store at version %d = %d %+v, want 200 ok", newer, status, health)
	}
	served := older.servedSchema(t)
	if served.Version != built {
		t.Errorf("the older build over a store at version %d serves version %d, want its own %d", newer, served.Version, built)
	}
	file := readBlockSchemaFile(t)
	if got, want := typeNames(served), typeNames(file); !slices.Equal(got, want) {
		t.Errorf("the older build over a store at version %d serves the types %v, want exactly the store's types read at version %d or below, %v",
			newer, got, built, want)
	}

	older.call(t, http.MethodPost, "/api/v1/projects", `{"key":"OLDR","name":"Older build"}`, http.StatusCreated)
	_, known := createSpecThrough(t, older, "Known elements", "# Known\n\nA paragraph this build reads.\n")
	if text := documentText(t, older, known); !strings.Contains(text, "A paragraph this build reads.") {
		t.Errorf("the older build reads the document of known elements as %q", text)
	}
	const newerBlock = ":::newer{#n1}\nWritten by the newer build.\n:::\n"
	refused := older.send(t, http.MethodPost, "/api/v1/artifacts/"+known+"/edits",
		encodedBody(t, map[string]any{"ops": []map[string]string{{"op": "insert", "after": "end", "markdown": newerBlock}}}),
		asUser(dispatchTestLogin))
	if refused.status != http.StatusBadRequest || !strings.Contains(string(refused.body), `unknown typed block \"newer\"`) {
		t.Errorf("the older build answered a write adding newer with %s, want 400 naming the unknown typed block newer", refused)
	}

	newerProcess := startDispatchProcessWith(t, databaseURL, dispatchOptions{
		SigningKey: olderBuildSigningKey,
		Binary:     buildAtNewerVersion(t, newer),
	})
	newerProcess.waitHealthy(t)
	if served := newerProcess.servedSchema(t); served.Version != newer || !servedType(served, "newer") {
		t.Fatalf("the build of version %d serves version %d with the types %v, want version %d with newer: it installs the whole store",
			newer, served.Version, typeNames(served), newer)
	}
	_, holding := createSpecThrough(t, newerProcess, "Newer element", "# Newer\n\n"+newerBlock)
	if text := documentText(t, newerProcess, holding); !strings.Contains(text, ":::newer") {
		t.Errorf("the build of version %d reads the document it wrote as %q, want it holding newer", newer, text)
	}

	answer := older.send(t, http.MethodGet, "/api/v1/artifacts/"+holding+"/text", "", asUser(dispatchTestLogin))
	if answer.status != http.StatusConflict || answer.code() != "DOC_NEWER_CONTENT" {
		t.Errorf("the older build answered the document holding newer with %s, want 409 DOC_NEWER_CONTENT", answer)
	}
}

// migrateStoreToANewerVersion applies, in one transaction, the type migration of the release whose
// version is newer: the typed block newer, read and created from that version, and every block
// type's version raised to it.
func migrateStoreToANewerVersion(t *testing.T, ctx context.Context, pool *store.Pool, newer int) {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the type migration: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `
		insert into graph_nodes (kind, id, supertag, visibility, state, attributes, made_at, made_by, source_event)
		values ('supertag', 'newer', 'supertag', 'project', 'live',
			jsonb_build_object('block_type', true, 'content', 'paragraph+', 'render', 'host',
				'version', $1::int, 'position', 98, 'since', $1::int, 'creatable_since', $1::int),
			now(), '{"kind": "system", "id": "older-build-test"}'::jsonb, 0)`, newer); err != nil {
		t.Fatalf("write the newer type: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		update graph_nodes set attributes = jsonb_set(attributes, '{version}', to_jsonb($1::int))
		where kind = 'supertag' and coalesce((attributes->>'block_type')::boolean, false)`, newer); err != nil {
		t.Fatalf("raise the store's version to %d: %v", newer, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the type migration: %v", err)
	}
}

// buildAtNewerVersion builds this tree's envoy-dispatch with its embedded blocks.json raised to
// newer and declaring the typed block newer at that version, as the release that introduces it
// would ship it. The change is a Go overlay of the one file, so the tree itself is not touched.
func buildAtNewerVersion(t *testing.T, newer int) string {
	t.Helper()
	source, err := filepath.Abs(blockSchemaFile)
	if err != nil {
		t.Fatalf("resolve %s: %v", blockSchemaFile, err)
	}
	body, err := os.ReadFile(source)
	if err != nil {
		t.Fatalf("read %s: %v", blockSchemaFile, err)
	}
	var manifest map[string]any
	if err := json.Unmarshal(body, &manifest); err != nil {
		t.Fatalf("decode %s: %v", blockSchemaFile, err)
	}
	types, ok := manifest["types"].([]any)
	if !ok {
		t.Fatalf("%s declares no types array", blockSchemaFile)
	}
	manifest["version"] = newer
	manifest["types"] = append(types, map[string]any{
		"name": "newer", "content": "paragraph+", "render": "host", "attributes": map[string]any{},
		"since": newer, "creatable_since": newer,
	})
	raised, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("encode the raised manifest: %v", err)
	}
	dir := t.TempDir()
	replacement := filepath.Join(dir, "blocks.json")
	if err := os.WriteFile(replacement, raised, 0o600); err != nil {
		t.Fatalf("write the raised manifest: %v", err)
	}
	overlay, err := json.Marshal(map[string]map[string]string{"Replace": {source: replacement}})
	if err != nil {
		t.Fatalf("encode the overlay: %v", err)
	}
	overlayFile := filepath.Join(dir, "overlay.json")
	if err := os.WriteFile(overlayFile, overlay, 0o600); err != nil {
		t.Fatalf("write the overlay: %v", err)
	}
	return cmdtest.BuildCommand(t, cmdtest.Command{Name: "envoy-dispatch-newer", Flags: []string{"-overlay", overlayFile}})
}

// createSpecThrough creates an issue of the project OLDR whose spec is markdown, through p, and
// returns its key and its spec's id.
func createSpecThrough(t *testing.T, p *dispatchProcess, title, markdown string) (string, string) {
	t.Helper()
	created := p.call(t, http.MethodPost, "/api/v1/issues",
		encodedBody(t, map[string]any{"project": "OLDR", "title": title, "spec": markdown, "force": true}), http.StatusCreated)
	var issue struct {
		Key               string `json:"key"`
		PrimaryArtifactID string `json:"primary_artifact_id"`
	}
	if err := json.Unmarshal(created, &issue); err != nil || issue.Key == "" || issue.PrimaryArtifactID == "" {
		t.Fatalf("create the issue %q: %v: %s", title, err, created)
	}
	return issue.Key, issue.PrimaryArtifactID
}

// documentText is the document's markdown as p reads it now (GET /api/v1/artifacts/{id}/text).
func documentText(t *testing.T, p *dispatchProcess, artifactID string) string {
	t.Helper()
	var document struct {
		Markdown string `json:"markdown"`
	}
	answer := p.call(t, http.MethodGet, "/api/v1/artifacts/"+artifactID+"/text", "", http.StatusOK)
	if err := json.Unmarshal(answer, &document); err != nil {
		t.Fatalf("decode the document's text: %v: %s", err, answer)
	}
	return document.Markdown
}

// encodedBody is value as a request's JSON body.
func encodedBody(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode a request body: %v", err)
	}
	return string(encoded)
}

// typeNames is the names of the typed blocks value declares, in its order.
func typeNames(value pmdoc.BlockSchema) []string {
	names := make([]string, 0, len(value.Types))
	for _, typ := range value.Types {
		names = append(names, typ.Name)
	}
	return names
}
