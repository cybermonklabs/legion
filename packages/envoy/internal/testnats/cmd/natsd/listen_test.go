package main

import (
	"bufio"
	"fmt"
	"io"
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

	"github.com/sjawhar/envoy/internal/testport"
)

// natsdArgsEnv, when set, makes the test binary natsd itself: TestMain runs run on these arguments,
// one per line, so a test can start natsd as a process and signal it as a script does.
const natsdArgsEnv = "NATSD_TEST_ARGS"

func TestMain(m *testing.M) {
	if args, ok := os.LookupEnv(natsdArgsEnv); ok {
		if err := run(strings.Split(args, "\n")); err != nil {
			fmt.Fprintf(os.Stderr, "natsd: %v\n", err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// startNatsd starts this test binary as natsd on args and returns it, with the address its log
// names on the line a script reads (`Listening for client connections on <address>:<port>`).
func startNatsd(t *testing.T, args ...string) (*exec.Cmd, string) {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), natsdArgsEnv+"="+strings.Join(args, "\n"))
	out, err := cmd.StdoutPipe()
	require.NoError(t, err)
	cmd.Stderr = cmd.Stdout
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		_ = cmd.Wait()
	})
	listening := regexp.MustCompile(`Listening for client connections on (\S+:\d+)$`)
	found := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(out)
		for scanner.Scan() {
			if match := listening.FindStringSubmatch(scanner.Text()); match != nil {
				found <- match[1]
				break
			}
		}
		_, _ = io.Copy(io.Discard, out)
	}()
	select {
	case address := <-found:
		return cmd, address
	case <-time.After(30 * time.Second):
		t.Fatalf("natsd %q logged no client address within 30s", args)
		return nil, ""
	}
}

// natsInfo reports whether a NATS server answers at address with its INFO line.
func natsInfo(address string) bool {
	conn, err := net.DialTimeout("tcp", address, 2*time.Second)
	if err != nil {
		return false
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	line, err := bufio.NewReader(conn).ReadString('\n')
	return err == nil && strings.HasPrefix(line, "INFO ")
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
	require.True(t, natsInfo(address), "the address natsd named, %s, does not answer", address)
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
	require.True(t, natsInfo(address))
	if runtime.GOOS == "linux" {
		_, err = os.Stat(filepath.Join(testport.ClaimDir(), host))
		require.ErrorIs(t, err, os.ErrNotExist, "natsd claimed %s, which its caller gave it", host)
	}

	for _, refused := range []string{"0.0.0.0", "10.1.2.3", "localhost"} {
		err := run([]string{"-a", refused, "-p", "-1", "-store_dir", t.TempDir()})
		require.ErrorContains(t, err, "no loopback address", "-a %s", refused)
	}
}

// With -claim-for, the claim is that process's and outlasts natsd: a script starts natsd with
// -claim-for $$, stops it, and restarts it with -a on the address it read and its old port, while
// the claim keeps every other stack off that address. A second natsd started in the gap, as a
// second stack starts one, is given another address, so its port cannot be the restart's.
func TestARestartWithClaimForKeepsTheAddressAndThePort(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("off Linux every natsd shares 127.0.0.1")
	}
	store := t.TempDir()
	first, address := startNatsd(t, "-claim-for", fmt.Sprint(os.Getpid()), "-p", "-1", "-store_dir", store)
	host, port, err := net.SplitHostPort(address)
	require.NoError(t, err)
	claim := filepath.Join(testport.ClaimDir(), host)
	require.NoError(t, first.Process.Signal(syscall.SIGTERM))
	require.NoError(t, first.Wait())
	raw, err := os.ReadFile(claim)
	require.NoError(t, err, "the claim on %s went with the first natsd", host)
	require.Equal(t, fmt.Sprint(os.Getpid()), strings.TrimSpace(string(raw)))

	_, other := startNatsd(t, "-p", port, "-store_dir", t.TempDir())
	otherHost, _, err := net.SplitHostPort(other)
	require.NoError(t, err)
	require.NotEqual(t, host, otherHost, "a second stack's natsd took the claimed %s", host)

	_, again := startNatsd(t, "-a", host, "-p", port, "-store_dir", store)
	require.Equal(t, address, again, "the restart came back elsewhere")
	require.True(t, natsInfo(again))
	require.NoError(t, testport.Release(testport.ClaimDir(), host, os.Getpid()))
}
