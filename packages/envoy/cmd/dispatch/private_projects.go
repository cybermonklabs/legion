package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/api"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The two settings that name which projects may be private: DISPATCH_PRIVATE_PROJECTS, the
// projects the deployment has provisioned as private, whose switch to private is their full switch,
// and DISPATCH_PRIVATE_BEFORE_FULL_SWITCH, the projects it makes private before their full switch
// (spec "Day one and the full switch"). Each is read by privateProjectsSetting.
const (
	privateProjectsVariable         = "DISPATCH_PRIVATE_PROJECTS"
	privateBeforeFullSwitchVariable = "DISPATCH_PRIVATE_BEFORE_FULL_SWITCH"
)

// privateProjectsSetting reads DISPATCH_PRIVATE_PROJECTS or DISPATCH_PRIVATE_BEFORE_FULL_SWITCH:
// the comma-separated keys of the projects this deployment has provisioned as private (the
// infrastructure repository's privateprojects.Projects()), or of those it makes private before
// their full switch, each entry trimmed and empty ones dropped, deduplicated and sorted. An entry
// that is no project key, the old on/off value 1 included, is never read as a key: it is answered
// in ignored, in the order given, and the boot logs it (reportIgnoredPrivateProjects). Ignoring it
// fails closed: it names no project, so the switch to private of the project it meant is refused
// while no list names its key; refusing the boot would stop every project for one bad entry.
// Unset or empty names no project.
func privateProjectsSetting(value string) (keys, ignored []string) {
	for entry := range strings.SplitSeq(value, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if !contracts.ValidProjectKey(entry) {
			ignored = append(ignored, entry)
			continue
		}
		keys = append(keys, entry)
	}
	slices.Sort(keys)
	return slices.Compact(keys), ignored
}

// reportIgnoredPrivateProjects logs one ERROR line for each entry of setting, one of the two
// settings above, that is no project key (privateProjectsSetting), naming the setting, the entry
// and what it costs.
func reportIgnoredPrivateProjects(setting string, ignored []string) {
	for _, entry := range ignored {
		slog.Error("dispatch: "+setting+" names project keys, and this entry is not one, so it is ignored: no project is named by it, "+
			"and the switch to private of the project it meant is refused while neither "+privateProjectsVariable+" nor "+privateBeforeFullSwitchVariable+
			" names its key: 409 PRIVATE_PROJECTS_OFF while both are empty, else 409 PRIVATE_PROJECT_NOT_PROVISIONED",
			"setting", setting, "entry", entry, "rule", contracts.ProjectKeyRule)
	}
}

// reportPrivateProjects is the boot's report of what the state's first read (state) says of
// private projects, right after every project has an owner. It makes no read of its own. It
// reports and never refuses: Dispatch runs as one process, so a boot that exited would stop every
// project, and the owner's repair through the API with it, while each condition here fails closed
// for its own projects alone.
//   - Each project the database holds private that neither list names is one ERROR line naming it
//     and both repairs (api.LogUnprovisionedPrivateProjects). It stays private, and only its
//     owner's switch to public changes its visibility.
//   - The keys on DISPATCH_PRIVATE_PROJECTS that no private project holds, provisioned but not
//     private yet, are one INFO line.
//   - The projects private before their full switch (BeforeFullSwitch) are one INFO line naming
//     each as such, so no reader of the log takes one for a fully switched project; the keys on
//     DISPATCH_PRIVATE_BEFORE_FULL_SWITCH that no private project holds yet are another.
//
// The store's own check, with either list, is the maintenance loop's as it starts and then hourly
// (api.RunMaintenanceSweep): it reads every project-owned table, so the boot serves without
// waiting for it, and its first line is not repeated a pass later.
func reportPrivateProjects(state *privateProjectsState) {
	api.LogUnprovisionedPrivateProjects(state.Unprovisioned())
	if notPrivate := state.notPrivate(state.list); len(notPrivate) > 0 {
		slog.Info("dispatch: private projects provisioned but not private yet", "projects", strings.Join(notPrivate, ","))
	}
	if before := state.BeforeFullSwitch(); len(before) > 0 {
		slog.Info("dispatch: private before its full switch: Dispatch hides each of these projects as any private project, "+
			"while its namespace, its stores and its bus gate come with its full switch ("+privateBeforeFullSwitchVariable+")",
			"projects", strings.Join(before, ","))
	}
	if notPrivate := state.notPrivate(state.beforeFullSwitch); len(notPrivate) > 0 {
		slog.Info("dispatch: may be made private before its full switch, not private yet ("+privateBeforeFullSwitchVariable+")",
			"projects", strings.Join(notPrivate, ","))
	}
}

// privateProjectsReadInterval is how often privateProjectsState reads the private projects again,
// so no request waits on the database and a probe adds no read.
const privateProjectsReadInterval = 30 * time.Second

