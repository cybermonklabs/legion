package groups_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/groups"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The groups and people the tests read: example.com names alone, since the repository is public.
const (
	approvers = "approvers@example.com"
	reviewers = "reviewers@example.com"
	member    = "member@example.com"
	outsider  = "outsider@example.com"
)

// clock is the Now a test moves: the reader's cut-off is two hours, which no test waits out.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *clock { return &clock{now: time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// listed is one entry members.list answers. derived marks a member through a nested group, which
// Google lists only when the request asks for includeDerivedMembership=true.
type listed struct {
	Email   string `json:"email"`
	Type    string `json:"type"`
	Role    string `json:"role"`
	derived bool
}

func user(email string) listed { return listed{Email: email, Type: "USER", Role: "MEMBER"} }

// directoryRequest is one request the Directory stand-in received.
type directoryRequest struct {
	method, path, group, bearer string
	query                       url.Values
}

// directory is a stand-in of the Directory API's members.list: it answers each group's members,
// pageSize to a page, refuses what refuse answers for a group and page token, and records each
// request. A group it does not list is a 404, as Google answers for one it does not hold.
type directory struct {
	server *httptest.Server

	mu       sync.Mutex
	groups   map[string][]listed
	pageSize int
	refuse   func(group, pageToken string) *refusal
	// hang, when set, holds a request for that group until its caller gives up.
	hang     string
	requests []directoryRequest
}

var membersPath = regexp.MustCompile(`^/admin/directory/v1/groups/([^/]+)/members$`)

