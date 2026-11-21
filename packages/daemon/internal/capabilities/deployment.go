package capabilities

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/bootprobe"
)

// Deployment is what the daemon knows of its deployment's capabilities: what its configuration
// decides and sets, what boot learned of the image, the broker, the pod's Oh My Pi and the pool's
// room for the probe, what its live sessions reported of the live rows, and what its repository
// caches last said of their room. Report renders every row of Table from it. A gap is reported,
// never refused (LEGION-578, "The check"): a daemon that will not run its pods would itself keep
// workers from working.
type Deployment struct {
	// Decided is legion.yaml's capabilities.decided: the deployment rows the operator has decided,
	// each with the reason, which the report shows in the gap's place.
	Decided map[Name]string
	// Runtime is the daemon's runtime, "tmux" or "kubernetes".
	Runtime string
	// Probed is whether the worker image passed the daemon's probe (kubernetes), whose log carries
	// every image row's evidence; the image rows then read present.
	Probed bool
	// AgentSecrets is whether runtime.kubernetes.agent_secrets is configured.
	AgentSecrets bool
	// SecretsLogin is the broker login's state (cipher.LoginState.State: "none", "issued" or
	// "failed"); "" with no broker.
	SecretsLogin string
	// UnreservedPods are the pods the daemon builds that reserve no CPU and memory, those whose
	// entry under runtime.kubernetes.resources lacks CPU and memory requests and limits: the issue
	// pod and, under controller: daemon, the controller's pod, in that order (PodIssue,
	// PodController).
	UnreservedPods []Pod
	// BurstablePods are the pods among the same set whose cpu or memory request is below its limit
	// (config.Reservation.Burstable). Every other pod requests its limit, and the image probe its
	// own fixed reservation, so a deployment that names none runs Guaranteed pods alone.
	BurstablePods []Pod
	// ModelFallback is whether the pod's Oh My Pi falls back to another model,
	// bootprobe.ModelFallbackOn or bootprobe.ModelFallbackOff as the probe's OK line reports it
	// (bootprobe.ImageReport) or the plugin gate read it under tmux, bootprobe.ModelFallbackUnread
	// when the probe could not read it, and "" when nothing has read it.
	ModelFallback string
	// ProbeCapacityWaits is how many of the image probe's attempts at boot ended with its pod
	// Unschedulable — the pool had no room for the probe's reservation — before one scheduled
	// (bootprobe.ImageReport.CapacityWaits); zero when the first pod scheduled, and under tmux,
	// which runs no probe pod. ProbeCapacityReason is the scheduler's message on the last such
	// pod; "" with no wait.
	ProbeCapacityWaits  int
	ProbeCapacityReason string
	// Sessions is every session the daemon judges live — its claim ready, working or idle, and its
	// report's incarnation its claim's current Locator's — each with its report of the live rows
	// (LEGION-663). The daemon filters; Report renders a live row from them.
	Sessions []Session
	// Caches is every repository cache the runtime keeps, in its order (by repository), each as the
	// daemon's last health read of it found it, which the repository-cache-room row reads; none
	// under tmux, which runs no cache, and none in what the configuration alone states
	// (daemon.Deployment). The daemon fills it from memory alone (supervision.caches).
	Caches []Cache
}

// Cache is one repository cache as the daemon's last health read of it found it: the runtime's
// sandbox.CacheStatus and the repocache.Health it carries, which the daemon maps here in one place
// (supervision.caches), since this package imports neither — the cache's package imports config,
// which imports this one.
type Cache struct {
	// Repo is the repository the cache serves, as its annotation names it.
	Repo string
	// Claim is the name of the cache's volume claim: the one an operator grows in place.
	Claim string
	// Read is whether the daemon's last health read of the cache answered. A cache that is not
	// Ready is not read, and one whose /healthz could not be read keeps why (HealthError): either
	// is unread and counts for nothing, and the present detail counts it.
	Read bool
	// Reported is whether that answer carried the room fields a cache of this release sends: a
	// volume figure or the error of the statfs that read it, or a failure time. A cache of an
	// earlier release sends none, so it counts for nothing, and the present detail counts it.
	Reported bool
	// VolumeBytes is the volume's size and AvailableBytes the bytes available to the cache, both
	// zero where the cache's statfs failed, whose error VolumeError is.
	VolumeBytes    uint64
	AvailableBytes uint64
	VolumeError    string
	// Bytes is the clone's, as the cache's /tar streams it.
	Bytes int64
	// FetchOutOfSpaceAt and RepackOutOfSpaceAt are the time of that step's last failure for lack of
	// space since its last success, zero while there is none.
	FetchOutOfSpaceAt  time.Time
	RepackOutOfSpaceAt time.Time
}

