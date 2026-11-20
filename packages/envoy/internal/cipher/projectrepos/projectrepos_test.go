package projectrepos_test

import (
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/projectrepos"
	"github.com/sjawhar/envoy/internal/cipher/projectrepos/projectrepostest"
)

// standIn serves PRIV, whose issues resolve to acme/pub and acme/two, and EMPTY, whose issues
// resolve to none.
func standIn(t *testing.T) *projectrepostest.Server {
	t.Helper()
	return projectrepostest.NewServer(t, map[string][]string{"PRIV": {"acme/pub", "acme/two"}, "EMPTY": nil})
}

func clientOf(server *projectrepostest.Server) *projectrepos.Client {
	return projectrepos.New(server.URL, server.Account, nil)
}

// TestProjectRepositoriesClientReadsDispatchsList: the list Dispatch answers for the project, read
// with the broker's own bearer, and an empty list, never nil, for a project none of whose issues
// resolves to a repository.
func TestProjectRepositoriesClientReadsDispatchsList(t *testing.T) {
	client := clientOf(standIn(t))
	got, err := client.Repositories(t.Context(), "PRIV")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"acme/pub", "acme/two"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Repositories(PRIV) = %q, want %q", got, want)
	}
	got, err = client.Repositories(t.Context(), "EMPTY")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("Repositories(EMPTY) = %#v, want an empty list that is not nil", got)
	}
}

// TestProjectRepositoriesClientAsksDispatchEveryTime: no answer is kept, so a list Dispatch changed
// between two reads is the second read's answer, and each read is one call.
func TestProjectRepositoriesClientAsksDispatchEveryTime(t *testing.T) {
	server := standIn(t)
	client := clientOf(server)
	if _, err := client.Repositories(t.Context(), "PRIV"); err != nil {
		t.Fatal(err)
	}
	server.SetRepositories("PRIV", []string{"acme/pub"})
	got, err := client.Repositories(t.Context(), "PRIV")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"acme/pub"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Repositories(PRIV) after Dispatch's list changed = %q, want %q: an answer is never kept", got, want)
	}
	if calls := server.Calls(); calls != 2 {
		t.Fatalf("two reads asked Dispatch %d times, want 2", calls)
	}
}

// TestProjectRepositoriesClientRefusesWhatIsNoWholeAnswer: every answer but a 200 whose body is
// the asked project's whole list is an error, and no list: Dispatch's 503, every other status (a
// project it does not hold, a Dispatch that serves no such route, one refusing the broker's
// token), a body that is no JSON, one about another project, one with no list or a list that is
// no list of owner/name strings, and one past the bound.
func TestProjectRepositoriesClientRefusesWhatIsNoWholeAnswer(t *testing.T) {
	for _, row := range []struct {
		name   string
		status int
		body   string
	}{
		{"Dispatch's 503", http.StatusServiceUnavailable, `{"code":"REPOSITORIES_UNRESOLVED","error":"too deep"}`},
		{"a project Dispatch does not hold", http.StatusNotFound, `{"code":"PROJECT_NOT_FOUND","error":"no such project"}`},
		{"a Dispatch that serves no such route", http.StatusNotFound, "404 page not found\n"},
		{"Dispatch refusing the broker's token", http.StatusForbidden, `{"code":"FORBIDDEN","error":"not an access account"}`},
		{"a body that is no JSON", http.StatusOK, `{"project":"PRIV","repositories":["acme/pub"`},
		{"a body that is no object", http.StatusOK, `["acme/pub"]`},
		{"another project's answer", http.StatusOK, `{"project":"OTHER","repositories":["acme/pub"]}`},
		{"no project", http.StatusOK, `{"repositories":["acme/pub"]}`},
		{"no list", http.StatusOK, `{"project":"PRIV"}`},
		{"a null list", http.StatusOK, `{"project":"PRIV","repositories":null}`},
		{"a list of numbers", http.StatusOK, `{"project":"PRIV","repositories":[1]}`},
		{"a repository with no owner", http.StatusOK, `{"project":"PRIV","repositories":["pub"]}`},
		{"a repository with two slashes", http.StatusOK, `{"project":"PRIV","repositories":["acme/pub/x"]}`},
		{"a repository with an empty name", http.StatusOK, `{"project":"PRIV","repositories":["acme/"]}`},
		{"a repository with a space", http.StatusOK, `{"project":"PRIV","repositories":["acme/p ub"]}`},
		{"a second value after the answer", http.StatusOK, `{"project":"PRIV","repositories":[]} {}`},
		{"a body past the bound", http.StatusOK, `{"project":"PRIV","repositories":["` + strings.Repeat("a", projectrepos.MaxAnswerBytes) + `/b"]}`},
	} {
		t.Run(row.name, func(t *testing.T) {
			server := standIn(t)
			server.SetAnswer(row.status, row.body)
			got, err := clientOf(server).Repositories(t.Context(), "PRIV")
			if err == nil || got != nil {
				t.Fatalf("Repositories(PRIV) answered %d %q = %q, %v; want an error and no list", row.status, row.body, got, err)
			}
			if strings.Contains(err.Error(), server.Token) {
				t.Fatalf("the error %q names the broker's bearer", err)
			}
		})
	}
}

// TestProjectRepositoriesClientGivesUpAfterTwoSeconds: a Dispatch that stalls is given 2 s, and
// then the read is an error, never a list.
func TestProjectRepositoriesClientGivesUpAfterTwoSeconds(t *testing.T) {
	if projectrepos.Timeout != 2*time.Second {
		t.Fatalf("Timeout = %s, want 2s", projectrepos.Timeout)
	}
	server := standIn(t)
	server.SetStall(3 * time.Second)
	start := time.Now()
	got, err := clientOf(server).Repositories(t.Context(), "PRIV")
	elapsed := time.Since(start)
	if err == nil || got != nil {
		t.Fatalf("Repositories(PRIV) from a Dispatch stalling 3 s = %q, %v; want an error and no list", got, err)
	}
	if elapsed < projectrepos.Timeout || elapsed > projectrepos.Timeout+time.Second {
		t.Fatalf("the read gave up after %s, want about %s", elapsed, projectrepos.Timeout)
	}
}

// TestProjectRepositoriesClientAsksNothingWithoutItsToken: an account token the broker cannot read
// is an error before Dispatch is asked.
func TestProjectRepositoriesClientAsksNothingWithoutItsToken(t *testing.T) {
	server := standIn(t)
	broken := errors.New("open /var/run/secrets/dispatch-access/token: no such file or directory")
	client := projectrepos.New(server.URL, failingAccount{broken}, nil)
	got, err := client.Repositories(t.Context(), "PRIV")
	if !errors.Is(err, broken) || got != nil {
		t.Fatalf("Repositories with an unreadable token = %q, %v; want an error wrapping the read's", got, err)
	}
	if calls := server.Calls(); calls != 0 {
		t.Fatalf("Dispatch was asked %d times, want none", calls)
	}
}

type failingAccount struct{ err error }

func (a failingAccount) Read() (string, error) { return "", a.err }
