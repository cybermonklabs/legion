package groups_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/groups"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// membersStand is the Directory's members.list wherever under its address a read comes: it
// answers one member over two pages and records the URL each read reached it with, which the
// directory stand-in, matching the members route alone, cannot show. The first answer alone
// names a next page, so a read that sends no page token ends at the second request rather than
// asking until the read's own bound.
type membersStand struct {
	server *httptest.Server
	mu     sync.Mutex
	asked  []url.URL
}

func newMembersStand(t *testing.T) *membersStand {
	t.Helper()
	s := &membersStand{}
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.asked = append(s.asked, *r.URL)
		first := len(s.asked) == 1
		s.mu.Unlock()
		page := map[string]any{"members": []map[string]string{{"email": member, "type": "USER"}}}
		if first {
			page["nextPageToken"] = "page-2"
		}
		_ = json.NewEncoder(w).Encode(page)
	}))
	t.Cleanup(s.server.Close)
	return s
}

func (s *membersStand) got() []url.URL {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]url.URL(nil), s.asked...)
}

// readerAt is a reader of the one group over base, with the pod's own token as its directory
// token, which has read every page by the time it returns.
func readerAt(t *testing.T, base string, client *http.Client, log *logtest.Recorder) *groups.Reader {
	t.Helper()
	reader := &groups.Reader{BaseURL: base, HTTP: client, Source: podToken(), Log: slog.New(log)}
	t.Cleanup(func() { reader.WaitReads(context.Background()) })
	reader.Want([]string{approvers})
	reader.WaitReads(context.Background())
	return reader
}

// TestAMembersReadGoesToTheRouteJoinedOntoTheDirectoryURL: the Directory URL is a setting
// (BROKER_DIRECTORY_URL), and it can carry a path, a query or a fragment. Each page goes to that
// URL's path with the members route joined onto it, the configured query kept ahead of the
// page's own parameters and the fragment dropped: http://host/dir?key=... reads
// /dir/admin/directory/v1/groups/<group>/members?key=...&includeDerivedMembership=true&... (query),
// and so does http://host/dir/?key=... (query-trailing-slash), where the route appended to the
// URL's text sat inside the query and the parameters' own "?" went into it too, so the Directory
// was asked /dir; http://host/dir#... reads the route (fragment), where it sat inside the
// fragment. A URL with neither reads where it did (no-query). The second page keeps the
// configured query ahead of its page token. A stand-in records the path and the query each read
// reaches it with.
func TestAMembersReadGoesToTheRouteJoinedOntoTheDirectoryURL(t *testing.T) {
	route := "/dir/admin/directory/v1/groups/" + approvers + "/members"
	const parameters = "includeDerivedMembership=true&maxResults=200"
	for _, c := range []struct{ name, suffix, query string }{
		{"query", "/dir?key=k3y-7Qz4k", "key=k3y-7Qz4k&"},
		{"query-trailing-slash", "/dir/?key=k3y-7Qz4k", "key=k3y-7Qz4k&"},
		{"fragment", "/dir#pane", ""},
		{"no-query", "/dir", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			stand := newMembersStand(t)
			reader := readerAt(t, stand.server.URL+c.suffix, stand.server.Client(), &logtest.Recorder{})
			want := []string{c.query + parameters, c.query + parameters + "&pageToken=page-2"}
			got := stand.got()
			if len(got) != len(want) {
				t.Fatalf("a Directory URL ending %q was asked %v; want the group's two pages", c.suffix, got)
			}
			for i := range want {
				if got[i].Path != route || got[i].RawQuery != want[i] {
					t.Errorf("a Directory URL ending %q: page %d read path %q, query %q; want path %q, query %q", c.suffix, i, got[i].Path, got[i].RawQuery, route, want[i])
				}
			}
			if listed, fresh := reader.Membership(approvers, member); !listed || !fresh {
				t.Errorf("after a read of a Directory URL ending %q, %s is listed %v, fresh %v; want both", c.suffix, member, listed, fresh)
			}
		})
	}
}

// TestADirectoryURLWithACredentialPrintsNoneOfIt: the Directory URL with redacttest's credentials
// in its user information and its query. One url.Parse refuses (an invalid escape in its path)
// fails the read in the reader's own fixed words, which its log line carries, with none of the
// URL (unparsed); http.NewRequest's error, which the read's construction printed, quotes it whole
// (twin). One url.Parse takes, whose joined URL, as url.URL.String writes it, url.Parse refuses
// (an IPv6 zone holding a byte that is not ASCII, which String escapes as %FD), is refused the
// same way (reparsed); its twin shows http.NewRequest's error for the joined URL holds the
// credentials. Captures groups.directory-url/<case>.
func TestADirectoryURLWithACredentialPrintsNoneOfIt(t *testing.T) {
	param := "access" + "_token"
	userinfo := redacttest.User + ":" + redacttest.Password + "@"
	route := "/admin/directory/v1/groups/" + approvers + "/members"
	const parameters = "?includeDerivedMembership=true&maxResults=200"
	unparsed := "http://" + userinfo + "127.0.0.1:1/dir%zz?" + param + "=" + redacttest.Token
	reparsed := "http://" + userinfo + "[::%25\xfd]/dir?" + param + "=" + redacttest.Token
	read := func(t *testing.T, base string) string {
		t.Helper()
		log := &logtest.Recorder{}
		readerAt(t, base, nil, log)
		for _, record := range log.Records() {
			if record.Message != groups.ReadMessage {
				continue
			}
			var said string
			record.Attrs(func(a slog.Attr) bool {
				if a.Key == "result" {
					said = a.Value.String()
				}
				return true
			})
			return said
		}
		t.Fatalf("the reader logged no %q line for a Directory URL that does not parse", groups.ReadMessage)
		return ""
	}
	t.Run("unparsed-twin", func(t *testing.T) {
		if _, err := http.NewRequest(http.MethodGet, unparsed+route+parameters, nil); err == nil || len(redacttest.Held(err.Error())) != len(redacttest.Secrets) {
			t.Errorf("http.NewRequest = %v; want an error that quotes the credentials, so the row can see them", err)
		}
	})
	t.Run("unparsed", func(t *testing.T) {
		said := read(t, unparsed)
		redacttest.RequireNoPiece(t, "groups.directory-url/unparsed", said)
		if want := "the Directory URL does not parse"; said != want {
			t.Errorf("the read's log line says %q; want %q", said, want)
		}
	})
	t.Run("reparsed-twin", func(t *testing.T) {
		parsed, err := url.Parse(reparsed)
		if err != nil {
			t.Fatalf("url.Parse refused the Directory URL (%v); the row needs one it takes", err)
		}
		if _, err := http.NewRequest(http.MethodGet, parsed.JoinPath(route).String()+parameters, nil); err == nil || len(redacttest.Held(err.Error())) != len(redacttest.Secrets) {
			t.Errorf("http.NewRequest of the joined URL = %v; want an error that quotes the credentials, so the row can see them", err)
		}
	})
	t.Run("reparsed", func(t *testing.T) {
		said := read(t, reparsed)
		redacttest.RequireNoPiece(t, "groups.directory-url/reparsed", said)
		if want := "the URL the Directory read joined does not parse"; said != want {
			t.Errorf("the read's log line says %q; want %q", said, want)
		}
	})
}