// The statuses a State carries: an image row is present or missing, a live or a deployment row
// present, reported, decided or open, a withheld row withheld, and an unmeasured row unchecked
// (in the probe table, the browser row under `probe-image --skip-browser`).
// StatusMissing and StatusReported are the probe table's alone (CheckImage): a deployment row
// `legion state` carries (api.CapabilityState) is never missing or reported, since the daemon
// either measures it or has nothing yet to measure it with. StatusUnanswered is the probe table's
// alone too: an image row the end of the check's context (the probe's deadline) left with no
// verdict, a command of its check stopped by that end, or kept from starting, or the check never
// run, which says nothing of the image.
const (
	StatusPresent    = "present"
	StatusMissing    = "missing"
	StatusReported   = "reported"
	StatusWithheld   = "withheld"
	StatusUnchecked  = "unchecked"
	StatusUnanswered = "unanswered"
	StatusDecided    = "decided"
	StatusOpen       = "open"
)

// Source is where a deployment row's measurement came from.
type Source string

const (
	// SourceConfiguration is what legion.yaml alone decides: runtime.kubernetes.resources, and
	// whether a broker is configured at all (runtime.kubernetes.agent_secrets).
	SourceConfiguration Source = "configuration"
	// SourceBoot is what boot measured: the probe's OK line, the plugin gate's read under tmux,
	// the broker login, and the pool's room for the probe at boot.
	SourceBoot Source = "boot"
	// SourceCaches is what the repository caches' last health reads said of their room: each
	// volume's size and available bytes, and each step's last failure for lack of space
	// (Deployment.Caches).
	SourceCaches Source = "caches"
)

// State is one row of the deployment's report, as `legion state` carries it (api.CapabilityState),
// and of the probe's table (CheckImage): one row type on every surface.
type State struct {
	Name   Name
	Status string
	// Detail is the row's evidence: what the probe checked, what the live sessions reported, the
	// cited issue, or the deployment's measurement — whatever the status.
	Detail string
	// Decision is the operator's reason on a decided row, "" otherwise.
	Decision string
	// ConfigLine is, on an open row, the legion.yaml line that records a decision:
	// `capabilities.decided.<name>: "<reason>"`; "" otherwise.
	ConfigLine string
	// Source is, on a deployment row, where its measurement came from; "" on every other row.
	Source Source
}

// String is the line as the probe prints it, and as the probe pod's log then carries it.
func (s State) String() string {
	return fmt.Sprintf("probe-image: capability %s: %s (%s)", s.Name, s.Status, s.Detail)
}

// OpenLine is the sentence an open row is logged with (Log) and `legion config check`
// prints: the gap and, on a deployment row, the line that records a decision. A live row has no
// such line: a session's failing check is a fact to fix, not a gap an operator decides.
func (s State) OpenLine() string {
	if s.ConfigLine == "" {
		return fmt.Sprintf("capability %s is open: %s", s.Name, s.Detail)
	}
	return fmt.Sprintf("capability %s is open: %s; to record a decision, add to legion.yaml: %s", s.Name, s.Detail, s.ConfigLine)
}

// Report renders every row of Table, in its order. An image row is present once the image passed
// the probe, else unchecked; a live row is open when a live session's check of it failed, present
// when none failed and one proved it, and unchecked when no session has reported; a withheld row
// carries its citation (as CheckImage renders it); a deployment row is present when the deployment
// satisfies it, decided when legion.yaml records a decision on it, and open otherwise, with the
// line that records one.
func (d Deployment) Report() []State {
	// The renderers run no command, and the walk's context never ends, so no image row is
	// unanswered.
	return render(context.Background(), renderer{image: d.image, live: d.live, deployment: d.deployment})
}

// Open is the rows whose status is open, in Table order: the deployment gaps with no decision, and
// the live rows a live session's check failed.
func (d Deployment) Open() []Name {
	var open []Name
	for _, state := range d.openStates() {
		open = append(open, state.Name)
	}
	return open
}

