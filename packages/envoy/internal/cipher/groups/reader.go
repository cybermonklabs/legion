// Package groups reads the members of the Google groups the broker's policy names, so a member of
// a group that owns a secret decides its requests, and a group entry in a secret's requesters
// admits its members' sessions. It reads each group once an hour with the Directory API's
// members.list, nested members included, with the token of a reader account the deployment names
// (token.go), and keeps the last good members; a group whose last good read is more than two
// hours old, or that was never read, admits nobody.
package groups

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"sync"
	"time"

	"golang.org/x/oauth2"

	"github.com/sjawhar/envoy/internal/cipher/record"
)

const (
	// ReadEvery is how often Run reads every wanted group.
	ReadEvery = time.Hour
	// StaleAfter is how old a group's last good read may be and the group still admit its
	// members: two hourly reads, so one failed read keeps the members and the second admits
	// nobody.
	StaleAfter = 2 * time.Hour
	// ReadMessage is the log line each group's read writes, with the group, the members the
	// reader holds for it after the read, and the result: ok, or what refused the read.
	ReadMessage = "group members read"
	// Scope is the one scope of the directory token: it reads a group's members and nothing else.
	Scope = "https://www.googleapis.com/auth/admin.directory.group.member.readonly"
)

// googleDirectoryURL is Google's Directory API, the base URL a Reader reads when BaseURL is empty.
const googleDirectoryURL = "https://admin.googleapis.com"

// pageSize is the members a read asks for in each page, the most members.list answers.
const pageSize = "200"

// readTimeout bounds one group's read, every page of it, so a Directory that never answers ends
// that read and leaves the group to the next one.
const readTimeout = time.Minute

// Reader reads the members of each wanted group and answers who is a member.
//
// BaseURL is the Directory API's (BROKER_DIRECTORY_URL), Google's when empty; HTTP its client,
// http.DefaultClient when nil; Source the directory token (NewTokenSource); Now the clock that
// dates each read, time.Now when nil; Log where each read's line goes, slog.Default() when nil.
// Every method may be called from any goroutine.
type Reader struct {
	BaseURL string
	HTTP    *http.Client
	Source  oauth2.TokenSource
	Now     func() time.Time
	Log     *slog.Logger

	mu sync.Mutex
	// wanted is each group the last Want named, by its bare address.
	wanted map[string]*group
	// inflight counts the reads Want started, which a test waits for.
	inflight sync.WaitGroup
	// every and readTimeout are ReadEvery and readTimeout unless a test sets them.
	every, readTimeout time.Duration
}

// group is one wanted group: the members its last good read kept, when that read was, and
// whether a read of it is under way, so two never run at once.
type group struct {
	members map[string]struct{}
	readAt  time.Time
	reading bool
}

// Want sets the groups to read to names, the groups a policy load names: a name it has not read
// is read at once, in a goroutine of its own, and a name the last call named is left to the
// hourly read. A group names leaves out is no longer read, and admits nobody. A name given twice
// is one group.
func (r *Reader) Want(names []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	next := make(map[string]*group, len(names))
	for _, name := range names {
		if _, ok := next[name]; ok {
			continue
		}
		if g, ok := r.wanted[name]; ok {
			next[name] = g
			continue
		}
		next[name] = &group{}
		r.inflight.Add(1)
		go func() {
			defer r.inflight.Done()
			r.read(context.Background(), name)
		}()
	}
	r.wanted = next
}

// ReadAll reads every wanted group once, one after another, but a group whose read is already
// under way.
func (r *Reader) ReadAll(ctx context.Context) {
	r.mu.Lock()
	names := slices.Sorted(maps.Keys(r.wanted))
	r.mu.Unlock()
	for _, name := range names {
		r.read(ctx, name)
	}
}

// Run calls ReadAll every ReadEvery until ctx ends. A group's first read is Want's, so Run reads
// nothing when it starts.
func (r *Reader) Run(ctx context.Context) {
	ticker := time.NewTicker(cmp.Or(r.every, ReadEvery))
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.ReadAll(ctx)
		}
	}
}

