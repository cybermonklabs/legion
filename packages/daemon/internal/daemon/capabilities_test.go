package daemon

import (
	"bytes"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/bootprobe"
	"github.com/sjawhar/legion/daemon/internal/capabilities"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/repocache"
	"github.com/sjawhar/legion/daemon/internal/runtime/sandbox"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// reportCapabilities logs the open rows once at boot and again only when the set changes, and
// answers the names each time: the tick carries them while the log stays quiet. Under tmux every
// deployment row starts open; the model-fallback read closing one changes the set.
func TestReportCapabilitiesLogsAtBootAndOnChangeAlone(t *testing.T) {
	t.Parallel()
	var logged bytes.Buffer
	s := &supervision{
		cfg:         config.Config{Runtime: config.Runtime{Name: "tmux"}},
		log:         slog.New(slog.NewTextHandler(&logged, nil)),
		refreshGate: supervise.NewRefreshGate(0),
	}
	warnings := func() int { return strings.Count(logged.String(), "level=WARN") }

	if got, want := s.reportCapabilities(), []string{"secrets", "model-fallback", "resource-limits"}; !slices.Equal(got, want) {
		t.Fatalf("reportCapabilities at boot = %v, want %v", got, want)
	}
	if warnings() != 3 {
		t.Fatalf("boot logged %d warnings, want one per open row (3):\n%s", warnings(), logged.String())
	}
	for _, want := range []string{
		`msg="capability secrets is open: the tmux runtime enrolls no process with the secrets broker; to record a decision, add to legion.yaml: capabilities.decided.secrets: \"<reason>\""`,
		`configLine="capabilities.decided.model-fallback: \"<reason>\""`,
	} {
		if !strings.Contains(logged.String(), want) {
			t.Errorf("the boot report lacks %s:\n%s", want, logged.String())
		}
	}

	s.reportCapabilities()
	if warnings() != 3 {
		t.Fatalf("an unchanged set logged again (%d warnings):\n%s", warnings(), logged.String())
	}

	s.imageReport.ModelFallback = "on"
	if got, want := s.reportCapabilities(), []string{"secrets", "resource-limits"}; !slices.Equal(got, want) {
		t.Fatalf("reportCapabilities after the read = %v, want %v", got, want)
	}
	if warnings() != 5 {
		t.Fatalf("the changed set logged %d warnings in all, want 3 then 2 (5):\n%s", warnings(), logged.String())
	}
	if strings.Count(logged.String(), "capability model-fallback is open") != 1 {
		t.Fatalf("model-fallback was logged after it closed:\n%s", logged.String())
	}
}

// What the probe's run saw of the pool reaches the report: a kubernetes daemon whose probe pod
// waited on capacity before it scheduled reports pool-capacity open, with the count and the
// scheduler's reason, until the file decides it; one whose pod scheduled at once reports it present.
func TestReportCapabilitiesCarriesTheProbesCapacityWaits(t *testing.T) {
	t.Parallel()
	reason := "probe pod legion-probe-legion-1d10089a0000 is Unschedulable: 0/83 nodes are available: 25 Insufficient cpu"
	decided := map[capabilities.Name]string{capabilities.Secrets: "dispatch://LEGION-205 enrolls pods later"}
	cfg := config.Config{
		Runtime:      config.Runtime{Name: "kubernetes", Kubernetes: &config.Kubernetes{Resources: config.DefaultResources()}},
		Capabilities: config.Capabilities{Decided: decided},
	}
	var logged bytes.Buffer
	s := &supervision{cfg: cfg, log: slog.New(slog.NewTextHandler(&logged, nil)), probed: true,
		imageReport: bootprobe.ImageReport{ModelFallback: "on", CapacityWaits: 2, CapacityReason: reason},
		refreshGate: supervise.NewRefreshGate(0)}

	if got, want := s.reportCapabilities(), []string{"pool-capacity"}; !slices.Equal(got, want) {
		t.Fatalf("reportCapabilities = %v, want %v", got, want)
	}
	if want := "the image probe pod was Unschedulable 2 time(s) at boot before it scheduled: " + reason; !strings.Contains(logged.String(), want) {
		t.Fatalf("the open row was logged without the count and the reason:\n%s", logged.String())
	}

	decided[capabilities.PoolCapacity] = "the pool is sized for the trees; the probe waits"
	if got := s.reportCapabilities(); len(got) != 0 {
		t.Fatalf("reportCapabilities with the row decided = %v, want none open", got)
	}

	s.imageReport.CapacityWaits, s.imageReport.CapacityReason = 0, ""
	if got := states(s.deployment().Report())[capabilities.PoolCapacity]; got.Status != capabilities.StatusPresent || got.Detail != "the image probe pod scheduled at its first attempt" {
		t.Fatalf("pool-capacity with no wait = %s (%s), want present", got.Status, got.Detail)
	}
}

// The report the tick reads (reportCapabilities) reads each repository cache's last health read,
// through the runtime's caches (cacheStatuses). While no cache's read carries a failure for lack of
// space, repository-cache-room is not among the open names, and the state's row, which the state
// route renders from the caches it read (deploymentWith), is present, counting the caches read, its
// source the caches. Once a cache's last read carries its repack's failure, the name is open and
// the gap is logged once, on one line naming the cache's repository and its claim with the row's
// config line and its source, however many ticks read it; once the read no longer carries it, the
// name is gone. The row's own words are TestTheRepositoryCacheRoomRow's (capabilities).
func TestTheCapabilityReportReadsTheCachesHealth(t *testing.T) {
	t.Parallel()
	cfg := config.Config{
		Runtime:      config.Runtime{Name: "kubernetes", Kubernetes: &config.Kubernetes{Resources: config.DefaultResources()}},
		Capabilities: config.Capabilities{Decided: map[capabilities.Name]string{capabilities.Secrets: "dispatch://LEGION-205 enrolls pods later"}},
	}
	volume, roomy, full := uint64(20<<30), uint64(12<<30), uint64(107374182)
	repackFailed := time.Date(2026, 11, 19, 2, 15, 33, 0, time.UTC)
	const claimName = "cache-legion-legion-cache-acme-widgets-d782c874"
	gadgets := sandbox.CacheStatus{Repo: ghrepo.MustParse("acme/gadgets"), Sandbox: "legion-legion-cache-acme-gadgets-1b2c3d4e", State: "ready", Ready: true,
		Claim: "cache-legion-legion-cache-acme-gadgets-1b2c3d4e", Health: &repocache.Health{Ready: true, Repo: "acme/gadgets", Bytes: 3 << 30, VolumeBytes: &volume, AvailableBytes: &roomy}}
	widgets := func(health repocache.Health) sandbox.CacheStatus {
		health.Ready, health.Repo, health.Bytes, health.VolumeBytes = true, "acme/widgets", 20830591386, &volume
		return sandbox.CacheStatus{Repo: ghrepo.MustParse("acme/widgets"), Sandbox: "legion-legion-cache-acme-widgets-d782c874", State: "ready", Ready: true,
			Claim: claimName, Health: &health}
	}
	var statuses []sandbox.CacheStatus
	var logged bytes.Buffer
	s := &supervision{cfg: cfg, log: slog.New(slog.NewTextHandler(&logged, nil)), probed: true,
		imageReport: bootprobe.ImageReport{ModelFallback: "on"}, refreshGate: supervise.NewRefreshGate(0),
		cacheStatuses: func() []sandbox.CacheStatus { return statuses }}
	const opened = "capability repository-cache-room is open: "

	statuses = []sandbox.CacheStatus{gadgets, widgets(repocache.Health{AvailableBytes: &roomy})}
	if got := s.reportCapabilities(); slices.Contains(got, string(capabilities.RepositoryCacheRoom)) {
		t.Fatalf("reportCapabilities with no cache out of room = %v, want no %s", got, capabilities.RepositoryCacheRoom)
	}
	row := states(s.deploymentWith(statuses).Report())[capabilities.RepositoryCacheRoom]
	if row.Status != capabilities.StatusPresent || row.Source != capabilities.SourceCaches || !strings.Contains(row.Detail, "(2 caches read)") {
		t.Fatalf("the state's row with no cache out of room = %s from %q (%s), want present from the caches, counting the two read", row.Status, row.Source, row.Detail)
	}

	statuses = []sandbox.CacheStatus{gadgets, widgets(repocache.Health{AvailableBytes: &full, RepackOutOfSpaceAt: repackFailed})}
	for tick := range 2 {
		if got, want := s.reportCapabilities(), []string{string(capabilities.RepositoryCacheRoom)}; !slices.Equal(got, want) {
			t.Fatalf("reportCapabilities at tick %d with a cache whose repack ran out of room = %v, want %v", tick, got, want)
		}
	}
	if count := strings.Count(logged.String(), opened); count != 1 {
		t.Fatalf("the open row was logged %d times over two ticks, want once:\n%s", count, logged.String())
	}
	var line string
	for logLine := range strings.Lines(logged.String()) {
		if strings.Contains(logLine, opened) {
			line = logLine
		}
	}
	for _, want := range []string{
		"acme/widgets",
		claimName,
		`configLine="capabilities.decided.repository-cache-room: \"<reason>\""`,
		`source=caches`,
	} {
		if !strings.Contains(line, want) {
			t.Errorf("the open row's warning lacks %s:\n%s", want, logged.String())
		}
	}

	statuses = []sandbox.CacheStatus{gadgets, widgets(repocache.Health{AvailableBytes: &roomy})}
	if got := s.reportCapabilities(); slices.Contains(got, string(capabilities.RepositoryCacheRoom)) {
		t.Fatalf("reportCapabilities once the read no longer carries the failure = %v, want no %s", got, capabilities.RepositoryCacheRoom)
	}
	if count := strings.Count(logged.String(), opened); count != 1 {
		t.Fatalf("the closed row was logged again (%d times in all):\n%s", count, logged.String())
	}
}

// states is a report by capability.
func states(report []capabilities.State) map[capabilities.Name]capabilities.State {
	m := map[capabilities.Name]capabilities.State{}
	for _, state := range report {
		m[state.Name] = state
	}
	return m
}
