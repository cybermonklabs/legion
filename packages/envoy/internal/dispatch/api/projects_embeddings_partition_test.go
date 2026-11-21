package api

import (
	"net/http"
	"testing"
)

// POST /api/v1/projects gives the project its partition of embeddings in the create's own
// transaction (migration 20261120105330's trigger on projects), before and after the partitioned
// table is swapped in, and the project's first issue stores its embeddings row in that partition,
// a graph of its project's vectors alone. A refused create leaves no partition behind.
func TestCreatingAProjectGivesItItsPartitionOfEmbeddings(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	ctx := t.Context()
	partition := func(key string) *string {
		t.Helper()
		var name *string
		if err := database.Pool.QueryRow(ctx, `select to_regclass('embeddings_project_' || lower($1))::text`, key).Scan(&name); err != nil {
			t.Fatalf("read %s's partition: %v", key, err)
		}
		return name
	}
	create := func(key string, want int) {
		t.Helper()
		if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
			"key": key, "name": "Project " + key,
		}, "alice"); response.Code != want {
			t.Fatalf("create %s: status=%d body=%s, want %d", key, response.Code, response.Body.String(), want)
		}
	}

	create("PARTN", http.StatusCreated)
	if partition("PARTN") == nil {
		t.Fatal("PARTN was created with no partition of embeddings")
	}
	create("PARTN", http.StatusConflict)
	if _, err := database.FillEmbeddingProjectKeys(ctx, ""); err != nil {
		t.Fatalf("partition embeddings: %v", err)
	}

	create("PARTM", http.StatusCreated)
	if partition("PARTM") == nil {
		t.Fatal("PARTM, created after the swap, has no partition of embeddings")
	}
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "PARTM", "title": "The first issue of PARTM",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create an issue of PARTM: status=%d body=%s", response.Code, response.Body.String())
	}
	var stored string
	if err := database.Pool.QueryRow(ctx, `select tableoid::regclass::text from embeddings where kind = 'issue' and id = 'PARTM-1'`).Scan(&stored); err != nil {
		t.Fatalf("read PARTM-1's embeddings row: %v", err)
	}
	if stored != "embeddings_project_partm" {
		t.Errorf("PARTM-1's embeddings row is stored in %s, want embeddings_project_partm", stored)
	}
}
