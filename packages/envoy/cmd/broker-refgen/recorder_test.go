package main

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// refusalBase is a tree holding the three packages checkRefusalWrites reads, each declaring its
// registered writers and, for airlockproxy, the forwarding writes the rule leaves out, whose own
// bodies write their status parameters. It writes no refusal outside them: the 201 and the 502
// that served and unreachable pass as a status parameter are no 4xx, and Register's pointer to a
// ServeMux builds none.
var refusalBase = map[string]string{
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

func Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /served", func(w http.ResponseWriter, _ *http.Request) { served(w) })
}
`,
	approvalDir + "/approval.go": `package approval

import "net/http"

func writeMessage(w http.ResponseWriter, status int, code, text string) { w.WriteHeader(status) }

func statusOf(r *http.Request) int { return len(r.URL.Path) }
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

// refusedForm is one way net/http writes a 4xx: the files added to refusalBase that write an
// answer that way outside every registered writer, and each problem the rule names in them.
type refusedForm struct {
	form     string
	files    map[string]string
	problems []problemAt
}

// problemAt is one problem the rule names: the file and the text of the line it names, and what it
// says from there. For a status on another line than its write, write is the text of the write's
// line, whose position fills the %s of says.
type problemAt struct{ file, line, says, write string }

// refusedForms is the table of ca-refusal-gate (the Verification document's check): one row for
// each way net/http writes a 4xx, the first eight named as that check names them. A row's files
// name each name of net/http that writes the row's way (http.NotFound's row http.NotFoundHandler
// too), and some of them as a value rather than a call. http.Error's row also writes through
// net/http imported by an alias, and imports it as . and as _, which name none of its writes.
var refusedForms = []refusedForm{
	{"WriteHeader", map[string]string{
		apiDir + "/conflict.go": `package api

import "net/http"

func writeStatus(w http.ResponseWriter, status int) { w.WriteHeader(status) }

func conflict(w http.ResponseWriter) { writeStatus(w, http.StatusConflict) }
`,
		approvalDir + "/answer.go": `package approval

import "net/http"

func answer(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(statusOf(r))
}

func later(w http.ResponseWriter) func(int) { return w.WriteHeader }
`,
	}, []problemAt{
		{apiDir + "/conflict.go", "http.StatusConflict", "http.StatusConflict is 409, which w.WriteHeader at %s writes outside a registered writer", "func writeStatus"},
		{approvalDir + "/answer.go", "w.WriteHeader(statusOf(r))", "w.WriteHeader writes statusOf(r), a status refgen cannot read as a constant", ""},
		{approvalDir + "/answer.go", "return w.WriteHeader", "w.WriteHeader, outside a registered writer, is taken as a value, a write whose status refgen cannot read", ""},
	}},
	{"http.Error", map[string]string{
		apiDir + "/forbid.go": `package api

import "net/http"

func forbid(w http.ResponseWriter) {
	http.Error(w, "forbidden", http.StatusForbidden)
}

var refuseAll = http.Error
`,
		apiDir + "/alias.go": `package api

import nethttp "net/http"

func unauthorized(w nethttp.ResponseWriter) {
	nethttp.Error(w, "unauthorized", nethttp.StatusUnauthorized)
}
`,
		approvalDir + "/dot.go": `package approval

import . "net/http"

func refuseDot(w ResponseWriter) { Error(w, "refused", StatusForbidden) }
`,
		airlockDir + "/blank.go": `package airlockproxy

import _ "net/http"
`,
	}, []problemAt{
		{apiDir + "/forbid.go", "http.StatusForbidden", "http.Error writes 403 outside a registered writer", ""},
		{apiDir + "/forbid.go", "var refuseAll", "http.Error, outside a registered writer, is taken as a value, a write whose status refgen cannot read", ""},
		{apiDir + "/alias.go", "nethttp.StatusUnauthorized", "nethttp.Error writes 401 outside a registered writer", ""},
		{approvalDir + "/dot.go", `import . "net/http"`, "net/http is imported as ., a write refgen cannot read", ""},
		{airlockDir + "/blank.go", `import _ "net/http"`, "net/http is imported as _, a write refgen cannot read", ""},
	}},
	{"http.Redirect", map[string]string{
		airlockDir + "/redirect.go": `package airlockproxy

import "net/http"

func gone(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/elsewhere", http.StatusGone)
}

var moved = http.RedirectHandler("/elsewhere", http.StatusNotFound)
`,
	}, []problemAt{
		{airlockDir + "/redirect.go", "http.StatusGone", "http.Redirect writes 410 outside a registered writer", ""},
		{airlockDir + "/redirect.go", "http.StatusNotFound", "http.RedirectHandler writes 404 outside a registered writer", ""},
	}},
	{"http.NotFound", map[string]string{
		apiDir + "/missing.go": `package api

import "net/http"

func missing(w http.ResponseWriter, r *http.Request) {
	http.NotFound(w, r)
}

var missingHandler = http.HandlerFunc(http.NotFound)

var nothing = http.NotFoundHandler()
`,
	}, []problemAt{
		{apiDir + "/missing.go", "http.NotFound(w, r)", "http.NotFound, outside a registered writer, writes a fixed 404", ""},
		{apiDir + "/missing.go", "http.HandlerFunc(http.NotFound)", "http.NotFound, outside a registered writer, writes a fixed 404", ""},
		{apiDir + "/missing.go", "http.NotFoundHandler()", "http.NotFoundHandler, outside a registered writer, is a handler that writes a fixed 404", ""},
	}},
	{"http.ServeContent", map[string]string{
		approvalDir + "/content.go": `package approval

import (
	"net/http"
	"strings"
	"time"
)

func content(w http.ResponseWriter, r *http.Request) {
	http.ServeContent(w, r, "page.html", time.Time{}, strings.NewReader("page"))
}
`,
	}, []problemAt{
		{approvalDir + "/content.go", "http.ServeContent(", "http.ServeContent, outside a registered writer, writes a status net/http chooses as it serves a range (412 or 416), a write refgen cannot read", ""},
	}},
	{"http.ServeFile", map[string]string{
		airlockDir + "/file.go": `package airlockproxy

import (
	"io/fs"
	"net/http"
)

func file(w http.ResponseWriter, r *http.Request, fsys fs.FS) {
	http.ServeFile(w, r, "index.html")
	http.ServeFileFS(w, r, fsys, "index.html")
}
`,
	}, []problemAt{
		{airlockDir + "/file.go", "http.ServeFile(", "http.ServeFile, outside a registered writer, writes a status net/http chooses as it serves a file (400, 403, 404, 412 or 416), a write refgen cannot read", ""},
		{airlockDir + "/file.go", "http.ServeFileFS(", "http.ServeFileFS, outside a registered writer, writes a status net/http chooses as it serves a file (400, 403, 404, 412 or 416), a write refgen cannot read", ""},
	}},
	{"http.FileServer", map[string]string{
		apiDir + "/files.go": `package api

import (
	"io/fs"
	"net/http"
)

var files = http.FileServer(http.Dir("static"))

func filesOf(fsys fs.FS) http.Handler { return http.FileServerFS(fsys) }
`,
	}, []problemAt{
		{apiDir + "/files.go", "http.FileServer(", "http.FileServer, outside a registered writer, is a handler that writes a status net/http chooses as it serves a file (403, 404, 412 or 416), a write refgen cannot read", ""},
		{apiDir + "/files.go", "http.FileServerFS(", "http.FileServerFS, outside a registered writer, is a handler that writes a status net/http chooses as it serves a file (403, 404, 412 or 416), a write refgen cannot read", ""},
	}},
	{"http.StripPrefix", map[string]string{
		approvalDir + "/prefix.go": `package approval

import "net/http"

func under(h http.Handler) http.Handler { return http.StripPrefix("/approval", h) }
`,
	}, []problemAt{
		{approvalDir + "/prefix.go", "http.StripPrefix(", "http.StripPrefix, outside a registered writer, is a handler that writes a fixed 404 to a path without its prefix", ""},
	}},
	{"http.ServeMux", map[string]string{
		airlockDir + "/mux.go": `package airlockproxy

import "net/http"

func routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /things", func(http.ResponseWriter, *http.Request) {})
	return mux
}

var spare = new(http.ServeMux)

func onDefault() {
	http.Handle("/things", routes())
	http.HandleFunc("/other", func(http.ResponseWriter, *http.Request) {})
}

func serveDefault(w http.ResponseWriter, r *http.Request) { http.DefaultServeMux.ServeHTTP(w, r) }
`,
	}, []problemAt{
		{airlockDir + "/mux.go", "http.NewServeMux()", "http.NewServeMux, outside a registered writer, builds a ServeMux, which writes a fixed 400, 404 or 405 of its own", ""},
		{airlockDir + "/mux.go", "new(http.ServeMux)", "http.ServeMux, outside a registered writer, builds a ServeMux, which writes a fixed 400, 404 or 405 of its own", ""},
		{airlockDir + "/mux.go", "http.Handle(", "http.Handle, outside a registered writer, registers on http.DefaultServeMux, a ServeMux, which writes a fixed 400, 404 or 405 of its own", ""},
		{airlockDir + "/mux.go", "http.HandleFunc(", "http.HandleFunc, outside a registered writer, registers on http.DefaultServeMux, a ServeMux, which writes a fixed 400, 404 or 405 of its own", ""},
		{airlockDir + "/mux.go", "http.DefaultServeMux", "http.DefaultServeMux, outside a registered writer, is a ServeMux, which writes a fixed 400, 404 or 405 of its own", ""},
	}},
	{"http.CrossOriginProtection", map[string]string{
		apiDir + "/origin.go": `package api

import "net/http"

func guarded(h http.Handler) http.Handler { return http.NewCrossOriginProtection().Handler(h) }

var origins http.CrossOriginProtection

func handedGuard(c *http.CrossOriginProtection, h http.Handler) http.Handler { return c.Handler(h) }
`,
	}, []problemAt{
		{apiDir + "/origin.go", "http.NewCrossOriginProtection()", "http.NewCrossOriginProtection, outside a registered writer, builds a CrossOriginProtection, whose Handler writes a fixed 403 of its own", ""},
		{apiDir + "/origin.go", "var origins", "http.CrossOriginProtection, outside a registered writer, is a CrossOriginProtection, whose Handler writes a fixed 403 of its own", ""},
		{apiDir + "/origin.go", "func handedGuard", "http.CrossOriginProtection, outside a registered writer, is a CrossOriginProtection, whose Handler writes a fixed 403 of its own", ""},
	}},
}

