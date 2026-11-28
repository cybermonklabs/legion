package githubapp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/architecture/notes"
)

// fileCommit is the commit the files below are read at.
const fileCommit = "0123456789abcdef0123456789abcdef01234567"

// fileAnswer is the contents read's answer for one file under the object media type, of content,
// with size its listed size.
func fileAnswer(t *testing.T, content string, size int) string {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"type": "file", "size": size, "encoding": "base64",
		"content": base64.StdEncoding.EncodeToString([]byte(content)),
	})
	if err != nil {
		t.Fatalf("encode the answer: %v", err)
	}
	return string(body)
}

// File reads one file at a commit with one contents read under the installation token, asking for
// the object media type, each part of the path escaped on its own so a part that holds an "@", a
// space or a "#" reaches GitHub as itself and a "/" between parts travels as a "/", and the
// commit as the ref. It answers the file's bytes.
func TestFileReadsOneFileAtACommit(t *testing.T) {
	const content = "jobs:\n  build:\n    runs-on: ubuntu-latest\n"
	var seen []string
	client := branchServer(t, func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.EscapedPath()+"?"+r.URL.RawQuery+" "+r.Header.Get("Authorization")+" "+r.Header.Get("Accept"))
		w.Write([]byte(fileAnswer(t, content, len(content))))
	})

	read, err := client.File(context.Background(), "ghs_installation", "acme", "web", fileCommit, ".github/workflows/ci @probe#1.yaml")
	if err != nil || string(read) != content {
		t.Fatalf("File = %q, %v; want %q", read, err, content)
	}
	want := "GET /repos/acme/web/contents/.github/workflows/ci%20@probe%231.yaml?ref=" + fileCommit + " Bearer ghs_installation application/vnd.github.object+json"
	if len(seen) != 1 || seen[0] != want {
		t.Fatalf("File sent %q, want the one request %q", seen, want)
	}
}

// GitHub's 403 and 404 are ErrRepositoryUnreadable, as every repository read's are: GitHub answers
// 404 both for a path the commit does not hold and for a repository the installation cannot see.
// Any other failure is an error that is not ErrRepositoryUnreadable, so a caller tells GitHub
// refusing the read from GitHub not answering it.
func TestFileGitHubRefusesIsUnreadable(t *testing.T) {
	for _, row := range []struct {
		status     int
		unreadable bool
	}{{http.StatusNotFound, true}, {http.StatusForbidden, true}, {http.StatusInternalServerError, false}, {http.StatusBadGateway, false}} {
		client := branchServer(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(row.status)
			w.Write([]byte(`{"message":"Not Found"}`))
		})
		_, err := client.File(context.Background(), "ghs_installation", "acme", "web", fileCommit, ".github/workflows/ci.yaml")
		if err == nil || errors.Is(err, ErrRepositoryUnreadable) != row.unreadable {
			t.Errorf("File answered %d: err = %v, want an error that is ErrRepositoryUnreadable: %v", row.status, err, row.unreadable)
		}
	}
}

// A file GitHub lists over notes.MaxFileSize is notes.ErrFileTooLarge, refused from the size GitHub
// names, as the object media type answers a file of over 1 MB with no content, and so is content
// that decodes past the bound whatever size GitHub names. A file at the bound reads whole.
func TestFileHoldsAFileToTheNotesBound(t *testing.T) {
	const path = ".github/workflows/ci.yaml"
	atBound := strings.Repeat("x", notes.MaxFileSize)
	for _, row := range []struct {
		name, answer string
		want         string
	}{
		{name: "a listed size over the bound", answer: `{"type":"file","size":1048577,"encoding":"none","content":""}`, want: "file too large: " + path + " is 1048577 bytes (limit 1048576)"},
		{name: "content over the bound", answer: fileAnswer(t, atBound+"x", 10), want: "file too large: " + path + " is 1048577 bytes (limit 1048576)"},
	} {
		client := branchServer(t, func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(row.answer)) })
		if _, err := client.File(context.Background(), "ghs_installation", "acme", "web", fileCommit, path); !errors.Is(err, notes.ErrFileTooLarge) || err.Error() != row.want {
			t.Errorf("%s: err = %v, want notes.ErrFileTooLarge %q", row.name, err, row.want)
		}
	}
	client := branchServer(t, func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(fileAnswer(t, atBound, len(atBound)))) })
	if read, err := client.File(context.Background(), "ghs_installation", "acme", "web", fileCommit, path); err != nil || len(read) != notes.MaxFileSize {
		t.Fatalf("a file at the bound: %d bytes, err = %v; want all %d", len(read), err, notes.MaxFileSize)
	}
}

// An answer that is no file, or no base64, or no JSON, is an error and never a file's bytes.
func TestFileRefusesAnAnswerThatIsNoFile(t *testing.T) {
	for name, answer := range map[string]string{
		"a directory":       `{"type":"dir","size":0,"entries":[]}`,
		"a symlink":         `{"type":"symlink","size":10,"target":"other.yaml"}`,
		"another encoding":  `{"type":"file","size":4,"encoding":"utf-8","content":"jobs"}`,
		"content no base64": `{"type":"file","size":4,"encoding":"base64","content":"!!!!"}`,
		"not JSON":          `<html>`,
	} {
		t.Run(name, func(t *testing.T) {
			client := branchServer(t, func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(answer)) })
			if read, err := client.File(context.Background(), "ghs_installation", "acme", "web", fileCommit, ".github/workflows/ci.yaml"); err == nil {
				t.Fatalf("File of %s = %q, want an error", answer, read)
			}
		})
	}
}

// A client with no App key answers ErrNoAppKey, as every read of this package does.
func TestFileWithNoAppKey(t *testing.T) {
	var client *Client
	if _, err := client.File(context.Background(), "ghs_installation", "acme", "web", fileCommit, ".github/workflows/ci.yaml"); !errors.Is(err, ErrNoAppKey) {
		t.Fatalf("File on a nil client: %v, want ErrNoAppKey", err)
	}
}
