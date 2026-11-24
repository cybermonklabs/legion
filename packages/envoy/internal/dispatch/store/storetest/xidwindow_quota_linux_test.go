//go:build !race

package storetest

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A CPU quota's throttling is a wait for a CPU to the rig, not a sleep of the code's: the kernel
// counts the time a quota throttles a thread in the thread's run_delay, which the rig reads as
// Runnable and takes out of a gap, so no judgment needs the cgroup's cpu.stat (XidGap). This holds
// it on the kernel the test runs on. A child (xidQuotaChild) spins in a scope of its own with a
// quota of 10% over a 10 ms period (xidQuota), so it runs about 1 ms of each period and waits
// throttled the rest, and reads its own thread's clocks as the rig reads a gap's - run_delay
// between two reads of schedstat that agree, the thread's CPU time and the clock between them - at
// the two ends of xidQuotaStretch. The time it ran and its run_delay must account for its time on
// the clock to within xidQuotaSlack, while its cgroup's cpu.stat shows it throttled for most of
// the stretch: throttled time that the kernel counted anywhere but run_delay would read as time
// asleep. cpu.stat is read only to show that the quota throttled, never to judge a window.
func TestAQuotasThrottlingReadsAsAWaitForACPU(t *testing.T) {
	RequireXidGapJudgment(t)
	stretch := measureUnderQuota(t, "spin")
	t.Logf("a spinning thread under a quota: %s", stretch)
	if stretch.throttled < xidQuotaStretch/2 {
		t.Fatalf("the quota throttled the thread %s of %s: it did not throttle, so this run proves nothing\n%s",
			XidMillis(stretch.throttled), XidMillis(stretch.clock), stretch)
	}
	if asleep := stretch.asleep(); asleep > xidQuotaSlack || asleep < -xidQuotaSlack {
		t.Fatalf("a thread its quota throttled %s of %s read %s neither running nor waiting for a CPU, past %s: the kernel did not count the throttled time in run_delay, where the rig reads it as the machine's\n%s",
			XidMillis(stretch.throttled), XidMillis(stretch.clock), XidMillis(asleep), XidMillis(xidQuotaSlack), stretch)
	}
}

// The same reading sees a sleep under the quota as a sleep, so the row above can fail: a thread
// that runs 1 ms of CPU and then sleeps 10 ms, again and again, reads most of its time neither
// running nor waiting for a CPU.
func TestAQuotasReadingSeesASleepAsASleep(t *testing.T) {
	RequireXidGapJudgment(t)
	stretch := measureUnderQuota(t, "sleep")
	t.Logf("a thread that sleeps under a quota: %s", stretch)
	if asleep := stretch.asleep(); asleep < xidQuotaStretch/4 {
		t.Fatalf("a thread that sleeps 10 ms after each 1 ms of CPU read %s asleep in %s, want at least %s\n%s",
			XidMillis(asleep), XidMillis(stretch.clock), XidMillis(xidQuotaStretch/4), stretch)
	}
}

// xidQuota is the scope's quota: 10% of a CPU over a 10 ms period, cpu.max "1000 10000".
var xidQuota = []string{"-p", "CPUQuota=10%", "-p", "CPUQuotaPeriodSec=10ms"}

// xidQuotaCPUMax is the cpu.max the child's cgroup holds once xidQuota is set.
const xidQuotaCPUMax = "1000 10000"

// xidQuotaStretch is how long the child reads itself for, and xidQuotaSlack how far its run time
// and run_delay may fall short of, or past, its time on the clock. The child reads its clocks
// exactly at their stamp, and over a stretch of 2 s such a spinning thread read 1.519 ms neither
// running nor waiting with no quota, and -0.312 and 0.593 ms under quotas of 20% and 50%; under
// this quota, 0.169 to 0.345 ms over half a second (2026-11-21).
const (
	xidQuotaStretch = 500 * time.Millisecond
	xidQuotaSlack   = 5 * time.Millisecond
)

// xidQuotaChild is the child, in Python, which envoy-go's job already runs (scripts/callout-check.sh): one thread,
// which the runtime never hands to another, so no runtime of the child's own sleeps it - a Go
// child's locked goroutine, preempted every 10 ms, sleeps its thread until another thread hands
// its P back, which under the quota waits for the next period, and an unlocked one moves between
// threads. In mode spin it spins for the stretch; in mode sleep it runs 1 ms of CPU and sleeps
// 10 ms, again and again. It prints one line, which xidQuotaLine reads.
const xidQuotaChild = `
import sys, time

def fail(message):
    print('XIDQUOTA error="%s"' % message.replace('"', "'"))
    sys.exit(3)

def clocks():
    for _ in range(1000):
        with open("/proc/thread-self/schedstat") as f:
            before = f.read()
        at = time.monotonic_ns()
        cpu = time.clock_gettime_ns(time.CLOCK_THREAD_CPUTIME_ID)
        with open("/proc/thread-self/schedstat") as f:
            after = f.read()
        if before.split()[1:] == after.split()[1:]:
            return at, cpu, int(before.split()[1])
    fail("the thread's clocks were never read between two switches")

def cgroup():
    with open("/proc/self/cgroup") as f:
        for line in f:
            if line.startswith("0::"):
                return "/sys/fs/cgroup" + line[3:].strip()
    fail("no cgroup v2 line in /proc/self/cgroup")

def throttled(path):
    with open(path + "/cpu.stat") as f:
        for line in f:
            name, value = line.split()
            if name == "throttled_usec":
                return int(value) * 1000
    fail(path + "/cpu.stat holds no throttled_usec")

mode, stretch = sys.argv[1], int(sys.argv[2])
path = cgroup()
with open(path + "/cpu.max") as f:
    cpumax = f.read().strip()
throttled_from = throttled(path)
at0, cpu0, delay0 = clocks()
while time.monotonic_ns() < at0 + stretch:
    if mode == "sleep":
        start = time.clock_gettime_ns(time.CLOCK_THREAD_CPUTIME_ID)
        while time.clock_gettime_ns(time.CLOCK_THREAD_CPUTIME_ID) - start < 1000000:
            pass
        time.sleep(0.01)
at1, cpu1, delay1 = clocks()
print("XIDQUOTA clock=%d ran=%d delay=%d throttled=%d cpumax=\"%s\"" % (at1 - at0, cpu1 - cpu0, delay1 - delay0, throttled(path) - throttled_from, cpumax))
`

