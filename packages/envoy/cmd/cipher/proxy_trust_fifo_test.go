//go:build unix

// packages/envoy/cmd/cipher/proxy_trust_fifo_test.go
//
// A named pipe or a device that the command's settings name: opening a named pipe for reading
// waits for a writer that may never come, so the bundle writer opens one without waiting, and
// reads what a writer sends.
package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/sjawhar/envoy/internal/testwait"
)

// TestProxyRootsTrustedSkipsANamedPipeInACertificateDirectory: the directory holds the root and a
// named pipe; the pipe is passed over, the root is read, and the answer comes.
func TestProxyRootsTrustedSkipsANamedPipeInACertificateDirectory(t *testing.T) {
	root := proxyTestRoot(t, "test proxy root")
	store := proxyTestPool(t, root)
	settings := proxyStoreSettings(t, writeLoginFile(t, "system.pem", root))
	dir := filepath.Dir(writeLoginFile(t, "root.pem", root))
	if err := syscall.Mkfifo(filepath.Join(dir, "pipe"), 0o600); err != nil {
		t.Fatalf("make the named pipe: %v", err)
	}
	if err := os.Symlink(filepath.Join(dir, "pipe"), filepath.Join(dir, "link-to-pipe")); err != nil {
		t.Fatal(err)
	}
	environ := withSettings(settings, "WGETRC="+writeLoginFile(t, "wgetrc", "ca_directory = "+dir+"\n"))
	answered := make(chan bool, 1)
	go func() { answered <- proxyRootsTrusted(environ, root, store) }()
	if !testwait.Receive(t, "proxyRootsTrusted over a directory holding a named pipe", answered) {
		t.Errorf("a directory holding the root, a named pipe and a link to it: proxyRootsTrusted = false, want true")
	}
}

// TestProxyRootsTrustedDoesNotWaitOnAPipeSwappedInAfterTheLook: a trust file that is a regular
// file when the decision looks, and a named pipe by the time it opens it (whoever may write in its
// directory can rename one over it), is opened without waiting and refused by what the open file
// is, so the decision answers, and answers false. The swap is made in the open itself, through the
// open the readers make (trustSourceOpen).
func TestProxyRootsTrustedDoesNotWaitOnAPipeSwappedInAfterTheLook(t *testing.T) {
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
	realOpen := trustSourceOpen
	t.Cleanup(func() { trustSourceOpen = realOpen })
	swapped := false
	trustSourceOpen = func(path string, mode int, perm uint32) (int, error) {
		if path == target && !swapped {
			swapped = true
			if err := os.Rename(pipe, target); err != nil {
				t.Errorf("swap the named pipe in: %v", err)
			}
		}
		return realOpen(path, mode, perm)
	}
	environ := withSettings(settings, "CURL_CA_BUNDLE="+target)
	answered := make(chan bool, 1)
	go func() { answered <- proxyRootsTrusted(environ, root, store) }()
	if testwait.Receive(t, "proxyRootsTrusted over a trust file swapped for a named pipe", answered) {
		t.Errorf("a trust file swapped for a named pipe after the look: proxyRootsTrusted = true, want false")
	}
	if !swapped {
		t.Errorf("the decision opened %s other than through trustSourceOpen, so the swap was never made", target)
	}
}

// TestProxyRootsTrustedDoesNotWaitOnAPipeSwappedInForACertificateDirectory: a wgetrc's
// ca_directory that is a directory when the decision looks and a named pipe by the time it opens
// it is opened without waiting, and as a directory (openDirNoWait), so the open fails rather than
// wait, and the decision answers false.
func TestProxyRootsTrustedDoesNotWaitOnAPipeSwappedInForACertificateDirectory(t *testing.T) {
	root := proxyTestRoot(t, "test proxy root")
	store := proxyTestPool(t, root)
	settings := proxyStoreSettings(t, writeLoginFile(t, "system.pem", root))
	certs := filepath.Dir(writeLoginFile(t, "root.pem", root))
	pipe := filepath.Join(t.TempDir(), "pipe")
	if err := syscall.Mkfifo(pipe, 0o600); err != nil {
		t.Fatalf("make the named pipe: %v", err)
	}
	realOpen := trustSourceOpen
	t.Cleanup(func() { trustSourceOpen = realOpen })
	swapped := false
	trustSourceOpen = func(path string, mode int, perm uint32) (int, error) {
		if path == certs && !swapped {
			swapped = true
			if err := os.RemoveAll(certs); err != nil {
				t.Errorf("take the directory away: %v", err)
			}
			if err := os.Rename(pipe, certs); err != nil {
				t.Errorf("swap the named pipe in: %v", err)
			}
		}
		return realOpen(path, mode, perm)
	}
	environ := withSettings(settings, "WGETRC="+writeLoginFile(t, "wgetrc", "ca_directory = "+certs+"\n"))
	answered := make(chan bool, 1)
	go func() { answered <- proxyRootsTrusted(environ, root, store) }()
	if testwait.Receive(t, "proxyRootsTrusted over a ca_directory swapped for a named pipe", answered) {
		t.Errorf("a ca_directory swapped for a named pipe after the look: proxyRootsTrusted = true, want false")
	}
	if !swapped {
		t.Errorf("the decision opened %s other than through trustSourceOpen, so the swap was never made", certs)
	}
}

