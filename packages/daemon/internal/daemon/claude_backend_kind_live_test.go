// The Claude Code backend in an issue pod, which the kind job of ci.yaml runs on the rig of
// kind_tree_rig_live_test.go: a daemon whose legion.yaml puts the implementer on Claude Code
// (`worker_backends: {implementer: claude}`) and whose runtime is Agent Sandbox launches one
// issue's pod in the kind cluster, and the implementer runs in that pod's one `roles` container,
// in namespaces of its own, as `legion claude-bridge -- claude …`: the Claude Code its generation's
// start script installed at launch from the image's pin, with the image's claude-envoy plugin as
// its MCP server, and its CLAUDE_CONFIG_DIR on the pod's own disk, which dies with the pod. The
// architect beside it is Oh My Pi. One Messages stand-in answers both, and the fake GitHub serves
// the issue's repository and branch.
//
// It holds the three checks of the Claude Code lane that only a pod shows: the implementer
// registers its pod's Claude Code transcript as its session; its command line is the bridge
// running the pinned Claude Code its own generation installed; and, the pod deleted and its disk
// with it, the transcript lives on as a row of the session table, the replacement restores it and
// resumes the same session with its earlier turns in context, and the architect comes back in the
// replacement too. The Stop hook's reminder and the completion through the MCP tool are held by
// packages/claude-envoy/tests/legion-phase-hook-claude.test.ts and TestHandoffRecordsLive, and the
// tmux lane's whole path by TestTheClaudeCodeImplementerRegistersCompletesThroughItsStopHookAndResumesLive.
package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ompsessions"
	"github.com/sjawhar/legion/daemon/internal/phase"
)

// Where the Claude Code lane keeps its files in an issue pod: the implementer's CLAUDE_CONFIG_DIR
// under its role's state home, the root its generations install Claude Code under, the binary
// each install holds, and the image's pin of the version.
const (
	claudeKindConfigDir   = "/legion/scratch/state/implementer/claude"
	claudeKindInstallRoot = "/legion/scratch/state/implementer/legion-claude-code"
	claudeKindBinary      = "node_modules/@anthropic-ai/claude-code-linux-x64/claude"
	claudeKindPin         = "/opt/legion/claude-code/package.json"
)

// claudeKindLive is one run: the rig, the issue, and what each check reads for the next.
type claudeKindLive struct {
	*kindTreeRig
	key string
	// implementer is the implementer's claim as check 1 read it, and transcript its session file.
	implementer api.OperatorClaim
	transcript  string
}

// TestTheClaudeCodeImplementerInAnIssuePodRegistersRunsThePinnedBridgeAndResumesLive boots the
// daemon on the kind rig with the implementer on Claude Code and runs the lane's three checks, each
// a subtest.
func TestTheClaudeCodeImplementerInAnIssuePodRegistersRunsThePinnedBridgeAndResumesLive(t *testing.T) {
	rig := newKindTreeRig(t, kindTreeOptions{
		prefix: "CK", admissionCap: 1,
		topLevel: "worker_backends:\n  implementer: claude\n",
		podEnv: map[string]string{
			"ANTHROPIC_BASE_URL": kindStandinURL, "ANTHROPIC_MODEL": kindTreeModel,
			// Behind ANTHROPIC_BASE_URL, Claude Code defers every MCP tool behind ToolSearch; with
			// this value each turn offers the `legion` server's tools by name.
			"ENABLE_TOOL_SEARCH":                "false",
			"DISABLE_NON_ESSENTIAL_MODEL_CALLS": "1", "DISABLE_AUTOUPDATER": "1", "DISABLE_TELEMETRY": "1", "DISABLE_ERROR_REPORTING": "1",
		},
		providerKeys: map[string]string{"ANTHROPIC_API_KEY": kindTreeAnthropicKey},
	})
	r := &claudeKindLive{kindTreeRig: rig}
	r.key = r.seedRoot("the Claude Code implementer in an issue pod", phase.Implementing, claim.RoleImplementer)
	r.branch(r.key)
	r.boot()
	r.checkpoint("claude-implementer-registers", r.registers)
	r.checkpoint("claude-pod-runs-the-bridge", r.runsTheBridge)
	r.checkpoint("pod-deleted-resumes-the-same-claude-session", r.resumesAfterThePodIsDeleted)
}