// OpenFromConfiguration is the open rows whose Source is SourceConfiguration, in Table order: the
// configuration's own gaps, whatever row they fall on, which `legion config check` prints.
// Boot's measurements are not its to say — a configured broker's login, model fallback, which the
// probe or the plugin gate reads, and pool capacity, which the probe's run measures — nor are the
// repository caches', which their health reads measure.
func (d Deployment) OpenFromConfiguration() []State {
	var open []State
	for _, state := range d.openStates() {
		if state.Source == SourceConfiguration {
			open = append(open, state)
		}
	}
	return open
}

// Log writes one warning per open row: the gap and, on a deployment row, the legion.yaml line that
// records a decision; a live row's configLine is "".
func (d Deployment) Log(log *slog.Logger) {
	for _, state := range d.openStates() {
		log.Warn(state.OpenLine(), "capability", string(state.Name), "detail", state.Detail, "configLine", state.ConfigLine, "source", string(state.Source))
	}
}

// openStates is the open rows of Report, in Table order: a deployment row with no decision, or a
// live row a live session's check failed; no image or withheld row is ever open.
func (d Deployment) openStates() []State {
	var open []State
	for _, state := range d.Report() {
		if state.Status == StatusOpen {
			open = append(open, state)
		}
	}
	return open
}

// image is one image row's state: present once the probe passed, which checked each; unchecked
// where nothing has checked — the tmux runtime runs the host's tools, which no probe checks, and a
// kubernetes daemon's probe may not have reported yet.
func (d Deployment) image(row Capability) State {
	switch {
	case d.Probed:
		return State{Name: row.Name, Status: StatusPresent, Detail: "checked by the daemon's probe of the worker image, which passed"}
	case d.Runtime == "tmux":
		return State{Name: row.Name, Status: StatusUnchecked, Detail: "the tmux runtime runs the host's tools, which no probe checks"}
	default:
		return State{Name: row.Name, Status: StatusUnchecked, Detail: "no probe has reported yet"}
	}
}

// live is one live row's state from the live sessions' reports. Open when any session's check of
// it failed: each failing session, in Sessions order, as its label and the fact its check found,
// the first three then how many more. Present when none failed and at least one proved it: how
// many did, and the latest measurement — the greatest MeasuredAt, the first in Sessions order on
// a tie — with its fact. Unchecked when no live session has reported: the citation whose check
// proves the row, and the summary.
func (d Deployment) live(row Capability) State {
	state := State{Name: row.Name}
	var named []string
	failed, proved, latest := 0, 0, -1
	for i, session := range d.Sessions {
		measured, ok := session.row(row.Name)
		switch {
		case !ok:
			continue
		case !measured.OK:
			// The first three failing sessions are named; the rest are counted, so a swarm's worth
			// of sessions never fills the line.
			if failed++; failed <= maxFailingNamed {
				named = append(named, session.label()+": "+measured.Detail)
			}
			continue
		}
		proved++
		if latest < 0 || session.Report.MeasuredAt.After(d.Sessions[latest].Report.MeasuredAt) {
			latest = i
		}
	}
	switch {
	case failed > 0:
		state.Status, state.Detail = StatusOpen, strings.Join(named, "; ")
		if more := failed - len(named); more > 0 {
			state.Detail += fmt.Sprintf(" +%d more", more)
		}
	case proved > 0:
		session := d.Sessions[latest]
		measured, _ := session.row(row.Name)
		state.Status = StatusPresent
		state.Detail = fmt.Sprintf("proved by %d live session(s); latest %s measured %s: %s",
			proved, session.label(), session.Report.MeasuredAt.Format(time.RFC3339), measured.Detail)
	default:
		state.Status, state.Detail = StatusUnchecked, "no session has reported yet ("+row.Citation+"): "+row.Summary
	}
	return state
}

// maxFailingNamed is how many failing sessions an open live row's detail names before it counts
// the rest.
const maxFailingNamed = 3

// row is s's measurement of name, and whether its report carries one.
func (s Session) row(name Name) (Row, bool) {
	for _, row := range s.Report.Rows {
		if row.Name == name {
			return row, true
		}
	}
	return Row{}, false
}

// label is how a live row's detail names s: its process (runtime.Locator.Label), then its claim's
// role and issue.
func (s Session) label() string {
	return fmt.Sprintf("%s (%s, %s)", s.Report.Locator.Label(), s.Role, s.Issue)
}

