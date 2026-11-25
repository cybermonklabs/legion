package githubapp

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/architecture/notes"
	"github.com/sjawhar/envoy/internal/dispatch/githubreads"
	"github.com/sjawhar/envoy/internal/githubtest"
	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/answertest"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// The rows of text GitHub answered that this package carries into its errors and its notes: a
// rate limit's header, a next page link, a head SHA a caller had from another answer, a tree's
// entry and a repository's default branch. Each test calls only what the tree before this change
// has too, so the check's control (19) runs it there, where it fails on its captured output.

// credentialUser is the probe's user information, and credentialQuery a query value that carries
// its token, written in two parts so no line of this file reads as a credential to a scanner.
const (
	credentialUser  = redacttest.User + ":" + redacttest.Password
	credentialQuery = "access" + "_token=" + redacttest.Token
)

// credentialShaped is a value an answer could give where a SHA belongs, shaped as a URL's user
// information and host: url.PathEscape keeps its ':' and '@', so a target that holds it holds the
// probe's user and password as written.
const credentialShaped = credentialUser + "@issuer.example"

// TestARateLimitsHeaderPrintsAsTheRuleReadsIt: GitHub answers a query at 200 with a Retry-After
// that quotes a URL carrying a credential, and with nothing in its envelope that names a limit,
// so the limit is worded by its headers (graphQLLimitWording). The limit's text and the client's
// note of it name the header's value as the rule reads it. Captures githubapp.limit-header.
func TestARateLimitsHeaderPrintsAsTheRuleReadsIt(t *testing.T) {
	header := http.Header{"Retry-After": {redacttest.Words}}
	limited, _ := GraphQLRateLimit(http.StatusOK, header, answertest.Of(http.StatusOK, `{"data":{"viewer":{"login":"ada"}}}`))
	if limited == nil {
		t.Fatal("GraphQLRateLimit read no limit in a 200 that names a Retry-After")
	}
	text := limited.Error() + " | " + limitNoted(http.MethodPost, "https://api.github.com/graphql", limited).Error()
	redacttest.RequireNone(t, "githubapp.limit-header", text)
	if want := "Retry-After: " + redacttest.Shown; !strings.Contains(text, want) {
		t.Errorf("the limit reads %q, want %q in it", text, want)
	}
}

// TestARateLimitsHeaderPrintsNoTokenItsRequestCarried: whatever answers in GitHub's place answers a
// read at 200 with a Retry-After that echoes the installation token the read carried, and with
// nothing in its envelope that names a limit, so GraphQLRateLimit, as Client.GraphQL calls it,
// words the limit by its headers. The limit's text and the client's note of it hold no piece of the
// token: the header's value is held with the record of the request the answer came to. Captures
// githubapp.limit-header-echo.
func TestARateLimitsHeaderPrintsNoTokenItsRequestCarried(t *testing.T) {
	client := branchServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		fmt.Fprint(w, `{"data":{"viewer":{"login":"ada"}}}`)
	})
	body, status, header, err := client.Read(context.Background(), redacttest.Token, "/graphql")
	if err != nil || status != http.StatusOK {
		t.Fatalf("Read answered %d, %v; want the stand-in's 200", status, err)
	}
	limited, _ := GraphQLRateLimit(status, header, body)
	if limited == nil {
		t.Fatal("GraphQLRateLimit read no limit in a 200 that names a Retry-After")
	}
	text := limited.Error() + " | " + limitNoted(http.MethodPost, client.base+"/graphql", limited).Error()
	redacttest.RequireNoPiece(t, "githubapp.limit-header-echo", text, redacttest.Token)
	if !strings.Contains(text, "Retry-After: ") {
		t.Errorf("the limit reads %q, want it worded by its Retry-After", text)
	}
}

