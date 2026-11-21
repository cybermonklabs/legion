package testport

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// deadPid is the id of a process that has exited.
func deadPid(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("run true: %v", err)
	}
	return cmd.Process.Pid
}

func writeClaim(t *testing.T, path string, pid int) {
	t.Helper()
	if err := os.WriteFile(path, []byte(strconv.Itoa(pid)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readClaim(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(raw))
}

// A claim is a loopback address that answers a bind, held in a file that names its owner, and two
// claims of this process are two addresses.
func TestAClaimIsAnAddressThatIsUpAndNamesItsOwner(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("off Linux Claim gives 127.0.0.1 and claims nothing")
	}
	dir := t.TempDir()
	first, err := Claim(dir, os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	second, err := Claim(dir, os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatalf("two claims gave %s twice", first)
	}
	for _, host := range []string{first, second} {
		if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() || host == "127.0.0.1" {
			t.Fatalf("Claim gave %q, want a loopback address other than 127.0.0.1", host)
		}
		if got := readClaim(t, filepath.Join(dir, host)); got != strconv.Itoa(os.Getpid()) {
			t.Fatalf("the claim on %s names %q, want this process", host, got)
		}
		listener, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
		if err != nil {
			t.Fatalf("%s does not bind: %v", host, err)
		}
		listener.Close()
	}
}

// An address a live process holds is not taken; one whose owner has exited is.
func TestAClaimSkipsALiveOwnerAndTakesADeadOnes(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "127.9.9.9")
	writeClaim(t, live, os.Getpid())
	taken, err := claim(live, 1<<22)
	if err != nil {
		t.Fatal(err)
	}
	if taken {
		t.Fatal("claim took an address this live process holds")
	}
	if got := readClaim(t, live); got != strconv.Itoa(os.Getpid()) {
		t.Fatalf("the live claim now names %q", got)
	}

	stale := filepath.Join(dir, "127.8.8.8")
	writeClaim(t, stale, deadPid(t))
	taken, err = claim(stale, os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if !taken {
		t.Fatal("claim left an address whose owner has exited")
	}
	if got := readClaim(t, stale); got != strconv.Itoa(os.Getpid()) {
		t.Fatalf("the taken claim names %q, want this process", got)
	}
}

// Claim removes every claim whose owner has exited, and leaves a live owner's.
func TestClaimSweepsTheClaimsOfExitedOwners(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("off Linux Claim claims nothing")
	}
	dir := t.TempDir()
	stale := filepath.Join(dir, "127.5.5.5")
	live := filepath.Join(dir, "127.4.4.4")
	writeClaim(t, stale, deadPid(t))
	writeClaim(t, live, os.Getpid())
	if _, err := Claim(dir, os.Getpid()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("Claim left the claim of an exited owner: %v", err)
	}
	if _, err := os.Stat(live); err != nil {
		t.Fatalf("Claim removed a live owner's claim: %v", err)
	}
}

// Release removes its owner's claim, and leaves one another process holds.
func TestReleaseRemovesOnlyItsOwnersClaim(t *testing.T) {
	dir := t.TempDir()
	writeClaim(t, filepath.Join(dir, "127.7.7.7"), os.Getpid())
	if err := Release(dir, "127.7.7.7", os.Getpid()+1); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "127.7.7.7")); err != nil {
		t.Fatalf("Release removed a claim another owner holds: %v", err)
	}
	if err := Release(dir, "127.7.7.7", os.Getpid()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "127.7.7.7")); !os.IsNotExist(err) {
		t.Fatalf("Release left its owner's claim: %v", err)
	}
	if err := Release(dir, "127.6.6.6", os.Getpid()); err != nil {
		t.Fatalf("Release of an address never claimed: %v", err)
	}
}