// gateForms is each row of refusedForms by its form, in order: the eight ca-refusal-gate names,
// then a mux's and a CrossOriginProtection's own answers.
var gateForms = []string{
	"WriteHeader", "http.Error", "http.Redirect", "http.NotFound", "http.ServeContent", "http.ServeFile", "http.FileServer", "http.StripPrefix",
	"http.ServeMux", "http.CrossOriginProtection",
}

// TestARefusalIsWrittenOnlyThroughARegisteredWriter drives the refusal rule over refusalBase,
// which it accepts; over refusalBase with each row of refusedForms added, where it refuses each
// problem the row names, with its file and line, and nothing else; and over the broker's own
// source, as the docs build reads it, which it accepts. It counts the rows it drove and fails
// when that is fewer than the table holds, or when the table is not one row for each of
// gateForms.
func TestARefusalIsWrittenOnlyThroughARegisteredWriter(t *testing.T) {
	forms := make([]string, len(refusedForms))
	for i, row := range refusedForms {
		forms[i] = row.form
	}
	if !slices.Equal(forms, gateForms) {
		t.Fatalf("refusedForms holds the rows %q, want one for each of %q", forms, gateForms)
	}
	t.Run("the fixture's writers alone", func(t *testing.T) {
		if err := checkRefusalWrites(writeTree(t, refusalBase)); err != nil {
			t.Fatalf("the tree is refused:\n%v", err)
		}
	})
	driven := 0
	for _, row := range refusedForms {
		t.Run(row.form, func(t *testing.T) {
			tree := maps.Clone(refusalBase)
			maps.Copy(tree, row.files)
			err := checkRefusalWrites(writeTree(t, tree))
			driven++
			want := make([]string, len(row.problems))
			for i, p := range row.problems {
				says := p.says
				if p.write != "" {
					says = fmt.Sprintf(says, strings.TrimSuffix(lineOf(t, tree, p.file, p.write), ": "))
				}
				want[i] = lineOf(t, tree, p.file, p.line) + says
			}
			if err == nil {
				t.Fatalf("the tree is accepted, want %d problems:\n%s", len(want), strings.Join(want, "\n"))
			}
			got := strings.Split(err.Error(), "\n")
			if len(got) != len(want) {
				t.Fatalf("got %d problems, want %d:\n%v", len(got), len(want), err)
			}
			for _, w := range want {
				i := slices.IndexFunc(got, func(g string) bool { return strings.HasPrefix(g, w) })
				if i < 0 {
					t.Errorf("no problem starts %q; got:\n%v", w, err)
					continue
				}
				got = slices.Delete(got, i, i+1)
			}
		})
	}
	t.Logf("drove %d of the %d rows of refusedForms", driven, len(refusedForms))
	if driven < len(refusedForms) {
		t.Fatalf("drove %d rows of refusedForms, fewer than the %d it holds", driven, len(refusedForms))
	}
	t.Run("the broker's own source", func(t *testing.T) {
		if err := checkRefusalWrites(filepath.Join("..", "..", "..", "..")); err != nil {
			t.Fatalf("the tree is refused:\n%v", err)
		}
	})
}

// writeTree writes files, each named by its path from the repository root, under a new
// directory, and returns that directory.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, text := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// lineOf is the file name of tree and the line of its first line holding text, as a problem
// starts.
func lineOf(t *testing.T, tree map[string]string, name, text string) string {
	t.Helper()
	for i, line := range strings.Split(tree[name], "\n") {
		if strings.Contains(line, text) {
			return fmt.Sprintf("%s:%d: ", name, i+1)
		}
	}
	t.Fatalf("%s holds no line with %q", name, text)
	return ""
}