// runPrepareAirlock runs prepareAirlock for environ and a release carrying caPEM, bounded by the
// test's deadline so that a read that waits forever fails the test rather than hanging it, and
// answers the files it wrote.
func runPrepareAirlock(t *testing.T, what string, environ []string, caPEM string) airlockFiles {
	t.Helper()
	type answer struct {
		files airlockFiles
		err   error
	}
	release := testRelease("AIRLOCK_ME", []string{"api.example.com"}, nil)
	release.CACertificate = caPEM
	answered := make(chan answer, 1)
	go func() {
		files, err := prepareAirlock(environ, release, "grant-one", nil)
		answered <- answer{files, err}
	}()
	got := testwait.Receive(t, "prepareAirlock with "+what, answered)
	if got.err != nil {
		t.Fatalf("prepareAirlock with %s: %v", what, got.err)
	}
	return got.files
}

// TestPrepareAirlockDoesNotWaitOnANamedPipeWithNoWriter: a trust variable or WGETRC naming a named
// pipe that no process writes, for a command that otherwise needs no bundle. The decision does not
// open the pipe, and the bundle writer opens it without waiting for a writer and reads it as
// empty: prepareAirlock answers, and writes the bundle, since a file the decision will not read
// cannot show that it holds the root. The SSL_CERT_FILE row holds for prepareAirlock alone: in a
// run whose broker URL is https, agent-secrets' own connection to the broker reads SSL_CERT_FILE
// before prepareAirlock does, and waits on such a pipe until its timeout (readTrustFile).
func TestPrepareAirlockDoesNotWaitOnANamedPipeWithNoWriter(t *testing.T) {
	root := proxyTestRoot(t, "test proxy root")
	system := writeLoginFile(t, "ca-certificates.crt", root)
	useSystemTrustFiles(t, system)
	pipe := filepath.Join(t.TempDir(), "pipe")
	if err := syscall.Mkfifo(pipe, 0o600); err != nil {
		t.Fatalf("make the named pipe: %v", err)
	}
	for _, key := range []string{"SSL_CERT_FILE", "CURL_CA_BUNDLE", "WGETRC"} {
		t.Run(key, func(t *testing.T) {
			environ := withSettings(proxyStoreSettings(t, system), "XDG_RUNTIME_DIR="+t.TempDir(), key+"="+pipe)
			if files := runPrepareAirlock(t, key+" naming a named pipe with no writer", environ, root); files.bundle == "" {
				t.Errorf("%s naming a named pipe: prepareAirlock wrote no bundle, want one", key)
			}
		})
	}
}

// pipeSending answers a path that opens the read end of a pipe whose writer sends body and then
// closes, as bash gives a process substitution: CURL_CA_BUNDLE=<(cat corp.pem) names /dev/fd/63.
// The writer writes from a goroutine, so a body larger than the pipe's buffer waits for a reader;
// the test's own read end keeps the pipe whole until the test ends.
func pipeSending(t *testing.T, body string) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("make the pipe: %v", err)
	}
	t.Cleanup(func() { r.Close() })
	go func() {
		_, _ = w.WriteString(body)
		w.Close()
	}()
	return "/dev/fd/" + strconv.Itoa(int(r.Fd()))
}