// xidQuotaStretchReading is the child's reading: its time on the clock, the time its thread ran
// and waited for a CPU meanwhile, and the time its cgroup's cpu.stat says it was throttled.
type xidQuotaStretchReading struct {
	clock, ran, delay, throttled time.Duration
	how                          string
}

// asleep is the stretch's time neither running nor waiting for a CPU.
func (s xidQuotaStretchReading) asleep() time.Duration { return s.clock - s.ran - s.delay }

func (s xidQuotaStretchReading) String() string {
	return fmt.Sprintf("%s on the clock: ran %s, run_delay %s, neither %s; its cgroup throttled %s (%s)",
		XidMillis(s.clock), XidMillis(s.ran), XidMillis(s.delay), XidMillis(s.asleep()), XidMillis(s.throttled), s.how)
}

// measureUnderQuota runs the child in mode in a scope with the quota xidQuota and answers its
// reading. The scope comes from the user's systemd manager (systemd-run --user --scope), else from
// the system's through sudo -n, as the calling user (--uid, --gid). That second way is safe to
// try: -n makes sudo refuse at once where it would ask for a password, and what it runs is this
// file's own Python child, as the user running the test, in a transient scope that ends with it;
// the child reads only its own /proc files and its own cgroup's. A machine that gives neither, or
// has no python3, cannot show a quota's throttling at all (requireXidQuota).
func measureUnderQuota(t *testing.T, mode string) xidQuotaStretchReading {
	t.Helper()
	child := []string{"python3", "-c", xidQuotaChild, mode, strconv.FormatInt(int64(xidQuotaStretch), 10)}
	ways := []struct {
		how  string
		args []string
	}{
		{"systemd-run --user --scope", append(append([]string{"systemd-run", "--user", "--scope", "--quiet"}, xidQuota...), child...)},
		{"sudo -n systemd-run --scope", append(append([]string{"sudo", "-n", "systemd-run", "--scope", "--quiet",
			"--uid=" + strconv.Itoa(os.Getuid()), "--gid=" + strconv.Itoa(os.Getgid())}, xidQuota...), child...)},
	}
	var refusals []string
	for _, way := range ways {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		var stdout, stderr bytes.Buffer
		cmd := exec.CommandContext(ctx, way.args[0], way.args[1:]...)
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		cancel()
		line, found := xidQuotaLine(stdout.String())
		if !found {
			refusals = append(refusals, fmt.Sprintf("%s: %v: %s", way.how, err, strings.TrimSpace(stderr.String())))
			continue
		}
		if message, failed := line["error"]; failed {
			t.Fatalf("the child under the quota (%s): %s", way.how, message)
		}
		if line["cpumax"] != xidQuotaCPUMax {
			t.Fatalf("the child ran in a cgroup holding cpu.max %q, not %q (%s): the quota was not set, so this run proves nothing", line["cpumax"], xidQuotaCPUMax, way.how)
		}
		reading := xidQuotaStretchReading{how: way.how}
		for name, into := range map[string]*time.Duration{"clock": &reading.clock, "ran": &reading.ran, "delay": &reading.delay, "throttled": &reading.throttled} {
			ns, err := strconv.ParseInt(line[name], 10, 64)
			if err != nil {
				t.Fatalf("the child's line %q has no %s: %v", stdout.String(), name, err)
			}
			*into = time.Duration(ns)
		}
		return reading
	}
	requireXidQuota(t, strings.Join(refusals, "; "))
	return xidQuotaStretchReading{}
}

// requireXidQuota stops a run that could not put the child under a quota, naming why: it fails
// where the run sets XidRequireGapsEnv, as CI's step that runs these rows does, so a runner that
// cannot set a quota turns that step red rather than green on nothing; and it skips everywhere
// else, as RequireXidGapJudgment does, so a plain go test on a machine without a user systemd
// manager, passwordless sudo or python3 is not failed by a rig that is fine.
func requireXidQuota(t *testing.T, why string) {
	t.Helper()
	problem := "this machine runs no child under a CPU quota, so this run cannot show how a quota's throttling reads: " + why
	if xidGapsRequired() {
		t.Fatalf("XID RIG PROBLEM (not a finding about the code): %s", problem)
	}
	t.Skip(problem)
}

// xidQuotaLine finds the child's XIDQUOTA line in its output and answers its fields by name: each
// name=value, a value either a decimal or a double-quoted string.
func xidQuotaLine(output string) (map[string]string, bool) {
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		rest, ok := strings.CutPrefix(scanner.Text(), "XIDQUOTA ")
		if !ok {
			continue
		}
		fields := map[string]string{}
		for rest != "" {
			name, value, ok := strings.Cut(rest, "=")
			if !ok {
				return nil, false
			}
			if quoted, after, ok := strings.Cut(strings.TrimPrefix(value, `"`), `"`); ok && strings.HasPrefix(value, `"`) {
				fields[name], rest = quoted, strings.TrimPrefix(after, " ")
				continue
			}
			value, rest, _ = strings.Cut(value, " ")
			fields[name] = value
		}
		return fields, true
	}
	return nil, false
}