func newDirectory(t *testing.T, groups map[string][]listed) *directory {
	t.Helper()
	d := &directory{groups: groups, pageSize: 200}
	d.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request := directoryRequest{method: r.Method, path: r.URL.Path, bearer: r.Header.Get("Authorization"), query: r.URL.Query()}
		if match := membersPath.FindStringSubmatch(r.URL.Path); match != nil {
			request.group = match[1]
		}
		d.mu.Lock()
		d.requests = append(d.requests, request)
		hang := d.hang != "" && d.hang == request.group
		d.mu.Unlock()
		if hang {
			<-r.Context().Done()
			return
		}
		d.mu.Lock()
		defer d.mu.Unlock()
		if r.Method != http.MethodGet || request.group == "" {
			http.NotFound(w, r)
			return
		}
		pageToken := request.query.Get("pageToken")
		if d.refuse != nil {
			if refused := d.refuse(request.group, pageToken); refused != nil {
				w.WriteHeader(refused.status)
				_, _ = io.WriteString(w, refused.body)
				return
			}
		}
		members, ok := d.groups[request.group]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"code":404,"message":"Resource Not Found: groupKey","errors":[{"message":"Resource Not Found: groupKey","domain":"global","reason":"notFound"}]}}`)
			return
		}
		var answered []listed
		for _, m := range members {
			if !m.derived || request.query.Get("includeDerivedMembership") == "true" {
				answered = append(answered, m)
			}
		}
		size := d.pageSize
		if asked, err := strconv.Atoi(request.query.Get("maxResults")); err == nil && asked < size {
			size = asked
		}
		start, _ := strconv.Atoi(pageToken)
		start = min(start, len(answered))
		end := min(start+size, len(answered))
		answer := map[string]any{"kind": "admin#directory#members", "members": answered[start:end]}
		if end < len(answered) {
			answer["nextPageToken"] = strconv.Itoa(end)
		}
		_ = json.NewEncoder(w).Encode(answer)
	}))
	t.Cleanup(d.server.Close)
	return d
}

func (d *directory) received() []directoryRequest {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.requests)
}

// calls is how many requests the stand-in received for group.
func (d *directory) calls(group string) int {
	n := 0
	for _, r := range d.received() {
		if r.group == group {
			n++
		}
	}
	return n
}

func (d *directory) set(change func(*directory)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	change(d)
}

// rig is a reader over the three stand-ins, a clock the test moves and a log it reads back.
type rig struct {
	google    *google
	directory *directory
	clock     *clock
	log       *logtest.Recorder
	reader    *groups.Reader
}

func newRig(t *testing.T, members map[string][]listed) *rig {
	t.Helper()
	g := newGoogle(t, brokerAccount)
	d := newDirectory(t, members)
	c := newClock()
	log := &logtest.Recorder{}
	reader := &groups.Reader{BaseURL: d.server.URL, HTTP: d.server.Client(), Source: g.source(), Now: c.Now, Log: slog.New(log)}
	t.Cleanup(func() { groups.Settle(reader) })
	return &rig{google: g, directory: d, clock: c, log: log, reader: reader}
}

// want names groups to the reader, as a policy load does, and waits for every read that started.
func (r *rig) want(names ...string) {
	r.reader.Want(names)
	groups.Settle(r.reader)
}

// readLine is one record of the reader's log line.
type readLine struct {
	level   slog.Level
	group   string
	members int64
	result  string
}

func (r *rig) reads() []readLine {
	var lines []readLine
	for _, record := range r.log.Records() {
		if record.Message != groups.ReadMessage {
			continue
		}
		line := readLine{level: record.Level}
		record.Attrs(func(a slog.Attr) bool {
			switch a.Key {
			case "group":
				line.group = a.Value.String()
			case "members":
				line.members = a.Value.Int64()
			case "result":
				line.result = a.Value.String()
			}
			return true
		})
		lines = append(lines, line)
	}
	return lines
}

// readsOf are the reader's log lines for group.
func (r *rig) readsOf(group string) []readLine {
	var lines []readLine
	for _, line := range r.reads() {
		if line.group == group {
			lines = append(lines, line)
		}
	}
	return lines
}

// TestANestedMemberIsAdmitted: the read asks for derived membership, so a person in a group
// nested in the group is a member; a nested group's own address is no login; and a member is
// kept by the lowercase form of the email Google answers, so the login a person signs in with
// matches it whatever case either is written in.
func TestANestedMemberIsAdmitted(t *testing.T) {
	r := newRig(t, map[string][]listed{approvers: {
		user("Member@Example.com"),
		{Email: "nested@example.com", Type: "USER", Role: "MEMBER", derived: true},
		{Email: "subteam@example.com", Type: "GROUP", Role: "MEMBER"},
	}})
	r.want(approvers)

	for _, login := range []string{member, "MEMBER@example.com", "nested@example.com"} {
		if !r.reader.HasMember(approvers, login) {
			t.Errorf("HasMember(%s, %s) = false, want a member", approvers, login)
		}
	}
	for _, login := range []string{"subteam@example.com", outsider} {
		if r.reader.HasMember(approvers, login) {
			t.Errorf("HasMember(%s, %s) = true, want no member", approvers, login)
		}
	}
	requests := r.directory.received()
	if len(requests) == 0 {
		t.Fatal("the Directory received no request: UNKNOWN, nothing was read")
	}
	for _, request := range requests {
		if request.query.Get("includeDerivedMembership") != "true" {
			t.Errorf("the Directory was asked %s?%s, want includeDerivedMembership=true", request.path, request.query.Encode())
		}
	}
}

// TestTheReaderReadsEachGroupOncePerHour: twelve policy loads five minutes apart that name the
// same two groups, one of them twice (as a secret's owner and in another's requesters), with
// decisions asked between them, read each group once, when it is first named; the hourly read is
// the next one. Each load waits for the reads it started, as five minutes would: loads made while
// a read is under way would hide a load that reads again, since a read under way is not started
// twice.
func TestTheReaderReadsEachGroupOncePerHour(t *testing.T) {
	r := newRig(t, map[string][]listed{approvers: {user(member)}, reviewers: {user(member)}})
	for range 12 {
		r.want(approvers, reviewers, approvers)
		for range 3 {
			r.reader.HasMember(approvers, member)
			r.reader.HasMember(reviewers, outsider)
			r.reader.GroupsOf(member)
		}
		r.clock.Advance(5 * time.Minute)
	}
	for _, group := range []string{approvers, reviewers} {
		if got := r.directory.calls(group); got != 1 {
			t.Fatalf("an hour of loads and decisions read %s %d times, want once", group, got)
		}
	}

	r.reader.ReadAll(t.Context())
	for _, group := range []string{approvers, reviewers} {
		if got := r.directory.calls(group); got != 2 {
			t.Fatalf("the hourly read left %s read %d times, want twice", group, got)
		}
	}
}

// TestRunReadsEveryWantedGroupEachTick: Run reads every wanted group at each tick until its
// context ends, and no more once it has.
func TestRunReadsEveryWantedGroupEachTick(t *testing.T) {
	r := newRig(t, map[string][]listed{approvers: {user(member)}, reviewers: {user(member)}})
	r.want(approvers, reviewers)
	groups.SetEvery(r.reader, 10*time.Millisecond)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.reader.Run(ctx)
	}()
	testwait.Eventually(t, "three ticks of Run reading both groups", func() bool {
		return r.directory.calls(approvers) >= 4 && r.directory.calls(reviewers) >= 4
	})
	cancel()
	<-done
	after := len(r.directory.received())
	time.Sleep(50 * time.Millisecond)
	if got := len(r.directory.received()); got != after {
		t.Fatalf("the Directory received %d requests after Run returned, want none", got-after)
	}
}

// TestANewGroupIsReadAtOnce: a load that names a group for the first time reads it at once,
// without waiting for the hourly read, and reads no group it already reads.
func TestANewGroupIsReadAtOnce(t *testing.T) {
	r := newRig(t, map[string][]listed{approvers: {user(member)}, reviewers: {user(member)}})
	r.want(approvers)
	if r.directory.calls(approvers) != 1 || r.reader.HasMember(reviewers, member) {
		t.Fatalf("the first load read %s %d times and admits a member of %s; want once, and nobody", approvers, r.directory.calls(approvers), reviewers)
	}
	r.want(approvers, reviewers)
	if got := r.directory.calls(reviewers); got != 1 {
		t.Fatalf("a load naming %s for the first time read it %d times, want once", reviewers, got)
	}
	if got := r.directory.calls(approvers); got != 1 {
		t.Fatalf("a load naming %s again read it %d times in all, want once", approvers, got)
	}
	if !r.reader.HasMember(reviewers, member) {
		t.Fatalf("HasMember(%s, %s) = false after its first read, want a member", reviewers, member)
	}
}

// TestAGroupNoLoadNamesIsNoLongerRead: once a load no longer names a group, the hourly read
// leaves it, and it admits nobody.
func TestAGroupNoLoadNamesIsNoLongerRead(t *testing.T) {
	r := newRig(t, map[string][]listed{approvers: {user(member)}, reviewers: {user(member)}})
	r.want(approvers, reviewers)
	r.want(approvers)
	r.reader.ReadAll(t.Context())
	if got := r.directory.calls(reviewers); got != 1 {
		t.Fatalf("%s, which the last load does not name, was read %d times, want only its first read", reviewers, got)
	}
	if got := r.directory.calls(approvers); got != 2 {
		t.Fatalf("%s was read %d times, want its first read and the hourly one", approvers, got)
	}
	if r.reader.HasMember(reviewers, member) || slices.Contains(r.reader.GroupsOf(member), reviewers) {
		t.Fatalf("%s admits %s after no load names it, want nobody", reviewers, member)
	}
}

// TestOneFailedReadKeepsTheLastMembers: a read that fails an hour after a good one keeps the
// members the good one read, and its log line names the failure.
func TestOneFailedReadKeepsTheLastMembers(t *testing.T) {
	r := newRig(t, map[string][]listed{approvers: {user(member)}})
	r.want(approvers)
	r.directory.set(func(d *directory) {
		d.refuse = func(string, string) *refusal {
			return &refusal{http.StatusServiceUnavailable, `{"error":{"code":503,"message":"backend unavailable","status":"UNAVAILABLE"}}`}
		}
	})
	r.clock.Advance(time.Hour)
	r.reader.ReadAll(t.Context())

	if !r.reader.HasMember(approvers, member) || !slices.Equal(r.reader.GroupsOf(member), []string{approvers}) {
		t.Fatalf("one failed read dropped %s from %s, want the last good members kept", member, approvers)
	}
	lines := r.readsOf(approvers)
	if len(lines) != 2 {
		t.Fatalf("the log holds %d reads of %s, want 2: %+v", len(lines), approvers, lines)
	}
	if last := lines[1]; last.level != slog.LevelWarn || !strings.Contains(last.result, "503") || !strings.Contains(last.result, "UNAVAILABLE") || last.members != 1 {
		t.Fatalf("the failed read logged %+v, want WARN naming 503 UNAVAILABLE and the one member kept", last)
	}
}

// TestAnAnswerThatIsNoPageFailsTheRead: a 200 whose body is no page of members (a proxy's page,
// say) fails the read, keeps the last good members and is logged as a failure, rather than read
// as a group with no members.
func TestAnAnswerThatIsNoPageFailsTheRead(t *testing.T) {
	r := newRig(t, map[string][]listed{approvers: {user(member)}})
	r.want(approvers)
	r.directory.set(func(d *directory) {
		d.refuse = func(string, string) *refusal { return &refusal{http.StatusOK, "<html>sign in</html>"} }
	})
	r.clock.Advance(time.Hour)
	r.reader.ReadAll(t.Context())

	if !r.reader.HasMember(approvers, member) {
		t.Fatalf("an answer that is no page dropped %s from %s, want the last good members kept", member, approvers)
	}
	lines := r.readsOf(approvers)
	if len(lines) != 2 || lines[1].level != slog.LevelWarn || !strings.Contains(lines[1].result, "no page of members") || lines[1].members != 1 {
		t.Fatalf("the log holds %+v for %s, want a good read and a WARN line saying the answer was no page of members", lines, approvers)
	}
}

// TestAGroupNotReadForTwoHoursAdmitsNobody: a group whose last good read is two hours old still
// admits its members, and one past that admits nobody, while a group read since still does.
func TestAGroupNotReadForTwoHoursAdmitsNobody(t *testing.T) {
	r := newRig(t, map[string][]listed{approvers: {user(member)}, reviewers: {user(member)}})
	r.want(approvers, reviewers)
	r.directory.set(func(d *directory) {
		d.refuse = func(group, _ string) *refusal {
			if group == approvers {
				return &refusal{http.StatusServiceUnavailable, `{"error":{"code":503,"status":"UNAVAILABLE"}}`}
			}
			return nil
		}
	})
	for range 2 {
		r.clock.Advance(time.Hour)
		r.reader.ReadAll(t.Context())
	}
	if !r.reader.HasMember(approvers, member) {
		t.Fatalf("%s, last read two hours ago, admits nobody; want its members until it is older", approvers)
	}

	r.clock.Advance(time.Nanosecond)
	if r.reader.HasMember(approvers, member) {
		t.Fatalf("%s, last read more than two hours ago, admits %s; want nobody", approvers, member)
	}
	if got := r.reader.GroupsOf(member); !slices.Equal(got, []string{reviewers}) {
		t.Fatalf("GroupsOf(%s) = %v, want only %s, the group read since", member, got, reviewers)
	}
	if !r.reader.HasMember(reviewers, member) {
		t.Fatalf("%s, read an instant ago, admits nobody", reviewers)
	}
}

// TestANeverReadGroupAdmitsNobody: a group whose every read failed admits nobody, and so does a
// group no load named, whatever the Directory would answer for it.
func TestANeverReadGroupAdmitsNobody(t *testing.T) {
	r := newRig(t, map[string][]listed{approvers: {user(member)}, reviewers: {user(member)}})
	r.directory.set(func(d *directory) {
		d.refuse = func(group, _ string) *refusal {
			if group == approvers {
				return &refusal{http.StatusServiceUnavailable, `{"error":{"code":503,"status":"UNAVAILABLE"}}`}
			}
			return nil
		}
	})
	r.want(approvers)
	if r.directory.calls(approvers) != 1 {
		t.Fatalf("%s was read %d times, want its one failed read", approvers, r.directory.calls(approvers))
	}
	for _, group := range []string{approvers, reviewers} {
		if r.reader.HasMember(group, member) {
			t.Errorf("HasMember(%s, %s) = true for a group never read, want nobody", group, member)
		}
	}
	if got := r.reader.GroupsOf(member); len(got) != 0 {
		t.Fatalf("GroupsOf(%s) = %v, want no group, since none was read", member, got)
	}
	if got := r.directory.calls(reviewers); got != 0 {
		t.Fatalf("%s, which no load names, was read %d times, want never", reviewers, got)
	}
}

// TestEachReadIsLogged: each group's read writes one line naming the group, the count of
// people kept (not the nested group) and ok, at INFO.
func TestEachReadIsLogged(t *testing.T) {
	r := newRig(t, map[string][]listed{
		approvers: {user(member), user("second@example.com"), {Email: "subteam@example.com", Type: "GROUP", Role: "MEMBER"}},
		reviewers: {user(member)},
	})
	r.want(approvers, reviewers)
	for group, members := range map[string]int64{approvers: 2, reviewers: 1} {
		lines := r.readsOf(group)
		want := readLine{level: slog.LevelInfo, group: group, members: members, result: "ok"}
		if len(lines) != 1 || lines[0] != want {
			t.Errorf("the log holds %+v for %s, want one %q line %+v", lines, group, groups.ReadMessage, want)
		}
	}
}

// TestTheDirectoryGetsTheAccountsOwnTokenAlone: every Directory request, over pages, groups and
// reads, carries the token minted for the pod's own account, and never the metadata server's
// own token, which the Admin SDK refuses.
func TestTheDirectoryGetsTheAccountsOwnTokenAlone(t *testing.T) {
	r := newRig(t, map[string][]listed{
		approvers: {user(member), user("second@example.com")},
		reviewers: {user(member), user("second@example.com")},
	})
	r.directory.set(func(d *directory) { d.pageSize = 1 })
	r.want(approvers, reviewers)
	r.clock.Advance(time.Hour)
	r.reader.ReadAll(t.Context())

	requests := r.directory.received()
	if len(requests) != 8 {
		t.Fatalf("the Directory received %d requests, want 8 (two pages of two groups, read twice)", len(requests))
	}
	for _, request := range requests {
		if request.bearer != "Bearer "+mintedToken {
			t.Fatalf("the Directory was sent Authorization %q, want the minted token alone", request.bearer)
		}
	}
	if !r.reader.HasMember(approvers, "second@example.com") {
		t.Fatal("the reads with the minted token admit nobody")
	}
}

// TestAPrincipalEmailReadsNoGroup: a pod that runs as a Workload Identity principal, whose
// metadata email names no service account, mints nothing and calls no Directory; every group is
// unread, and each one's log line says why.
func TestAPrincipalEmailReadsNoGroup(t *testing.T) {
	r := newRig(t, map[string][]listed{approvers: {user(member)}, reviewers: {user(member)}})
	r.google.mu.Lock()
	r.google.email = principal
	r.google.mu.Unlock()
	r.want(approvers, reviewers)

	if _, paths := r.google.metadataRead(); len(paths) == 0 {
		t.Fatal("the metadata server was never asked: UNKNOWN, the read never looked for its account")
	}
	if got := len(r.google.minted()); got != 0 {
		t.Fatalf("IAM was asked %d mints for a principal, want none", got)
	}
	if got := len(r.directory.received()); got != 0 {
		t.Fatalf("the Directory received %d requests, want none", got)
	}
	for _, group := range []string{approvers, reviewers} {
		if r.reader.HasMember(group, member) {
			t.Errorf("%s admits %s, want nobody", group, member)
		}
		lines := r.readsOf(group)
		if len(lines) != 1 || lines[0].level != slog.LevelWarn || !strings.HasPrefix(lines[0].result, "no service account") {
			t.Errorf("the log holds %+v for %s, want one WARN line whose result is no service account", lines, group)
		}
	}
}

// TestARefusedMintIsLoggedWithItsReason: a mint IAM refuses, which is what an account with no
// Token Creator on itself gets, and a Directory that refuses the token each leave every group
// unread, and each group's log line names Google's reason, so one line says what to fix.
func TestARefusedMintIsLoggedWithItsReason(t *testing.T) {
	for _, tc := range []struct {
		name         string
		setUp        func(*rig)
		names        []string
		directoryHit bool
	}{
		{"the mint", func(r *rig) {
			r.google.mu.Lock()
			r.google.refused = &mintDenied
			r.google.mu.Unlock()
		}, []string{"403", "IAM_PERMISSION_DENIED"}, false},
		{"the Directory", func(r *rig) {
			r.directory.set(func(d *directory) {
				d.refuse = func(string, string) *refusal { return &directoryUnauthenticated }
			})
		}, []string{"401", "UNAUTHENTICATED"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, map[string][]listed{approvers: {user(member)}, reviewers: {user(member)}})
			tc.setUp(r)
			r.want(approvers, reviewers)
			if hit := len(r.directory.received()) > 0; hit != tc.directoryHit {
				t.Fatalf("the Directory received requests: %v, want %v", hit, tc.directoryHit)
			}
			for _, group := range []string{approvers, reviewers} {
				if r.reader.HasMember(group, member) {
					t.Errorf("%s admits %s after a refused read, want nobody", group, member)
				}
				lines := r.readsOf(group)
				if len(lines) != 1 || lines[0].level != slog.LevelWarn || lines[0].members != 0 {
					t.Fatalf("the log holds %+v for %s, want one WARN line with no member", lines, group)
				}
				for _, name := range tc.names {
					if !strings.Contains(lines[0].result, name) {
						t.Errorf("the log line's result %q does not name %q", lines[0].result, name)
					}
				}
				for _, secret := range []string{defaultToken, mintedToken} {
					if strings.Contains(lines[0].result, secret) {
						t.Errorf("the log line's result %q quotes a token", lines[0].result)
					}
				}
			}
		})
	}
}

// TestTheReaderCallsMembersListAlone: every request the read makes is a GET of one wanted
// group's members, asking for derived members two hundred to a page: members.list and no other
// Directory method, since the token's one scope reads members alone.
func TestTheReaderCallsMembersListAlone(t *testing.T) {
	r := newRig(t, map[string][]listed{
		approvers: {user(member), user("second@example.com"), user("third@example.com")},
		reviewers: {user(member)},
	})
	r.directory.set(func(d *directory) { d.pageSize = 2 })
	r.want(approvers, reviewers)
	r.reader.ReadAll(t.Context())

	requests := r.directory.received()
	if len(requests) == 0 {
		t.Fatal("the Directory received no request: UNKNOWN, nothing was read")
	}
	only := regexp.MustCompile(`^/admin/directory/v1/groups/(approvers|reviewers)@example\.com/members$`)
	for _, request := range requests {
		if request.method != http.MethodGet || !only.MatchString(request.path) {
			t.Errorf("the Directory was asked %s %s, want a GET of a wanted group's members alone", request.method, request.path)
		}
		if request.query.Get("includeDerivedMembership") != "true" || request.query.Get("maxResults") != "200" {
			t.Errorf("the Directory was asked %s?%s, want includeDerivedMembership=true and maxResults=200", request.path, request.query.Encode())
		}
	}
}

// TestTheReadFollowsEveryPage: a group whose members span pages is read to its last page, and a
// later read that fails part of the way fails whole, keeping the last good members rather than
// the pages it read.
func TestTheReadFollowsEveryPage(t *testing.T) {
	everyone := []listed{user("a@example.com"), user("b@example.com"), user("c@example.com"), user("d@example.com"), user("e@example.com")}
	r := newRig(t, map[string][]listed{approvers: everyone})
	r.directory.set(func(d *directory) { d.pageSize = 2 })
	r.want(approvers)

	var tokens []string
	for _, request := range r.directory.received() {
		tokens = append(tokens, request.query.Get("pageToken"))
	}
	if !slices.Equal(tokens, []string{"", "2", "4"}) {
		t.Fatalf("the read asked for pages %q, want the first and each nextPageToken: \"\", 2, 4", tokens)
	}
	for _, m := range everyone {
		if !r.reader.HasMember(approvers, m.Email) {
			t.Errorf("%s, on a later page, is no member", m.Email)
		}
	}

	// The group is now its first three people, two pages; the second page fails, so a read that
	// kept the pages it read would hold a and b alone.
	r.directory.set(func(d *directory) {
		d.groups[approvers] = everyone[:3]
		d.refuse = func(_, pageToken string) *refusal {
			if pageToken == "" {
				return nil
			}
			return &refusal{http.StatusServiceUnavailable, `{"error":{"code":503,"status":"UNAVAILABLE"}}`}
		}
	})
	r.clock.Advance(time.Hour)
	r.reader.ReadAll(t.Context())
	if requests := r.directory.received(); len(requests) != 5 || requests[3].query.Get("pageToken") != "" || requests[4].query.Get("pageToken") != "2" {
		t.Fatalf("the second read made %d requests in all, want 5, its own the first page and the failing second", len(requests))
	}
	for _, m := range everyone {
		if !r.reader.HasMember(approvers, m.Email) {
			t.Errorf("a read whose second page failed dropped %s, want the last good members kept whole", m.Email)
		}
	}
	if lines := r.readsOf(approvers); len(lines) != 2 || lines[1].level != slog.LevelWarn {
		t.Fatalf("the log holds %+v for %s, want a good read and a failed one", lines, approvers)
	}
}

// TestAHungReadEndsAndTheGroupIsReadAgain: a read the Directory never answers gives up, says
// so, and leaves the group to the next read rather than holding it as read forever.
func TestAHungReadEndsAndTheGroupIsReadAgain(t *testing.T) {
	r := newRig(t, map[string][]listed{approvers: {user(member)}})
	groups.SetReadTimeout(r.reader, 50*time.Millisecond)
	r.directory.set(func(d *directory) { d.hang = approvers })
	r.want(approvers)
	if lines := r.readsOf(approvers); len(lines) != 1 || lines[0].level != slog.LevelWarn || lines[0].result == "ok" {
		t.Fatalf("the log holds %+v for a read the Directory never answered, want one WARN line", lines)
	}

	r.directory.set(func(d *directory) { d.hang = "" })
	r.reader.ReadAll(t.Context())
	if !r.reader.HasMember(approvers, member) {
		t.Fatalf("%s admits nobody after a read the Directory answered, want %s", approvers, member)
	}
}