// deliveriesServer is a client of a GitHub whose first page of webhook deliveries names link(host),
// host its own, as the next page, and answers every other request with next; hits counts the
// requests GitHub was sent.
func deliveriesServer(t *testing.T, link func(host string) string, next http.HandlerFunc) (*Client, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	client := branchServer(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.RequestURI() == "/app/hook/deliveries?per_page=100" {
			w.Header().Set("Link", "<"+link(r.Host)+`>; rel="next"`)
			fmt.Fprint(w, "[]")
			return
		}
		next(w, r)
	})
	return client, &hits
}

// serverError answers GitHub's 500.
func serverError(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusInternalServerError)
	fmt.Fprint(w, `{"message":"Server Error"}`)
}

// TestANextLinkPrintsNoTokenItsRequestCarried: whatever answers in GitHub's place names a next page
// whose path echoes the App's JWT the listing's request carried, on the API origin, and answers
// that page 500. The refusal-free link is followed, and the error Deliveries answers and the note
// send makes on the attempt name the next page's target with no piece of the JWT: the link, a
// header's value, is held with the record of the request its answer came to. Captures
// githubapp.next-link-echo.
func TestANextLinkPrintsNoTokenItsRequestCarried(t *testing.T) {
	var jwt atomic.Value
	client := branchServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RequestURI() == "/app/hook/deliveries?per_page=100" {
			token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			jwt.Store(token)
			w.Header().Set("Link", "<http://"+r.Host+"/app/hook/deliveries/"+token+`?per_page=100>; rel="next"`)
			fmt.Fprint(w, "[]")
			return
		}
		serverError(w, r)
	})
	ctx, attempt := githubreads.Begin(context.Background())
	_, err := client.Deliveries(ctx, time.Time{}, nil)
	if err == nil {
		t.Fatal("a next page GitHub failed answered no error")
	}
	_, _, cause := attempt.Outcome()
	token, _ := jwt.Load().(string)
	if token == "" {
		t.Fatal("the listing's first request carried no JWT")
	}
	redacttest.RequireNoPiece(t, "githubapp.next-link-echo", err.Error()+" | "+cause, token)
	if !strings.Contains(err.Error(), "status 500") || cause == "" {
		t.Errorf("Deliveries answered %q and noted %q, want the next page's 500 named and noted", err, cause)
	}
}

// TestACredentialedNextLinkIsRefusedAsTheRuleReadsIt: the deliveries listing's Link header names a
// next page that carries a credential, and nextLink refuses it: one longer than nextLinkMaxBytes,
// one url.Parse cannot parse, one on another origin, and one on the API origin that carries user
// information, which would otherwise be followed. Each refusal names the link as the rule reads
// it, and the last sends no second request. Captures githubapp.next-link/<row>.
func TestACredentialedNextLinkIsRefusedAsTheRuleReadsIt(t *testing.T) {
	for _, row := range []struct {
		name, refusal string
		link          func(host string) string
	}{{
		name:    "longer-than-the-bound",
		refusal: "is longer than",
		link: func(host string) string {
			return "https://" + credentialUser + "@" + host + "/app/hook/deliveries?" + credentialQuery + "&cursor=" + strings.Repeat("v", nextLinkMaxBytes)
		},
	}, {
		name:    "not-a-url",
		refusal: "parse next page link",
		link: func(host string) string {
			return "https://" + credentialUser + "@" + host + "/app/hook/deliveries%zz?" + credentialQuery
		},
	}, {
		name:    "another-origin",
		refusal: "leaves the API origin",
		link: func(string) string {
			return "https://collector.example/app/hook/deliveries?" + credentialQuery
		},
	}, {
		name:    "user-information-on-the-origin",
		refusal: "carries user information",
		link: func(host string) string {
			return "http://" + credentialUser + "@" + host + "/app/hook/deliveries?page=2"
		},
	}} {
		t.Run(row.name, func(t *testing.T) {
			client, hits := deliveriesServer(t, row.link, serverError)
			_, err := client.Deliveries(context.Background(), time.Time{}, nil)
			if err == nil {
				t.Fatal("Deliveries followed the link to a page and answered no error")
			}
			redacttest.RequireNone(t, "githubapp.next-link/"+row.name, err.Error())
			if !strings.Contains(err.Error(), row.refusal) {
				t.Errorf("Deliveries answered %q, want nextLink's refusal %q", err, row.refusal)
			}
			if n := hits.Load(); n != 1 {
				t.Errorf("GitHub was sent %d requests, want the first page's alone", n)
			}
		})
	}
}

