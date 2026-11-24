package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// refsShapeServer answers every references read with body, as Dispatch does.
func refsShapeServer(t *testing.T, body []byte) *api {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/references") {
			t.Errorf("refs-shape read %s, which is no references answer", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return newAPI(srv.URL, "refs-shape-bearer", 2)
}

// refsShapeSeed is a seed of n LGA issues.
func refsShapeSeed(n int) seedFile {
	var sf seedFile
	for i := 1; i <= n; i++ {
		sf.Issues = append(sf.Issues, seedIssue{Key: fmt.Sprintf("LGA-%d", i), Project: "LGA"})
	}
	return sf
}

// refsShapeOutput is what refsShape prints, and its error.
func refsShapeOutput(t *testing.T, a *api, sf seedFile) (string, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = w
	shapeErr := refsShape(context.Background(), a, sf)
	os.Stdout = stdout
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	return string(out), shapeErr
}

// TestRefsShapeMeasuresEveryAuthorList: a references answer in production's shape, refProdRows
// version rows over two members whose author lists are those of 160 writers (refWriter), passes
// the shape check with each list's compact bytes measured and compared. The answer is indented,
// so a check that measured a list as it came, or read none, would print another figure and fail.
func TestRefsShapeMeasuresEveryAuthorList(t *testing.T) {
	type version struct {
		Authors []refActor `json:"authors"`
	}
	type member struct {
		Artifact struct {
			Versions []version `json:"versions"`
		} `json:"artifact"`
	}
	const writers = 160
	members := make([]member, 2)
	for i := range refProdRows {
		m := &members[i%len(members)]
		m.Artifact.Versions = append(m.Artifact.Versions, version{Authors: []refActor{refWriter(i % writers)}})
	}
	body, err := json.MarshalIndent(map[string]any{"members": members}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	out, err := refsShapeOutput(t, refsShapeServer(t, body), refsShapeSeed(2))
	if err != nil {
		t.Errorf("refsShape = %v, want the shape check to pass", err)
	}
	for _, want := range []string{
		fmt.Sprintf("distinct author lists / version rows: %.4f ", float64(writers)/refProdRows),
		fmt.Sprintf("author bytes per row: %d.0\n", refAuthorBytes),
		"): PASS\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("refsShape printed %q, want %q in it", out, want)
		}
	}
}

// TestRefsShapeRefusesAnAnswerThatDoesNotDecode: an answer that spells its members member its own
// way, which encoding/json matches in any case and its error quotes, and holds a string there is
// refused, naming the issue and the member as refsAnswer names it, in fixed text that quotes none
// of the answer.
func TestRefsShapeRefusesAnAnswerThatDoesNotDecode(t *testing.T) {
	const spelled = "mEMBERS"
	sf := refsShapeSeed(1)
	_, err := refsShapeOutput(t, refsShapeServer(t, []byte(`{"`+spelled+`":"x"}`)), sf)
	if want := "references of " + sf.Issues[0].Key + `: the answer's "members" is a JSON string`; err == nil || !strings.HasPrefix(err.Error(), want) {
		t.Fatalf("refsShape = %v, want a refusal beginning %q", err, want)
	}
	if strings.Contains(err.Error(), spelled) {
		t.Errorf("refsShape = %v, which quotes the answer's %s", err, spelled)
	}
}
