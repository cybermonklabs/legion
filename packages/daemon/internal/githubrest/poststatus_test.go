package githubrest

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// GitHub answers some writes with one of two 2xx statuses that mean two things, and the second
// carries no body: POST /merges is 201 with the merge commit, or 204 with nothing when the base
// holds the head already. PostStatus answers the status, reads a body into into where there is
// one, and reads nothing from a 204, which is no malformed answer; any other answer is an *Answer
// as Post's is.
func TestPostStatusAnswersTheStatusAndReadsOnlyABody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/widgets/joined":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"sha":"c0de"}`))
		case "/repos/acme/widgets/nothing":
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, `{"message":"Merge conflict"}`, http.StatusConflict)
		}
	}))
	defer server.Close()
	client := Client{Token: "token", API: server.URL + "/repos/acme/widgets"}
	var merged struct {
		SHA string `json:"sha"`
	}
	if status, err := client.PostStatus(context.Background(), "/joined", map[string]string{}, &merged); err != nil || status != http.StatusCreated || merged.SHA != "c0de" {
		t.Fatalf("PostStatus of a 201 = %d, %+v, %v; want 201 and its body read", status, merged, err)
	}
	merged.SHA = ""
	if status, err := client.PostStatus(context.Background(), "/nothing", map[string]string{}, &merged); err != nil || status != http.StatusNoContent || merged.SHA != "" {
		t.Fatalf("PostStatus of a 204 = %d, %+v, %v; want 204 and nothing read", status, merged, err)
	}
	status, err := client.PostStatus(context.Background(), "/conflict", map[string]string{}, &merged)
	var answer *Answer
	if !errors.As(err, &answer) || answer.Status != http.StatusConflict || status != 0 {
		t.Fatalf("PostStatus of a 409 = %d, %v; want no status and an *Answer with 409", status, err)
	}
}
