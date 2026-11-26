// Package shimwire is the wire between a phase worker's `legion worker-shim` and the daemon:
// the NDJSON framing both ends read and write, the frames they exchange, the rpc_chunk transport
// for a frame too large for one line, and the shim-side delivery dedupe.
//
// It is shared by `cmd/legion worker-shim` and `internal/stream`, and it is where the protocol's
// behaviour is kept rather than re-derived at each end. Every rule here was ported from the
// TypeScript daemon LEGION-223 removed; a `worker-rpc.ts:N`, `line-reader.ts:N`,
// `socket-writer.ts:N`, `worker-stream-listener.ts:N` or `worker-shim.ts:N` citation at a rule
// names that source in this repository's history. The structure is Go's.
package shimwire

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/redact"
)

// The frame type names, verbatim from the shipped protocol.
const (
	TypeHello                  = "hello"
	TypeHelloAck               = "hello_ack"
	TypeNegotiateProtocol      = "negotiate_protocol"
	TypePrompt                 = "prompt"
	TypeResponse               = "response"
	TypeGetState               = "get_state"
	TypeHandoff                = "handoff"
	TypeAgentStart             = "agent_start"
	TypeAgentEnd               = "agent_end"
	TypeToolExecutionStart     = "tool_execution_start"
	TypeToolExecutionEnd       = "tool_execution_end"
	TypePromptResult           = "prompt_result"
	TypeShutdown               = "shutdown"
	TypeAdoptWorkingCopy       = "adopt-working-copy"
	TypeAdoptWorkingCopyResult = "adopt-working-copy-result"
	TypeRPCChunk               = "rpc_chunk"
	// hello2 is the hello every shim sends since DaemonAPIVersion 8: the boot token and, under a
	// runtime that enrolls pods with the secrets broker, the pod's key thumbprint and
	// projected token. A daemon that predates it decodes it as Raw and refuses "malformed hello";
	// this daemon refuses the v1 hello by name (internal/stream).
	TypeHello2                       = "hello2"
	TypeAgentSecretsEnrollment       = "agent-secrets-enrollment"
	TypeAgentSecretsEnrollmentResult = "agent-secrets-enrollment-result"
	TypeAgentSecretsToken            = "agent-secrets-token"
	TypeAgentSecretsTokenResult      = "agent-secrets-token-result"
	TypeLauncherHello                = "launcher_hello"
	TypeLauncherHelloAck             = "launcher_hello_ack"
	TypeLauncherState                = "launcher_state"
	TypeLauncherStart                = "launcher_start"
	TypeLauncherStartResult          = "launcher_start_result"
	TypeLauncherStop                 = "launcher_stop"
	TypeLauncherStopResult           = "launcher_stop_result"
	// The report an earlier launcher sends, unasked, after it restored a released pod's saved work
	// into the pod that replaced it. This daemon saves and restores no work: it decodes the frame
	// only so that such a launcher's connection stays open, and drops it (LauncherRestored).
	TypeLauncherRestored = "launcher_restored"
	// The workspace read: the daemon asking one role's shim whether the workspace every role of
	// its issue shares holds work that no branch or tag on origin holds, and the shim's answer.
	// It is a jj query that moves none of the work, so the answer is small and crosses on one line.
	TypeWorkspaceWork       = "workspace-work"
	TypeWorkspaceWorkResult = "workspace-work-result"
	// The launcher's memory guard reports each process it ended, unasked, so the daemon can tell
	// that process's role why it is gone (INFRA-3557). The settings the guard runs by travel the
	// other way, on the launcher_hello_ack (LauncherMemoryGuard).
	TypeLauncherMemoryKill = "launcher_memory_kill"
	// The cache trimmer in a launcher process reports a sweep of the pod's volume, unasked: the
	// daemon reads it as the one word it has on a volume filling under the roles that share it,
	// and tells the issue's architect (INFRA-2009). The settings the trimmer runs by travel the
	// other way, on the launcher_hello_ack (LauncherVolumeTrim).
	TypeLauncherVolumeReport = "launcher_volume_report"
	// The shared public leak check's two directions: the daemon pushing the check's packed
	// directory, in slices under the frame limit, to a launcher whose hello named a tag, and the
	// launcher answering each slice, the last with the outcome of its install.
	TypeLauncherLeakCheck       = "launcher_leak_check"
	TypeLauncherLeakCheckResult = "launcher_leak_check_result"
)

// SyntheticModel is the model Claude Code names on a message no model wrote, such as its own answer
// to a provider's refusal (internal/claudebridge's testdata/api-error-429.jsonl:4). Such a message
// answers nothing: the bridge writes no message_end for it, and the stream reads none that names it
// as an answer (internal/stream's answeredBy).
const SyntheticModel = "<synthetic>"

// The protocol's sizes, verified against the shipped files: the largest plain line including its
// newline (worker-rpc.ts:8-12), the base64 payload of one chunk (worker-rpc.ts:16-18), the
// largest frame a chunk sequence may carry (worker-rpc.ts:13-15), and the bytes a connection may
// send before its first newline (worker-stream-listener.ts:11-13).
const (
	MaxFrameBytes       = 1 << 20
	ChunkPayloadBytes   = 256 << 10
	MaxReassembledBytes = 64 << 20
	MaxHelloBytes       = 4096
)

var (
	// ErrNotAFrame is a line that is not a JSON object. An unknown frame *type* is not this: the
	// shim is a bridge and forwards what it does not model.
	ErrNotAFrame = errors.New("shimwire: line is not a JSON object")
	// ErrMalformedFrame is a frame of a known type whose fields are not what its only sender
	// sends. Validate reports it; decoding does not, so a bridge can still forward.
	ErrMalformedFrame = errors.New("shimwire: malformed frame")
)

// Frame is one decoded line. The concrete types below model every frame the shim and the daemon
// exchange; anything else decodes to Raw and crosses unchanged.
type Frame interface {
	// FrameType is the frame's `type` member on the wire.
	FrameType() string
}

// Hello authenticates a reverse-dialed shim: its first line, carrying the boot token the daemon
// minted for the pane (worker-stream-listener.ts:139-148).
type Hello struct {
	BootToken string `json:"bootToken"`
}

// AgentSecretsHello is the pod's session identity as the shim reports it: the base64url SHA-256
// JWK thumbprint of the P-256 key `agent-secrets keygen` wrote into the pod's key directory, and
// the projected service-account token for the broker's audience, read from its mount at the
// hello. The daemon relays both to the broker and keeps neither past the enrollment; it asks for
// the token again (AgentSecretsToken) when the hello's would expire before it enrolls.
type AgentSecretsHello struct {
	Thumbprint string `json:"thumbprint"`
	PodToken   string `json:"podToken"`
}

// Hello2 authenticates a reverse-dialed shim: its first line, carrying the boot token the daemon
// minted for the pane or pod, and the pod's identity when the shim was started with
// --agent-secrets-key-dir (a tmux pane never is, and sends none).
type Hello2 struct {
	BootToken    string             `json:"bootToken"`
	AgentSecrets *AgentSecretsHello `json:"agentSecrets,omitempty"`
}

// AgentSecretsEnrollment hands the shim the broker's enrollment id for this pod generation; the
// shim writes it beside the key (`<key dir>/enrollment`, what `agent-secrets` reads) and starts
// the lease renewer, then answers AgentSecretsEnrollmentResult with the same ID.
type AgentSecretsEnrollment struct {
	ID           string `json:"id"`
	EnrollmentID string `json:"enrollmentId"`
}