// TestANextPagesTargetPrintsItsQueryAsTheRuleReadsIt: the deliveries listing's next page is on the
// API origin, and a value of its query carries a credential, which nextLink leaves to the rule:
// the next request's target holds text GitHub answered. Where that request fails - a status other
// than 200, an answer past responseLimit, an answer cut short - the error Deliveries answers, and
// the note send makes on the attempt (the cause GET /healthz serves), name the target as the rule
// reads it. Captures githubapp.next-target/<row>.
func TestANextPagesTargetPrintsItsQueryAsTheRuleReadsIt(t *testing.T) {
	link := func(host string) string {
		return "http://" + host + "/app/hook/deliveries?" + credentialQuery + "&per_page=100"
	}
	for _, row := range []struct {
		name, failure string
		next          http.HandlerFunc
	}{{
		name:    "status-500",
		failure: "status 500",
		next:    serverError,
	}, {
		name:    "past-the-bound",
		failure: "exceeds the",
		next: func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, strings.Repeat("x", responseLimit+1))
		},
	}, {
		name:    "cut-short",
		failure: "response:",
		next: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", "64")
			fmt.Fprint(w, `[{"id":1`)
		},
	}} {
		t.Run(row.name, func(t *testing.T) {
			client, _ := deliveriesServer(t, link, row.next)
			ctx, attempt := githubreads.Begin(context.Background())
			_, err := client.Deliveries(ctx, time.Time{}, nil)
			if err == nil {
				t.Fatal("a next page GitHub failed answered no error")
			}
			_, _, cause := attempt.Outcome()
			redacttest.RequireNone(t, "githubapp.next-target/"+row.name, err.Error()+" | "+cause)
			if !strings.Contains(err.Error(), row.failure) || cause == "" {
				t.Errorf("Deliveries answered %q and noted %q, want the failure %q named and noted", err, cause, row.failure)
			}
		})
	}
}

// TestAHeadThatIsNotASHAIsRefusedBeforeAnyRequest: a head SHA a caller had from another of
// GitHub's answers (cireconcile's head OID) holds a user and a password before an @, which
// url.PathEscape keeps. Each read that takes a SHA refuses it before it builds a target: GitHub is
// sent nothing, and neither the read's error nor the cause the attempt keeps once the caller notes
// that error (cireconcile's headFailed) holds the value. Captures githubapp.head-sha/<read>.
func TestAHeadThatIsNotASHAIsRefusedBeforeAnyRequest(t *testing.T) {
	for _, read := range []struct {
		name string
		call func(context.Context, *Client) error
	}{{
		name: "check-runs",
		call: func(ctx context.Context, client *Client) error {
			_, err := client.CommitCheckRuns(ctx, "ghs_test", "acme", "web", credentialShaped, 1, "")
			return err
		},
	}, {
		name: "check-suites",
		call: func(ctx context.Context, client *Client) error {
			_, err := client.CommitCheckSuites(ctx, "ghs_test", "acme", "web", credentialShaped, 1, "")
			return err
		},
	}, {
		name: "statuses",
		call: func(ctx context.Context, client *Client) error {
			_, err := client.CommitStatuses(ctx, "ghs_test", "acme", "web", credentialShaped, 1, "")
			return err
		},
	}, {
		name: "has-commit",
		call: func(ctx context.Context, client *Client) error {
			_, err := client.HasCommit(ctx, "ghs_test", "acme", "web", credentialShaped)
			return err
		},
	}, {
		name: "commit-on-branch",
		call: func(ctx context.Context, client *Client) error {
			// The branch comes from a client with no App key, which answers its refusal
			// before any request: the read this row is about refuses the sha before it
			// builds a target, so the branch it is given says nothing. This file is read
			// at this change's base as well (the inventory's leak entries copy it there),
			// where DefaultBranch answers the name alone, so the branch is taken from that
			// read rather than written out here.
			branch, _ := (*Client)(nil).DefaultBranch(ctx, "ghs_test", "acme", "web")
			_, err := client.CommitOnBranch(ctx, "ghs_test", "acme", "web", branch, credentialShaped)
			return err
		},
	}} {
		t.Run(read.name, func(t *testing.T) {
			var hits atomic.Int32
			client := branchServer(t, func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				serverError(w, r)
			})
			ctx, attempt := githubreads.Begin(context.Background())
			err := read.call(ctx, client)
			if err == nil {
				t.Fatal("the read answered no error")
			}
			githubreads.NoteFailure(ctx, err)
			_, _, cause := attempt.Outcome()
			redacttest.RequireNone(t, "githubapp.head-sha/"+read.name, err.Error()+" | "+cause)
			if n := hits.Load(); n != 0 {
				t.Errorf("GitHub was sent %d requests, want none: the read refuses a value that is not a SHA before it builds a target", n)
			}
		})
	}
}

