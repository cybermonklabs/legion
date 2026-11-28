// packages/envoy/cmd/cipher/proxy_trust_fifo_linux_test.go
//
// The skip decision and a named pipe, observed so the check cannot miss: Linux's inotify reports
// every open of the pipe, and a pipe filled before the decision runs holds bytes whatever order
// the goroutines are scheduled in.
package main

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/sjawhar/envoy/internal/testwait"
)

// pipeOpens watches path for opens (inotify IN_OPEN) and answers a function that reports whether
// any open happened since the watch began.
func pipeOpens(t *testing.T, path string) func() bool {
	t.Helper()
	fd, err := syscall.InotifyInit1(syscall.IN_NONBLOCK | syscall.IN_CLOEXEC)
	if err != nil {
		t.Fatalf("inotify: %v", err)
	}
	t.Cleanup(func() { syscall.Close(fd) })
	if _, err := syscall.InotifyAddWatch(fd, path, syscall.IN_OPEN); err != nil {
		t.Fatalf("watch %s: %v", path, err)
	}
	return func() bool {
		buf := make([]byte, 4096)
		n, err := syscall.Read(fd, buf)
		if errors.Is(err, syscall.EAGAIN) {
			return false
		}
		if err != nil {
			t.Fatalf("read the inotify events: %v", err)
		}
		return n > 0
	}
}

// TestProxyRootsTrustedDoesNotOpenANamedPipeItLooksAt: the skip decision looks at a trust file
// before it opens it, and opens no named pipe that is there when it looks. Opening one would let a
// writer waiting in its own open go on, and its bytes would then go to a reader that closes them
// unread: a pipe's bytes can be read once, and the bundle writer reads them after the decision.
// The control opens the same pipe without waiting, and the watch sees that open.
func TestProxyRootsTrustedDoesNotOpenANamedPipeItLooksAt(t *testing.T) {
	root := proxyTestRoot(t, "test proxy root")
	store := proxyTestPool(t, root)
	settings := proxyStoreSettings(t, writeLoginFile(t, "system.pem", root))
	pipe := filepath.Join(t.TempDir(), "pipe")
	if err := syscall.Mkfifo(pipe, 0o600); err != nil {
		t.Fatalf("make the named pipe: %v", err)
	}
	opened := pipeOpens(t, pipe)
	if proxyRootsTrusted(withSettings(settings, "CURL_CA_BUNDLE="+pipe), root, store) {
		t.Errorf("CURL_CA_BUNDLE naming a named pipe: proxyRootsTrusted = true, want false")
	}
	if opened() {
		t.Errorf("the decision opened the named pipe CURL_CA_BUNDLE names")
	}
	f, err := os.OpenFile(pipe, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatalf("control: open the pipe: %v", err)
	}
	f.Close()
	if !opened() {
		t.Fatalf("control: the watch saw no open of the pipe after one; it cannot see the decision's either")
	}
}

// TestProxyRootsTrustedReadsNoBytesOfAPipeSwappedInAfterTheLook: a trust file that is a regular
// file when the decision looks, and by the time it opens it a named pipe that already holds the
// proxy's root, is refused by what the open file is and never read: the decision answers false,
// and the root is still in the pipe for the bundle writer. The pipe is filled through an end the
// test holds open read-write, which it closes once the decision's open has returned, so the bytes
// are there and no writer is left, however the goroutines run.
func TestProxyRootsTrustedReadsNoBytesOfAPipeSwappedInAfterTheLook(t *testing.T) {
	root := proxyTestRoot(t, "test proxy root")
	store := proxyTestPool(t, root)
	settings := proxyStoreSettings(t, writeLoginFile(t, "system.pem", root))
	dir := t.TempDir()
	target := filepath.Join(dir, "corp.pem")
	if err := os.WriteFile(target, []byte(root), 0o600); err != nil {
		t.Fatal(err)
	}
	pipe := filepath.Join(dir, "pipe")
	if err := syscall.Mkfifo(pipe, 0o600); err != nil {
		t.Fatalf("make the named pipe: %v", err)
	}
	holder, err := os.OpenFile(pipe, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("hold the pipe open: %v", err)
	}
	if _, err := holder.WriteString(root); err != nil {
		t.Fatalf("fill the pipe: %v", err)
	}
	var kept *os.File
	t.Cleanup(func() {
		if kept != nil {
			kept.Close()
		}
	})
	realOpen := trustSourceOpen
	t.Cleanup(func() { trustSourceOpen = realOpen })
	swapped := false
	trustSourceOpen = func(path string, mode int, perm uint32) (int, error) {
		if path != target || swapped {
			return realOpen(path, mode, perm)
		}
		swapped = true
		if err := os.Rename(pipe, target); err != nil {
			t.Errorf("swap the named pipe in: %v", err)
		}
		fd, openErr := realOpen(path, mode, perm)
		// A reader of the test's own keeps the pipe's bytes alive after the decision's reader
		// closes, so what is left in it can be read below; then the writer goes.
		var keepErr error
		if kept, keepErr = os.OpenFile(target, os.O_RDONLY|syscall.O_NONBLOCK, 0); keepErr != nil {
			t.Errorf("open a reader of the test's own: %v", keepErr)
		}
		holder.Close()
		return fd, openErr
	}
	environ := withSettings(settings, "CURL_CA_BUNDLE="+target)
	answered := make(chan bool, 1)
	go func() { answered <- proxyRootsTrusted(environ, root, store) }()
	if testwait.Receive(t, "proxyRootsTrusted over a trust file swapped for a filled named pipe", answered) {
		t.Errorf("a trust file swapped for a named pipe holding the root: proxyRootsTrusted = true, want false")
	}
	if !swapped {
		t.Fatalf("the decision opened %s other than through trustSourceOpen, so the swap was never made", target)
	}
	if kept == nil {
		t.Fatalf("no reader of the test's own was opened")
	}
	left := make([]byte, len(root)+1)
	n, _ := kept.Read(left)
	if n != len(root) {
		t.Errorf("the pipe holds %d of the root's %d bytes after the decision: it read them", n, len(root))
	}
}
