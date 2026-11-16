package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/api"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// projectKey is a project key as contracts.ProjectKeyPattern spells one.
var projectKey = regexp.MustCompile(contracts.ProjectKeyPattern)

// privateProjectsSetting reads DISPATCH_PRIVATE_PROJECTS: the comma-separated keys of the projects
// this deployment has provisioned as private (agent-infra's legion.PrivateProjects), each entry
// trimmed and empty ones dropped, deduplicated and sorted. An entry that is no project key is a
// malformed setting and refuses the boot naming it, the old on/off value 1 included, so it is never
// read as a list. Unset or empty is no project that can be made private.
func privateProjectsSetting(value string) ([]string, error) {
	var keys []string
	for entry := range strings.SplitSeq(value, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if !projectKey.MatchString(entry) {
			return nil, fmt.Errorf("DISPATCH_PRIVATE_PROJECTS names project keys; %q is not a project key: %s", entry, contracts.ProjectKeyRule)
		}
		keys = append(keys, entry)
	}
	slices.Sort(keys)
	return slices.Compact(keys), nil
}

// reportPrivateProjects is the boot's report of what the state's first read (state) and the store
// say of private projects, right after every project has an owner. It makes no read of the private
// projects of its own. It reports and never refuses: Dispatch runs as one process, so a boot that
// exited would stop every project, and the owner's repair through the API with it, while each
// condition here fails closed for its own projects alone.
//   - Each project the database holds private that the list does not name is one ERROR line
//     naming it and both repairs (api.LogUnprovisionedPrivateProjects). It stays private, and only
//     its owner's switch to public changes its visibility.
//   - The keys on the list that no private project holds, provisioned but not private yet, are one
//     INFO line.
//   - With a list, a store a private project's reads would answer wrongly is one ERROR line naming
//     its first rows and the command that repairs it (api.StoreIncomplete); each switch to private
//     is refused until it is complete.
func reportPrivateProjects(ctx context.Context, database *store.Store, state *privateProjectsState) {
	api.LogUnprovisionedPrivateProjects(state.Unprovisioned())
	if len(state.list) == 0 {
		return
	}
	if notPrivate := state.provisionedNotPrivate(); len(notPrivate) > 0 {
		slog.Info("dispatch: private projects provisioned but not private yet", "projects", strings.Join(notPrivate, ","))
	}
	if err := api.StoreIncomplete(store.WithTransactionTracking(ctx), database.Pool); err != nil {
		slog.Error("dispatch: private projects: the store cannot hold a private project yet; every switch to private is refused until it can", "error", err)
	}
}

// privateProjectsReadInterval is how often privateProjectsState reads the private projects again,
// so no request waits on the database and a probe adds no read.
const privateProjectsReadInterval = 30 * time.Second

// privateProjectsState is the one cached read of the projects the database holds private, behind
// the Dispatch-Private-Projects header every response carries (privateProjectsHeader), /healthz's
// private_projects_unprovisioned and the boot's report. Its first read runs at boot, after the
// database's open and migrate and before the first loop and the repository seed, and a failure of
// it stops the boot (startPrivateProjectsState); it then reads every privateProjectsReadInterval
// in a goroutine of its own (run), where a failure counts as a private project. A nil state reads
// nothing and names no project.
type privateProjectsState struct {
	querier access.Querier
	list    []string

	mu         sync.Mutex
	anyPrivate bool
	// private is every key the last read found private, sorted; nil after a failed read.
	private []string
}

// startPrivateProjectsState makes the state's first read through q and, once it has answered,
// starts its later reads until ctx is done. A first read that fails answers its error and no
// state, and starts nothing: main logs it and exits 1, as it does when the database will not open,
// so the boot never serves on a value it did not read.
func startPrivateProjectsState(ctx context.Context, q access.Querier, list []string) (*privateProjectsState, error) {
	s := &privateProjectsState{querier: q, list: list}
	private, err := s.privateProjects(store.WithTransactionTracking(ctx))
	if err != nil {
		return nil, err
	}
	s.anyPrivate, s.private = len(private) > 0, private
	go s.run(ctx)
	return s, nil
}

// read is a later read of the private projects. A failed one logs a WARN, names no unprovisioned
// project, and counts as a private project, the closed direction.
func (s *privateProjectsState) read(ctx context.Context) {
	private, err := s.privateProjects(store.WithTransactionTracking(ctx))
	anyPrivate := len(private) > 0
	if err != nil {
		slog.Warn("dispatch: read of the private projects failed; every response says one is private", "error", err)
		anyPrivate, private = true, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.anyPrivate, s.private = anyPrivate, private
}

// privateProjects reads, in one statement, the keys of the projects the database holds private,
// sorted.
func (s *privateProjectsState) privateProjects(ctx context.Context) ([]string, error) {
	rows, err := s.querier.Query(ctx, `select key from projects where visibility = 'private' order by key`)
	if err != nil {
		return nil, fmt.Errorf("read the private projects: %w", err)
	}
	defer rows.Close()
	var private []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, fmt.Errorf("read the private projects: %w", err)
		}
		private = append(private, key)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the private projects: %w", err)
	}
	return private, nil
}

// provisionedNotPrivate answers the keys on the list that the last read did not find private: a key
// of no project, or of a public one.
func (s *privateProjectsState) provisionedNotPrivate() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var keys []string
	for _, key := range s.list {
		if _, found := slices.BinarySearch(s.private, key); !found {
			keys = append(keys, key)
		}
	}
	return keys
}

// run reads the private projects every privateProjectsReadInterval until ctx is done.
func (s *privateProjectsState) run(ctx context.Context) {
	ticker := time.NewTicker(privateProjectsReadInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.read(ctx)
		}
	}
}

// header is the Dispatch-Private-Projects value: 1 when the list names a project, without a read,
// or the last read found a private project or failed; 0 otherwise.
func (s *privateProjectsState) header() string {
	if len(s.list) > 0 {
		return "1"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.anyPrivate {
		return "1"
	}
	return "0"
}

// Unprovisioned answers, sorted, the private projects the list does not name, as the last read
// found them; nil for none, for a nil state, and after a failed read.
func (s *privateProjectsState) Unprovisioned() []string {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var keys []string
	for _, key := range s.private {
		if _, listed := slices.BinarySearch(s.list, key); !listed {
			keys = append(keys, key)
		}
	}
	return keys
}

// serveHandler is the one handler the HTTP server serves (newHTTPServer): h behind the response
// middleware every Dispatch response passes through. TestTheServerServesOnlyTheWrappedHandler names
// any server that serves another.
func serveHandler(h http.Handler, state *privateProjectsState) http.Handler {
	return privateProjectsHeader(h, state)
}

// privateProjectsHeader sets Dispatch-Private-Projects on every response of h before h runs, so an
// error answer, a 500 included, carries it. A websocket's 101 is written by its upgrader, which
// reads no header of the writer's; each upgrade site hands the value on (access.UpgradeHeader,
// docs.Service.ServeHTTP).
func privateProjectsHeader(h http.Handler, state *privateProjectsState) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(access.PrivateProjectsHeader, state.header())
		h.ServeHTTP(w, r)
	})
}