// Check 1. The implementer's pod launches and its claim registers a Claude Code transcript as its
// session: the file Claude Code records under the CLAUDE_CONFIG_DIR the launch places on the pod's
// disk, under the role's own state home, which the implementer's Claude Code says it was told (its
// own environment, read in its namespaces, since the runtime carries CLAUDE_CONFIG_DIR in the
// launcher's start command and not the pod's spec). The claim registers the transcript only once
// the launcher's first save of it has committed, so the session table already holds its row.
func (r *claudeKindLive) registers() {
	t := r.t
	held := r.awaitLive(r.key, claim.RoleImplementer)
	pod := held.Locator.Sandbox.Name
	env, err := r.roleEnviron(pod, claim.RoleImplementer, "/claude")
	if err != nil {
		t.Fatalf("pod %s has no readable Claude Code environment in its implementer's namespaces: %v", pod, err)
	}
	if got := kindEnvValue(env, "CLAUDE_CONFIG_DIR"); got != claudeKindConfigDir {
		t.Fatalf("the implementer's Claude Code is told CLAUDE_CONFIG_DIR=%q, want %s, a directory of its role's state home on the pod's disk", got, claudeKindConfigDir)
	}
	transcript := held.SessionFile
	if !strings.HasPrefix(transcript, claudeKindConfigDir+"/projects/") || !strings.HasSuffix(transcript, "/"+held.Session+".jsonl") {
		t.Fatalf("the implementer registered session file %q, want %s/projects/<cwd>/%s.jsonl", transcript, claudeKindConfigDir, held.Session)
	}
	size := r.transcriptSize(pod, transcript)
	if size == 0 {
		t.Fatalf("the implementer's transcript %s is empty in pod %s", transcript, pod)
	}
	row, ok := r.sessionRow(transcript)
	if !ok {
		t.Fatalf("the session table holds no row for the registered transcript %s", transcript)
	}
	r.implementer, r.transcript = held, transcript
	r.note("implementer claim %s: generation %d, pod %s (uid %s), Claude Code session %s", held.Token, held.Generation, pod, held.Locator.Sandbox.PodUID, held.Session)
	r.note("its transcript %s holds %d bytes on the pod's disk under CLAUDE_CONFIG_DIR=%s, and the session table's row %d", transcript, size, claudeKindConfigDir, len(row))
}

// Check 2. The implementer's command line is the Claude Code launch: `legion claude-bridge --`
// running the Claude Code this generation's start script installed, on this generation's session,
// with the MCP document the launcher wrote into the generation's private directory as its
// `--mcp-config`. The image carries Claude Code's pin and no Claude Code: the bridge names, and the
// process under it is, the binary under this generation's own directory of the install root on the
// pod's disk, never /usr/local/bin/claude; and that binary prints the version the pin names.
func (r *claudeKindLive) runsTheBridge() {
	t := r.t
	pod, generation := r.implementer.Locator.Sandbox.Name, r.implementer.Generation
	bridge, err := r.roleArgv(pod, claim.RoleImplementer, 1, "claude-bridge")
	if err != nil {
		t.Fatalf("the implementer's namespaces of %s run no Claude Code under legion claude-bridge: %v", pod, err)
	}
	if !argvHasPair(bridge, "claude-bridge", "--") || !argvHasPair(bridge, "--session-id", r.implementer.Session) {
		t.Fatalf("the implementer's command line is %q, want claude-bridge -- … --session-id %s", bridge, r.implementer.Session)
	}
	installed := claudeKindInstall(generation)
	if got := argValue(bridge, "--"); got != installed {
		t.Fatalf("the bridge runs %s, not the Claude Code generation %d's start script installed, %s", got, generation, installed)
	}
	agent, err := r.roleArgv(pod, claim.RoleImplementer, 0, installed)
	if err != nil || len(agent) == 0 || agent[0] != installed {
		t.Fatalf("the process under the bridge is %q (%v), not %s", agent, err, installed)
	}
	if out, err := r.roleExec(pod, claim.RoleImplementer, "test", "-e", "/usr/local/bin/claude"); err == nil {
		t.Fatalf("pod %s's image carries /usr/local/bin/claude; the worker image carries Claude Code's pin and no Claude Code: %s", pod, out)
	}
	manifest, err := r.roleExec(pod, claim.RoleImplementer, "cat", claudeKindPin)
	if err != nil {
		t.Fatalf("pod %s carries no Claude Code pin at %s: %v", pod, claudeKindPin, err)
	}
	var pin struct {
		Dependencies map[string]string `json:"dependencies"`
	}
	if err := json.Unmarshal([]byte(manifest), &pin); err != nil || pin.Dependencies["@anthropic-ai/claude-code"] == "" {
		t.Fatalf("%s names no @anthropic-ai/claude-code: %v: %s", claudeKindPin, err, manifest)
	}
	pinned := pin.Dependencies["@anthropic-ai/claude-code"]
	version, err := r.roleExec(pod, claim.RoleImplementer, installed, "--version")
	if err != nil || strings.Fields(version + " ")[0] != pinned {
		t.Fatalf("the installed Claude Code %s prints --version %q (%v); the image's pin names %s", installed, version, err, pinned)
	}
	mcp := argValue(bridge, "--mcp-config")
	if mcp == "" {
		t.Fatalf("the Claude Code launch carries no --mcp-config: %q", bridge)
	}
	if out, err := r.roleExec(pod, claim.RoleImplementer, "test", "-f", mcp); err != nil {
		t.Fatalf("the launcher file %s the launch reads its MCP document from is not in the implementer's view of the pod: %v %s", mcp, err, out)
	}
	r.note("pod %s, the implementer's namespaces: %s", pod, strings.Join(bridge, " "))
	r.note("its Claude Code is %s, installed at launch from %s: --version %s, the pin's %s; its MCP document is %s", installed, claudeKindPin, version, pinned, mcp)
}