// TestATreeEntryPrintsAsTheRuleReadsIt: an entry of GitHub's tree answer (Dir) names a blob that
// is not a SHA, or a path that holds a user and a password before an @. DirFiles refuses the blob
// before it builds the blob's target, so GitHub is sent nothing, and names the path as the rule
// reads it: in notes.CheckSize's refusal of a listed size past its bound, and in the error of a
// blob read GitHub failed. Captures githubapp.tree-entry/<row>.
func TestATreeEntryPrintsAsTheRuleReadsIt(t *testing.T) {
	path := "docs/" + credentialShaped + ".md"
	for _, row := range []struct {
		name, want string
		file       DirFile
		sent       int32
	}{{
		name: "blob-not-a-sha",
		want: "(the blob is not a SHA)",
		file: DirFile{Name: "a.md", Path: "docs/a.md", SHA: credentialShaped, Size: 1},
	}, {
		name: "path-past-the-bound",
		want: redact.WordsNotReadable,
		file: DirFile{Name: "a.md", Path: path, SHA: mainCommit, Size: notes.MaxFileSize + 1},
	}, {
		name: "path-of-a-failed-read",
		want: redact.WordsNotReadable,
		file: DirFile{Name: "a.md", Path: path, SHA: mainCommit, Size: 1},
		sent: 1,
	}} {
		t.Run(row.name, func(t *testing.T) {
			var hits atomic.Int32
			client := branchServer(t, func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				serverError(w, r)
			})
			_, err := client.DirFiles(context.Background(), "ghs_test", "acme", "web", Dir{SHA: "t1", Files: []DirFile{row.file}})
			if err == nil {
				t.Fatal("DirFiles answered no error")
			}
			redacttest.RequireNone(t, "githubapp.tree-entry/"+row.name, err.Error())
			if !strings.Contains(err.Error(), row.want) {
				t.Errorf("DirFiles answered %q, want %q in it", err, row.want)
			}
			if n := hits.Load(); n != row.sent {
				t.Errorf("GitHub was sent %d requests, want %d", n, row.sent)
			}
		})
	}
}

