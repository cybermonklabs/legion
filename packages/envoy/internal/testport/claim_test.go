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

// claimsDir is a claims directory of this test's own, as Claim makes ClaimDir: missing until a
// row makes it, so Claim makes it with mode 0700. t.TempDir itself is made with the umask's mode.
func claimsDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "claims")
}

// draws draws hosts in order, then fails the row: no row here draws more than it names.
func draws(t *testing.T, hosts ...string) func() net.IP {
	return func() net.IP {
		if len(hosts) == 0 {
			t.Fatal("Claim drew more addresses than the row named")
		}
		host := hosts[0]
		hosts = hosts[1:]
		return net.ParseIP(host)
	}
}

// A claim is a loopback address that answers a bind, held in a file that names its owner, and two
// claims of this process are two addresses.
func TestAClaimIsAnAddressThatIsUpAndNamesItsOwner(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("off Linux Claim gives 127.0.0.1 and claims nothing")
	}
	dir := claimsDir(t)
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
	dir := claimsDir(t)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
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

// Claim draws again past an address a live process holds, and leaves that claim as it was; a draw
// that only ever meets held addresses ends, naming the last; and an address that does not bind
// fails at once, naming itself, with nothing claimed.
func TestClaimDrawsPastALiveOwnersAddressAndRefusesOneThatIsNotUp(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("off Linux Claim gives 127.0.0.1 and claims nothing")
	}
	dir := claimsDir(t)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	const held, free = "127.31.41.59", "127.31.41.58"
	writeClaim(t, filepath.Join(dir, held), os.Getpid())
	got, err := ClaimFrom(dir, 1<<22, draws(t, held, free))
	if err != nil {
		t.Fatal(err)
	}
	if got != free {
		t.Fatalf("Claim gave %s; the first draw, %s, is held by this live process, so want the second, %s", got, held, free)
	}
	if owner := readClaim(t, filepath.Join(dir, held)); owner != strconv.Itoa(os.Getpid()) {
		t.Fatalf("the live claim on %s now names %q", held, owner)
	}

	always := make([]string, claimDraws)
	for i := range always {
		always[i] = held
	}
	_, err = ClaimFrom(dir, 1<<22, draws(t, always...))
	if err == nil || !strings.Contains(err.Error(), "the last "+held) {
		t.Fatalf("a draw that only meets %s, which is held, gave %v; want a refusal naming it", held, err)
	}

	// 192.0.2.1 is documentation's address (RFC 5737), up on no machine.
	_, err = ClaimFrom(dir, os.Getpid(), draws(t, "192.0.2.1"))
	if err == nil || !strings.Contains(err.Error(), "loopback address 192.0.2.1 is not up") {
		t.Fatalf("an address that does not bind gave %v; want a refusal naming it", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "192.0.2.1")); !os.IsNotExist(err) {
		t.Fatalf("Claim claimed an address that does not bind: %v", err)
	}
}

// Claim takes its directory only when it is a directory, not a link, that this user owns with
// mode 0700: the shared /tmp lets another user make it first.
func TestClaimRefusesADirectoryAnotherUserCouldEnter(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("off Linux Claim claims nothing")
	}
	open := claimsDir(t)
	if err := os.Mkdir(open, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(open, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := Claim(open, os.Getpid())
	if err == nil || !strings.Contains(err.Error(), open+" is a directory with mode 0755") {
		t.Fatalf("Claim in a directory of mode 0755 gave %v; want a refusal naming it", err)
	}
	if entries, _ := os.ReadDir(open); len(entries) != 0 {
		t.Fatalf("Claim wrote %d claims into a directory it refused", len(entries))
	}

	link := filepath.Join(t.TempDir(), "link")
	target := claimsDir(t)
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Claim(link, os.Getpid()); err == nil || !strings.Contains(err.Error(), link+" is a symbolic link") {
		t.Fatalf("Claim through a link to a directory gave %v; want a refusal naming the link", err)
	}

	// Another user's directory of mode 0700: /root, where a run as any other user reads it.
	if os.Getuid() == 0 {
		t.Log("run as root, so /root is this user's own; the owner check is not exercised")
		return
	}
	info, err := os.Lstat("/root")
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Logf("/root is no directory of mode 0700 here (%v, %v); the owner check is not exercised", info, err)
		return
	}
	if _, err := Claim("/root", os.Getpid()); err == nil || !strings.Contains(err.Error(), "owned by uid 0") {
		t.Fatalf("Claim in root's /root gave %v; want a refusal naming its owner", err)
	}
}