// Check 3. The pod is deleted, and its disk with it, the transcript's file included: the
// transcript lives on as a row of the session table, and the replacement resumes the same Claude
// Code session from that row. Before the deletion the transcript's size and the sha256 of those
// bytes are read in the pod; once no pod holds the deleted one's uid, the row's first that many
// bytes have that sha256, and so do the restored file's in the new pod. The claim keeps its session
// and session file, its new generation runs `--resume <id>` with the binary its own generation
// installed, the deleted generation's install went with the deleted disk, and the resumed session's
// next request carries the stand-in's earlier answer, the run's marker, in its context. The
// architect, an Oh My Pi role of the same pod whose session is a row of the same table, runs a
// later generation in the replacement and registers there.
func (r *claudeKindLive) resumesAfterThePodIsDeleted() {
	t := r.t
	held := r.claimNow(claim.RoleImplementer)
	pod, uid, generation := held.Locator.Sandbox.Name, held.Locator.Sandbox.PodUID, held.Generation
	// Check 1 waits for the implementer alone, so the architect's launch into the same pod may
	// still be running: the deletion waits until it has registered there.
	architect := r.awaitLive(r.key, claim.RoleArchitect)
	size := r.transcriptSize(pod, r.transcript)
	prefix, err := r.roleExec(pod, claim.RoleImplementer, "sh", "-c", `head -c "$1" "$2" | sha256sum | cut -d' ' -f1`, "prefix", strconv.FormatInt(size, 10), r.transcript)
	if err != nil {
		t.Fatalf("hash the first %d bytes of %s in pod %s: %v", size, r.transcript, pod, err)
	}
	if _, err := r.kubectl("delete", "pod", pod, "--wait=false"); err != nil {
		t.Fatalf("the operator could not delete pod %s: %v", pod, err)
	}
	r.awaitPodGone(pod, uid)
	row, ok := r.sessionRow(r.transcript)
	if !ok || int64(len(row)) < size {
		t.Fatalf("the session table's row for %s holds %d bytes once pod %s is gone, short of the %d the pod held", r.transcript, len(row), pod, size)
	}
	if sum := sha256.Sum256(row[:size]); hex.EncodeToString(sum[:]) != prefix {
		t.Fatalf("the first %d bytes of the session table's row for %s are not the transcript's in pod %s before its deletion (sha256 %s)", size, r.transcript, pod, prefix)
	}
	r.note("pod %s (uid %s) is gone; the session table's row for %s holds %d bytes, its first %d the transcript's from before the deletion", pod, uid, r.transcript, len(row), size)

	since := time.Now()
	var resumed api.OperatorClaim
	r.await("the implementer to relaunch in a replacement pod and register", kindTreeWait, func() bool {
		resumed = r.claimNow(claim.RoleImplementer)
		return kindLive(resumed) && resumed.Generation > generation && resumed.Locator != nil && resumed.Locator.Sandbox != nil &&
			resumed.Locator.Sandbox.PodUID != uid
	})
	if resumed.Session != held.Session || resumed.SessionFile != r.transcript {
		t.Fatalf("the relaunch registered session %s at %s, want %s at %s", resumed.Session, resumed.SessionFile, held.Session, r.transcript)
	}
	newPod, newUID := resumed.Locator.Sandbox.Name, resumed.Locator.Sandbox.PodUID
	bridge, err := r.roleArgv(newPod, claim.RoleImplementer, 1, "claude-bridge")
	if err != nil || !argvHasPair(bridge, "--resume", held.Session) {
		t.Fatalf("the relaunched claude was not started with --resume %s: %q (%v)", held.Session, bridge, err)
	}
	reinstalled := claudeKindInstall(resumed.Generation)
	if got := argValue(bridge, "--"); got != reinstalled {
		t.Fatalf("the relaunched bridge runs %s, not the Claude Code generation %d's start script installed, %s", got, resumed.Generation, reinstalled)
	}
	if out, err := r.roleExec(newPod, claim.RoleImplementer, "test", "-x", reinstalled); err != nil {
		t.Fatalf("the replacement pod holds no executable Claude Code at %s: %v %s", reinstalled, err, out)
	}
	if _, err := r.roleExec(newPod, claim.RoleImplementer, "test", "-e", fmt.Sprintf("%s/g%d", claudeKindInstallRoot, generation)); err == nil {
		t.Fatalf("the deleted generation %d's Claude Code install is in the replacement pod, whose disk is its own", generation)
	}
	restored, err := r.roleExec(newPod, claim.RoleImplementer, "sh", "-c", `head -c "$1" "$2" | sha256sum | cut -d' ' -f1`, "prefix", strconv.FormatInt(size, 10), r.transcript)
	if err != nil || restored != prefix {
		t.Fatalf("the transcript the replacement pod restored at %s does not begin with the %d bytes the table and the deleted pod held (sha256 %s, read %s, %v)", r.transcript, size, prefix, restored, err)
	}
	var architectNow api.OperatorClaim
	r.await("the architect to relaunch past generation "+strconv.FormatUint(architect.Generation, 10)+" in the replacement pod", kindTreeWait, func() bool {
		architectNow = r.claimNow(claim.RoleArchitect)
		return kindLive(architectNow) && architectNow.Generation > architect.Generation && architectNow.Locator != nil &&
			architectNow.Locator.Sandbox != nil && architectNow.Locator.Sandbox.PodUID == newUID
	})
	r.send(held.Session, "Kind proof after a relaunch: reply with one line starting WAITING that quotes the proof marker of your earlier turns. Change nothing and call no tool.")
	r.await("the resumed session's next request to carry its earlier turns' marker", 5*time.Minute, func() bool {
		return len(r.model.requests(since, r.model.marker, "mcp__legion__")) > 0
	})
	r.note("deleted pod %s (uid %s, generation %d): generation %d runs %s --resume %s in pod %s (uid %s), generation %d's install gone with the deleted disk",
		pod, uid, generation, resumed.Generation, reinstalled, held.Session, newPod, newUID, generation)
	r.note("the architect registered at generation %d (was %d) in the replacement pod; the resumed session's request carried %s", architectNow.Generation, architect.Generation, r.model.marker)
}