// AgentSecretsEnrollmentResult answers AgentSecretsEnrollment.
type AgentSecretsEnrollmentResult struct {
	ID    string `json:"id"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// AgentSecretsToken asks the shim to read its projected token again: the kubelet rewrites the
// file before the token expires, and a token read at the hello can expire before the daemon
// enrolls with it. The shim answers AgentSecretsTokenResult with the same ID. A shim built before
// this frame forwards it to OMP, which answers it as an unknown command.
type AgentSecretsToken struct {
	ID string `json:"id"`
}

// AgentSecretsTokenResult answers AgentSecretsToken: the token as the shim just read it, or why it
// could not.
type AgentSecretsTokenResult struct {
	ID       string `json:"id"`
	OK       bool   `json:"ok"`
	PodToken string `json:"podToken,omitempty"`
	Error    string `json:"error,omitempty"`
}

// LauncherHello authenticates a role launcher independently from its child shim. The daemon
// minted Token for one role and Secret epoch, and accepts it only from the pod UID its Secret
// binds: the pod a launch made, or a pod Agent Sandbox made again from the same Sandbox outside a
// launch, which the daemon binds at its launchers' first hello. LauncherID changes each time
// Kubernetes restarts the launcher container.
//
// LeakCheckTag is the launcher's own pin of the shared public leak check (its environment's
// LEGION_LEAK_CHECK_TAG), empty when its environment names none: a hello that names a tag is a
// launcher that takes the check from the daemon (LauncherLeakCheck) rather than a role reading
// GitHub for it. LeakCheckInstalled is this launcher process having already installed that tag,
// so the daemon pushes it nothing, and a daemon that restarts makes a running pod read nothing
// again; a launcher process that restarts says false and is pushed the check again. The hello
// does not refuse a tag of another shape than LeakCheckTagPattern: the launcher connects, and the
// daemon refuses that tag as the cause of the start it cannot serve the check for.
//
// ResumesBySession is this launcher saying it can look a session up by its id and resume the file
// it finds (LauncherStart.ResumeSession). A launcher built before the member sends none, and the
// daemon names a session only to a launcher that said resumesBySession: to any other it names no
// session, and the agent's own command carries `--resume=<session id>` instead.
type LauncherHello struct {
	Token              string `json:"token"`
	Sandbox            string `json:"sandbox"`
	Role               string `json:"role"`
	PodUID             string `json:"podUid"`
	LauncherID         string `json:"launcherId"`
	LeakCheckTag       string `json:"leakCheckTag,omitempty"`
	LeakCheckInstalled bool   `json:"leakCheckInstalled,omitempty"`
	ResumesBySession   bool   `json:"resumesBySession,omitempty"`
}

// LauncherHelloAck admits a launcher to receive child process commands, and hands it the
// settings its memory guard and its cache trimmer run by, when this daemon has any to hand it,
// and whether this daemon knows a kill report the pod's own kernel made.
type LauncherHelloAck struct {
	// MemoryGuard is absent rather than zero when no settings are sent, and that absence is what
	// turns the guard off: a daemon built before the guard and a deployment that turned it off
	// both send no member at all, where a present member whose numbers are zero is settings the
	// launcher would refuse. The pointer is what tells the two apart, as it does for
	// Hello2.AgentSecrets and LauncherState.Child.
	MemoryGuard *LauncherMemoryGuard `json:"memoryGuard,omitempty"`
	// VolumeTrim is absent rather than zero when no settings are sent: a daemon built before
	// this work and a deployment that turned the trimmer off both send no member at all, and the
	// launcher's trimmer then reads and deletes nothing and discards a report it was holding,
	// since that daemon has no case for one. A present member whose numbers are zero is settings,
	// which the launcher refuses, logging each value; its trimmer deletes nothing on them either,
	// but it tries a report it was holding. The pointer is what tells an absent member from a
	// zero one, as it does for Hello2.AgentSecrets, StateData.IsStreaming and LauncherState.Child.
	VolumeTrim *LauncherVolumeTrim `json:"volumeTrim,omitempty"`
	// KernelKills says the daemon reads a launcher_memory_kill whose cause is
	// MemoryKillCauseKernel or MemoryKillCauseKernelProtected as the kernel's kill and tells its
	// role so, or the project's controller where the report is of the controller's pod. The
	// daemon sets it on every launcher's acknowledgement, an issue pod's and the controller's pod's
	// alike, whether or not it sends MemoryGuard, since the controller's pod runs in a microVM where
	// the project's sandbox is one (INFRA-4567); a daemon of an earlier release sets it on the issue
	// pods' alone. A microVM pod's launcher sends its kill reports only on a connection whose
	// acknowledgement carries it, whatever MemoryGuard says, so a daemon built before it, which
	// sends no member, never reads a kernel's kill with the guard's words; a gVisor pod's launcher
	// ignores it and sends its guard's reports where MemoryGuard came. It is left out when false,
	// so an acknowledgement without it is the bytes an older daemon writes.
	KernelKills bool `json:"kernelKills,omitempty"`
}

// LauncherMemoryGuard is what a launcher's memory guard runs by: it reads the pod's anonymous
// memory every IntervalMilliseconds, and at ActPercent of the container's limit it ends, of the
// processes it may end, the process that grew the most over its last few reads, else the largest
// one it has read at most once in its last two readings near the limit, else the one that grew the
// most since the guard's first kill while the reading stays near the limit, else the largest, a
// fork that still runs its parent's program counted by what it holds over its parent, where those
// processes hold at least FloorPercent of the limit between them, and at or above the urgent
// share, halfway from ActPercent to the limit, every other process that grew over its last few
// reads at the same reading and, where a process it may end is not new to its stay near the
// limit, every other one new to it that holds memory of its own; after the kills of a reading it
// ends no other until every killed process has exited or WaitSeconds has passed, and it gives the
// rise up once WaitSeconds has passed with the reading not fallen and nothing it may end grown
// since the kills.
//
// It holds no `enabled` member: a deployment that turns the guard off sends no member at all,
// which is the same absence a daemon that does not know the guard sends, so one rule in the
// launcher covers both (LauncherHelloAck.MemoryGuard).
type LauncherMemoryGuard struct {
	ActPercent           int `json:"actPercent"`
	FloorPercent         int `json:"floorPercent"`
	WaitSeconds          int `json:"waitSeconds"`
	IntervalMilliseconds int `json:"intervalMilliseconds"`
}

// The ranges LauncherMemoryGuard's four numbers must be in, and the settings a guard runs by when
// nobody has said otherwise. Both sides of the wire read them: the daemon's loader refuses a file
// outside the ranges, and the launcher checks a frame against them a second time, on its own side
// of a connection that carries version skew both ways (Validate). The two checks are deliberate;
// their values are one source here, because a widening the launcher did not know would load
// cleanly in the daemon and then be refused in every pod.
//
// The defaults are one source too: they are the settings the launcher's guard runs by before any
// acknowledgement reaches it, and the daemon's configuration defaults, so a change to one moves
// both. The act share's default is the one a measurement may move. Every minimum is above zero
// because a guard cannot be armed with a zero interval or wait.
//
// MemoryGuardControllerWaitDefault is the controller pod's wait after a kill in place of
// MemoryGuardWaitDefault, an issue pod's (INFRA-3531, R4): its guard's start
// (memoryguard.ControllerDefaults) and the daemon's default for
// `runtime.kubernetes.memory_guard.controller_pod`. It is below the 3.4 seconds this issue's tester
// measured from a kill that freed nothing to the host's kill of a 4 GiB pod, so the controller's
// guard decides on the next process before that kill comes.
//
// MemoryGuardConfigActMax is the highest act share the daemon's loader takes for either pod kind,
// `runtime.kubernetes.memory_guard.issue_pods.act_percent` and `controller_pod.act_percent`
// (INFRA-3531 R4 of spec version 43): the urgent share, the mark plus half the distance from the
// mark to the limit, is then at most 95% of the limit and stays below the lowest kill point this
// issue's tester measured at 4 GiB, 96.88% on 2026-11-18; at an act share of 95 the urgent share
// would be 97.5%, at or above that kill point. Both kinds run the guard's one core, so both take
// the same bound. The wire keeps MemoryGuardActMax, and Validate with it, so a launcher of this
// release still takes the settings a daemon of an earlier release sends, which may name 91 to 95.
const (
	MemoryGuardActMin      = 50
	MemoryGuardActMax      = 95
	MemoryGuardFloorMin    = 1
	MemoryGuardFloorMax    = 50
	MemoryGuardWaitMin     = 1
	MemoryGuardWaitMax     = 60
	MemoryGuardIntervalMin = 50
	MemoryGuardIntervalMax = 5000

	MemoryGuardActDefault      = 85
	MemoryGuardFloorDefault    = 10
	MemoryGuardWaitDefault     = 5
	MemoryGuardIntervalDefault = 100

	MemoryGuardControllerWaitDefault = 1
	MemoryGuardConfigActMax          = 90
)

// LauncherMemoryKill is one process of a role that was stopped because the pod was out of memory:
// ended with SIGKILL by the launcher's memory guard, or, in a microVM pod, stopped by the pod's own
// kernel (Cause). The launcher sends it, unasked, on the connection of the role whose process it
// was, and the daemon tells that role why the process is gone (INFRA-3557).
//
// Guard is the identifier the guard minted for itself when the launcher process started, and
// Sequence the guard's own count of its kills, from 1: together they name the kill once, so a
// report sent twice is applied once. Executable names the process as every line of the guard
// does: the base name of its executable, or its stat name where the guard could not read that,
// each character that does not print escaped and the whole cut to MemoryKillExecutableBytes. It
// carries none of the process's arguments, which may hold a credential no masking rule names, such
// as a positional password; the role that started the process has them. SizeBytes is the
// process's anonymous memory as the guard measured it, ReadingBytes the pod's anonymous memory
// (/proc/meminfo's AnonPages) when it acted, CounterBytes the cgroup's memory counter then, and
// LimitBytes the container's memory limit.
//
// Cause is MemoryKillCauseGuard, empty, for the guard's own kill, the only kill a launcher that
// predates the member reports, and MemoryKillCauseKernel or MemoryKillCauseKernelProtected for a
// process the pod's kernel stopped, which the launcher's watch of that kernel reports with an
// identifier and a count of its own in Guard and Sequence. It is left out when empty, so the
// guard's report is the bytes it was before the member existed; Validate refuses any other value.
type LauncherMemoryKill struct {
	Guard        string          `json:"guard"`
	Sequence     uint64          `json:"sequence"`
	PID          int             `json:"pid"`
	Executable   string          `json:"executable"`
	SizeBytes    uint64          `json:"sizeBytes"`
	ReadingBytes uint64          `json:"readingBytes"`
	CounterBytes uint64          `json:"counterBytes"`
	LimitBytes   uint64          `json:"limitBytes"`
	At           time.Time       `json:"at"`
	Cause        MemoryKillCause `json:"cause,omitempty"`
}

// MemoryKillCause is what stopped the process a launcher_memory_kill names, which chooses the
// note its role is told. It is one kind with three values, since a kill has exactly one of them,
// and its empty value, MemoryKillCauseGuard, is the guard's own kill, which every launcher built
// before the member reports.
//
// MemoryKillCauseKernel and MemoryKillCauseKernelProtected are a process the pod's own kernel
// stopped because the pod ran out of memory: in a microVM pod, whose kernel is the pod's alone and
// stops one process rather than the whole sandbox. The first is a command of the role's, one the
// guard would have been free to end; the second is one of the processes the launcher's classifier
// protects, which run the role's session (the role-init, the shim, the agent and their helpers),
// so the role is told what that process's end does to it. A launcher sends either only on a
// connection whose acknowledgement carried LauncherHelloAck.KernelKills.
type MemoryKillCause string

const (
	MemoryKillCauseGuard           MemoryKillCause = ""
	MemoryKillCauseKernel          MemoryKillCause = "kernel"
	MemoryKillCauseKernelProtected MemoryKillCause = "kernel-protected"
)

// MemoryKillExecutableBytes is the most bytes of a process's name a launcher_memory_kill carries:
// a base name on Linux is at most 255 bytes, so the bound holds whole any name that needs no
// escape, and it bounds what one report puts into a role's note whatever its sender wrote.
const MemoryKillExecutableBytes = 256

// LauncherVolumeTrim is what a launcher's cache trimmer runs by: it reads the pod's volume every
// IntervalSeconds, sweeps the cache roots when the used fraction reaches HighPercent, deletes
// only entries in which nothing was modified for MinAgeMinutes, and stops deleting at the first
// reading at or below LowPercent, if nothing stops it sooner (launcher's sweep).
//
// It holds no `enabled` member: a deployment that turns the trimmer off sends no member at all,
// which is the same absence a daemon that does not know this work sends, so one rule in the
// launcher covers both rather than two that could disagree (LauncherHelloAck.VolumeTrim).
type LauncherVolumeTrim struct {
	HighPercent     int `json:"highPercent"`
	LowPercent      int `json:"lowPercent"`
	MinAgeMinutes   int `json:"minAgeMinutes"`
	IntervalSeconds int `json:"intervalSeconds"`
}

// The ranges LauncherVolumeTrim's four numbers must be in. The launcher's trimmer reads all six
// and refuses a frame outside them (launcher's refusedTrimSettings), on its own side of a
// connection this design expects to carry version skew both ways. The daemon's loader refuses a
// file outside them too (config's readVolumeTrim): it reads the three maxima here, and its
// minimum is its own rule that each value be a positive integer, which is the minimum of 1 the
// three take here. The two checks are deliberate, and the maxima are one source here, because a
// widening of the loader's maximum the trimmer did not know would load cleanly and then be
// refused by every pod's trimmer, which then sweeps nothing until a daemon sends settings inside
// its ranges.
//
// A mark at 100 is a volume already full: a high mark there would start a sweep only once the
// roles' writes were already failing for want of space.
// VolumeIntervalMax is an hour, the longest wait between two looks that is still a trimmer: a
// pod's caches can fill a volume inside one. VolumeMinAgeMax is seven days, a generous age for a
// cache entry and far below where the arithmetic breaks: the sweep's cutoff is its start less
// MinAgeMinutes of time.Duration, which is int64 nanoseconds, so 153,722,868 minutes and above
// wrap the cutoff past the sweep's own start and make every entry of every root eligible however
// recently it was written (launcher's sweep). At the maximum, with the Go build cache's extra
// hour taken off as well (launcher's goCacheMtimeSlack), the cutoff is about 6.1e14 nanoseconds
// back, against an int64 limit of 9.2e18. The daemon bounds its other keys written in minutes for
// the same overflow (config's maxTimerMinutes). The bound says nothing about how long a pod
// lives, which configuration decides (`idle_release`'s `idle_minutes: 0` turns release off and a
// pod then lives as long as its issue runs), and a trimmer is switched off with `enabled: false`,
// never with an age. The minima are what a trimmer can be armed with at all: time.NewTicker
// panics on a non-positive period, so a trimmer armed with an interval of 0 would fault each time
// its interval loop started, and three of those in a row would switch it off for the life of its
// launcher process (the launcher's run). It is not armed with one: the trimmer refuses an interval
// below the minimum before it starts a clock, logs the refusal and stays off, and counts no fault
// (the launcher's refusedTrimSettings). A high mark of 0 would sweep the caches of a volume that
// is empty.
const (
	VolumeMarkMin     = 1
	VolumeMarkMax     = 99
	VolumeMinAgeMin   = 1
	VolumeMinAgeMax   = 7 * 24 * 60
	VolumeIntervalMin = 1
	VolumeIntervalMax = 3600
)

// LauncherVolumeSize is one target of the trimmer's size walk — a directory two levels under the
// pod's volume root, a regular file beside such directories in an immediate child of the root,
// or an immediate child of the root that holds no directory of its own — as the walk measured
// it: the bytes of the regular files under it, or of the file itself, and of those the bytes of
// the files whose modification time is more than an hour old and more than a day old, which are
// what tell a reader a working set from cold bytes.
//
// Refused names the first read under Path the walk did not make, and Refusals is how many there
// were in all: a read the kernel refused, or a directory deeper below Path than the walk
// descends, which it does not open (internal/launcher's volumeWalkDepth). It is Path/<name> for a
// name in Path, and Path/.../<name> (depth N) for a name N levels below it, so its size is set
// by two names and never by the depth of the tree. A size that carries them is what the walk
// could read under Path and no more: Bytes and its two splits are a lower bound of the
// directory's own, never the whole of it. The walk went on past each refusal, so neither makes
// the walk partial (LauncherVolumeReport.SizesPartial). A target the walk could read nothing of
// — a directory the kernel refused it the open or the listing of, a file whose lstat it refused
// — is no size: the report names it among SizesMissing with that read (LauncherVolumeMissing).
type LauncherVolumeSize struct {
	Path          string `json:"path"`
	Bytes         int64  `json:"bytes"`
	BytesOverHour int64  `json:"bytesOverHour"`
	BytesOverDay  int64  `json:"bytesOverDay"`
	Refused       string `json:"refused,omitempty"`
	Refusals      int    `json:"refusals,omitempty"`
}

// LauncherVolumeMissing is one target the size walk measured none of, or the volume root or an
// immediate child of it that the walk did not list in full. Reason is why, for a directory the
// walk could not open or list or a file whose lstat was refused: the read the kernel refused and
// its answer, as `openat: permission denied`, and never a path, since Path names it already. It
// is empty for one the cut of a walk left unmeasured, which the report's SizesPartial says.
type LauncherVolumeMissing struct {
	Path   string `json:"path"`
	Reason string `json:"reason,omitempty"`
}

// The three things that raise an episode, which a report's RaisedBy names. An episode is the run
// of readings the daemon's notice bounds count against, so what raised it is what a reader needs
// to tell three reports apart: one of a volume that has just filled, one of a volume that fell to
// its low mark or below and has filled again, and one whose last two reported sweeps started five
// points or more above where the episode before it was anchored. A reported sweep, here and below,
// is one that ended above the low mark and that the settings did not stop, whose start the trimmer
// reads before it writes its lines about the sweep and makes the sweep's report, so one whose line
// or report then panics, which sends no report, counts too (the launcher's riseAt).
const (
	// VolumeEpisodeFirst is the trimmer's first episode: its reading reached the high mark.
	VolumeEpisodeFirst = "first"
	// VolumeEpisodeRecovery is a reading of the trimmer's own at or below the low mark, an
	// interval's or a sweep's last, after one at or above the high mark.
	VolumeEpisodeRecovery = "recovery"
	// VolumeEpisodeRise is a floor five percentage points or more above the anchor of the
	// episode it ends, the floor being the lower of the last two reported sweep starts, and no
	// other growth: a reading inside a pause or below the high mark raises none. The anchor of an
	// episode no rise raised, the first and one a recovery raised, is the lowest start of its
	// reported sweeps, which a later reported sweep that starts lower lowers; that of one a rise
	// raised is the floor that raised it, which never moves. So a volume that keeps filling raises
	// a new episode at each five points its sweep starts climb, one reported sweep after the first
	// that starts there, except after two reported starts in a row read a transient high such as
	// 97%: that raises a rise anchored there, and a fill from lower down then raises none until
	// two starts stand five points above it, and none at all where it is above 95%, until a
	// recovery starts a new chain. A peak that one start reads raises nothing. Each rise lifts
	// the anchor five points or more and a reading is at most 100%, so until a recovery a chain
	// whose anchor is A at its first rise holds at most (100 − A) / 5 rises, rounded down; every
	// anchor is a reported sweep's start, at or above the high mark that sweep ran by, so a chain
	// anchored at the high mark holds at most (100 − high mark) / 5, three at the default of 85.
	// An anchor above 95% can never be passed, and from one of 95% a rise needs two reported sweeps
	// in a row that start at exactly 100.0%. The daemon sends a report of an episode raised this
	// way without holding it for the pod's hour, and holds it to the three of its episode like any
	// other, which only the report of a trimmer that switched itself off passes
	// (internal/workflow's volumeNoticeHeldBy).
	VolumeEpisodeRise = "rise"
)

// LauncherVolumeReport is one sweep of the pod's volume as the launcher's cache trimmer ran it.
// The launcher sends it unasked, and the daemon tells the issue's architect: nothing else in the
// system sees a volume the roles of an issue share fill under them (INFRA-2009).
//
// Trimmer is the identifier the trimmer minted for itself when the launcher process started. It
// is what makes a restarted roles container a new counting line for the daemon's notice bounds
// rather than a repeat of the first episode. Episode is the trimmer's own episode number, from
// 1, and RaisedBy which of the three things above raised it.
//
// UsedBytes and FreeBytes are the volume as the sweep's last reading saw it, and FreedBytes what
// the sweep freed, read from the volume itself and never counted from the bytes unlinked: a
// deletion the kernel has not accounted for yet is not space anything can use. FreedBytes is the
// sweep's first reading's used bytes less its last's, so it is negative where the volume's used
// bytes grew while the sweep ran. StopReason is why the sweep stopped, in the trimmer's own
// words, and RefusedReads the /proc reads the kernel refused that no report has carried yet, of
// this sweep and of earlier ones that sent no report: one per process it could not read, or one
// for /proc itself when /proc could not be listed.
//
// Sizes are the size walk's targets (LauncherVolumeSize) and SizesAt when they were
// measured: a report made between walks carries the last walk's numbers with that walk's time,
// so a reader never reads a stale measurement as this sweep's. SizesPartial is a walk that ended
// early, which is a walk its budget or the container's stop cut before it reached every target.
// SizesMissing is every target the walk did not measure at all, and the volume root or an
// immediate child of it where the walk did not list one in full, each with its reason where a
// read was refused (LauncherVolumeMissing): a directory it could not open or list or a file whose
// lstat was refused, which ends nothing, and, of a walk that was cut, every target it had not
// measured when it was cut — where the cut came while it listed them, every one it had listed,
// the child of the volume root whose names it was reading where the cut came inside one, named
// whole, and every child it had not yet listed, or the volume root alone where the cut came
// inside the root's own listing. A directory the walk could read only in part is a size of its
// own, which names the first read under it the walk did not make (LauncherVolumeSize.Refused).
//
// Faults are the panics the trimmer recovered from, each with its stack, and SwitchedOff the one
// report no sweep produces: the trimmer has stopped itself after three faults in a row of one kind
// of its work, its readings of the volume, a row that also counts its wait for settings and its
// interval clock with the line that starts it, or its sweeps and their reports, and this is the
// last the daemon hears from it until the launcher restarts.
//
// Every list above is bounded, because a path two levels under /legion is a name a role of the
// pod chose and a pod may hold thousands of them: the launcher that built the report cuts each to
// its cap below (internal/launcher's bound, and for the sizes its walk, which keeps the largest
// as it measures), and the four Omitted counts are how many entries that cut left out, so a
// reader is never shown a cut list as the whole of what the trimmer measured. Validate refuses a
// report whose list is longer than its cap, so the daemon holds a report to the caps whatever
// sent it. Each path, each reason, the stop reason and each fault are cut in runes by that
// launcher too, and Validate checks none of them.
type LauncherVolumeReport struct {
	Trimmer             string                  `json:"trimmer"`
	Episode             int                     `json:"episode"`
	RaisedBy            string                  `json:"raisedBy"`
	UsedBytes           int64                   `json:"usedBytes"`
	FreeBytes           int64                   `json:"freeBytes"`
	FreedBytes          int64                   `json:"freedBytes"`
	StopReason          string                  `json:"stopReason"`
	RefusedReads        []string                `json:"refusedReads,omitempty"`
	RefusedReadsOmitted int                     `json:"refusedReadsOmitted,omitempty"`
	Sizes               []LauncherVolumeSize    `json:"sizes"`
	SizesOmitted        int                     `json:"sizesOmitted,omitempty"`
	SizesAt             time.Time               `json:"sizesAt"`
	SizesPartial        bool                    `json:"sizesPartial,omitempty"`
	SizesMissing        []LauncherVolumeMissing `json:"sizesMissing,omitempty"`
	SizesMissingOmitted int                     `json:"sizesMissingOmitted,omitempty"`
	Faults              []string                `json:"faults,omitempty"`
	FaultsOmitted       int                     `json:"faultsOmitted,omitempty"`
	SwitchedOff         bool                    `json:"switchedOff,omitempty"`
}

// The most entries each list of a LauncherVolumeReport may hold: the launcher's trimmer cuts a
// list to its cap and counts the rest in the list's Omitted count (internal/launcher's bound and
// its walk's walkResult.keep), and Validate refuses a report that holds more. They are here, in
// the one package both ends read, so that the cap the launcher cuts at and the cap the daemon
// refuses past are one number and not two that could disagree.
const (
	// VolumeReportSizes is how many sizes a report carries (Sizes): the largest of what the walk
	// measured, in the order the walk measured them. A pod's own layout makes about fifteen
	// directories two levels under the volume root, and a role can make thousands under
	// /legion/scratch; fifty is room for a layout that grows, and the largest fifty are the ones
	// a reader acts on, whichever of them the walk reached first.
	VolumeReportSizes = 50
	// VolumeReportSizesMissing is how many unmeasured targets a report names, with the volume
	// root or an immediate child of it where the walk did not list one in full (SizesMissing).
	// The list can run as long as the one above, so it takes the same cap.
	VolumeReportSizesMissing = 50
	// VolumeReportRefusedReads is how many refused /proc reads a report names (RefusedReads): one
	// per process of the pod the kernel refused a read of, or one for /proc itself. A pod runs
	// six roles and their children, and an operator who must go and look needs a handful of pids
	// rather than one per process.
	VolumeReportRefusedReads = 25
	// VolumeReportFaults is how many recovered panics a report carries (Faults), the newest ones.
	// The launcher's trimmer stops itself after three in a row (its volumeFaultLimit) of one kind
	// of its work, counting its readings' panics, with those of its wait for settings and of its
	// interval clock and the line that starts it, and its sweeps' in two rows, where a panic of
	// one kind neither ends the other's row nor adds to it. So the switched-off report's newest
	// three end with the panic that stopped the trimmer, and either of the two before it can be of
	// the other kind: two sweep panics, two reading panics, and then a reading run to its end whose
	// sweep panics stop the trimmer in the sweep row with both reading panics among the three.
	VolumeReportFaults = 3
)

// LauncherChild is the actual worker-shim child the launcher has running, if any.
type LauncherChild struct {
	Generation uint64 `json:"generation"`
	PID        int    `json:"pid"`
}

// LauncherExit is the last child exit the launcher observed. A launcher reports it after a
// reconnect before it accepts another start, so the daemon never invents Gone from a lost reply.
// Code is the process's exit status, or one of two values no process exits with: -1 when the
// launcher has no status for it (killed by a signal, which Signal then names, or never waited on),
// and LauncherExitSetupFailed.
type LauncherExit struct {
	Generation uint64 `json:"generation"`
	Code       int    `json:"code"`
	Signal     string `json:"signal,omitempty"`
}

// LauncherExitSetupFailed is the Code of an issue pod role's generation whose role-init failed
// before it started the shim: setting up the role's namespaces, or starting the shim itself, which
// role-init alone tells the launcher by its exit status (launcher.RoleInitSetupFailedExit). Its
// reason is role-init's own line in the role's log. It is apart from every code a shim exits
// with, so the daemon never reads a setup failure as the agent's own exit.
const LauncherExitSetupFailed = -2

// LauncherState is the launcher's actual child state at hello and after every child transition.
type LauncherState struct {
	Child    *LauncherChild `json:"child,omitempty"`
	LastExit *LauncherExit  `json:"lastExit,omitempty"`
}

// LauncherStart starts one worker-shim child. The daemon owns the generation and request ID; a
// repeated request is idempotent only when its payload, Files included, is unchanged. Files are
// the generation's credentials, the boot token among them: the launcher writes each, owner-only
// and exclusively, into a fresh directory for this generation in its own memory-backed storage
// before the child starts, and removes the directory when the child ends — so a role started in a
// running pod never waits on the kubelet to propagate a Secret. Argv and Env name those files by
// path; no credential is ever in Argv, Env, or an error.
//
// ResumeFile is the session file the child resumes. ResumeSession is the id of a session the
// child resumes where the daemon knows no file of it: the launcher looks the id up in the session
// table and resumes the file it finds, as it resumes ResumeFile. An older launcher ignores
// resumeSession and sends no resumesBySession in its hello, and the daemon names a session only to
// a launcher that said resumesBySession (LauncherHello), so an older launcher is never sent one; an
// older daemon sends no resumeSession.
type LauncherStart struct {
	ID            string            `json:"id"`
	Generation    uint64            `json:"generation"`
	Argv          []string          `json:"argv"`
	Env           []string          `json:"env"`
	Files         map[string]string `json:"files,omitempty"`
	ResumeFile    string            `json:"resumeFile,omitempty"`
	ResumeSession string            `json:"resumeSession,omitempty"`
	// AgentExecutable is the agent's executable as /proc/<pid>/exe reads it once any start
	// wrapper has exec'd it: an absolute path, such as the image's /opt/omp/bin/omp. Argv cannot
	// say it, because a deployment's setup wraps the agent in `/bin/sh -c <script> legion-start`.
	// The daemon fills it for an Oh My Pi launch, and a launcher with a session writer token
	// knows the agent's process by it (internal/launcher's sessionPeer). It is empty for a
	// Claude Code launch, whose agent writes no session itself.
	AgentExecutable string `json:"agentExecutable,omitempty"`
}

// GenerationDir is the directory under a launcher's private directory that holds one generation's
// Files. Both ends name it: the daemon in the Argv and Env that point at a file, the launcher when
// it writes them.
func GenerationDir(generation uint64) string { return "g" + strconv.FormatUint(generation, 10) }

// DispatchTokenBrokerVariable is the daemon's word, in a role's environment (LauncherStart's Env),
// that its Dispatch credential is a token its Dispatch client trades its pod's exchange identity
// for at the secrets broker, and nothing else: under it the client reads no bearer from any source
// (packages/envoy-client's dispatch-config.ts, brokerRouteForced), and the role's worker shim
// refuses a providers-Secret key that would hand it the shared one (shim.ReadProviderEnv). The
// Sandbox runtime tells it, as 1, under dispatch_token_broker alone.
const DispatchTokenBrokerVariable = "DISPATCH_TOKEN_BROKER"

// SessionRefusal is why a launcher refused a start over the session it was asked to resume. It is
// one kind with two values rather than a flag for each, because a start meets at most one of them:
// SessionGone is the session table answering that it holds no such session, a session that is
// gone, and SessionStoreUnreachable is that table being absent or the store not answering at all,
// which says nothing about the session. The empty value is no session problem, which every start
// that succeeded and every refusal over anything else carries. A value is also the error the
// launcher wraps its own refusal in, so the sentence the daemon reads and the kind it acts on are
// the same thing said once (internal/launcher's resumable).
type SessionRefusal string

const (
	SessionGone             SessionRefusal = "gone"
	SessionStoreUnreachable SessionRefusal = "store-unreachable"
)

// Error is the sentence a refusal of this kind reads as, so a launcher can wrap it into the error
// it answers with. A kind this build does not know reads as its own value, which keeps a newer
// launcher's answer legible rather than empty.
func (r SessionRefusal) Error() string {
	switch r {
	case SessionGone:
		return "the session table holds no such session"
	case SessionStoreUnreachable:
		return "the session store could not be asked"
	}
	return string(r)
}

// LauncherStartResult is the launcher's answer to LauncherStart. A start the launcher refused over
// the session it was asked to resume names the refusal's kind, so the daemon never has to read an
// error's prose to tell the two apart: the sandbox runtime maps the kind to the sentinel the
// supervisor acts on (runtime.ErrSessionFileMissing, runtime.ErrSessionStore). Error carries the
// sentence either way.
type LauncherStartResult struct {
	ID                string         `json:"id"`
	OK                bool           `json:"ok"`
	RunningGeneration uint64         `json:"runningGeneration,omitempty"`
	Error             string         `json:"error,omitempty"`
	Session           SessionRefusal `json:"session,omitempty"`
}

// LauncherStop stops only the child of Generation. The launcher refuses an older generation
// rather than risking a newer role process.
type LauncherStop struct {
	ID         string `json:"id"`
	Generation uint64 `json:"generation"`
	GraceMs    int    `json:"graceMs"`
}

// LauncherStopResult is the launcher's answer to LauncherStop.
type LauncherStopResult struct {
	ID    string `json:"id"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// HelloAck accepts the hello. The shim spawns OMP only after it (worker-stream-listener.ts:14).
type HelloAck struct{}

// NegotiateProtocol opens a connection's RPC protocol version (worker-rpc.ts:430).
type NegotiateProtocol struct {
	ID              string `json:"id"`
	ProtocolVersion int    `json:"protocolVersion"`
}

// Prompt delivers one task to OMP. DeliveryID names the logical delivery the daemon is retrying
// — the id the dedupe keys on — while ID is this attempt's request (worker-rpc.ts:452).
type Prompt struct {
	ID         string `json:"id"`
	DeliveryID string `json:"deliveryId"`
	Message    string `json:"message"`
}

// Response is OMP's answer to a request, carrying the request's own id. Data is the answer's
// payload, left raw because its shape belongs to the command (worker-rpc.ts:369-377).
//
// Code is Oh My Pi's class of a refusal: a refused request's error response carries it at the
// top level, right after Error (`RpcResponse` in packages/coding-agent/src/modes/rpc/rpc-types.ts
// of cybermonklabs/oh-my-pi at the pinned tag). For a `prompt` or `abort_and_prompt` its one value
// is `session_busy`: the prompt was refused, or failed, because the agent was busy (docs/rpc.md
// there). Nothing in the daemon reads it; it is modelled so the frame survives this package
// whole, the way every other frame does.
type Response struct {
	ID      string          `json:"id"`
	Command string          `json:"command"`
	Success bool            `json:"success"`
	Error   string          `json:"error,omitempty"`
	Code    string          `json:"code,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// StateData is the get_state answer's payload. IsStreaming is absent rather than false when the
// worker does not report it, which is the difference between "idle" and "unknown"
// (worker-rpc.ts:476-485).
type StateData struct {
	IsStreaming *bool `json:"isStreaming"`
}

// GetState asks the worker whether a turn is running.
type GetState struct {
	ID string `json:"id"`
}

// Handoff asks Oh My Pi to shorten the session's context with a handoff: `{id?, type:
// "handoff", customInstructions?: string}` (`RpcCommand` in
// packages/coding-agent/src/modes/rpc/rpc-types.ts of cybermonklabs/oh-my-pi at the pinned tag).
// Oh My Pi has the session's own model write a handoff document over the live system prompt,
// tools and turns, with no tool to call, and commits that document as the session's compaction
// entry, method `handoff`, on the same session. The shim answers none of it — it forwards it to
// Oh My Pi as it forwards a prompt — and Oh My Pi answers a `response` of the same command:
// `success: true` with the handoff's result in `data` for a session shortened, `success: true`
// with `data: null` for a handoff that made no document, and `success: false` with the reason
// for one it refused, a turn in progress among them.
//
// The daemon sends it where it sent `compact` before dispatch://INFRA-5798, and never `compact`:
// `compact` writes its summary from the history quoted as text in one message, which
// Anthropic's classifier refuses for a long Claude session.
//
// CustomInstructions is the focus the handoff document is written to, and the daemon always
// sends one: the instructions keep the reviewer's entry in the document (api's
// compactInstructions), and a document that dropped it leaves the reviewer no entry to write. A
// frame with none is refused here rather than sent.
type Handoff struct {
	ID                 string `json:"id"`
	CustomInstructions string `json:"customInstructions"`
}

// AgentStart is OMP beginning a turn. DeliveryID is set only on the shim's own synthetic replay
// of a start it already observed (worker-shim.ts:365); OMP never sets it.
type AgentStart struct {
	DeliveryID string `json:"deliveryId,omitempty"`
}

// AgentEnd is OMP finishing a turn. Its real form carries the whole transcript, which is why it
// is the frame that arrives chunked.
type AgentEnd struct{}

// PromptResult is Oh My Pi reporting how one prompt's work ended, and the only frame that names
// the prompt a turn belonged to: ID is the `prompt` command's own request id, where AgentEnd
// carries no prompt identity at all ("that frame carries no prompt identity, so correlate
// prompts through `prompt_result`", omp://rpc.md). An accepted prompt completes exactly once,
// either as `data.agentInvoked: false` on its response — it finished locally and started no turn
// — or as this frame, always written after that response.
//
// AgentInvoked is false for a prompt that ran no turn of its own, and Error is Oh My Pi's reason
// when the work ended in one — an object, never a string (PromptError). Oh My Pi's own
// prompt_result carries more than these three members — `status`, its word for how the work
// ended, and `sessionSettled` among them — which nothing in the daemon reads; the decode drops
// them.
type PromptResult struct {
	ID           string       `json:"id"`
	AgentInvoked bool         `json:"agentInvoked"`
	Error        *PromptError `json:"error,omitempty"`
}

// PromptError is how a prompt's work failed, as the pinned Oh My Pi declares it: `{message,
// provider?, model?, httpStatus?, retryable, code?}`, the "Failure detail for a `prompt_result`
// with `status: \"error\"`" (`RpcPromptError` in packages/coding-agent/src/modes/rpc/rpc-types.ts
// of cybermonklabs/oh-my-pi at the pinned tag). Oh My Pi writes it for a prompt
// whose last assistant message ended with stopReason "error" — a provider request its own retries
// could not get through, where Retryable says it may still succeed later — and for one it failed
// outright before any turn.
//
// Code is Oh My Pi's class of the failure, when it gives one, and it declares three values there:
// `cost_cap`, the session reached `session.maxCost` and Oh My Pi sent no request; `session_busy`,
// the agent was busy: the prompt was refused, or it waited at least 30 s and another run still
// held the agent; and `cyber_policy`, OpenAI refused the session's content for cybersecurity risk,
// or Oh My Pi held the request after such a refusal. A `cyber_policy` failure always has Retryable
// false, and the status it carries is whatever a gateway relayed the refusal under, a 503 among
// them (`promptError` in packages/coding-agent/src/modes/rpc/rpc-prompt-results.ts there).
//
// Message is the reason a failed prompt is recorded with (PromptResult.Reason), and
// internal/stream carries Retryable, HTTPStatus, Provider, Model and Code onto its own
// PromptResult event. Supervise's providerErrored decides on HTTPStatus and Code: a
// `cyber_policy` failure holds the issue whatever its status, and a `cost_cap` one is named as the
// cause. Every member is modelled, read or not, so the frame survives this package whole, the way
// every other frame does. A decode that kept only what today's reader wants would quietly drop the
// provider, the model and the status behind a failure from anything that re-encodes it.
type PromptError struct {
	Message    string `json:"message"`
	Provider   string `json:"provider,omitempty"`
	Model      string `json:"model,omitempty"`
	HTTPStatus int    `json:"httpStatus,omitempty"`
	Retryable  bool   `json:"retryable"`
	Code       string `json:"code,omitempty"`
}

// Reason is Oh My Pi's message for work that ended in an error, and the empty string for work
// that did not: the reason a failed prompt is recorded with.
func (f PromptResult) Reason() string {
	if f.Error == nil {
		return ""
	}
	return f.Error.Message
}

// ToolExecutionStart is Oh My Pi beginning one tool call of a turn, and ToolExecutionEnd that
// same call finishing. They are the only frames that say an agent mid-turn is still moving: the
// daemon stamps each on receipt and keeps the last of them on the claim, so a turn whose agent
// has stopped stepping is found rather than read as hours of work (INFRA-1274).
//
// ToolCallID is what pairs an end with its start, and the only thing that does: two calls of one
// tool in one assistant message come back with byte-identical ToolName and Args, and their ends
// in either order. Args is the call's arguments as Oh My Pi wrote them, kept raw because their
// shape belongs to the tool; nothing in the daemon keeps their text, which routinely holds a
// credential (supervise's StepRecord). Intent is the agent's own words for the step.
//
// Neither frame carries a time, which is why the daemon stamps both on receipt. The end repeats
// the tool and the call id and nothing else of the call — no Args, no Intent — so whatever is
// recorded of the arguments beside an end is what the start left (supervise's openCall). Oh My
// Pi's own end carries a `result` object too, which nothing in the daemon reads; the decode
// drops it, as it drops the members of a prompt_result nothing reads. The Claude Code bridge
// writes a start with no call id at all (internal/claudebridge), which is a step with no call to
// name rather than a frame to refuse.
type ToolExecutionStart struct {
	ToolCallID string          `json:"toolCallId"`
	ToolName   string          `json:"toolName"`
	Args       json.RawMessage `json:"args"`
	Intent     string          `json:"intent"`
}

// ToolExecutionEnd is the call ToolExecutionStart opened, finished. IsError is whether the tool
// refused the call, which the daemon records nothing of: a call that ended is a step either way.
type ToolExecutionEnd struct {
	ToolCallID string `json:"toolCallId"`
	ToolName   string `json:"toolName"`
	IsError    bool   `json:"isError"`
}

// Shutdown asks the shim to end the wrapped process: SIGTERM, then SIGKILL if it is still running
// when the shim's grace runs out. The shim acts on it and never forwards it.
type Shutdown struct{}

// AdoptWorkingCopy asks the shim to run the working-copy adoption in its own workspace under the
// given jj identity. The daemon names the identity and the budget, never a command or a path.
type AdoptWorkingCopy struct {
	ID        string `json:"id"`
	JJUser    string `json:"jjUser"`
	JJEmail   string `json:"jjEmail"`
	TimeoutMs int    `json:"timeoutMs"`
}

// AdoptWorkingCopyResult answers AdoptWorkingCopy (worker-shim.ts:332-341).
type AdoptWorkingCopyResult struct {
	ID    string `json:"id"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// WorkspaceWork asks the shim whether the workspace it runs in holds work that no branch or tag
// on origin holds: a read, which moves no bytes of the work and writes only jj's snapshot of the
// working copy. The daemon names the issue whose workspace it asks about and the budget, never a
// command or a path: the shim reads the one workspace its environment names, which is that
// issue's, with the jj it already works with.
type WorkspaceWork struct {
	ID        string `json:"id"`
	Issue     string `json:"issue"`
	TimeoutMs int    `json:"timeoutMs"`
}

// MaxWorkspaceWorkCommits is the most work commits one WorkspaceWorkResult lists. Twenty ids are
// enough for a person to find the work, and they keep the answer one short line whatever the
// workspace holds; Count says how many there are in all.
const MaxWorkspaceWorkCommits = 20

// WorkspaceWorkResult answers WorkspaceWork: the commit the working copy sits on (Head), whether
// that commit holds a change (Changed), the first MaxWorkspaceWorkCommits work commits and how
// many there are in all (Count). It always fits one line, so it crosses the shim's lane with no
// rpc_chunk sequence.
type WorkspaceWorkResult struct {
	ID      string   `json:"id"`
	OK      bool     `json:"ok"`
	Error   string   `json:"error,omitempty"`
	Head    string   `json:"head,omitempty"`
	Changed bool     `json:"changed"`
	Commits []string `json:"commits,omitempty"`
	Count   int      `json:"count"`
}

// LauncherRestored is the report an earlier launcher sends, unasked, once it has restored a
// released pod's saved work into the pod that replaced it, and again on each later connection
// until one carries it. Earlier daemons saved a released pod's unpushed work and sent it to the
// replacement's launcher to restore; this daemon releases no pod whose workspace holds unpushed
// work, so it saves nothing and sends nothing. A pod an earlier daemon started still runs that
// earlier launcher after the upgrade, though, and a frame type the daemon's launcher loop does not
// know closes the connection, so the daemon decodes this one, logs it, and drops it
// (internal/runtime/sandbox's ServeLauncher). ID is the slice sequence the work arrived under,
// Release the release whose save it was, OK whether the restore put the work back, and Error why
// it did not.
type LauncherRestored struct {
	ID      string `json:"id"`
	Release int64  `json:"release,omitempty"`
	OK      bool   `json:"ok"`
	Error   string `json:"error,omitempty"`
}

// LauncherLeakCheck is one slice of the shared public leak check on its way to a pod's launcher:
// the daemon reads the check's directory at the tag the launcher's hello named
// (LauncherHello.LeakCheckTag), packs it, and pushes it before it starts a role of that launcher,
// so no role of the pod reads GitHub for it. The launcher has no rpc_chunk lane of its own, so
// the slicing is the frame's: Payload is the Index-th LauncherSlicePayloadBytes of the packed
// directory (SplitLeakCheck), Count how many slices there are, and SHA256 the digest of the body
// the slices rebuild, which the launcher checks before it installs anything. Tag is the tag the
// body was read at, which the launcher refuses unless it is its own pin. ID names the sequence,
// `leak-check-<tag>-<first 12 hex of the digest>`, so a repeated slice is recognised as a
// repeated launcher_start id is.
type LauncherLeakCheck struct {
	ID      string `json:"id"`
	Index   int    `json:"index"`
	Count   int    `json:"count"`
	Tag     string `json:"tag"`
	SHA256  string `json:"sha256"`
	Payload []byte `json:"payload"`
}

// LauncherLeakCheckResult answers one LauncherLeakCheck. OK is the slice taken. Installed is the
// launcher having installed the whole, digest-checked check at its tag, which it answers the last
// slice of the sequence with, or any slice of a sequence whose tag and digest it has already
// installed: the sequence is over and the daemon sends no more of it. A refusal carries Error, the
// pod's cause sentence for why the check could not be installed, which the daemon refuses the
// role's start with.
type LauncherLeakCheckResult struct {
	ID        string `json:"id"`
	Index     int    `json:"index"`
	OK        bool   `json:"ok"`
	Installed bool   `json:"installed,omitempty"`
	Error     string `json:"error,omitempty"`
}

// RPCChunk is one slice of a logical frame too large for a single line. Data is base64 of the
// frame's Index-th ChunkPayloadBytes slice; ByteLength is the whole frame's size. See chunks.go.
type RPCChunk struct {
	ChunkID    string `json:"chunkId"`
	Index      int    `json:"index"`
	Count      int    `json:"count"`
	ByteLength int    `json:"byteLength"`
	Data       string `json:"data"`
}

// Raw is a frame this package does not model: OMP's message updates and error events, and
// whatever a later OMP adds. It keeps the line verbatim and re-encodes to exactly those bytes,
// so a bridge that decodes to inspect a frame can still pass it on unchanged.
type Raw struct {
	Type string
	JSON json.RawMessage
}

func (Hello) FrameType() string                        { return TypeHello }
func (HelloAck) FrameType() string                     { return TypeHelloAck }
func (NegotiateProtocol) FrameType() string            { return TypeNegotiateProtocol }
func (Prompt) FrameType() string                       { return TypePrompt }
func (Response) FrameType() string                     { return TypeResponse }
func (GetState) FrameType() string                     { return TypeGetState }
func (Handoff) FrameType() string                      { return TypeHandoff }
func (AgentStart) FrameType() string                   { return TypeAgentStart }
func (AgentEnd) FrameType() string                     { return TypeAgentEnd }
func (ToolExecutionStart) FrameType() string           { return TypeToolExecutionStart }
func (ToolExecutionEnd) FrameType() string             { return TypeToolExecutionEnd }
func (PromptResult) FrameType() string                 { return TypePromptResult }
func (Shutdown) FrameType() string                     { return TypeShutdown }
func (AdoptWorkingCopy) FrameType() string             { return TypeAdoptWorkingCopy }
func (AdoptWorkingCopyResult) FrameType() string       { return TypeAdoptWorkingCopyResult }
func (RPCChunk) FrameType() string                     { return TypeRPCChunk }
func (r Raw) FrameType() string                        { return r.Type }
func (Hello2) FrameType() string                       { return TypeHello2 }
func (AgentSecretsEnrollment) FrameType() string       { return TypeAgentSecretsEnrollment }
func (AgentSecretsEnrollmentResult) FrameType() string { return TypeAgentSecretsEnrollmentResult }
func (AgentSecretsToken) FrameType() string            { return TypeAgentSecretsToken }
func (AgentSecretsTokenResult) FrameType() string      { return TypeAgentSecretsTokenResult }
func (LauncherHello) FrameType() string                { return TypeLauncherHello }
func (LauncherHelloAck) FrameType() string             { return TypeLauncherHelloAck }
func (LauncherState) FrameType() string                { return TypeLauncherState }
func (LauncherStart) FrameType() string                { return TypeLauncherStart }
func (LauncherStartResult) FrameType() string          { return TypeLauncherStartResult }
func (LauncherStop) FrameType() string                 { return TypeLauncherStop }
func (LauncherStopResult) FrameType() string           { return TypeLauncherStopResult }
func (WorkspaceWork) FrameType() string                { return TypeWorkspaceWork }
func (WorkspaceWorkResult) FrameType() string          { return TypeWorkspaceWorkResult }
func (LauncherRestored) FrameType() string             { return TypeLauncherRestored }
func (LauncherMemoryKill) FrameType() string           { return TypeLauncherMemoryKill }
func (LauncherVolumeReport) FrameType() string         { return TypeLauncherVolumeReport }
func (LauncherLeakCheck) FrameType() string            { return TypeLauncherLeakCheck }
func (LauncherLeakCheckResult) FrameType() string      { return TypeLauncherLeakCheckResult }

func (f Hello) MarshalJSON() ([]byte, error) {
	type plain Hello
	return marshalFrame(TypeHello, plain(f))
}

func (HelloAck) MarshalJSON() ([]byte, error) { return marshalFrame(TypeHelloAck, struct{}{}) }

func (f NegotiateProtocol) MarshalJSON() ([]byte, error) {
	type plain NegotiateProtocol
	return marshalFrame(TypeNegotiateProtocol, plain(f))
}

func (f Prompt) MarshalJSON() ([]byte, error) {
	type plain Prompt
	return marshalFrame(TypePrompt, plain(f))
}

func (f Response) MarshalJSON() ([]byte, error) {
	type plain Response
	return marshalFrame(TypeResponse, plain(f))
}

func (f GetState) MarshalJSON() ([]byte, error) {
	type plain GetState
	return marshalFrame(TypeGetState, plain(f))
}

func (f Handoff) MarshalJSON() ([]byte, error) {
	type plain Handoff
	return marshalFrame(TypeHandoff, plain(f))
}

func (f AgentStart) MarshalJSON() ([]byte, error) {
	type plain AgentStart
	return marshalFrame(TypeAgentStart, plain(f))
}

func (AgentEnd) MarshalJSON() ([]byte, error) { return marshalFrame(TypeAgentEnd, struct{}{}) }

func (f ToolExecutionStart) MarshalJSON() ([]byte, error) {
	type plain ToolExecutionStart
	return marshalFrame(TypeToolExecutionStart, plain(f))
}

func (f ToolExecutionEnd) MarshalJSON() ([]byte, error) {
	type plain ToolExecutionEnd
	return marshalFrame(TypeToolExecutionEnd, plain(f))
}

func (f PromptResult) MarshalJSON() ([]byte, error) {
	type plain PromptResult
	return marshalFrame(TypePromptResult, plain(f))
}

func (Shutdown) MarshalJSON() ([]byte, error) { return marshalFrame(TypeShutdown, struct{}{}) }

func (f AdoptWorkingCopy) MarshalJSON() ([]byte, error) {
	type plain AdoptWorkingCopy
	return marshalFrame(TypeAdoptWorkingCopy, plain(f))
}

func (f AdoptWorkingCopyResult) MarshalJSON() ([]byte, error) {
	type plain AdoptWorkingCopyResult
	return marshalFrame(TypeAdoptWorkingCopyResult, plain(f))
}

func (f RPCChunk) MarshalJSON() ([]byte, error) {
	type plain RPCChunk
	return marshalFrame(TypeRPCChunk, plain(f))
}

func (r Raw) MarshalJSON() ([]byte, error) { return r.JSON, nil }

func (f Hello2) MarshalJSON() ([]byte, error) {
	type plain Hello2
	return marshalFrame(TypeHello2, plain(f))
}

func (f AgentSecretsEnrollment) MarshalJSON() ([]byte, error) {
	type plain AgentSecretsEnrollment
	return marshalFrame(TypeAgentSecretsEnrollment, plain(f))
}

func (f AgentSecretsEnrollmentResult) MarshalJSON() ([]byte, error) {
	type plain AgentSecretsEnrollmentResult
	return marshalFrame(TypeAgentSecretsEnrollmentResult, plain(f))
}

func (f AgentSecretsToken) MarshalJSON() ([]byte, error) {
	type plain AgentSecretsToken
	return marshalFrame(TypeAgentSecretsToken, plain(f))
}

func (f AgentSecretsTokenResult) MarshalJSON() ([]byte, error) {
	type plain AgentSecretsTokenResult
	return marshalFrame(TypeAgentSecretsTokenResult, plain(f))
}

func (f LauncherHello) MarshalJSON() ([]byte, error) {
	type plain LauncherHello
	return marshalFrame(TypeLauncherHello, plain(f))
}

func (f LauncherHelloAck) MarshalJSON() ([]byte, error) {
	type plain LauncherHelloAck
	return marshalFrame(TypeLauncherHelloAck, plain(f))
}

func (f LauncherState) MarshalJSON() ([]byte, error) {
	type plain LauncherState
	return marshalFrame(TypeLauncherState, plain(f))
}

func (f LauncherStart) MarshalJSON() ([]byte, error) {
	type plain LauncherStart
	return marshalFrame(TypeLauncherStart, plain(f))
}

func (f LauncherStartResult) MarshalJSON() ([]byte, error) {
	type plain LauncherStartResult
	return marshalFrame(TypeLauncherStartResult, plain(f))
}

func (f LauncherStop) MarshalJSON() ([]byte, error) {
	type plain LauncherStop
	return marshalFrame(TypeLauncherStop, plain(f))
}

func (f LauncherStopResult) MarshalJSON() ([]byte, error) {
	type plain LauncherStopResult
	return marshalFrame(TypeLauncherStopResult, plain(f))
}

func (f WorkspaceWork) MarshalJSON() ([]byte, error) {
	type plain WorkspaceWork
	return marshalFrame(TypeWorkspaceWork, plain(f))
}

func (f WorkspaceWorkResult) MarshalJSON() ([]byte, error) {
	type plain WorkspaceWorkResult
	return marshalFrame(TypeWorkspaceWorkResult, plain(f))
}

func (f LauncherRestored) MarshalJSON() ([]byte, error) {
	type plain LauncherRestored
	return marshalFrame(TypeLauncherRestored, plain(f))
}

func (f LauncherMemoryKill) MarshalJSON() ([]byte, error) {
	type plain LauncherMemoryKill
	return marshalFrame(TypeLauncherMemoryKill, plain(f))
}

// MarshalJSON keeps `sizes` an array on the wire, empty rather than `null`, as
// record.UpdateStalled.MarshalJSON keeps `waiting` one and for the same reason: a strict reader
// refuses a null. The `plain` alias is what keeps the method from re-entering itself.
func (f LauncherVolumeReport) MarshalJSON() ([]byte, error) {
	type plain LauncherVolumeReport
	out := plain(f)
	if out.Sizes == nil {
		out.Sizes = []LauncherVolumeSize{}
	}
	return marshalFrame(TypeLauncherVolumeReport, out)
}

func (f LauncherLeakCheck) MarshalJSON() ([]byte, error) {
	type plain LauncherLeakCheck
	return marshalFrame(TypeLauncherLeakCheck, plain(f))
}

func (f LauncherLeakCheckResult) MarshalJSON() ([]byte, error) {
	type plain LauncherLeakCheckResult
	return marshalFrame(TypeLauncherLeakCheckResult, plain(f))
}

// Validate refuses a hello the listener would refuse (worker-stream-listener.ts:140-148).
func (f Hello) Validate() error {
	if f.BootToken == "" {
		return fmt.Errorf("%w: hello carries no bootToken", ErrMalformedFrame)
	}
	return nil
}

// Validate refuses an adoption request the shim would refuse. The daemon is its only sender, so
// a malformed one is a bug to name rather than a condition to paper over
// (worker-shim.ts:171-180).
func (f AdoptWorkingCopy) Validate() error {
	switch {
	case f.ID == "":
		return fmt.Errorf("%w: adopt-working-copy carries no id", ErrMalformedFrame)
	case f.JJUser == "" || f.JJEmail == "":
		return fmt.Errorf("%w: adopt-working-copy carries no jj identity", ErrMalformedFrame)
	case f.TimeoutMs <= 0:
		return fmt.Errorf("%w: adopt-working-copy timeoutMs is %d", ErrMalformedFrame, f.TimeoutMs)
	}
	return nil
}

// Validate refuses a handoff request Oh My Pi could not answer for this daemon: one with no
// request id to correlate its response by, and one with no instructions, whose document would
// not keep the reviewer's entry (Handoff).
func (f Handoff) Validate() error {
	switch {
	case f.ID == "":
		return fmt.Errorf("%w: handoff carries no id", ErrMalformedFrame)
	case f.CustomInstructions == "":
		return fmt.Errorf("%w: handoff carries no customInstructions", ErrMalformedFrame)
	}
	return nil
}

// issueKeyShape is a Dispatch issue key, the one shape an issue has. A workspace read names the
// issue whose workspace the shim reads, so a frame whose issue is not one is refused here rather
// than acted on.
var issueKeyShape = regexp.MustCompile(`^[A-Z][A-Z0-9]*-[0-9]+$`)

// Validate refuses a workspace read the shim would refuse. The daemon is its only sender.
func (f WorkspaceWork) Validate() error {
	switch {
	case f.ID == "":
		return fmt.Errorf("%w: workspace-work carries no id", ErrMalformedFrame)
	case !issueKeyShape.MatchString(f.Issue):
		return fmt.Errorf("%w: workspace-work issue %q is not an issue key", ErrMalformedFrame, f.Issue)
	case f.TimeoutMs <= 0:
		return fmt.Errorf("%w: workspace-work timeoutMs is %d", ErrMalformedFrame, f.TimeoutMs)
	}
	return nil
}

// Validate refuses a workspace read's answer the daemon could not act on: one answering no
// request, a refusal with no reason, a reading with no working-copy commit, more commits than
// MaxWorkspaceWorkCommits, a count below the commits it lists, or a head or a commit that is not a
// commit id.
//
// A commit id is 40 lowercase hexadecimal digits (commitIDShape), the ids jj prints for a git
// repository, and the daemon reads no other: the head and the commits come from the jj of the pod,
// which the pod's agent controls, and the daemon writes them into a hold record that every issue
// pod's agent reads (read_release_records) and into the message it sends the role (ASVS
// v5.0.0-2.2.1). A refusal's head and commits are read as nothing, and are checked all the same.
func (f WorkspaceWorkResult) Validate() error {
	switch {
	case f.ID == "":
		return fmt.Errorf("%w: workspace-work-result carries no id", ErrMalformedFrame)
	case !f.OK && f.Error == "":
		return fmt.Errorf("%w: workspace-work-result refuses without a reason", ErrMalformedFrame)
	case f.OK && f.Head == "":
		return fmt.Errorf("%w: workspace-work-result carries no head", ErrMalformedFrame)
	case f.Head != "" && !commitIDShape.MatchString(f.Head):
		return fmt.Errorf("%w: workspace-work-result head %q is not a commit id", ErrMalformedFrame, cutForError(f.Head))
	case len(f.Commits) > MaxWorkspaceWorkCommits:
		return fmt.Errorf("%w: workspace-work-result lists %d commits, limit %d", ErrMalformedFrame, len(f.Commits), MaxWorkspaceWorkCommits)
	case f.Count < len(f.Commits):
		return fmt.Errorf("%w: workspace-work-result counts %d commits and lists %d", ErrMalformedFrame, f.Count, len(f.Commits))
	}
	for i, commit := range f.Commits {
		if !commitIDShape.MatchString(commit) {
			return fmt.Errorf("%w: workspace-work-result commit %d, %q, is not a commit id", ErrMalformedFrame, i, cutForError(commit))
		}
	}
	return nil
}

// commitIDShape is a commit id as jj prints one for a git repository: 40 lowercase hexadecimal
// digits.
var commitIDShape = regexp.MustCompile(`^[0-9a-f]{40}$`)

// cutForError is at most the first 64 bytes of a value a refusal names, so a refusal of a long
// value stays one short line.
func cutForError(value string) string {
	if len(value) <= 64 {
		return value
	}
	return value[:64] + "…"
}

// digestShape is a hex-encoded SHA-256: 64 lowercase hex characters.
var digestShape = regexp.MustCompile(`^[0-9a-f]{64}$`)

// LauncherSlicePayloadBytes is how much of a sequence's body one launcher_leak_check frame
// carries. Base64 of it, plus the frame's other members, stays well inside MaxFrameBytes, which is
// the whole reason the sequence exists: the launcher reads one frame per line and nothing
// reassembles rpc_chunk on its side.
const LauncherSlicePayloadBytes = 256 << 10

// MaxLauncherSequenceBytes is the largest body a launcher_leak_check sequence carries, and every
// allocation this file makes for one is under it, as is every allocation the launcher makes for
// one. It is the wire's own bound rather than the reader's: the daemon refuses a packed check far
// below it (internal/leakguard's maxPackedBytes), and this bound says what the sequence itself
// will build, so a caller that never read that policy cannot ask for a slice count or a body
// buffer sized by a length nothing checked.
const MaxLauncherSequenceBytes = 64 << 20

// LeakCheckTagPattern is the published shape of a tag of the shared public leak check. The pattern
// is written here rather than imported so the wire depends on nothing of the daemon's; it is the
// one copy, which internal/leakguard reads too (ValidLeakCheckTag), since a tag becomes a path of a
// GitHub API endpoint there.
const LeakCheckTagPattern = `^leak-check-v[1-9][0-9]*$`

var leakCheckTagShape = regexp.MustCompile(LeakCheckTagPattern)

// ValidLeakCheckTag is whether tag is of the published shape LeakCheckTagPattern.
func ValidLeakCheckTag(tag string) bool { return leakCheckTagShape.MatchString(tag) }

// maxLauncherSlices is the most slices a sequence of LauncherSlicePayloadBytes each may count
// without the body they rebuild passing MaxLauncherSequenceBytes.
const maxLauncherSlices = MaxLauncherSequenceBytes / LauncherSlicePayloadBytes

// Validate refuses a leak check slice the launcher could not place in its sequence, or one whose
// count asks it to hold more than the sequence's bound.
func (f LauncherLeakCheck) Validate() error {
	switch {
	case f.ID == "":
		return fmt.Errorf("%w: launcher_leak_check carries no id", ErrMalformedFrame)
	case f.Count <= 0:
		return fmt.Errorf("%w: launcher_leak_check count is %d", ErrMalformedFrame, f.Count)
	case f.Count > maxLauncherSlices:
		return fmt.Errorf("%w: launcher_leak_check count is %d, which could pass the %d bytes a sequence carries",
			ErrMalformedFrame, f.Count, MaxLauncherSequenceBytes)
	case f.Index < 0 || f.Index >= f.Count:
		return fmt.Errorf("%w: launcher_leak_check index %d of %d", ErrMalformedFrame, f.Index, f.Count)
	case !ValidLeakCheckTag(f.Tag):
		return fmt.Errorf("%w: launcher_leak_check tag %q is not of the shape %s", ErrMalformedFrame, f.Tag, LeakCheckTagPattern)
	case len(f.Payload) == 0:
		return fmt.Errorf("%w: launcher_leak_check %d carries no payload", ErrMalformedFrame, f.Index)
	case len(f.Payload) > LauncherSlicePayloadBytes:
		return fmt.Errorf("%w: launcher_leak_check payload is %d bytes, limit %d", ErrMalformedFrame, len(f.Payload), LauncherSlicePayloadBytes)
	case !digestShape.MatchString(f.SHA256):
		return fmt.Errorf("%w: launcher_leak_check sha256 %q is not a hex SHA-256", ErrMalformedFrame, f.SHA256)
	}
	return nil
}

// Validate refuses a leak check answer the daemon could not act on: one answering no sequence or
// no slice of it, a refusal with no cause, or an install the launcher says it both made and refused.
func (f LauncherLeakCheckResult) Validate() error {
	switch {
	case f.ID == "":
		return fmt.Errorf("%w: launcher_leak_check_result carries no id", ErrMalformedFrame)
	case f.Index < 0:
		return fmt.Errorf("%w: launcher_leak_check_result index is %d", ErrMalformedFrame, f.Index)
	case !f.OK && f.Error == "":
		return fmt.Errorf("%w: launcher_leak_check_result refuses without a cause", ErrMalformedFrame)
	case !f.OK && f.Installed:
		return fmt.Errorf("%w: launcher_leak_check_result refuses an install it says it made", ErrMalformedFrame)
	}
	return nil
}

// SplitLeakCheck slices body, the shared public leak check's directory packed at tag, into the
// launcher_leak_check sequence that carries it, every slice declaring the tag and the digest of
// the whole body so the launcher can refuse one that did not arrive as it was sent or was read at
// another tag than its pin. The sequence's id is the tag and the digest's first 12 hex, so the
// same check pushed again is the same sequence and the launcher answers a slice it already took
// with the answer it gave.
func SplitLeakCheck(tag string, body []byte) ([]LauncherLeakCheck, error) {
	switch {
	case !ValidLeakCheckTag(tag):
		return nil, fmt.Errorf("%w: a launcher_leak_check sequence of the tag %q, which is not of the shape %s",
			ErrMalformedFrame, tag, LeakCheckTagPattern)
	case len(body) == 0:
		return nil, fmt.Errorf("%w: a launcher_leak_check sequence of %s carries no body", ErrMalformedFrame, tag)
	case len(body) > MaxLauncherSequenceBytes:
		return nil, fmt.Errorf("%w: a leak check body of %d bytes is over the %d a launcher_leak_check sequence carries",
			ErrMalformedFrame, len(body), MaxLauncherSequenceBytes)
	}
	digest := SequenceDigest(body)
	id := "leak-check-" + tag + "-" + digest[:12]
	count := (len(body) + LauncherSlicePayloadBytes - 1) / LauncherSlicePayloadBytes
	frames := make([]LauncherLeakCheck, 0, count)
	for payload := range slices.Chunk(body, LauncherSlicePayloadBytes) {
		frames = append(frames, LauncherLeakCheck{
			ID: id, Index: len(frames), Count: count, Tag: tag,
			SHA256: digest, Payload: payload,
		})
	}
	return frames, nil
}

// SequenceDigest is the digest a launcher_leak_check sequence declares for the body it carries.
func SequenceDigest(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// thumbprintShape is a base64url-encoded SHA-256: 43 characters, no padding (RFC 7638).
var thumbprintShape = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

// Validate refuses a hello2 the listener refuses: no boot token, or an identity that is not a
// thumbprint plus a JWT. The token is not verified here — the broker verifies it — only shaped.
func (f Hello2) Validate() error {
	if f.BootToken == "" {
		return fmt.Errorf("%w: hello2 carries no bootToken", ErrMalformedFrame)
	}
	if f.AgentSecrets == nil {
		return nil
	}
	switch id := f.AgentSecrets; {
	case id.Thumbprint == "":
		return fmt.Errorf("%w: hello2 agentSecrets carries no thumbprint", ErrMalformedFrame)
	case !thumbprintShape.MatchString(id.Thumbprint):
		return fmt.Errorf("%w: hello2 agentSecrets thumbprint is not a base64url SHA-256", ErrMalformedFrame)
	case id.PodToken == "":
		return fmt.Errorf("%w: hello2 agentSecrets carries no podToken", ErrMalformedFrame)
	case strings.Count(id.PodToken, ".") != 2:
		return fmt.Errorf("%w: hello2 agentSecrets podToken is not a JWT", ErrMalformedFrame)
	}
	return nil
}

// Validate refuses an unactionable launcher hello before the listener hands it to the runtime: a
// leak check said to be installed with no tag is one. The tag's shape is the daemon's to refuse
// when it serves the check (LauncherHello).
func (f LauncherHello) Validate() error {
	switch {
	case f.Token == "":
		return fmt.Errorf("%w: launcher_hello carries no token", ErrMalformedFrame)
	case f.Sandbox == "":
		return fmt.Errorf("%w: launcher_hello carries no sandbox", ErrMalformedFrame)
	case f.Role == "":
		return fmt.Errorf("%w: launcher_hello carries no role", ErrMalformedFrame)
	case f.PodUID == "":
		return fmt.Errorf("%w: launcher_hello carries no podUid", ErrMalformedFrame)
	case f.LauncherID == "":
		return fmt.Errorf("%w: launcher_hello carries no launcherId", ErrMalformedFrame)
	case f.LeakCheckInstalled && f.LeakCheckTag == "":
		return fmt.Errorf("%w: launcher_hello says a leak check is installed and names no leakCheckTag", ErrMalformedFrame)
	}
	return nil
}

// Validate refuses a launcher start the launcher cannot act on: one with no id, no generation or
// no argv, or with a file name that is not a plain file name. A start that names its session by
// its id alone (ResumeSession) is refused too when it also names a file (ResumeFile), which is two
// sessions to resume, when the id is not a single path element (it holds `/` or NUL), which no
// session file name ends in, or when its argv does not hold exactly one element
// `--resume=<ResumeSession>`: the launcher resumes the file it finds for the id by replacing that
// one element with the file's path, and with none or two there is nothing certain to replace. The
// daemon is the frame's only sender, so each of these is a bug of the daemon's, which the refusal
// names.
func (f LauncherStart) Validate() error {
	switch {
	case f.ID == "":
		return fmt.Errorf("%w: launcher_start carries no id", ErrMalformedFrame)
	case f.Generation == 0:
		return fmt.Errorf("%w: launcher_start carries no generation", ErrMalformedFrame)
	case len(f.Argv) == 0:
		return fmt.Errorf("%w: launcher_start carries no argv", ErrMalformedFrame)
	}
	for name := range f.Files {
		if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\x00") {
			return fmt.Errorf("%w: launcher_start file name %q is not a plain file name", ErrMalformedFrame, name)
		}
	}
	if f.ResumeSession != "" {
		if f.ResumeFile != "" {
			return fmt.Errorf("%w: launcher_start names both resumeFile and resumeSession", ErrMalformedFrame)
		}
		if strings.ContainsAny(f.ResumeSession, "/\x00") {
			return fmt.Errorf("%w: launcher_start resumeSession %q is not a single path element", ErrMalformedFrame, f.ResumeSession)
		}
		resume := "--resume=" + f.ResumeSession
		held := 0
		for _, arg := range f.Argv {
			if arg == resume {
				held++
			}
		}
		if held != 1 {
			return fmt.Errorf("%w: launcher_start argv holds %s %d times, want it exactly once for resumeSession", ErrMalformedFrame, resume, held)
		}
	}
	return nil
}

func (f LauncherStop) Validate() error {
	switch {
	case f.ID == "":
		return fmt.Errorf("%w: launcher_stop carries no id", ErrMalformedFrame)
	case f.Generation == 0:
		return fmt.Errorf("%w: launcher_stop carries no generation", ErrMalformedFrame)
	case f.GraceMs <= 0:
		return fmt.Errorf("%w: launcher_stop graceMs is %d", ErrMalformedFrame, f.GraceMs)
	}
	return nil
}

// Validate refuses guard settings outside the ranges both sides of the wire hold them to, naming
// the member and its range. The launcher checks them again rather than trusting the daemon's
// loader, because a daemon of another release may hold other ranges (MemoryGuardActMin).
func (g LauncherMemoryGuard) Validate() error {
	for _, member := range []struct {
		name          string
		value, lo, hi int
	}{
		{"actPercent", g.ActPercent, MemoryGuardActMin, MemoryGuardActMax},
		{"floorPercent", g.FloorPercent, MemoryGuardFloorMin, MemoryGuardFloorMax},
		{"waitSeconds", g.WaitSeconds, MemoryGuardWaitMin, MemoryGuardWaitMax},
		{"intervalMilliseconds", g.IntervalMilliseconds, MemoryGuardIntervalMin, MemoryGuardIntervalMax},
	} {
		if member.value < member.lo || member.value > member.hi {
			return fmt.Errorf("%w: launcher_hello_ack memoryGuard.%s is %d, not between %d and %d",
				ErrMalformedFrame, member.name, member.value, member.lo, member.hi)
		}
	}
	return nil
}

// Validate refuses a kill report the daemon could not apply once or tell a role of: one naming no
// guard or a sequence below the 1 the guard counts from, which together are the report's
// identity; one naming no process or no limit; one whose executable is longer than the bound a
// role's note is held to; and one whose cause is none of the three MemoryKillCause values, which
// no note says.
func (f LauncherMemoryKill) Validate() error {
	switch {
	case f.Guard == "":
		return fmt.Errorf("%w: launcher_memory_kill carries no guard", ErrMalformedFrame)
	case f.Sequence < 1:
		return fmt.Errorf("%w: launcher_memory_kill sequence is %d", ErrMalformedFrame, f.Sequence)
	case f.PID <= 0:
		return fmt.Errorf("%w: launcher_memory_kill pid is %d", ErrMalformedFrame, f.PID)
	case f.LimitBytes == 0:
		return fmt.Errorf("%w: launcher_memory_kill carries no limitBytes", ErrMalformedFrame)
	case len(f.Executable) > MemoryKillExecutableBytes:
		return fmt.Errorf("%w: launcher_memory_kill executable is %d bytes, limit %d", ErrMalformedFrame, len(f.Executable), MemoryKillExecutableBytes)
	case f.Cause != MemoryKillCauseGuard && f.Cause != MemoryKillCauseKernel && f.Cause != MemoryKillCauseKernelProtected:
		return fmt.Errorf("%w: launcher_memory_kill cause is %q, not %q, %q or none",
			ErrMalformedFrame, f.Cause, MemoryKillCauseKernel, MemoryKillCauseKernelProtected)
	}
	return nil
}

// Validate refuses a volume report the daemon could not count: one naming no trimmer, which is
// the identity its notice bounds count a restarted roles container apart by, and one whose
// episode is below the 1 the trimmer counts from. It refuses, too, a report holding more entries
// in a list than that list's cap (VolumeReportSizes and the three beside it): the daemon renders
// every entry it is given into a notice, and the launcher's own cut runs only where the sender is
// that launcher.
func (f LauncherVolumeReport) Validate() error {
	switch {
	case f.Trimmer == "":
		return fmt.Errorf("%w: launcher_volume_report carries no trimmer", ErrMalformedFrame)
	case f.Episode < 1:
		return fmt.Errorf("%w: launcher_volume_report episode is %d", ErrMalformedFrame, f.Episode)
	}
	for _, list := range []struct {
		name      string
		held, cap int
	}{
		{"sizes", len(f.Sizes), VolumeReportSizes},
		{"sizesMissing", len(f.SizesMissing), VolumeReportSizesMissing},
		{"refusedReads", len(f.RefusedReads), VolumeReportRefusedReads},
		{"faults", len(f.Faults), VolumeReportFaults},
	} {
		if list.held > list.cap {
			return fmt.Errorf("%w: launcher_volume_report %s holds %d entries, past the %d a report may hold", ErrMalformedFrame, list.name, list.held, list.cap)
		}
	}
	return nil
}

// Validate refuses an enrollment frame with no request id or no enrollment id: the daemon is its
// only sender, so either is a bug to name.
func (f AgentSecretsEnrollment) Validate() error {
	switch {
	case f.ID == "":
		return fmt.Errorf("%w: agent-secrets-enrollment carries no id", ErrMalformedFrame)
	case f.EnrollmentID == "":
		return fmt.Errorf("%w: agent-secrets-enrollment carries no enrollmentId", ErrMalformedFrame)
	}
	return nil
}

// Validate refuses a token request with no request id: the daemon is its only sender.
func (f AgentSecretsToken) Validate() error {
	if f.ID == "" {
		return fmt.Errorf("%w: agent-secrets-token carries no id", ErrMalformedFrame)
	}
	return nil
}

// Decode reads one line as a frame. A type this package models decodes to its own Go type; any
// other object — including one with no type at all — decodes to Raw, because the shim is a
// bridge and a frame it does not understand still has somewhere to go. Only a line that is not a
// JSON object, or a modelled frame whose members are of the wrong JSON type, is an error.
//
// The returned frame owns its bytes: line may be reused as soon as Decode returns.
func Decode(line []byte) (Frame, error) {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		// The line is the far side's: the URL rule reads it whole before it is cut (redact.Runes).
		return nil, fmt.Errorf("%w: %q", ErrNotAFrame, redact.Runes(string(line), 80))
	}
	var head struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(trimmed, &head); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrNotAFrame, err)
	}
	switch head.Type {
	case TypeHello:
		return decodeInto[Hello](trimmed)
	case TypeHelloAck:
		return HelloAck{}, nil
	case TypeNegotiateProtocol:
		return decodeInto[NegotiateProtocol](trimmed)
	case TypePrompt:
		return decodeInto[Prompt](trimmed)
	case TypeResponse:
		return decodeInto[Response](trimmed)
	case TypeGetState:
		return decodeInto[GetState](trimmed)
	case TypeHandoff:
		return decodeInto[Handoff](trimmed)
	case TypeAgentStart:
		return decodeInto[AgentStart](trimmed)
	case TypeAgentEnd:
		return AgentEnd{}, nil
	case TypeToolExecutionStart:
		return decodeInto[ToolExecutionStart](trimmed)
	case TypeToolExecutionEnd:
		return decodeInto[ToolExecutionEnd](trimmed)
	case TypePromptResult:
		return decodeInto[PromptResult](trimmed)
	case TypeShutdown:
		return Shutdown{}, nil
	case TypeAdoptWorkingCopy:
		return decodeInto[AdoptWorkingCopy](trimmed)
	case TypeAdoptWorkingCopyResult:
		return decodeInto[AdoptWorkingCopyResult](trimmed)
	case TypeRPCChunk:
		return decodeInto[RPCChunk](trimmed)
	case TypeHello2:
		return decodeInto[Hello2](trimmed)
	case TypeAgentSecretsEnrollment:
		return decodeInto[AgentSecretsEnrollment](trimmed)
	case TypeAgentSecretsEnrollmentResult:
		return decodeInto[AgentSecretsEnrollmentResult](trimmed)
	case TypeAgentSecretsToken:
		return decodeInto[AgentSecretsToken](trimmed)
	case TypeAgentSecretsTokenResult:
		return decodeInto[AgentSecretsTokenResult](trimmed)
	case TypeLauncherHello:
		return decodeInto[LauncherHello](trimmed)
	case TypeLauncherHelloAck:
		return decodeInto[LauncherHelloAck](trimmed)
	case TypeLauncherState:
		return decodeInto[LauncherState](trimmed)
	case TypeLauncherStart:
		return decodeInto[LauncherStart](trimmed)
	case TypeLauncherStartResult:
		return decodeInto[LauncherStartResult](trimmed)
	case TypeLauncherStop:
		return decodeInto[LauncherStop](trimmed)
	case TypeLauncherStopResult:
		return decodeInto[LauncherStopResult](trimmed)
	case TypeWorkspaceWork:
		return decodeInto[WorkspaceWork](trimmed)
	case TypeWorkspaceWorkResult:
		return decodeInto[WorkspaceWorkResult](trimmed)
	case TypeLauncherRestored:
		return decodeInto[LauncherRestored](trimmed)
	case TypeLauncherMemoryKill:
		return decodeInto[LauncherMemoryKill](trimmed)
	case TypeLauncherVolumeReport:
		return decodeInto[LauncherVolumeReport](trimmed)
	case TypeLauncherLeakCheck:
		return decodeInto[LauncherLeakCheck](trimmed)
	case TypeLauncherLeakCheckResult:
		return decodeInto[LauncherLeakCheckResult](trimmed)
	default:
		return Raw{Type: head.Type, JSON: bytes.Clone(trimmed)}, nil
	}
}

// decodeInto unmarshals a modelled frame. The `plain` alias is what keeps this from re-entering
// the frame's own MarshalJSON side: only the fields are read here.
func decodeInto[T Frame](line []byte) (Frame, error) {
	var frame T
	if err := json.Unmarshal(line, &frame); err != nil {
		return nil, fmt.Errorf("decode the %s frame: %w", frame.FrameType(), err)
	}
	return frame, nil
}

// marshalFrame renders a frame's own fields plus its type member. No frame struct carries a type
// field: the type is the frame's identity, not data a caller could set to something else. Every
// name it is given is a constant of this package, none of which needs JSON escaping.
func marshalFrame(frameType string, fields any) ([]byte, error) {
	rest, err := json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	head := `{"type":"` + frameType + `"`
	if len(rest) == len("{}") {
		return []byte(head + "}"), nil
	}
	out := make([]byte, 0, len(head)+len(rest))
	out = append(out, head...)
	out = append(out, ',')
	return append(out, rest[1:]...), nil
}
