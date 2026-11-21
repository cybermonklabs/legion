package redact

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/sjawhar/envoy/testport"
)

// failureSecret stands for a credential a person put where a setting expected a URL, a host or a
// path. No output of Class or File may hold it.
const failureSecret = "hunter2pwFAILURE"

// TestClassPrintsTheFailuresKindAlone: every error a request to a server other than the broker
// can end in prints as its class, in this package's fixed text, whatever its own text repeats of
// the URL or the answer - the whole URL a parse error quotes, the user name and query net/http
// leaves in a *url.Error, the body x/oauth2 keeps - and still unwraps to the error itself.
func TestClassPrintsTheFailuresKindAlone(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, parseErr := url.Parse("https://user:" + failureSecret + "@host.example/%zz")
	// A port the test holds and nothing listens on, so the dial is refused and no other process can
	// take the port and answer (testport.Refused).
	closed := testport.Refused(t)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+failureSecret+"@"+closed+"/?token="+failureSecret, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, refusedErr := http.DefaultClient.Do(req)
	answer := &oauth2.RetrieveError{
		Response: &http.Response{StatusCode: http.StatusBadRequest},
		Body:     []byte("echo of " + failureSecret), ErrorCode: "invalid_grant", ErrorDescription: failureSecret,
	}
	for _, c := range []struct {
		name string
		err  error
		want string
	}{
		{"a value that does not parse", parseErr, "not a valid URL"},
		{"a refused connection, through net/http", refusedErr, "connection refused"},
		{"an OAuth answer with a registered code", answer, "the server answered HTTP 400 invalid_grant"},
		{"an OAuth answer with a code of its own", &oauth2.RetrieveError{
			Response: &http.Response{StatusCode: http.StatusUnauthorized}, ErrorCode: failureSecret,
		}, "the server answered HTTP 401"},
		{"a deadline", fmt.Errorf("call %s: %w", failureSecret, context.DeadlineExceeded), "timed out"},
		{"anything else", errors.New("refused " + failureSecret), "the request failed"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.err == nil {
				t.Fatal("the probe made no error")
			}
			got := Class(c.err)
			if got.Error() != c.want {
				t.Errorf("Class = %q, want %q", got, c.want)
			}
			if !errors.Is(got, c.err) {
				t.Errorf("Class(%v) does not unwrap to the error it classifies", c.err)
			}
		})
	}
	if Class(nil) != nil {
		t.Error("Class(nil) is not nil")
	}
}

// TestAnsweredNamesTheStatusAndARegisteredCodeAlone: a server's refusal prints its status and,
// where the body names one, its error code - an OAuth `error` or Google's `error.status` - only
// when that code is one of the registered ones this package spells itself. No other text of the
// body prints: not a description, not a message, not a code of the server's own, which can be
// anything, an echo of the request included.
func TestAnsweredNamesTheStatusAndARegisteredCodeAlone(t *testing.T) {
	for _, c := range []struct {
		name, body, want string
	}{
		{"an OAuth refusal", `{"error":"invalid_grant","error_description":"` + failureSecret + `"}`, "the server answered HTTP 400 invalid_grant"},
		{"a Google API refusal", `{"error":{"code":400,"message":"` + failureSecret + `","status":"PERMISSION_DENIED"}}`, "the server answered HTTP 400 PERMISSION_DENIED"},
		{"a code of the server's own", `{"error":"` + failureSecret + `"}`, "the server answered HTTP 400"},
		{"a body that is not JSON", "echo of " + failureSecret, "the server answered HTTP 400"},
		{"no body", "", "the server answered HTTP 400"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := Answered(http.StatusBadRequest, []byte(c.body)); got != c.want {
				t.Errorf("Answered = %q, want %q", got, c.want)
			}
		})
	}
}

// TestFilePrintsTheOperationAndTheSystemsReasonAlone: a file call's error names its operation and
// the system's reason for it, never the path it was handed - which, for a setting that names a
// secret's file, is where a person who pasted the secret itself put it - and still unwraps to the
// error itself, so errors.Is(err, fs.ErrNotExist) holds. Any other error is its class.
func TestFilePrintsTheOperationAndTheSystemsReasonAlone(t *testing.T) {
	_, missing := os.ReadFile(filepath.Join(t.TempDir(), failureSecret))
	_, isDir := os.ReadFile(t.TempDir())
	renamed := os.Rename(filepath.Join(t.TempDir(), failureSecret), filepath.Join(t.TempDir(), "x", failureSecret))
	for _, c := range []struct {
		name string
		err  error
		want string
	}{
		{"a missing file", missing, "open: no such file or directory"},
		{"a directory", isDir, "read: is a directory"},
		{"a rename", renamed, "rename: no such file or directory"},
		{"a path error holding no errno", &fs.PathError{Op: "open", Path: failureSecret, Err: errors.New(failureSecret)}, "open failed"},
		{"no file error at all", errors.New("open " + failureSecret), "the call failed"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.err == nil {
				t.Fatal("the probe made no error")
			}
			got := File(c.err)
			if got.Error() != c.want {
				t.Errorf("File = %q, want %q", got, c.want)
			}
			if strings.Contains(got.Error(), failureSecret) {
				t.Errorf("File = %q, which holds the path", got)
			}
			if !errors.Is(got, c.err) {
				t.Errorf("File(%v) does not unwrap to the error it names", c.err)
			}
		})
	}
	if !errors.Is(File(missing), fs.ErrNotExist) || !errors.Is(File(missing), syscall.ENOENT) {
		t.Error("File's error of a missing file is not fs.ErrNotExist")
	}
	if File(nil) != nil {
		t.Error("File(nil) is not nil")
	}
}

// TestMaskedPrintsItsTextAndUnwrapsToTheError: a failure a caller has put in words of its own - a
// status the transport recorded, fixed text - prints those words alone and none of the error's
// text, and still unwraps to the error, so errors.Is and errors.As classify it as before.
func TestMaskedPrintsItsTextAndUnwrapsToTheError(t *testing.T) {
	cause := &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
	err := fmt.Errorf("get https://%s@issuer.example: %w", failureSecret, cause)
	got := Masked(err, "the server answered HTTP 404")
	if got.Error() != "the server answered HTTP 404" {
		t.Errorf("Masked = %q, want the text it was given alone", got)
	}
	if errors.Unwrap(got) != err {
		t.Errorf("Masked unwraps to %v, want the error it masks", errors.Unwrap(got))
	}
	var op *net.OpError
	if !errors.Is(got, syscall.ECONNREFUSED) || !errors.As(got, &op) || op != cause {
		t.Error("errors.Is and errors.As do not reach through Masked to the cause")
	}
	if Masked(nil, "the server answered HTTP 404") != nil {
		t.Error("Masked(nil, text) is not nil")
	}
}
