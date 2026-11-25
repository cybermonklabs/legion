package projectrepos_test

import (
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/dispatchread"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess/projectaccesstest"
	"github.com/sjawhar/envoy/internal/cipher/projectrepos"
)

// standIn serves PRIV, whose issues resolve to acme/pub and acme/two, and EMPTY, whose issues
// resolve to none. Each also has the membership answer's own list, which the repository answer
// must not carry: a client that read it would answer acme/member.
func standIn(t *testing.T) *projectaccesstest.Server {
	t.Helper()
	return projectaccesstest.NewServer(t, map[string]projectaccesstest.Row{
		"PRIV":  {Visibility: "private", Repositories: []string{"acme/member"}, Resolved: []string{"acme/pub", "acme/two"}},
		"EMPTY": {Visibility: "private", Repositories: []string{"acme/member"}},
	}, nil)
}

func clientOf(server *projectaccesstest.Server) *projectrepos.Client {
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
// no list of owner/name strings, and one past the bound. The body is read strictly, as Dispatch
// writes it: its two keys in their own case, each once, no other key, and the list sorted
// byte-wise with each repository once, so no body is read as an answer it does not spell.
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
		{"the project written twice", http.StatusOK, `{"project":"OTHER","project":"PRIV","repositories":["acme/pub"]}`},
		{"the list written twice", http.StatusOK, `{"project":"PRIV","repositories":[],"repositories":["acme/pub"]}`},
		{"the project's key in another case", http.StatusOK, `{"PROJECT":"PRIV","repositories":["acme/pub"]}`},
		{"the list's key in another case", http.StatusOK, `{"project":"PRIV","Repositories":["acme/pub"]}`},
		{"a key Dispatch does not write", http.StatusOK, `{"project":"PRIV","repositories":["acme/pub"],"owner":"alice"}`},
		{"a list out of order", http.StatusOK, `{"project":"PRIV","repositories":["acme/two","acme/pub"]}`},
		{"a repository named twice", http.StatusOK, `{"project":"PRIV","repositories":["acme/pub","acme/pub"]}`},
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
// is dispatchread.ErrBrokerRefused naming BROKER_PROJECT_ACCESS_TOKEN_FILE, as it is to the
// membership client, and Dispatch is asked nothing.
func TestProjectRepositoriesClientAsksNothingWithoutItsToken(t *testing.T) {
	server := standIn(t)
	broken := errors.New("open /var/run/secrets/dispatch-access/token: no such file or directory")
	client := projectrepos.New(server.URL, failingAccount{broken}, nil)
	got, err := client.Repositories(t.Context(), "PRIV")
	if !errors.Is(err, broken) || !errors.Is(err, dispatchread.ErrBrokerRefused) || !strings.Contains(err.Error(), "BROKER_PROJECT_ACCESS_TOKEN_FILE") || got != nil {
		t.Fatalf("Repositories with an unreadable token = %q, %v; want ErrBrokerRefused naming BROKER_PROJECT_ACCESS_TOKEN_FILE and wrapping the read's", got, err)
	}
	if calls := server.Calls(); calls != 0 {
		t.Fatalf("Dispatch was asked %d times, want none", calls)
	}
}

// TestProjectRepositoriesClientCallsARefusedTokenTheBrokersOwn: Dispatch answering 401 or 403 to
// the broker's own account token is dispatchread.ErrBrokerRefused naming the code, as it is to the
// membership client: the broker's own credential is broken, a misconfiguration and not Dispatch
// out of reach. A 503 is not.
func TestProjectRepositoriesClientCallsARefusedTokenTheBrokersOwn(t *testing.T) {
	server := standIn(t)
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		server.SetAnswer(status, `{"code":"ACCESS_ACCOUNT_REQUIRED","error":"not an access account"}`)
		got, err := clientOf(server).Repositories(t.Context(), "PRIV")
		if !errors.Is(err, dispatchread.ErrBrokerRefused) || !strings.Contains(err.Error(), "ACCESS_ACCOUNT_REQUIRED") || got != nil {
			t.Fatalf("Repositories answered %d = %q, %v; want ErrBrokerRefused naming the code, and no list", status, got, err)
		}
	}
	server.SetAnswer(http.StatusServiceUnavailable, `{"code":"REPOSITORIES_UNRESOLVED","error":"too deep"}`)
	if _, err := clientOf(server).Repositories(t.Context(), "PRIV"); err == nil || errors.Is(err, dispatchread.ErrBrokerRefused) {
		t.Fatalf("Repositories answered 503 = %v; want an error that is not ErrBrokerRefused", err)
	}
}

// TestTheRepositoryAnswerIsTheIssuesListNotTheMembershipList: one Dispatch, read by both of the
// broker's clients with one account as cmd/cipherd wires them, keeps two lists of a project's
// repositories apart. The repository client answers the list the project's issues resolve to, and
// never the membership answer's own list, which the membership client is answered on the same
// Dispatch; a project none of whose issues resolves to a repository is [], whatever that list
// holds.
func TestTheRepositoryAnswerIsTheIssuesListNotTheMembershipList(t *testing.T) {
	server := standIn(t)
	for project, want := range map[string][]string{"PRIV": {"acme/pub", "acme/two"}, "EMPTY": {}} {
		got, err := clientOf(server).Repositories(t.Context(), project)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("Repositories(%s) = %q, %v; want the issues' list %q, not the membership answer's own list", project, got, err, want)
		}
		answer, err := projectaccess.New(server.URL, server.Account, nil).Access(t.Context(), project, "")
		if err != nil || !reflect.DeepEqual(answer.Repositories, []string{"acme/member"}) {
			t.Fatalf("Access(%s) from the same Dispatch with the same account = %q, %v; want the membership answer's own list acme/member", project, answer.Repositories, err)
		}
	}
}

// TestARefusedAnswerIsQuotedAtMostSixtyFourBytes: a key or a value of Dispatch's answer that an
// error quotes is cut to its first 64 bytes, so a key as long as the answer is never logged whole.
func TestARefusedAnswerIsQuotedAtMostSixtyFourBytes(t *testing.T) {
	long := strings.Repeat("k", 1000)
	for name, body := range map[string]string{
		"a long key":               `{"project":"PRIV","repositories":[],"` + long + `":1}`,
		"a long project":           `{"project":"` + long + `","repositories":[]}`,
		"a long repository":        `{"project":"PRIV","repositories":["` + long + `"]}`,
		"a long repository, twice": `{"project":"PRIV","repositories":["a/` + long + `","a/` + long + `"]}`,
	} {
		t.Run(name, func(t *testing.T) {
			server := standIn(t)
			server.SetAnswer(http.StatusOK, body)
			_, err := clientOf(server).Repositories(t.Context(), "PRIV")
			if err == nil || !strings.Contains(err.Error(), strings.Repeat("k", 60)) || strings.Contains(err.Error(), strings.Repeat("k", 65)) {
				t.Fatalf("the refusal = %v; want it to quote the first 64 bytes and no more", err)
			}
		})
	}
}

type failingAccount struct{ err error }

func (a failingAccount) Read() (string, error) { return "", a.err }