// withheldDetail is a withheld row's detail as CheckImage renders it: the citation, the summary.
func withheldDetail(row Capability) string { return row.Citation + ": " + row.Summary }

// deployment is one deployment row: its measurement, and the status that follows. A decision on a
// row the deployment satisfies is moot — the measurement stands, and the row reads present.
func (d Deployment) deployment(row Capability) State {
	satisfied, source, detail := d.measure(row.Name)
	state := State{Name: row.Name, Detail: detail, Source: source}
	switch {
	case satisfied:
		state.Status = StatusPresent
	case d.Decided[row.Name] != "":
		state.Status, state.Decision = StatusDecided, d.Decided[row.Name]
	default:
		state.Status, state.ConfigLine = StatusOpen, configLine(row.Name)
	}
	return state
}

// configLine is the legion.yaml line that records a decision on name.
func configLine(name Name) string {
	return fmt.Sprintf("capabilities.decided.%s: \"<reason>\"", name)
}

// measure is one deployment row's measurement: whether the deployment satisfies it, where that
// measurement came from, and the detail that says how.
func (d Deployment) measure(name Name) (bool, Source, string) {
	tmux := d.Runtime == "tmux"
	switch name {
	case Secrets:
		switch {
		case tmux:
			return false, SourceConfiguration, "the tmux runtime enrolls no process with the secrets broker"
		case !d.AgentSecrets:
			return false, SourceConfiguration, "runtime.kubernetes.agent_secrets is not configured"
		case d.SecretsLogin == "issued":
			return true, SourceBoot, "every pod is enrolled with the agent-secrets broker runtime.kubernetes.agent_secrets names, and the daemon's login is issued"
		default:
			return false, SourceBoot, fmt.Sprintf("the agent-secrets login is %s, not issued", d.SecretsLogin)
		}
	case ModelFallback:
		where, reader := "the pod's Oh My Pi settings (read by the probe)", "no probe has reported"
		if tmux {
			where, reader = "the host's Oh My Pi settings (read by the plugin gate)", "the plugin gate could not read"
		}
		switch d.ModelFallback {
		case bootprobe.ModelFallbackOn:
			return true, SourceBoot, "retry.modelFallback is true under " + where
		case bootprobe.ModelFallbackOff:
			return false, SourceBoot, "retry.modelFallback is false under " + where
		case bootprobe.ModelFallbackUnread:
			return false, SourceBoot, "not read: the probe could not read retry.modelFallback under the pod's Oh My Pi settings (the probe pod's log says why)"
		default:
			return false, SourceBoot, "not read: " + reader + " retry.modelFallback"
		}
	case ResourceLimits:
		switch {
		case tmux:
			return false, SourceConfiguration, "the tmux runtime sets no requests or limits on a pane"
		case len(d.UnreservedPods) > 0:
			return false, SourceConfiguration, "no CPU and memory requests and limits under runtime.kubernetes.resources for " + joinPods(d.UnreservedPods)
		case len(d.BurstablePods) == 0:
			return true, SourceConfiguration, "every pod has CPU and memory requests and limits under runtime.kubernetes.resources, each request equal to its limit, so every pod is Guaranteed"
		default:
			return true, SourceConfiguration, "every pod has CPU and memory requests and limits under runtime.kubernetes.resources; " + d.BurstableLine()
		}
	case PoolCapacity:
		switch {
		case tmux:
			return true, SourceConfiguration, "the tmux runtime runs no probe pod"
		case d.ProbeCapacityWaits == 0:
			return true, SourceBoot, "the image probe pod scheduled at its first attempt"
		default:
			return false, SourceBoot, fmt.Sprintf("the image probe pod was Unschedulable %d time(s) at boot before it scheduled: %s; the pool had no room for its 500m / 1Gi reservation",
				d.ProbeCapacityWaits, d.ProbeCapacityReason)
		}
	case RepositoryCacheRoom:
		satisfied, detail := d.cacheRoom()
		return satisfied, SourceCaches, detail
	}
	// Table names no other deployment row; TestEveryDeploymentRowIsMeasured holds it to this switch.
	return false, "", "no measurement"
}