// HasMember answers whether login is a member of group, by the members its last good read kept,
// when that read is StaleAfter old or newer. Both are compared as record.CanonicalLogin forms.
func (r *Reader) HasMember(group, login string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	g, ok := r.wanted[group]
	if !ok || !r.fresh(g) {
		return false
	}
	_, ok = g.members[record.CanonicalLogin(login)]
	return ok
}

// GroupsOf answers every wanted group HasMember admits login to, sorted.
func (r *Reader) GroupsOf(login string) []string {
	login = record.CanonicalLogin(login)
	r.mu.Lock()
	defer r.mu.Unlock()
	var names []string
	for name, g := range r.wanted {
		if _, ok := g.members[login]; ok && r.fresh(g) {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}

// Fresh answers whether group's last good read is StaleAfter old or newer, the rule HasMember
// admits members by: false for a group never read and for one no load names. A decider a fresh
// group does not admit is one the group does not list, and one a stale group does not admit is
// one nobody can tell.
func (r *Reader) Fresh(group string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	g, ok := r.wanted[group]
	return ok && r.fresh(g)
}

// fresh is Fresh for g; r.mu is held.
func (r *Reader) fresh(g *group) bool {
	return !g.readAt.IsZero() && !r.now().After(g.readAt.Add(StaleAfter))
}

func (r *Reader) now() time.Time {
	if r.Now == nil {
		return time.Now()
	}
	return r.Now()
}

// read reads name's members, keeps them when every page was read, and logs the read. A name no
// longer wanted, or one whose read is under way, is not read.
func (r *Reader) read(ctx context.Context, name string) {
	r.mu.Lock()
	g, ok := r.wanted[name]
	if !ok || g.reading {
		r.mu.Unlock()
		return
	}
	g.reading = true
	r.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, cmp.Or(r.readTimeout, readTimeout))
	members, err := r.list(ctx, name)
	cancel()

	r.mu.Lock()
	g.reading = false
	if err == nil {
		g.members, g.readAt = members, r.now()
	}
	kept := len(g.members)
	r.mu.Unlock()

	log := cmp.Or(r.Log, slog.Default())
	if err != nil {
		log.Warn(ReadMessage, "group", name, "members", kept, "result", err.Error())
		return
	}
	log.Info(ReadMessage, "group", name, "members", kept, "result", "ok")
}

// list reads every page of name's members.list, derived members included, and answers the
// people among them by their record.CanonicalLogin form. A failed page fails the whole read.
func (r *Reader) list(ctx context.Context, name string) (map[string]struct{}, error) {
	token, err := r.Source.Token()
	if err != nil {
		return nil, err
	}
	client := cmp.Or(r.HTTP, http.DefaultClient)
	endpoint := cmp.Or(r.BaseURL, googleDirectoryURL) + "/admin/directory/v1/groups/" + url.PathEscape(name) + "/members"
	members := map[string]struct{}{}
	query := url.Values{"includeDerivedMembership": {"true"}, "maxResults": {pageSize}}
	for {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?"+query.Encode(), nil)
		if err != nil {
			return nil, err
		}
		token.SetAuthHeader(request)
		payload, err := call(client, request, "the Directory")
		if err != nil {
			return nil, err
		}
		var page struct {
			Members []struct {
				Email string `json:"email"`
				Type  string `json:"type"`
			} `json:"members"`
			NextPageToken string `json:"nextPageToken"`
		}
		if err := json.Unmarshal(payload, &page); err != nil {
			return nil, errDirectoryAnswer
		}
		for _, m := range page.Members {
			if m.Type == "USER" && m.Email != "" {
				members[record.CanonicalLogin(m.Email)] = struct{}{}
			}
		}
		if page.NextPageToken == "" {
			return members, nil
		}
		query.Set("pageToken", page.NextPageToken)
	}
}

// errDirectoryAnswer is a members.list answer that is no page of members.
var errDirectoryAnswer = errors.New("the Directory answered no page of members")
