package main

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/testport"
	"github.com/sjawhar/envoy/internal/testwait"
)

// natsdArgsEnv, when set, makes the test binary natsd itself: TestMain runs run on these arguments,
// one per line, so a test can start natsd as a process and signal it as a script does.
const natsdArgsEnv = "NATSD_TEST_ARGS"

// natsdDrawsEnv, beside natsdArgsEnv, names the addresses that natsd's claim draws first, comma
// separated, before LoopbackIP's: a row forces the address a second natsd meets.
const natsdDrawsEnv = "NATSD_TEST_DRAWS"

func TestMain(m *testing.M) {
	if args, ok := os.LookupEnv(natsdArgsEnv); ok {
		if draws := os.Getenv(natsdDrawsEnv); draws != "" {
			drawHost = drawsFirst(strings.Split(draws, ","))
		}
		if err := run(strings.Split(args, "\n")); err != nil {
			fmt.Fprintf(os.Stderr, "natsd: %v\n", err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// drawsFirst draws hosts in order, then what testport.LoopbackIP draws.
func drawsFirst(hosts []string) func() net.IP {
	return func() net.IP {
		if len(hosts) == 0 {
			return testport.LoopbackIP()
		}
		host := hosts[0]
		hosts = hosts[1:]
		return net.ParseIP(host)
	}
}

// startNatsd starts this test binary as natsd on args and returns it, with the address its log
// names on the line a script reads (`Listening for client connections on <address>:<port>`).
func startNatsd(t *testing.T, args ...string) (*exec.Cmd, string) {
	t.Helper()
	return startNatsdWith(t, nil, args...)
}

// startNatsdWith is startNatsd with env added to natsd's environment. It keeps everything natsd
// writes, and the wait for its address ends on one of two facts: the line names an address, or
// natsd's output ends (natsd has exited). The second fails at once with natsd's own output and its
// exit status, so a natsd that refuses to start (a claims directory it refuses, say) is named by
// its own message. The wait has no span of its own: testwait.Receive bounds it by the binary's
// deadline.
func startNatsdWith(t *testing.T, env []string, args ...string) (*exec.Cmd, string) {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(append(os.Environ(), env...), natsdArgsEnv+"="+strings.Join(args, "\n"))
	out, err := cmd.StdoutPipe()
	require.NoError(t, err)
	cmd.Stderr = cmd.Stdout
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		_ = cmd.Wait()
	})
	listening := regexp.MustCompile(`Listening for client connections on (\S+:\d+)$`)
	output := &logtest.Buffer{}
	// One value: the address natsd names, or "" once its output has ended with none named.
	found := make(chan string, 1)
	go func() {
		named := false
		scanner := bufio.NewScanner(out)
		for scanner.Scan() {
			_, _ = output.Write(append(scanner.Bytes(), '\n'))
			if match := listening.FindStringSubmatch(scanner.Text()); match != nil && !named {
				named = true
				found <- match[1]
			}
		}
		if !named {
			found <- ""
		}
	}()
	address := testwait.Receive(t, fmt.Sprintf("natsd %q to name its client address", args), found)
	if address == "" {
		t.Fatalf("natsd %q exited before it named a client address (%v); its output:\n%s", args, cmd.Wait(), output.String())
	}
	return cmd, address
}

// natsInfo reports whether a NATS server answers at address with its INFO line within 2 s. It is
// for the negative check alone: nothing listening refuses a dial at once, so the short span never
// decides it. A positive check waits with requireNatsInfo.
func natsInfo(address string) bool {
	return natsInfoWithin(address, 2*time.Second) == nil
}

// natsInfoWithin dials address and reads its first line, each step bounded by span (none when
// span is 0), and says why that line is not a NATS INFO line, or nil when it is.
func natsInfoWithin(address string, span time.Duration) error {
	dialer := net.Dialer{Timeout: span}
	conn, err := dialer.Dial("tcp", address)
	if err != nil {
		return err
	}
	defer conn.Close()
	if span > 0 {
		_ = conn.SetReadDeadline(time.Now().Add(span))
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}
	if !strings.HasPrefix(line, "INFO ") {
		return fmt.Errorf("first line %q, not INFO", line)
	}
	return nil
}

// requireNatsInfo waits for the NATS server at address to answer with its INFO line and fails the
// test, naming why, when it does not. natsd writes INFO only once its accept loop runs, which a
// loaded machine can hold well after natsd has logged its address, so the wait has no span of its
// own: testwait.Receive bounds it by the binary's deadline.
func requireNatsInfo(t *testing.T, address string) {
	t.Helper()
	answered := make(chan error, 1)
	go func() { answered <- natsInfoWithin(address, 0) }()
	require.NoError(t, testwait.Receive(t, fmt.Sprintf("a NATS INFO line from %s", address), answered),
		"the address natsd named, %s, does not answer", address)
}

// Given no -a, natsd listens on a loopback address of its own, claimed for its process while it
// runs, and names that address on the line a script reads its port from. A client that dials
// 127.0.0.1 at the same port reaches no server: the stray client of another stack, which dials the
// address its own server had, is out of its reach. Its control: the address the log names answers
// with the server's INFO.
func TestNatsdListensOnALoopbackAddressOfItsOwnAndNamesIt(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("only Linux brings up every address of 127.0.0.0/8; elsewhere natsd listens on 127.0.0.1")
	}
	cmd, address := startNatsd(t, "-p", "-1", "-store_dir", t.TempDir())
	host, port, err := net.SplitHostPort(address)
	require.NoError(t, err)
	ip := net.ParseIP(host)
	require.NotNil(t, ip, "natsd named %q, no IP address", host)
	require.True(t, ip.IsLoopback(), "natsd listens on %s, no loopback address", host)
	require.NotEqual(t, "127.0.0.1", host, "natsd still listens on 127.0.0.1")
	requireNatsInfo(t, address)
	require.False(t, natsInfo(net.JoinHostPort("127.0.0.1", port)), "127.0.0.1:%s reaches natsd", port)

	claim := filepath.Join(testport.ClaimDir(), host)
	raw, err := os.ReadFile(claim)
	require.NoError(t, err, "natsd holds no claim on %s", host)
	require.Equal(t, fmt.Sprint(cmd.Process.Pid), strings.TrimSpace(string(raw)))

	require.NoError(t, cmd.Process.Signal(syscall.SIGTERM))
	require.NoError(t, cmd.Wait())
	_, err = os.Stat(claim)
	require.ErrorIs(t, err, os.ErrNotExist, "natsd left its claim on %s behind", host)
}

// Given -a, natsd listens on that address and claims nothing, since the caller that keeps the
// address for a restart owns it; an address that is no loopback address is refused.
func TestNatsdTakesTheLoopbackAddressItIsGivenAndRefusesAnyOther(t *testing.T) {
	host := "127.0.0.1"
	if runtime.GOOS == "linux" {
		host = testport.LoopbackIP().String()
	}
	_, address := startNatsd(t, "-a", host, "-p", "-1", "-store_dir", t.TempDir())
	got, _, err := net.SplitHostPort(address)
	require.NoError(t, err)
	require.Equal(t, host, got)
	requireNatsInfo(t, address)
	if runtime.GOOS == "linux" {
		_, err = os.Stat(filepath.Join(testport.ClaimDir(), host))
		require.ErrorIs(t, err, os.ErrNotExist, "natsd claimed %s, which its caller gave it", host)
	}

	// listenHost itself, the check run makes before it starts a server: a check that let one of
	// these through then fails here by name, not by a server started in the test process.
	for _, refused := range []string{"0.0.0.0", "10.1.2.3", "localhost"} {
		_, _, err := listenHost(refused, 0)
		require.ErrorContains(t, err, "no loopback address", "-a %s", refused)
	}
}

// With -claim-for, the claim is that process's and outlasts natsd: a script starts natsd with
// -claim-for $$, stops it, and restarts it with -a on the address it read and its old port, while
// the claim keeps every other stack off that address. A second natsd started in the gap, as a
// second stack starts one, whose claim draws that very address first, is given another, so its
// port cannot be the restart's, and the claim still names the script.
func TestARestartWithClaimForKeepsTheAddressAndThePort(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("off Linux every natsd shares 127.0.0.1")
	}
	store := t.TempDir()
	first, address := startNatsd(t, "-claim-for", fmt.Sprint(os.Getpid()), "-p", "-1", "-store_dir", store)
	host, port, err := net.SplitHostPort(address)
	require.NoError(t, err)
	claim := filepath.Join(testport.ClaimDir(), host)
	t.Cleanup(func() { _ = testport.Release(testport.ClaimDir(), host, os.Getpid()) })
	require.NoError(t, first.Process.Signal(syscall.SIGTERM))
	require.NoError(t, first.Wait())
	raw, err := os.ReadFile(claim)
	require.NoError(t, err, "the claim on %s went with the first natsd", host)
	require.Equal(t, fmt.Sprint(os.Getpid()), strings.TrimSpace(string(raw)))

	_, other := startNatsdWith(t, []string{natsdDrawsEnv + "=" + host}, "-p", port, "-store_dir", t.TempDir())
	otherHost, _, err := net.SplitHostPort(other)
	require.NoError(t, err)
	require.NotEqual(t, host, otherHost, "a second stack's natsd, whose claim drew %s first, took it while the script held it", host)
	raw, err = os.ReadFile(claim)
	require.NoError(t, err, "the claim on %s is gone after a second natsd drew it", host)
	require.Equal(t, fmt.Sprint(os.Getpid()), strings.TrimSpace(string(raw)), "the second natsd took the claim on %s", host)

	_, again := startNatsd(t, "-a", host, "-p", port, "-store_dir", store)
	require.Equal(t, address, again, "the restart came back elsewhere")
	requireNatsInfo(t, again)
}