// privateProjectsState is the one cached read of the projects the database holds private, behind
// the Dispatch-Private-Projects header every response carries (privateProjectsHeader), /healthz's
// degraded reasons (never a key: unprovisionedPrivateProjectReason and
// privateBeforeFullSwitchReason) and the boot's report. The keys appear in two places only, both
// the operator's log: the boot's report and each pass of the maintenance loop
// (api.RunMaintenanceSweep), which reads them itself. Its first read runs at boot, after the
// database's open and migrate and before the first loop and the repository seed, and a failure of
// it stops the boot (startPrivateProjectsState); it then reads every privateProjectsReadInterval
// in a goroutine of its own (run), where a failure counts as a private project. Unprovisioned and
// BeforeFullSwitch answer a nil state as naming no project, which /healthz reads for a handler
// built with none; every other method needs the state main builds.
type privateProjectsState struct {
	querier access.Querier
	// list is DISPATCH_PRIVATE_PROJECTS and beforeFullSwitch DISPATCH_PRIVATE_BEFORE_FULL_SWITCH,
	// each sorted and each key once, and disjoint: the boot takes a key both settings name off
	// beforeFullSwitch, since it takes its full switch (resolveBootConfig).
	list, beforeFullSwitch []string
	// last is the last read, published whole, so a response's header reads it without a lock.
	last atomic.Pointer[privateProjectsRead]
}

// privateProjectsRead is one read of the private projects: whether it found one or failed, and
// every key it found private, sorted; nil after a failed read.
type privateProjectsRead struct {
	anyPrivate bool
	private    []string
}

// startPrivateProjectsState makes the state's first read through q and, once it has answered,
// starts its later reads until ctx is done. A first read that fails answers its error and no
// state, and starts nothing: main logs it and exits 1, as it does when the database will not open,
// so the boot never serves on a value it did not read.
func startPrivateProjectsState(ctx context.Context, q access.Querier, list, beforeFullSwitch []string) (*privateProjectsState, error) {
	s := &privateProjectsState{querier: q, list: list, beforeFullSwitch: beforeFullSwitch}
	private, err := s.privateProjects(store.WithTransactionTracking(ctx))
	if err != nil {
		return nil, err
	}
	s.last.Store(&privateProjectsRead{anyPrivate: len(private) > 0, private: private})
	go s.run(ctx)
	return s, nil
}

// read is a later read of the private projects. A failed one logs a WARN, names no unprovisioned
// project, and counts as a private project, the closed direction.
func (s *privateProjectsState) read(ctx context.Context) {
	private, err := s.privateProjects(store.WithTransactionTracking(ctx))
	if err != nil {
		slog.Warn("dispatch: read of the private projects failed; every response says one is private", "error", err)
		s.last.Store(&privateProjectsRead{anyPrivate: true})
		return
	}
	s.last.Store(&privateProjectsRead{anyPrivate: len(private) > 0, private: private})
}

// privateProjects reads, in one statement, the keys of the projects the database holds private,
// sorted.
func (s *privateProjectsState) privateProjects(ctx context.Context) ([]string, error) {
	rows, err := s.querier.Query(ctx, `select key from projects where visibility = 'private' order by key`)
	if err != nil {
		return nil, fmt.Errorf("read the private projects: %w", err)
	}
	private, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("read the private projects: %w", err)
	}
	return private, nil
}

// notPrivate answers the keys of keys, list or beforeFullSwitch, that the last read did not find
// private: a key of no project, or of a public one.
func (s *privateProjectsState) notPrivate(keys []string) []string {
	last := s.last.Load()
	var notPrivate []string
	for _, key := range keys {
		if _, private := slices.BinarySearch(last.private, key); !private {
			notPrivate = append(notPrivate, key)
		}
	}
	return notPrivate
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

// header is the Dispatch-Private-Projects value: 1 when either list names a project, without a
// read, or the last read found a private project or failed; 0 otherwise.
func (s *privateProjectsState) header() string {
	if len(s.list) > 0 || len(s.beforeFullSwitch) > 0 || s.last.Load().anyPrivate {
		return "1"
	}
	return "0"
}

// unprovisionedPrivateProjectReason is the one reason /healthz's degraded field gives while the
// database holds a private project neither list names. It is fixed: /healthz answers a caller
// with no credential, so it carries no key and no count, which would tell that caller of projects
// it cannot see (R2). The keys are the operator's, in reportPrivateProjects' ERROR line.
const unprovisionedPrivateProjectReason = "private_project_unprovisioned"

// privateBeforeFullSwitchReason is the reason /healthz's degraded field gives while the database
// holds a project private before its full switch (BeforeFullSwitch): Dispatch hides it, while the
// walls its full switch brings (its namespace, its stores, the bus gate) are not up yet. It is
// fixed, for unprovisionedPrivateProjectReason's reason: the keys are the operator's, in
// reportPrivateProjects' INFO line, and the project's own people read the state in its access
// (private_before_full_switch).
const privateBeforeFullSwitchReason = "private_before_full_switch"

// Unprovisioned answers, sorted, the private projects neither list names, as the last read found
// them; nil for none, for a nil state, and after a failed read.
func (s *privateProjectsState) Unprovisioned() []string {
	if s == nil {
		return nil
	}
	var keys []string
	for _, key := range s.last.Load().private {
		_, provisioned := slices.BinarySearch(s.list, key)
		if _, before := slices.BinarySearch(s.beforeFullSwitch, key); !provisioned && !before {
			keys = append(keys, key)
		}
	}
	return keys
}

// BeforeFullSwitch answers, sorted, the private projects DISPATCH_PRIVATE_BEFORE_FULL_SWITCH
// names, as the last read found them: the projects private before their full switch. nil for
// none, for a nil state, and after a failed read.
func (s *privateProjectsState) BeforeFullSwitch() []string {
	if s == nil {
		return nil
	}
	var keys []string
	for _, key := range s.last.Load().private {
		if _, before := slices.BinarySearch(s.beforeFullSwitch, key); before {
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
