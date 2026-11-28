package main

// agent_memory_group_test.go holds check c20: in a Docker container whose memory limit is 8 GiB,
// the server ends its agent above the agent's ceiling before the kernel's out-of-memory killer
// acts in the container. Only its CI job runs it (DISPATCH_TEST_MEMORY_GROUP=1), on an amd64
// runner with control groups v2 and Docker; everywhere else it skips.

import (
	"bytes"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// memoryGroupImage is the image of the test's container, into which the test mounts the server's
// binary: ghcr.io/cybermonklabs/legion/ci, the copy of debian:bookworm-slim
// (.github/ci-images.yaml). DISPATCH_TEST_MEMORY_GROUP_IMAGE names another. The CI job pulls it
// before the test runs; keep it a single-line string constant.
const memoryGroupImage = "ghcr.io/cybermonklabs/legion/ci@sha256:99c0b25c46d0214d5a2091092e7908907dcaecd6a962cde0ac700acee222b6f3"

const (
	// memoryGroupLimit is the container's memory limit L, which docker run's --memory 8g sets.
	memoryGroupLimit int64 = 8 << 30
	// memoryGroupWindow is the span of the agent's growth that c20 measures: the 50 ms between two
	// of the server's reads of the agent's memory that section 5 of the background-runs design
	// sets. It is fixed here rather than read from agentMemoryEvery, so a slower watch meets the
	// margin a 50 ms watch needs.
	memoryGroupWindow = 50 * time.Millisecond
	// productionGrowthMargin is the room production's container of 24 GiB keeps for the agent's
	// growth between two reads (section 5).
	productionGrowthMargin int64 = 1 << 30
	// memoryGroupGrowAfter is how long after each start the agent waits before it allocates at
	// full speed (DISPATCH_TEST_AGENT_GROW_AFTER): time for the test to find the agent and read
	// what the container holds beside it while the agent is still small.
	memoryGroupGrowAfter = 5 * time.Second
)

// TestTheServerEndsTheAgentBeforeTheKernel: in a container whose memory limit is 8 GiB, the server
// reads memory.max and sets the limits of section 5 of the background-runs design for it, and its
// agent allocates at full speed with GOMAXPROCS=4. The agent's fastest growth in 50 ms is under
// the 1 GiB production keeps for it. With the server's heap held at the container's limit less the
// agent's ceiling, the load outside the Go heaps and twice that growth, the server kills the agent
// for its memory before the kernel kills anything in the container: memory.events counts no
// oom_kill, and the server's process id does not change (check c20).
func TestTheServerEndsTheAgentBeforeTheKernel(t *testing.T) {
	if os.Getenv("DISPATCH_TEST_MEMORY_GROUP") != "1" {
		t.Skip("DISPATCH_TEST_MEMORY_GROUP=1 runs this test, as its CI job does: it needs Docker, control groups v2 and 8 GiB of memory for a container (check c20)")
	}
	if runtime.GOARCH != "amd64" {
		t.Fatalf("the test runs the server built for linux/amd64, as production runs it, so it runs on amd64; this machine is %s", runtime.GOARCH)
	}
	if _, err := os.Stat("/sys/fs/cgroup/cgroup.controllers"); err != nil {
		t.Fatalf("/sys/fs/cgroup is not control groups v2, whose memory.events the test reads: %v", err)
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Fatalf("docker is not on PATH: %v", err)
	}
	if os.Getenv(storetest.DatabaseURLEnv) == "" {
		t.Fatalf("%s names no Postgres for the server in the container", storetest.DatabaseURLEnv)
	}
	database := storetest.Open(t)
	binary := cmdtest.BuildCommand(t, cmdtest.Command{Name: "envoy-dispatch",
		Env: append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH=amd64")})
	box := startMemoryGroupContainer(t, binary)
	limits := agentLimits(memoryGroupLimit)

	// First the agent's fastest growth in 50 ms, beside a server that holds no more than it needs.
	measuring := box.startServer(t, database, nil)
	measuring.requireLimits(t, limits)
	agent := box.agentOf(t, measuring, 1)
	measuring.requireAgentProcs(t, agentMaxProcs)
	besideMeasuring := box.besideTheAgent(t, agent)
	samples, gone := sampleResidentMemory(agent.host, memoryGroupGrowAfter+time.Minute)
	if !gone {
		t.Fatalf("the agent, process %d in the container, still ran a minute after it began to grow:\n%s", agent.pid, measuring.Output.String())
	}
	measuring.awaitTheKill(t, box, agent.pid)
	box.requireNoOOMKill(t, measuring, "while the agent grew beside a server that held no more than it needs")
	measuring.requireKilledForMemory(t, agent.pid, limits.Ceiling)
	growth := fastestGrowth(samples, memoryGroupWindow)
	t.Logf("the agent's fastest growth in %v: %s, from %d reads of its VmRSS", memoryGroupWindow, mib(growth), len(samples))
	if growth <= 0 {
		t.Fatalf("the agent's VmRSS never rose in %d reads:\n%s", len(samples), measuring.Output.String())
	}
	if growth >= productionGrowthMargin {
		t.Errorf("the agent grew %s in %v, not under the %s production's container keeps for its growth between two reads", mib(growth), memoryGroupWindow, mib(productionGrowthMargin))
	}
	box.stopServer(t, measuring)

	// Then the server's heap held near its peak. c20 holds it at L less the agent's ceiling, a load
	// outside the Go heaps and twice the growth. What the container held beside the small agent
	// (besideMeasuring: memory.current less the agent's VmRSS) is the server's own heap and that
	// load: the server's runtime outside its heap, the container's init and sleep, its page cache
	// and the kernel's memory charged to the group. So the server holds
	//
	//	held = L - ceiling - besideMeasuring - 2*growth
	//
	// more, which makes its whole heap L less the ceiling, the load outside the Go heaps and twice
	// the growth, and an agent at its ceiling leaves the container twice the growth under its limit.
	held := memoryGroupLimit - limits.Ceiling - besideMeasuring - 2*growth
	if held <= 0 {
		t.Fatalf("the container holds %s beside the agent: with a ceiling of %s and twice the growth, %s, the server can hold no heap in %s", mib(besideMeasuring), mib(limits.Ceiling), mib(2*growth), mib(memoryGroupLimit))
	}
	running := box.startServer(t, database, map[string]string{"DISPATCH_TEST_SERVER_HEAP": strconv.FormatInt(held, 10)})
	agent = box.agentOf(t, running, 1)
	// This server is another process than the measured one, so the margin is read again before the
	// agent grows: the run tests c20 only while the margin is near twice the growth, with room for
	// one growth at the least and less than room for three.
	margin := memoryGroupLimit - limits.Ceiling - box.besideTheAgent(t, agent)
	t.Logf("the server holds %s more; with the agent at its ceiling of %s the container keeps %s under its limit, for twice the growth, %s",
		mib(held), mib(limits.Ceiling), mib(margin), mib(2*growth))
	if margin < growth || margin > 3*growth {
		t.Fatalf("the container's margin beside an agent at its ceiling is %s, not near twice the growth, %s: the run would not test c20", mib(margin), mib(2*growth))
	}
	running.awaitTheKill(t, box, agent.pid)
	events := box.memoryEvents(t)
	if events["oom_kill"] != 0 {
		t.Errorf("the kernel killed %d process(es) in the container before the server killed its agent: memory.events %v\n%s", events["oom_kill"], events, running.Output.String())
	}
	running.requireTheSameProcess(t)
	running.requireKilledForMemory(t, agent.pid, limits.Ceiling)
	if peak, err := os.ReadFile(filepath.Join(box.group, "memory.peak")); err == nil {
		if value, err := strconv.ParseInt(strings.TrimSpace(string(peak)), 10, 64); err == nil {
			t.Logf("the container's peak: %s of its %s", mib(value), mib(memoryGroupLimit))
		}
	}
}

// memoryGroupContainer is the test's container, by name, and its control group's directory on the
// host.
type memoryGroupContainer struct {
	name  string
	group string
}

// startMemoryGroupContainer starts the test's container: the image with the server's binary
// mounted read-only at /envoy-dispatch and an empty dashboard at /web, a memory limit of 8 GiB and
// no swap, so the limit is the kernel's, on the host's network, so the server reaches the test's
// Postgres and the test the server at 127.0.0.1. Its first process sleeps under Docker's init,
// so the container outlives any server the test starts in it. The container is removed when the
// test ends, whatever happens.
func startMemoryGroupContainer(t *testing.T, binary string) memoryGroupContainer {
	t.Helper()
	image := memoryGroupImage
	if named := strings.TrimSpace(os.Getenv("DISPATCH_TEST_MEMORY_GROUP_IMAGE")); named != "" {
		image = named
	}
	web := t.TempDir()
	box := memoryGroupContainer{name: "dispatch-memory-group-" + uuid.NewString()}
	// Registered before the container exists, so one that docker made but failed to start is
	// removed too, and after the directories it mounts, so they are removed once it is gone.
	t.Cleanup(func() {
		out, err := exec.Command("docker", "rm", "--force", box.name).CombinedOutput()
		if err != nil && !strings.Contains(string(out), "No such container") {
			t.Errorf("remove the container %s: %v\n%s", box.name, err, out)
		}
	})
	docker(t, "run", "--detach", "--init", "--name", box.name,
		"--memory", "8g", "--memory-swap", "8g", "--network", "host",
		"--volume", binary+":/envoy-dispatch:ro", "--volume", web+":/web:ro",
		image, "sleep", "infinity")
	first := docker(t, "inspect", "--format", "{{.State.Pid}}", box.name)
	membership, err := os.ReadFile(filepath.Join("/proc", first, "cgroup"))
	if err != nil {
		t.Fatalf("read the control group of the container's first process %s: %v", first, err)
	}
	// Under control groups v2 a process is in one group, written "0::<path>".
	group, ok := strings.CutPrefix(strings.TrimSpace(string(membership)), "0::")
	if !ok {
		t.Fatalf("the container's first process %s names no control group v2: %q", first, membership)
	}
	box.group = filepath.Join("/sys/fs/cgroup", group)
	if got, want := box.read(t, "memory.max"), strconv.FormatInt(memoryGroupLimit, 10); got != want {
		t.Fatalf("the container's memory.max is %s, want %s", got, want)
	}
	if got := box.read(t, "memory.swap.max"); got != "0" {
		t.Fatalf("the container's memory.swap.max is %s, want 0: with swap the kernel would page the agent out rather than kill", got)
	}
	return box
}

// docker runs docker with args and answers its standard output, trimmed; it fails the test with
// docker's standard error when docker fails.
func docker(t *testing.T, args ...string) string {
	t.Helper()
	var stderr bytes.Buffer
	cmd := exec.Command("docker", args...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, stderr.String())
	}
	return strings.TrimSpace(string(out))
}

// read is the trimmed contents of file of the container's control group.
func (c memoryGroupContainer) read(t *testing.T, file string) string {
	t.Helper()
	contents, err := os.ReadFile(filepath.Join(c.group, file))
	if err != nil {
		t.Fatalf("read the container's %s: %v", file, err)
	}
	return strings.TrimSpace(string(contents))
}

// memoryCurrent is the memory the kernel charges the container's group now (memory.current),
// against its memory.max.
func (c memoryGroupContainer) memoryCurrent(t *testing.T) int64 {
	t.Helper()
	value, err := strconv.ParseInt(c.read(t, "memory.current"), 10, 64)
	if err != nil {
		t.Fatalf("the container's memory.current: %v", err)
	}
	return value
}

// memoryEvents is the container's memory.events, each count by its name.
func (c memoryGroupContainer) memoryEvents(t *testing.T) map[string]int64 {
	t.Helper()
	text := c.read(t, "memory.events")
	events := map[string]int64{}
	for line := range strings.Lines(text) {
		name, value, ok := strings.Cut(strings.TrimSpace(line), " ")
		if count, err := strconv.ParseInt(value, 10, 64); ok && err == nil {
			events[name] = count
		}
	}
	if _, counted := events["oom_kill"]; !counted {
		t.Fatalf("the container's memory.events counts no oom_kill: %q", text)
	}
	return events
}

// requireNoOOMKill fails the test when the kernel has killed a process in the container, during
// what it names, which a failure quotes with server's output.
func (c memoryGroupContainer) requireNoOOMKill(t *testing.T, server containerServer, during string) {
	t.Helper()
	if events := c.memoryEvents(t); events["oom_kill"] != 0 {
		t.Fatalf("the kernel killed %d process(es) in the container %s: memory.events %v\n%s", events["oom_kill"], during, events, server.Output.String())
	}
}

// hostPid is the host's process id of the container's process pid, found by the NSpid line of
// each process of the container's group.
func (c memoryGroupContainer) hostPid(t *testing.T, pid int) int {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		for _, field := range strings.Fields(c.read(t, "cgroup.procs")) {
			if host, err := strconv.Atoi(field); err == nil && namespacePid(host) == pid {
				return host
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no process of the container's group %s is process %d in the container", c.group, pid)
		}
	}
}

// namespacePid is host process pid's id in the innermost process namespace it is in, the last of
// its NSpid line; 0 where that cannot be read.
func namespacePid(pid int) int {
	status, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0
	}
	for line := range strings.Lines(string(status)) {
		if ids, ok := strings.CutPrefix(line, "NSpid:"); ok {
			fields := strings.Fields(ids)
			if len(fields) == 0 {
				return 0
			}
			inner, _ := strconv.Atoi(fields[len(fields)-1])
			return inner
		}
	}
	return 0
}

// containerAgent is an agent process the server in the container started: its process id in the
// container and on the host, and when the server started it.
type containerAgent struct {
	pid, host int
	started   time.Time
}

// agentOf waits up to a minute for server's count-th agent to start, and answers it.
func (c memoryGroupContainer) agentOf(t *testing.T, server containerServer, count int) containerAgent {
	t.Helper()
	for deadline := time.Now().Add(time.Minute); ; time.Sleep(20 * time.Millisecond) {
		if matches := agentStartedLine.FindAllStringSubmatch(server.Output.String(), -1); len(matches) >= count {
			pid, _ := strconv.Atoi(matches[count-1][1])
			line := server.lines(`msg="dispatch: started the agent process" pid=` + matches[count-1][1] + " ")[0]
			return containerAgent{pid: pid, host: c.hostPid(t, pid), started: logTime(t, line)}
		}
		select {
		case <-server.Exited:
			t.Fatalf("the server in the container exited before its agent number %d started:\n%s", count, server.Output.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("the server in the container started no agent number %d within a minute:\n%s", count, server.Output.String())
		}
	}
}

// besideTheAgent is what the container holds beside agent: its memory.current less the agent's
// VmRSS, the median of nine reads 50 ms apart, read a second after the test found the agent and
// before the agent begins to grow.
func (c memoryGroupContainer) besideTheAgent(t *testing.T, agent containerAgent) int64 {
	t.Helper()
	time.Sleep(time.Second)
	reads := make([]int64, 0, 9)
	for range 9 {
		current := c.memoryCurrent(t)
		rss, err := residentMemory(agent.host)
		if err != nil {
			t.Fatalf("read the VmRSS of the agent, process %d on the host: %v", agent.host, err)
		}
		reads = append(reads, current-rss)
		time.Sleep(50 * time.Millisecond)
	}
	if since := time.Since(agent.started); since >= memoryGroupGrowAfter {
		t.Fatalf("the test read the container beside the agent %v after the agent's start, when the agent had begun to grow %v after it", since, memoryGroupGrowAfter)
	}
	slices.Sort(reads)
	return reads[len(reads)/2]
}

// containerServer is a Dispatch server the test started in its container with docker exec: the
// docker command's process, which ends with the server, the port the server serves on, and the
// server's process id in the container and on the host.
type containerServer struct {
	*cmdtest.Process
	port      int
	pid, host int
}

// serverPidLine is the line the shell writes before it becomes the server, naming its process id,
// which the server keeps.
var serverPidLine = regexp.MustCompile(`(?m)^server pid (\d+)$`)

// startServer starts a server in the container against database, with the agent growing at full
// speed memoryGroupGrowAfter after each start, and settings over the rows below, and returns once
// it answers /healthz.
func (c memoryGroupContainer) startServer(t *testing.T, database *store.Store, settings map[string]string) containerServer {
	t.Helper()
	port := freeLoopbackPort(t)
	environment := map[string]string{
		"HOME":                             "/tmp",
		"XDG_CONFIG_HOME":                  "/tmp",
		"XDG_DATA_HOME":                    "/tmp",
		"DISPATCH_AGENT_TOKEN":             dispatchTestAgentToken,
		"DISPATCH_IDENTITY":                "header:X-Dispatch-User",
		"DISPATCH_IDENTITY_HEADER_TRUSTED": "1",
		"DISPATCH_LISTEN_HOST":             "127.0.0.1",
		"DISPATCH_PORT":                    strconv.Itoa(port),
		"DISPATCH_WEB_DIST":                "/web",
		"DISPATCH_NATS_DISABLED":           "1",
		"DISPATCH_TEST_HOOKS":              "1",
		"DISPATCH_TEST_AGENT_GROW_AFTER":   memoryGroupGrowAfter.String(),
	}
	maps.Copy(environment, settings)
	args := []string{"exec"}
	for _, name := range slices.Sorted(maps.Keys(environment)) {
		args = append(args, "--env", name+"="+environment[name])
	}
	// DATABASE_URL is passed by name, from the docker command's own environment, so its password is
	// on no command line.
	args = append(args, "--env", "DATABASE_URL", c.name, "sh", "-c", `echo "server pid $$"; exec /envoy-dispatch`)
	cmd := exec.Command("docker", args...)
	cmd.Env = append(os.Environ(), "DATABASE_URL="+database.Pool.Config().ConnString())
	server := containerServer{Process: cmdtest.Start(t, "Dispatch in the container", cmd), port: port}
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if match := serverPidLine.FindStringSubmatch(server.Output.String()); match != nil {
			server.pid, _ = strconv.Atoi(match[1])
			break
		}
		select {
		case <-server.Exited:
			t.Fatalf("docker exec of the server exited before the server started:\n%s", server.Output.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("docker exec named no server process within 30s:\n%s", server.Output.String())
		}
	}
	server.host = c.hostPid(t, server.pid)
	for deadline := time.Now().Add(2 * time.Minute); !server.healthy(); time.Sleep(200 * time.Millisecond) {
		select {
		case <-server.Exited:
			t.Fatalf("the server in the container exited before it answered /healthz:\n%s", server.Output.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("the server in the container did not answer /healthz 200 within 2 minutes:\n%s", server.Output.String())
		}
	}
	return server
}

// freeLoopbackPort is a loopback TCP port nothing listens on now, for a server in the container,
// which shares the host's network, to serve on.
func freeLoopbackPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find a free loopback port: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatalf("free the loopback port %d: %v", port, err)
	}
	return port
}

// healthy reports whether the server answers /healthz 200 within 2 s.
func (s containerServer) healthy() bool {
	client := http.Client{Timeout: 2 * time.Second}
	response, err := client.Get("http://127.0.0.1:" + strconv.Itoa(s.port) + "/healthz")
	if err != nil {
		return false
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	return response.StatusCode == http.StatusOK
}

// lines is every line of the server's output that holds text.
func (s containerServer) lines(text string) []string {
	var found []string
	for line := range strings.Lines(s.Output.String()) {
		if strings.Contains(line, text) {
			found = append(found, strings.TrimRight(line, "\n"))
		}
	}
	return found
}

// requireLimits fails the test unless the server read the container's limit of 8 GiB and logged
// the limits agentLimits gives for it: the agent's ceiling and GOMEMLIMIT, its slots and the
// server's own limit.
func (s containerServer) requireLimits(t *testing.T, limits agentMemory) {
	t.Helper()
	want := fmt.Sprintf(`msg="dispatch: memory limits" container=%d server=%d agent_ceiling=%d agent_gomemlimit=%d agent_slots=%d`,
		memoryGroupLimit, limits.ServerLimit, limits.Ceiling, limits.AgentLimit, limits.Slots)
	if len(s.lines(want)) == 0 {
		t.Fatalf("the server did not log the limits of a container of 8 GiB (%s):\n%s", want, s.Output.String())
	}
}

// agentProcsLine is the line the agent's growth hook writes at its start, naming its GOMAXPROCS.
var agentProcsLine = regexp.MustCompile(`DISPATCH_TEST_AGENT_GROW_AFTER: allocating at full speed .* \(GOMAXPROCS=(\d+)\)`)

// requireAgentProcs fails the test unless the server's agent logged that it grows on procs
// goroutines, one for each CPU of its GOMAXPROCS, within 10 s.
func (s containerServer) requireAgentProcs(t *testing.T, procs int) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if match := agentProcsLine.FindStringSubmatch(s.Output.String()); match != nil {
			if got, _ := strconv.Atoi(match[1]); got != procs {
				t.Fatalf("the agent grows with GOMAXPROCS=%d, want %d: c20 needs a runner of %d CPUs or more", got, procs, procs)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the agent never logged its growth hook:\n%s", s.Output.String())
		}
	}
}

// agentExitedLine is the line the server writes once an agent has exited and its runs are freed.
const agentExitedLine = `msg="dispatch: the agent process exited; it starts again"`

// killedLine is the start of the line the server writes when it kills agent pid for its memory.
func killedLine(pid int) string {
	return `msg="dispatch: agent killed for memory" pid=` + strconv.Itoa(pid) + " "
}

// awaitTheKill waits until the server has logged its kill of agent pid for memory and the agent's
// exit after it, or the server has exited, or the kernel has killed a process in the container,
// whichever comes first; it fails the test when none comes within a minute of the agent's growth.
func (s containerServer) awaitTheKill(t *testing.T, box memoryGroupContainer, pid int) {
	t.Helper()
	for deadline := time.Now().Add(memoryGroupGrowAfter + time.Minute); ; time.Sleep(20 * time.Millisecond) {
		output := s.Output.String()
		if at := strings.Index(output, killedLine(pid)); at >= 0 && strings.Contains(output[at:], agentExitedLine) {
			return
		}
		select {
		case <-s.Exited:
			return
		default:
		}
		if box.memoryEvents(t)["oom_kill"] > 0 {
			// The process the kernel chose ends, and what follows from it is logged.
			time.Sleep(time.Second)
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("neither the server nor the kernel killed the agent, process %d in the container, within a minute of its growth:\n%s", pid, s.Output.String())
		}
	}
}

// requireKilledForMemory fails the test unless the server logged its kill of agent pid for memory
// above ceiling, and logs how far above it the server read the agent.
func (s containerServer) requireKilledForMemory(t *testing.T, pid int, ceiling int64) {
	t.Helper()
	lines := s.lines(killedLine(pid))
	if len(lines) == 0 {
		t.Errorf("the server never logged its kill of the agent, process %d in the container, for memory:\n%s", pid, s.Output.String())
		return
	}
	memory := killedMemory(t, lines[0])
	if memory <= ceiling {
		t.Errorf("the server killed the agent at %s, not above its ceiling of %s: %s", mib(memory), mib(ceiling), lines[0])
	}
	t.Logf("the server read the agent at %s, %s above its ceiling of %s, and killed it", mib(memory), mib(memory-ceiling), mib(ceiling))
}

// requireTheSameProcess fails the test unless the server still runs as the process the test
// started, with its process id in the container, and answers /healthz within 10 s: nothing in the
// container starts a server again, so a kill would have ended it.
func (s containerServer) requireTheSameProcess(t *testing.T) {
	t.Helper()
	select {
	case <-s.Exited:
		t.Errorf("the server, process %d in the container, exited (%s):\n%s", s.pid, exitOf(s.Cmd), s.Output.String())
		return
	default:
	}
	if processGone(s.host) || namespacePid(s.host) != s.pid {
		t.Errorf("the server, process %d in the container and %d on the host, is gone:\n%s", s.pid, s.host, s.Output.String())
	}
	for deadline := time.Now().Add(10 * time.Second); !s.healthy(); time.Sleep(200 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Errorf("the server no longer answers /healthz 200:\n%s", s.Output.String())
			return
		}
	}
}