// TestPrepareAirlockReadsAPipeOrDeviceATrustSettingNames: on the bundle path, what a trust
// variable or WGETRC names is read as the command's own client reads it, whatever kind of file it
// is. A pipe whose writer sends trust keeps that trust in the bundle, and its lines in the
// wgetrc, also where the skip decision looked at the pipe first; SSL_CERT_FILE=/dev/null adds
// nothing, and so does not stand for the system roots; and a pipe or a device that sends more
// than proxyCertFileMaxBytes is read no further and adds nothing.
func TestPrepareAirlockReadsAPipeOrDeviceATrustSettingNames(t *testing.T) {
	proxy := proxyTestRoot(t, "test proxy root")
	systemRoot := proxyTestRoot(t, "test system root")
	corp := proxyTestRoot(t, "test corporate authority")
	useSystemTrustFiles(t, writeLoginFile(t, "ca-certificates.crt", systemRoot))
	read := func(t *testing.T, path string) string {
		t.Helper()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		return string(data)
	}
	// No row carries the nine settings, so each takes the bundle path.
	environ := func(t *testing.T, setting string) []string {
		return []string{"XDG_RUNTIME_DIR=" + t.TempDir(), setting}
	}

	t.Run("CURL_CA_BUNDLE names a pipe whose writer sends a root", func(t *testing.T) {
		files := runPrepareAirlock(t, "CURL_CA_BUNDLE naming a pipe", environ(t, "CURL_CA_BUNDLE="+pipeSending(t, corp)), proxy)
		if bundle := read(t, files.bundle); !strings.Contains(bundle, corp) || !strings.Contains(bundle, proxy) {
			t.Errorf("the bundle lacks the root the pipe sent or the proxy's:\n%s", bundle)
		}
	})
	t.Run("WGETRC names a pipe whose writer sends wgetrc lines", func(t *testing.T) {
		files := runPrepareAirlock(t, "WGETRC naming a pipe", environ(t, "WGETRC="+pipeSending(t, "tries = 7\n")), proxy)
		if wgetrc := read(t, files.wgetrc); !strings.Contains(wgetrc, "tries = 7\n") || !strings.Contains(wgetrc, "ca_certificate = "+files.bundle) {
			t.Errorf("the wgetrc lacks the lines the pipe sent or the bundle:\n%s", wgetrc)
		}
	})
	t.Run("SSL_CERT_FILE names the null device", func(t *testing.T) {
		files := runPrepareAirlock(t, "SSL_CERT_FILE=/dev/null", environ(t, "SSL_CERT_FILE=/dev/null"), proxy)
		if bundle := read(t, files.bundle); strings.Contains(bundle, systemRoot) || !strings.Contains(bundle, proxy) {
			t.Errorf("SSL_CERT_FILE=/dev/null: the bundle holds the system roots or lacks the proxy's, want the proxy's alone:\n%s", bundle)
		}
	})
	t.Run("CURL_CA_BUNDLE names a pipe that sends more than the bound", func(t *testing.T) {
		long := strings.Repeat("#\n", proxyCertFileMaxBytes/2+1) + corp
		files := runPrepareAirlock(t, "CURL_CA_BUNDLE naming a long pipe", environ(t, "CURL_CA_BUNDLE="+pipeSending(t, long)), proxy)
		if bundle := read(t, files.bundle); strings.Contains(bundle, corp) {
			t.Errorf("the bundle holds the root at the end of a pipe longer than %d bytes, want it read no further", proxyCertFileMaxBytes)
		}
	})
	t.Run("CURL_CA_BUNDLE names the zero device, which never ends", func(t *testing.T) {
		files := runPrepareAirlock(t, "CURL_CA_BUNDLE=/dev/zero", environ(t, "CURL_CA_BUNDLE=/dev/zero"), proxy)
		if bundle := read(t, files.bundle); strings.Contains(bundle, "\x00") || !strings.Contains(bundle, proxy) {
			t.Errorf("CURL_CA_BUNDLE=/dev/zero: the bundle holds what the device sent or lacks the proxy's root, want the device read no further")
		}
	})
	t.Run("CURL_CA_BUNDLE names a pipe, and the skip decision looks at it first", func(t *testing.T) {
		// The store holds the release's root and the command carries the nine settings, so the
		// decision reaches the trust files themselves: the control, a regular CURL_CA_BUNDLE that
		// holds the root, gets no bundle. A pipe's bytes can be read once, and the decision refuses
		// it without reading it, so the bundle writer still gets what its writer sent.
		useSystemTrustFiles(t, writeLoginFile(t, "ca-certificates.crt", proxy+systemRoot))
		settings := withSettings(proxyStoreSettings(t, writeLoginFile(t, "system.pem", proxy)), "XDG_RUNTIME_DIR="+t.TempDir())
		control := runPrepareAirlock(t, "a regular CURL_CA_BUNDLE holding the root", withSettings(settings, "CURL_CA_BUNDLE="+writeLoginFile(t, "corp-and-proxy.pem", corp+proxy)), proxy)
		if control.bundle != "" {
			t.Fatalf("control: a regular CURL_CA_BUNDLE holding the root got the bundle %q, so the decision never reached the trust files", control.bundle)
		}
		files := runPrepareAirlock(t, "CURL_CA_BUNDLE naming a pipe", withSettings(settings, "CURL_CA_BUNDLE="+pipeSending(t, corp)), proxy)
		if files.bundle == "" {
			t.Fatalf("CURL_CA_BUNDLE naming a pipe: prepareAirlock wrote no bundle, want one")
		}
		if bundle := read(t, files.bundle); !strings.Contains(bundle, corp) {
			t.Errorf("the bundle lacks the root the pipe sent, so something read the pipe before the bundle writer:\n%s", bundle)
		}
	})
}