// TestADefaultBranchGitHubAnsweredPrintsAsTheRuleReadsIt: GitHub answers a repository's
// default_branch with a URL that carries a credential, which DefaultBranch hands its caller as it
// came, and the comparison against that branch then fails: GitHub answers it 500, past
// responseLimit, or with a status that does not decode. Neither the error CommitOnBranch answers
// (CheckRepositoryResponse's, send's or its decode's) nor the note send makes on the attempt holds
// the credential: the comparison's target names its branch as the rule reads it. Captures
// githubapp.default-branch/<row>.
func TestADefaultBranchGitHubAnsweredPrintsAsTheRuleReadsIt(t *testing.T) {
	for _, row := range []struct {
		name    string
		compare http.HandlerFunc
	}{{
		name:    "status-500",
		compare: serverError,
	}, {
		name: "past-the-bound",
		compare: func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, strings.Repeat("x", responseLimit+1))
		},
	}, {
		name: "not-decoded",
		compare: func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, `{"status":7}`)
		},
	}} {
		t.Run(row.name, func(t *testing.T) {
			client := branchServer(t, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/repos/acme/web":
					fmt.Fprintf(w, `{"default_branch":%q}`, redacttest.Code)
				case strings.HasPrefix(r.URL.Path, "/repos/acme/web/compare/"):
					row.compare(w, r)
				default:
					githubtest.Unexpected(t, w, r)
				}
			})
			ctx, attempt := githubreads.Begin(context.Background())
			branch, err := client.DefaultBranch(ctx, "ghs_test", "acme", "web")
			if err != nil || !strings.Contains(fmt.Sprint(branch), redacttest.Code) {
				t.Fatalf("DefaultBranch = %v, %v; want GitHub's default_branch as it came", branch, err)
			}
			_, err = client.CommitOnBranch(ctx, "ghs_test", "acme", "web", branch, mainCommit)
			if err == nil {
				t.Fatal("a comparison GitHub failed answered no error")
			}
			githubreads.NoteFailure(ctx, err)
			_, _, cause := attempt.Outcome()
			redacttest.RequireNone(t, "githubapp.default-branch/"+row.name, err.Error()+" | "+cause)
		})
	}
}

// echoToken is the installation token a request to a stand-in GitHub carried, which the stand-in
// echoes in its answer.
func echoToken(r *http.Request) string {
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
}

// TestATreePathPrintsNoTokenItsReadCarried: whatever answers in GitHub's place lists a note whose
// path echoes the installation token the tree read (Dir) carried. DirFiles names that path, as
// the record of the tree's answer holds it, with no piece of the token: in notes.CheckSize's
// refusal of a listed size past its bound, and in the error of a blob read GitHub failed. Captures
// githubapp.tree-path-echo/<row>.
func TestATreePathPrintsNoTokenItsReadCarried(t *testing.T) {
	const token = redacttest.Token
	for _, row := range []struct {
		name string
		size int64
	}{
		{"path-past-the-bound", notes.MaxFileSize + 1},
		{"path-of-a-failed-read", 1},
	} {
		t.Run(row.name, func(t *testing.T) {
			client := branchServer(t, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasPrefix(r.URL.Path, "/repos/acme/web/git/trees/"):
					fmt.Fprintf(w, `{"sha":"t1","truncated":false,"tree":[{"path":%q,"mode":"100644","type":"blob","sha":%q,"size":%d}]}`,
						"note-"+echoToken(r)+".md", mainCommit, row.size)
				case strings.HasPrefix(r.URL.Path, "/repos/acme/web/git/blobs/"):
					serverError(w, r)
				default:
					githubtest.Unexpected(t, w, r)
				}
			})
			ctx := context.Background()
			listing, err := client.Dir(ctx, token, "acme", "web", mainCommit, "docs")
			if err != nil || len(listing.Files) != 1 || listing.Files[0].Path != "docs/note-"+token+".md" {
				t.Fatalf("Dir = %+v, %v; want the one note the stand-in listed, its path as it came", listing, err)
			}
			_, err = client.DirFiles(ctx, token, "acme", "web", listing)
			if err == nil {
				t.Fatal("DirFiles answered no error")
			}
			redacttest.RequireNoPiece(t, "githubapp.tree-path-echo/"+row.name, err.Error(), token)
			if !strings.Contains(err.Error(), "docs/note-") {
				t.Errorf("DirFiles answered %q, want the note's path named", err)
			}
		})
	}
}