// cacheRoom is the repository-cache-room row's measurement, from each cache's last health read.
// It is satisfied while no cache that reports its room carries a failure for lack of space, and
// its detail then counts the caches read, those among them of an earlier release, which send no
// room figures, and those unread, neither of which opens it. A cache of a repository the cache
// set no longer names counts like any other while it stays. Otherwise its detail names each failing
// cache (outOfRoom), then where room comes from: growing the claim in place gives that cache room
// now, and runtime.kubernetes.repository_cache.volume sizes the caches the daemon makes later. The
// tmux runtime runs no cache, and a Kubernetes deployment with none has nothing to report.
func (d Deployment) cacheRoom() (bool, string) {
	if d.Runtime == "tmux" {
		return true, "the tmux runtime runs no repository cache"
	}
	if len(d.Caches) == 0 {
		return true, "no repository cache is running"
	}
	var failing []string
	read, silent, unread := 0, 0, 0
	for _, cache := range d.Caches {
		switch {
		case !cache.Read:
			unread++
			continue
		case !cache.Reported:
			silent++
		}
		read++
		if !cache.FetchOutOfSpaceAt.IsZero() || !cache.RepackOutOfSpaceAt.IsZero() {
			failing = append(failing, cache.outOfRoom())
		}
	}
	if len(failing) > 0 {
		return false, fmt.Sprintf("%d of %d repository caches ran out of room: %s. Room comes from growing the cache's claim in place and raising runtime.kubernetes.repository_cache.volume",
			len(failing), len(d.Caches), strings.Join(failing, ". "))
	}
	counts := []string{counted(read, "cache read", "caches read")}
	if silent > 0 {
		counts = append(counts, counted(silent, "sends no room figures", "send no room figures"))
	}
	if unread > 0 {
		counts = append(counts, fmt.Sprintf("%d unread", unread))
	}
	return true, "no repository cache reports a failure for lack of space (" + strings.Join(counts, ", ") + ")"
}

// outOfRoom is how the open repository-cache-room row names c: its repository, its volume's size
// with its claim and the bytes available, or why the cache could not read them, its clone's bytes,
// and each step that failed for lack of space with that failure's time. Sizes are in GiB with one
// decimal and times in RFC 3339 UTC. The statfs error is the cache's own text, so it is cleaned as
// a session's detail is (cleanDetail): one line, no credential.
func (c Cache) outOfRoom() string {
	volume := fmt.Sprintf("on a %s volume (claim %s) with %s available", gib(float64(c.VolumeBytes)), c.Claim, gib(float64(c.AvailableBytes)))
	if c.VolumeError != "" {
		volume = fmt.Sprintf("on a volume (claim %s) whose room it could not read (%s)", c.Claim, cleanDetail(c.VolumeError))
	}
	var steps []string
	for _, step := range []struct {
		name string
		at   time.Time
	}{{"fetch", c.FetchOutOfSpaceAt}, {"repack", c.RepackOutOfSpaceAt}} {
		if !step.at.IsZero() {
			steps = append(steps, "its "+step.name+" failed for lack of space at "+step.at.UTC().Format(time.RFC3339))
		}
	}
	return fmt.Sprintf("%s %s, its clone %s; %s", c.Repo, volume, gib(float64(c.Bytes)), strings.Join(steps, " and "))
}

// gib is bytes in GiB with one decimal, as the repository-cache-room row prints a size.
func gib(bytes float64) string { return fmt.Sprintf("%.1f GiB", bytes/(1<<30)) }

// counted is n and the words that follow it: one when n is 1, many otherwise.
func counted(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// Pod is one of the pods the daemon builds as the resource-limits row names it.
type Pod string

// The pods the resource-limits row reads: every issue pod, whose one container runs every workflow
// role (runtime.kubernetes.resources.issue), and the controller's pod the daemon launches under
// controller: daemon (runtime.kubernetes.resources.controller).
const (
	PodIssue      Pod = "the issue pod"
	PodController Pod = "the controller's pod"
)

// BurstableLine names the Burstable pods and says every other pod is Guaranteed; "" when no pod
// requests less than its limit. The resource-limits row's detail carries it, and `legion config
// check` prints it after Config OK.
func (d Deployment) BurstableLine() string {
	if len(d.BurstablePods) == 0 {
		return ""
	}
	return "Burstable (a request below the limit): " + joinPods(d.BurstablePods) + "; every other pod is Guaranteed"
}

// joinPods is pods as a list joined by "and", in their order.
func joinPods(pods []Pod) string {
	names := make([]string, len(pods))
	for i, pod := range pods {
		names[i] = string(pod)
	}
	return strings.Join(names, " and ")
}
