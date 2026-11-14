package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// refusalFixture is a tree holding the three packages checkRefusalWrites reads, each declaring
// its registered writers and, for airlockproxy, the forwarding writes the rule leaves out, whose
// own bodies write their status parameters. Beside them, three refusals written outside every
// registered writer: in the API package, the 409 conflict passes as writeJSON's status and an
// http.Error's 403; on the approval page, a w.WriteHeader of a status refgen cannot read.
var refusalFixture = map[string]string{
	apiDir + "/server.go": `package api

import "net/http"

func writeError(w http.ResponseWriter, status int, code, msg string) { w.WriteHeader(status) }

func writePageRefusal(w http.ResponseWriter, status int, code, msg, approveURL string) {
	writeError(w, status, code, msg)
}

func writeJSON(w http.ResponseWriter, status int) { w.WriteHeader(status) }

func served(w http.ResponseWriter) {
	status := http.StatusCreated
	writeJSON(w, status)
}

func conflict(w http.ResponseWriter) { writeJSON(w, http.StatusConflict) }

func forbid(w http.ResponseWriter) {
	http.Error(w, "forbidden", http.StatusForbidden)
}
`,
	approvalDir + "/approval.go": `package approval

import "net/http"

func writeMessage(w http.ResponseWriter, status int, text string) { w.WriteHeader(status) }

func statusOf(r *http.Request) int { return len(r.URL.Path) }

func answer(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(statusOf(r))
}
`,
	airlockDir + "/proxy.go": `package airlockproxy

import "net/http"

type Server struct{}

type statusWriter struct{ http.ResponseWriter }

type answerWriter struct{ http.ResponseWriter }

func (s *Server) refuse(w http.ResponseWriter, status int) { http.Error(w, "refused", status) }

func (w *statusWriter) WriteHeader(status int) { w.ResponseWriter.WriteHeader(status) }

func (w *answerWriter) WriteHeader(status int) { w.ResponseWriter.WriteHeader(status) }

func fail(w http.ResponseWriter, status int) { http.Error(w, "failed", status) }

func unreachable(w http.ResponseWriter) { fail(w, http.StatusBadGateway) }
`,
}

// TestARefusalIsWrittenOnlyThroughARegisteredWriter drives the refusal rule over two trees: the
// fixture, whose three refusals outside a registered writer it refuses, each with its file and
// line and nothing else; and the broker's own source, as the docs build reads it, which it
// accepts. A status parameter is followed to each call of its function: the 409 conflict passes
// writeJSON is refused at that call, while the 201 and the 502 the other calls pass are accepted,
// as are the fixture's writers and its forwarding writes, so the three lines are all it refuses.
func TestARefusalIsWrittenOnlyThroughARegisteredWriter(t *testing.T) {
	fixture := t.TempDir()
	for name, text := range refusalFixture {
		path := filepath.Join(fixture, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// at is the fixture's file name and the line of its first line holding text.
	at := func(name, text string) string {
		for i, line := range strings.Split(refusalFixture[name], "\n") {
			if strings.Contains(line, text) {
				return fmt.Sprintf("%s:%d: ", name, i+1)
			}
		}
		t.Fatalf("%s holds no line with %q", name, text)
		return ""
	}
	driven := 0
	for _, row := range []struct {
		name, root string
		want       []string // the start of each problem, in order; none for a tree accepted
	}{
		{"a fixture writing a 409 through a status parameter, a 403 and an unreadable status outside a registered writer", fixture, []string{
			at(apiDir+"/server.go", "http.StatusConflict") + "http.StatusConflict is 409, which w.WriteHeader at " + strings.TrimSuffix(at(apiDir+"/server.go", "func writeJSON"), ": ") + " writes outside a registered writer",
			at(apiDir+"/server.go", "http.StatusForbidden") + "http.Error writes 403 outside a registered writer",
			at(approvalDir+"/approval.go", "w.WriteHeader(statusOf(r))") + "w.WriteHeader writes statusOf(r), a status refgen cannot read as a constant",
		}},
		{"the broker's own source", filepath.Join("..", "..", "..", ".."), nil},
	} {
		t.Run(row.name, func(t *testing.T) {
			driven++
			err := checkRefusalWrites(row.root)
			if row.want == nil {
				if err != nil {
					t.Fatalf("the tree is refused:\n%v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("the tree is accepted, want %d problems:\n%s", len(row.want), strings.Join(row.want, "\n"))
			}
			got := strings.Split(err.Error(), "\n")
			if len(got) != len(row.want) {
				t.Fatalf("got %d problems, want %d:\n%v", len(got), len(row.want), err)
			}
			for i, want := range row.want {
				if !strings.HasPrefix(got[i], want) {
					t.Errorf("problem %d = %q, want it to start %q", i, got[i], want)
				}
			}
		})
	}
	if driven != 2 {
		t.Fatalf("the table drove %d trees, want 2", driven)
	}
}