// TestADefaultBranchPrintsNoTokenItsReadCarried: whatever answers in GitHub's place names a
// default_branch that echoes the installation token the read carried, and the comparison against
// that branch, made under the same token, then fails: GitHub answers it 500, past responseLimit,
// or with a status that does not decode. Neither the error CommitOnBranch answers nor the note send
// makes on the attempt holds a piece of the token. Captures githubapp.default-branch-echo/<row>.
func TestADefaultBranchPrintsNoTokenItsReadCarried(t *testing.T) {
	const token = redacttest.Token
	for _, row := range []struct {
		name    string
		compare http.HandlerFunc
	}{{
		name:    "status-500",
		compare: serverError,
	}, {
		name: "past-the-bound",
		compare: func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, strings.Repeat("x", responseLimit+1))
		},
	}, {
		name: "not-decoded",
		compare: func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, `{"status":7}`)
		},
	}} {
		t.Run(row.name, func(t *testing.T) {
			client := branchServer(t, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/repos/acme/web":
					fmt.Fprintf(w, `{"default_branch":%q}`, "release-"+echoToken(r))
				case strings.HasPrefix(r.URL.Path, "/repos/acme/web/compare/"):
					row.compare(w, r)
				default:
					githubtest.Unexpected(t, w, r)
				}
			})
			ctx, attempt := githubreads.Begin(context.Background())
			branch, err := client.DefaultBranch(ctx, token, "acme", "web")
			if err != nil || !strings.Contains(fmt.Sprint(branch), "release-"+token) {
				t.Fatalf("DefaultBranch = %v, %v; want GitHub's default_branch as it came", branch, err)
			}
			_, err = client.CommitOnBranch(ctx, token, "acme", "web", branch, mainCommit)
			if err == nil {
				t.Fatal("a comparison GitHub failed answered no error")
			}
			githubreads.NoteFailure(ctx, err)
			_, _, cause := attempt.Outcome()
			redacttest.RequireNoPiece(t, "githubapp.default-branch-echo/"+row.name, err.Error()+" | "+cause, token)
			if !strings.Contains(err.Error(), "release-") {
				t.Errorf("CommitOnBranch answered %q, want the branch named", err)
			}
		})
	}
}

// TestADefaultBranchPrintsNoTokenAnEarlierMintCarried: the read that names a repository's
// default branch carries one installation token, and the comparison against that branch carries
// another, as it does where the client minted a token for the installation between the two reads
// (RepositoryToken hands out one until it expires, so the earlier one is live for up to an hour).
// Whatever answers in GitHub's place echoes the read's token in the default_branch it names, and
// answers the comparison 500. The branch prints as the read that named it printed it, so neither
// the error the comparison answers nor the note it leaves on the attempt holds a piece of that
// earlier token. Captures githubapp.default-branch-minted-again.
func TestADefaultBranchPrintsNoTokenAnEarlierMintCarried(t *testing.T) {
	const read = redacttest.Token
	const compare = "ghs_the_comparisons_own_token"
	client := branchServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/acme/web":
			fmt.Fprintf(w, `{"default_branch":%q}`, "release-"+echoToken(r))
		case strings.HasPrefix(r.URL.Path, "/repos/acme/web/compare/"):
			serverError(w, r)
		default:
			githubtest.Unexpected(t, w, r)
		}
	})
	ctx, attempt := githubreads.Begin(context.Background())
	branch, err := client.DefaultBranch(ctx, read, "acme", "web")
	if err != nil || !strings.Contains(fmt.Sprint(branch), "release-"+read) {
		t.Fatalf("DefaultBranch = %v, %v; want GitHub's default_branch as it came", branch, err)
	}
	_, err = client.CommitOnBranch(ctx, compare, "acme", "web", branch, mainCommit)
	if err == nil {
		t.Fatal("a comparison GitHub failed answered no error")
	}
	githubreads.NoteFailure(ctx, err)
	_, _, cause := attempt.Outcome()
	redacttest.RequireNoPiece(t, "githubapp.default-branch-minted-again", err.Error()+" | "+cause, read)
	if !strings.Contains(err.Error(), "release-") {
		t.Errorf("CommitOnBranch answered %q, want the branch named", err)
	}
}