// claimNow is role's claim on the issue as the listing shows it now.
func (r *claudeKindLive) claimNow(role claim.Role) api.OperatorClaim {
	held, ok := r.claimOf(r.key, role)
	if !ok {
		r.t.Fatalf("the daemon holds no %s claim of %s", role, r.key)
	}
	return held
}

// transcriptSize is the size of the transcript in the implementer's view of pod.
func (r *claudeKindLive) transcriptSize(pod, transcript string) int64 {
	out, err := r.roleExec(pod, claim.RoleImplementer, "sh", "-c", `wc -c <"$1"`, "size", transcript)
	if err != nil {
		r.t.Fatalf("read the size of %s in pod %s: %v", transcript, pod, err)
	}
	size, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil {
		r.t.Fatalf("the size of %s in pod %s reads %q", transcript, pod, out)
	}
	return size
}

// sessionRow is the session table's row for path, as a launcher reads it to restore the file.
func (r *claudeKindLive) sessionRow(path string) ([]byte, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	conn, err := ompsessions.ConnectURL(ctx, r.pg.Host)
	if err != nil {
		r.t.Fatalf("connect to the session store: %v", err)
	}
	defer conn.Close(ctx)
	content, ok, err := ompsessions.Read(ctx, conn, path)
	if err != nil {
		r.t.Fatalf("read the session table's row for %s: %v", path, err)
	}
	return content, ok
}

// claudeKindInstall is the Claude Code binary generation installs on the pod's disk.
func claudeKindInstall(generation uint64) string {
	return fmt.Sprintf("%s/g%d/%s", claudeKindInstallRoot, generation, claudeKindBinary)
}

// kindEnvValue is NAME's value in an environment of NAME=value lines, "" when it is unset.
func kindEnvValue(env []string, name string) string {
	if i := slices.IndexFunc(env, func(line string) bool { return strings.HasPrefix(line, name+"=") }); i >= 0 {
		return strings.TrimPrefix(env[i], name+"=")
	}
	return ""
}