// stopServer stops server with SIGTERM, as a deploy does, and waits for it to exit.
func (c memoryGroupContainer) stopServer(t *testing.T, server containerServer) {
	t.Helper()
	docker(t, "exec", c.name, "sh", "-c", "kill -TERM "+strconv.Itoa(server.pid))
	server.WaitExit(t, "SIGTERM")
}

// rssSample is one read of a process's VmRSS, and when it was read.
type rssSample struct {
	at  time.Time
	rss int64
}

// sampleResidentMemory reads host process pid's VmRSS about every millisecond until the process
// is gone or within has passed, and reports whether it is gone.
func sampleResidentMemory(pid int, within time.Duration) (samples []rssSample, gone bool) {
	for deadline := time.Now().Add(within); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		rss, err := residentMemory(pid)
		if err != nil {
			return samples, true
		}
		samples = append(samples, rssSample{at: time.Now(), rss: rss})
	}
	return samples, false
}

// fastestGrowth is the most samples rose in window: for each sample, the rise to the first sample
// at least window after it, scaled to window by the time between the two, so a pause between two
// reads does not lengthen the window.
func fastestGrowth(samples []rssSample, window time.Duration) int64 {
	var fastest int64
	later := 0
	for _, sample := range samples {
		for later < len(samples) && samples[later].at.Sub(sample.at) < window {
			later++
		}
		if later == len(samples) {
			break
		}
		span := samples[later].at.Sub(sample.at)
		rise := samples[later].rss - sample.rss
		fastest = max(fastest, int64(float64(rise)*float64(window)/float64(span)))
	}
	return fastest
}

// mib writes bytes in mebibytes, for the test's log.
func mib(bytes int64) string {
	return fmt.Sprintf("%.0f MiB", float64(bytes)/(1<<20))
}
