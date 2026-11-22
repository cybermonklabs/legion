package ompbroker

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// brokerEnv makes the test binary a stand-in broker (TestMain): a process whose command line
// carries BrokerArg, which starts one `sleep` per service it is told of, in a session of its own,
// with that service's environment, prints each pid, and waits.
const brokerEnv = "OMPBROKER_TEST_BROKER"

func TestMain(m *testing.M) {
	if os.Getenv(brokerEnv) == "1" {
		runStandInBroker()
		return
	}
	os.Exit(m.Run())
}

// runStandInBroker starts a service for each `--service=<k=v,k=v>` argument and prints its pid.
func runStandInBroker() {
	var services []*exec.Cmd
	for _, arg := range os.Args[1:] {
		spec, ok := strings.CutPrefix(arg, "--service=")
		if !ok {
			continue
		}
		service := exec.Command("sleep", "1000")
		service.Env = append([]string{"PATH=" + os.Getenv("PATH")}, strings.Split(spec, ",")...)
		service.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := service.Start(); err != nil {
			fmt.Println("error", err)
			os.Exit(1)
		}
		services = append(services, service)
		fmt.Println(service.Process.Pid)
	}
	for _, service := range services {
		go func() { _ = service.Wait() }()
	}
	time.Sleep(time.Hour)
}

// startBroker runs a stand-in broker with the services' environments and answers their pids.
func startBroker(t *testing.T, envs ...string) []int {
	t.Helper()
	args := []string{BrokerArg}
	for _, env := range envs {
		args = append(args, "--service="+env)
	}
	broker := exec.Command(os.Args[0], args...)
	broker.Env = append(os.Environ(), brokerEnv+"=1")
	out, err := broker.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := broker.Start(); err != nil {
		t.Fatal(err)
	}
	var pids []int
	scanner := bufio.NewScanner(out)
	for len(pids) < len(envs) && scanner.Scan() {
		pid, err := strconv.Atoi(strings.TrimSpace(scanner.Text()))
		if err != nil {
			t.Fatalf("the stand-in broker said %q", scanner.Text())
		}
		pids = append(pids, pid)
	}
	t.Cleanup(func() {
		for _, pid := range pids {
			if alive(pid) {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
		_ = broker.Process.Kill()
		_ = broker.Wait()
	})
	return pids
}

func serviceEnv(stateDir, project, issue, role string, generation int) string {
	return fmt.Sprintf("LEGION_STATE_DIR=%s,LEGION_PROJECT=%s,LEGION_ISSUE=%s,LEGION_ROLE=%s,LEGION_GENERATION=%d",
		stateDir, project, issue, role, generation)
}

func awaitEnded(pid int) bool {
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if !alive(pid) {
			return true
		}
	}
	return false
}

// End ends the services a claim's processes started through a broker, and only those: not another
// claim's, not another daemon's, not a newer generation's when bounded, and not a process that is
// no broker's.
func TestEndEndsOnlyTheClaimsServices(t *testing.T) {
	state := t.TempDir()
	token, err := claim.NewToken("acme", "ACME-1", claim.RoleImplementer)
	if err != nil {
		t.Fatal(err)
	}
	pids := startBroker(t,
		serviceEnv(state, "ACME", "ACME-1", "implementer", 1),          // the claim's, generation 1
		serviceEnv(state, "ACME", "ACME-1", "implementer", 2),          // the claim's, generation 2
		serviceEnv(state, "ACME", "ACME-2", "implementer", 1),          // another claim's
		serviceEnv(state+"-other", "ACME", "ACME-1", "implementer", 1), // another daemon's
		serviceEnv(state, "ACME", "", "controller", 1),                 // the project's controller's
	)
	ownOld, ownNew, otherClaim, otherDaemon, controller := pids[0], pids[1], pids[2], pids[3], pids[4]
	// A process with the claim's environment that is no broker's child.
	orphan := exec.Command("sleep", "1000")
	orphan.Env = append([]string{"PATH=" + os.Getenv("PATH")}, strings.Split(serviceEnv(state, "ACME", "ACME-1", "implementer", 1), ",")...)
	if err := orphan.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = orphan.Process.Kill(); _ = orphan.Wait() })

	ended, err := End(Claim{StateDir: state, Token: token, Before: 2}, time.Second)
	if err != nil {
		t.Fatalf("End before generation 2 = %v", err)
	}
	if len(ended) != 1 || ended[0].Pid != ownOld || ended[0].Generation != 1 || !awaitEnded(ownOld) {
		t.Fatalf("End before generation 2 ended %+v, want only pid %d", ended, ownOld)
	}
	if !alive(ownNew) {
		t.Errorf("End before generation 2 ended generation 2's service")
	}

	ended, err = End(Claim{StateDir: state, Token: token}, time.Second)
	if err != nil {
		t.Fatalf("End = %v", err)
	}
	if len(ended) != 1 || ended[0].Pid != ownNew || !awaitEnded(ownNew) {
		t.Fatalf("End ended %+v, want only pid %d", ended, ownNew)
	}
	for name, pid := range map[string]int{"another claim's": otherClaim, "another daemon's": otherDaemon, "the controller's": controller, "a non-broker's": orphan.Process.Pid} {
		if !alive(pid) {
			t.Errorf("End ended %s process %d", name, pid)
		}
	}

	ended, err = End(Claim{StateDir: state, Token: claim.ControllerToken("acme")}, time.Second)
	if err != nil || len(ended) != 1 || ended[0].Pid != controller || !awaitEnded(controller) {
		t.Fatalf("End of the controller = %+v, %v; want only pid %d", ended, err, controller)
	}
}

// A service that does not end on SIGTERM is killed once the grace is spent. A stopped process
// leaves its SIGTERM pending and unanswered until the SIGKILL, which stands for a service that
// ignores it.
func TestEndKillsAServiceThatOutlivesItsGrace(t *testing.T) {
	state := t.TempDir()
	token, _ := claim.NewToken("acme", "ACME-1", claim.RoleImplementer)
	pids := startBroker(t, serviceEnv(state, "ACME", "ACME-1", "implementer", 1))
	if err := syscall.Kill(pids[0], syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	ended, err := End(Claim{StateDir: state, Token: token}, 200*time.Millisecond)
	if err != nil || len(ended) != 1 || !ended[0].Killed || !awaitEnded(pids[0]) {
		t.Fatalf("End = %+v, %v; want pid %d killed", ended, err, pids[0])
	}
}

func TestEndRefusesAClaimItCannotName(t *testing.T) {
	if _, err := End(Claim{Token: "legion-acme-controller"}, time.Second); err == nil {
		t.Error("End with no state directory succeeded")
	}
}
