package sockettest

import (
	"errors"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
)

// A listening socket this process holds on two descriptors, the listener's and a copy of it, is
// found on exactly those two, in ascending order; once both are closed it is found on none.
func TestDescriptorsAreEveryCopyOfTheSocketThisProcessHolds(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skipf("Descriptors reads Linux's /proc, and this is %s", runtime.GOOS)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen on a loopback port: %v", err)
	}
	defer listener.Close()
	copied, err := listener.(*net.TCPListener).File()
	if err != nil {
		t.Fatalf("copy the listener's descriptor: %v", err)
	}
	defer copied.Close()
	raw, err := listener.(*net.TCPListener).SyscallConn()
	if err != nil {
		t.Fatalf("read the listener's descriptor: %v", err)
	}
	var own int
	if err := raw.Control(func(fd uintptr) { own = int(fd) }); err != nil {
		t.Fatalf("read the listener's descriptor: %v", err)
	}
	info, err := copied.Stat()
	if err != nil {
		t.Fatalf("stat the socket: %v", err)
	}
	inode := info.Sys().(*syscall.Stat_t).Ino

	want := []int{own, int(copied.Fd())}
	slices.Sort(want)
	if got, err := Descriptors(os.Getpid(), inode); err != nil || !slices.Equal(got, want) {
		t.Fatalf("Descriptors(this process, the socket) = %v, %v; want %v, nil", got, err, want)
	}
	_ = copied.Close()
	_ = listener.Close()
	if got, err := Descriptors(os.Getpid(), inode); err != nil || len(got) != 0 {
		t.Fatalf("Descriptors(this process, the closed socket) = %v, %v; want none, nil", got, err)
	}
}

// A process with no /proc entry, as one that has exited and been reaped, holds no descriptor.
// Linux's pid_max is at most 4194304 (1<<22), so no process has the pid above it.
func TestAProcessThatIsGoneHoldsNone(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skipf("Descriptors reads Linux's /proc, and this is %s", runtime.GOOS)
	}
	if got, err := Descriptors(1<<22+1, 1); err != nil || len(got) != 0 {
		t.Fatalf("Descriptors(a pid no process has) = %v, %v; want none, nil", got, err)
	}
}

// A socket that listens is in the TCP tables by its inode while it listens, and gone once it is
// closed.
func TestTCPRowsListAListeningSocketUntilItCloses(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skipf("TCPRows reads Linux's /proc, and this is %s", runtime.GOOS)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen on a loopback port: %v", err)
	}
	file, err := listener.(*net.TCPListener).File()
	if err != nil {
		t.Fatalf("copy the listener's descriptor: %v", err)
	}
	info, err := file.Stat()
	if err != nil {
		t.Fatalf("stat the socket: %v", err)
	}
	inode := info.Sys().(*syscall.Stat_t).Ino
	_ = file.Close()
	listed := func() []Row {
		rows, err := TCPRows()
		if err != nil {
			t.Fatalf("TCPRows: %v", err)
		}
		return slices.DeleteFunc(rows, func(row Row) bool { return row.Inode != inode })
	}
	if rows := listed(); len(rows) != 1 || rows[0].State != "0A" {
		t.Fatalf("the listening socket's rows = %+v; want one, in state 0A", rows)
	}
	_ = listener.Close()
	if rows := listed(); len(rows) != 0 {
		t.Fatalf("the closed socket's rows = %+v; want none", rows)
	}
}

// The tables as tcpRows reads them, laid out under a root of the test's own, so the cases run on
// any machine and say the same on every one: one whose IPv6 is off has no tcp6 table at all.
func TestTCPRowsReadTcp6OnlyWhereItIs(t *testing.T) {
	const header = "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode"
	listening := func(local, inode string) string {
		return "   0: " + local + " 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 " + inode + " 1 0000000000000000 100 0 0 10 0"
	}
	root := func(t *testing.T, tables map[string][]string) string {
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, "net"), 0o755); err != nil {
			t.Fatal(err)
		}
		for table, rows := range tables {
			text := strings.Join(append([]string{header}, rows...), "\n") + "\n"
			if err := os.WriteFile(filepath.Join(dir, "net", table), []byte(text), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}
	inodes := func(rows []Row) []uint64 {
		var got []uint64
		for _, row := range rows {
			got = append(got, row.Inode)
		}
		return got
	}

	both := root(t, map[string][]string{
		"tcp":  {listening("0100007F:1F90", "101")},
		"tcp6": {listening("00000000000000000000000001000000:1F91", "102")},
	})
	if rows, err := tcpRows(both); err != nil || !slices.Equal(inodes(rows), []uint64{101, 102}) {
		t.Errorf("both tables: rows %+v, error %v; want inodes 101 and 102", rows, err)
	}
	ipv4Only := root(t, map[string][]string{"tcp": {listening("0100007F:1F90", "201")}})
	if rows, err := tcpRows(ipv4Only); err != nil || !slices.Equal(inodes(rows), []uint64{201}) {
		t.Errorf("no tcp6 table: rows %+v, error %v; want inode 201", rows, err)
	}
	noTCP := root(t, map[string][]string{"tcp6": {listening("00000000000000000000000001000000:1F91", "301")}})
	if _, err := tcpRows(noTCP); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("no tcp table: error %v; want the missing file's", err)
	}
	unreadable := root(t, map[string][]string{"tcp": {listening("0100007F:1F90", "401")}})
	if err := os.Mkdir(filepath.Join(unreadable, "net", "tcp6"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := tcpRows(unreadable); err == nil {
		t.Errorf("a tcp6 that is a directory: no error; want the read's")
	}
}
