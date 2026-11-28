// packages/envoy/cmd/cipher/proxy_trust_test.go
//
// When a command needs no trust bundle: the machine's system trust bundle already trusts every
// root the proxy's release carries, every client family shows in the command's environment that
// it reads that store, and no trust variable, wgetrc or certificate directory the command has
// lacks a root. Then nothing is written and no trust variable is set. Otherwise prepareAirlock
// writes the bundle and every trust variable names it. The roots here are made for each test and
// are public certificates, so these tests print what they compare.
package main

import (
	"crypto/x509"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/proxyrules/proxyrulestest"
)

// proxyTestRoot is a self-signed certificate authority made for one test, in the proxy root's
// shape, as the PEM a release carries.
func proxyTestRoot(t *testing.T, name string) string {
	t.Helper()
	authority, err := proxyrulestest.NewAuthority(name)
	if err != nil {
		t.Fatalf("make %s: %v", name, err)
	}
	return string(authority.CertificatePEM())
}

// proxyTestPool answers a certificate pool that holds the roots given, standing for a system store.
func proxyTestPool(t *testing.T, roots ...string) func() *x509.CertPool {
	t.Helper()
	pool := x509.NewCertPool()
	for _, root := range roots {
		if !pool.AppendCertsFromPEM([]byte(root)) {
			t.Fatalf("the test pool took no certificate from %q", root)
		}
	}
	return func() *x509.CertPool { return pool }
}

// proxyStoreFileSettingNames and proxyStoreSettingNames are the nine settings with which a pod
// image and a launcher session make each client family that does not read the system store by
// default read it: the eight bundle variables, each naming the system bundle, then uv's switch.
// Written out here rather than taken from airlock.go, so a name dropped there fails a row here.
var (
	proxyStoreFileSettingNames = []string{
		"SSL_CERT_FILE", "CURL_CA_BUNDLE", "REQUESTS_CA_BUNDLE", "NODE_EXTRA_CA_CERTS", "GIT_SSL_CAINFO",
		"CLOUDSDK_CORE_CUSTOM_CA_CERTS_FILE", "AWS_CA_BUNDLE", "GRPC_DEFAULT_SSL_ROOTS_FILE_PATH",
	}
	proxyStoreSettingNames = append(slices.Clone(proxyStoreFileSettingNames), "UV_SYSTEM_CERTS")
)

// proxyStoreSettings is a command environment with the nine settings, each file setting naming
// file, and a home and a system wgetrc of the test's own that hold no wget setting, so no startup
// file of the machine running the test is read.
func proxyStoreSettings(t *testing.T, file string) []string {
	t.Helper()
	environ := []string{"HOME=" + t.TempDir(), "SYSTEM_WGETRC=" + writeLoginFile(t, "system-wgetrc", "")}
	for _, key := range proxyStoreFileSettingNames {
		environ = append(environ, key+"="+file)
	}
	return append(environ, "UV_SYSTEM_CERTS=1")
}

// withoutSetting is environ less every entry for key.
func withoutSetting(environ []string, key string) []string {
	return slices.DeleteFunc(slices.Clone(environ), func(e string) bool { return strings.HasPrefix(e, key+"=") })
}

// withSettings is environ with more entries after it, which win over its own as execve reads them.
func withSettings(environ []string, more ...string) []string {
	return append(slices.Clone(environ), more...)
}

// TestProxyRootsTrustedOnlyWhenTheStoreHoldsEveryRoot: the store must trust each root the release
// carries. While the proxy's root changes, a release carries two, the old root and the new, so a
// store that holds one of the two still gets the bundle. A release with no root that parses is no
// root the store trusts.
func TestProxyRootsTrustedOnlyWhenTheStoreHoldsEveryRoot(t *testing.T) {
	newRoot, oldRoot := proxyTestRoot(t, "test proxy root"), proxyTestRoot(t, "test old authority")
	settings := proxyStoreSettings(t, writeLoginFile(t, "settings.pem", oldRoot+newRoot))
	for _, row := range []struct {
		name    string
		release string
		store   []string
		want    bool
	}{
		{name: "the store holds the one root", release: newRoot, store: []string{newRoot}, want: true},
		{name: "the store holds another root", release: newRoot, store: []string{oldRoot}},
		{name: "an empty store", release: newRoot},
		{name: "a two-root release and a store that holds one of them", release: oldRoot + newRoot, store: []string{newRoot}},
		{name: "a two-root release and a store that holds both", release: oldRoot + newRoot, store: []string{newRoot, oldRoot}, want: true},
		{name: "a release with no certificate", release: "", store: []string{newRoot}},
		{name: "a release whose one block does not parse", release: testAirlockCertificate, store: []string{newRoot}},
		{name: "a release with a root and a block that does not parse", release: newRoot + testAirlockCertificate, store: []string{newRoot}},
		{name: "a release with a block that does not parse and a root", release: testAirlockCertificate + newRoot, store: []string{newRoot}},
	} {
		t.Run(row.name, func(t *testing.T) {
			if got := proxyRootsTrusted(settings, row.release, proxyTestPool(t, row.store...)); got != row.want {
				t.Fatalf("proxyRootsTrusted = %v, want %v", got, row.want)
			}
		})
	}
}

// TestProxyRootsTrustedOnlyWhereEveryClientReadsTheStore: Node, Python requests and httpx, uv,
// git, the AWS CLI, gcloud and gRPC's C core each read their own roots, not the system store,
// unless a setting points them at it. A command without one of those settings, a shell started
// before a launcher set them or a runtime that passes on only some variables, keeps the bundle,
// which reaches each of those clients; with all nine the command needs none. Node counts only
// through NODE_EXTRA_CA_CERTS naming a file that holds every root, which every Node from v7.3
// reads: NODE_USE_SYSTEM_CA exists only from v24.6.0 and v22.19.0, so it proves nothing about the
// Node a command runs. An empty GIT_SSL_CAINFO is a setting git reads, as an empty CA file that
// fails every HTTPS request. UV_SYSTEM_CERTS is on as uv reads it: 1, t, true, y, yes or on in
// any case (uv 0.13.5).
func TestProxyRootsTrustedOnlyWhereEveryClientReadsTheStore(t *testing.T) {
	root, other := proxyTestRoot(t, "test proxy root"), proxyTestRoot(t, "test private authority")
	store := proxyTestPool(t, root)
	file := writeLoginFile(t, "system.pem", root)
	without := writeLoginFile(t, "private.pem", other)
	settings := proxyStoreSettings(t, file)
	if !proxyRootsTrusted(settings, root, store) {
		t.Fatalf("every setting naming a file that holds the root: proxyRootsTrusted = false, want true")
	}
	for _, key := range proxyStoreSettingNames {
		if proxyRootsTrusted(withoutSetting(settings, key), root, store) {
			t.Errorf("%s unset: proxyRootsTrusted = true, want false", key)
		}
		if proxyRootsTrusted(withSettings(settings, key+"="), root, store) {
			t.Errorf("%s set and empty: proxyRootsTrusted = true, want false", key)
		}
	}
	noNodeFile := withoutSetting(settings, "NODE_EXTRA_CA_CERTS")
	for _, row := range []struct {
		name    string
		environ []string
		want    bool
	}{
		{name: "NODE_USE_SYSTEM_CA=1 and no NODE_EXTRA_CA_CERTS, which a Node before v24.6.0 or v22.19.0 does not read", environ: withSettings(noNodeFile, "NODE_USE_SYSTEM_CA=1")},
		{name: "NODE_USE_SYSTEM_CA=1 and NODE_EXTRA_CA_CERTS naming a file without the root", environ: withSettings(settings, "NODE_USE_SYSTEM_CA=1", "NODE_EXTRA_CA_CERTS="+without)},
		{name: "NODE_USE_SYSTEM_CA=0 and NODE_EXTRA_CA_CERTS naming a file that holds the root", environ: withSettings(settings, "NODE_USE_SYSTEM_CA=0"), want: true},
		{name: "UV_SYSTEM_CERTS=0", environ: withSettings(settings, "UV_SYSTEM_CERTS=0")},
		{name: "UV_SYSTEM_CERTS=false", environ: withSettings(settings, "UV_SYSTEM_CERTS=false")},
		{name: "UV_SYSTEM_CERTS=2", environ: withSettings(settings, "UV_SYSTEM_CERTS=2")},
		{name: "UV_SYSTEM_CERTS=true", environ: withSettings(settings, "UV_SYSTEM_CERTS=true"), want: true},
		{name: "UV_SYSTEM_CERTS=Yes", environ: withSettings(settings, "UV_SYSTEM_CERTS=Yes"), want: true},
		{name: "UV_SYSTEM_CERTS=on", environ: withSettings(settings, "UV_SYSTEM_CERTS=on"), want: true},
	} {
		if got := proxyRootsTrusted(row.environ, root, store); got != row.want {
			t.Errorf("%s: proxyRootsTrusted = %v, want %v", row.name, got, row.want)
		}
	}
}

// TestProxyRootsTrustedLeavesGitsConfigToGitSSLCAInfo: git reads GIT_SSL_CAINFO after every
// configuration level, an http.sslCAInfo in a gitconfig or a URL-scoped one included (measured
// with git 2.47.3: an empty http.sslCAInfo fails every HTTPS request, and GIT_SSL_CAINFO naming
// the system bundle beside it lets them verify; on Debian's build an http.sslCAInfo that lacks the
// root still verifies, through the CA directory /etc/ssl/certs curl reads beside it). So the
// decision needs GIT_SSL_CAINFO and reads no git configuration: with it unset, a gitconfig whose
// http.sslCAInfo is empty or lacks the root gets the bundle, as any command without it does; with
// it naming a file that holds the root, git never reads that gitconfig's value, and the bundle is
// skipped.
func TestProxyRootsTrustedLeavesGitsConfigToGitSSLCAInfo(t *testing.T) {
	root, other := proxyTestRoot(t, "test proxy root"), proxyTestRoot(t, "test private authority")
	store := proxyTestPool(t, root)
	settings := proxyStoreSettings(t, writeLoginFile(t, "system.pem", root))
	noGitFile := withoutSetting(settings, "GIT_SSL_CAINFO")
	empty := writeLoginFile(t, "gitconfig", "[http]\n\tsslCAInfo =\n")
	lacking := writeLoginFile(t, "gitconfig", "[http]\n\tsslCAInfo = "+writeLoginFile(t, "private.pem", other)+"\n")
	for _, row := range []struct {
		name    string
		environ []string
		want    bool
	}{
		{name: "a gitconfig with an empty http.sslCAInfo, GIT_SSL_CAINFO unset", environ: withSettings(noGitFile, "GIT_CONFIG_GLOBAL="+empty)},
		{name: "a gitconfig whose http.sslCAInfo lacks the root, GIT_SSL_CAINFO unset", environ: withSettings(noGitFile, "GIT_CONFIG_GLOBAL="+lacking)},
		{name: "a gitconfig with an empty http.sslCAInfo, GIT_SSL_CAINFO naming a file that holds the root", environ: withSettings(settings, "GIT_CONFIG_GLOBAL="+empty), want: true},
		{name: "a gitconfig whose http.sslCAInfo lacks the root, GIT_SSL_CAINFO naming a file that holds the root", environ: withSettings(settings, "GIT_CONFIG_GLOBAL="+lacking), want: true},
	} {
		if got := proxyRootsTrusted(row.environ, root, store); got != row.want {
			t.Errorf("%s: proxyRootsTrusted = %v, want %v", row.name, got, row.want)
		}
	}
}

// TestProxyRootsTrustedCountsNoRelativePath: a relative path is resolved from the directory of
// whoever reads it, and the command may read it from a directory other than the one it starts in
// (`git -C ../x`, `sh -c 'cd sub && curl'`). So a relative path never counts toward the skip,
// whatever this process finds under it: a trust variable, WGETRC, SYSTEM_WGETRC, a HOME whose
// .wgetrc wget would read, or a wgetrc's ca_directory.
func TestProxyRootsTrustedCountsNoRelativePath(t *testing.T) {
	root := proxyTestRoot(t, "test proxy root")
	store := proxyTestPool(t, root)
	settings := proxyStoreSettings(t, writeLoginFile(t, "system.pem", root))
	start := t.TempDir()
	t.Chdir(start)
	write := func(name, body string) {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(start, name)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(start, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("certs/ca.pem", root)
	write("certs/root.pem", root)
	write("wgetrc", "tries = 3\n")
	write("home/.wgetrc", "tries = 3\n")
	if !proxyRootsTrusted(withSettings(settings, "CURL_CA_BUNDLE="+filepath.Join(start, "certs/ca.pem")), root, store) {
		t.Fatalf("control: CURL_CA_BUNDLE naming the same file by its absolute path: proxyRootsTrusted = false, want true")
	}
	// A relative SYSTEM_WGETRC or HOME that names nothing here is one wget may find from the
	// command's own directory, so it counts no more than one that names a file here. The built-in
	// system wgetrc it would otherwise fall back to is the test's own, with no setting.
	old := systemWgetrcFile
	systemWgetrcFile = writeLoginFile(t, "etc-wgetrc", "tries = 3\n")
	t.Cleanup(func() { systemWgetrcFile = old })
	rows := map[string][]string{
		"WGETRC=wgetrc":                       {"WGETRC=wgetrc"},
		"SYSTEM_WGETRC=wgetrc":                {"SYSTEM_WGETRC=wgetrc"},
		"SYSTEM_WGETRC=not-here":              {"SYSTEM_WGETRC=not-here"},
		"HOME=home, WGETRC unset":             {"HOME=home"},
		"HOME=not-here, WGETRC unset":         {"HOME=not-here"},
		"a wgetrc's ca_directory = certs":     {"WGETRC=" + writeLoginFile(t, "abs-wgetrc", "ca_directory = certs\n")},
		"a wgetrc's ca_directory = not-there": {"WGETRC=" + writeLoginFile(t, "abs-wgetrc", "ca_directory = not-there\n")},
	}
	for _, key := range airlockBundleVars {
		rows[key+"=certs/ca.pem"] = []string{key + "=certs/ca.pem"}
	}
	for name, environ := range rows {
		if proxyRootsTrusted(withSettings(settings, environ...), root, store) {
			t.Errorf("%s, relative: proxyRootsTrusted = true, want false", name)
		}
	}
}

// TestProxyRootsTrustedReadsTheTrustVariablesTheCommandHas: a trust variable the command already
// has sets its client's roots, and with no bundle the command keeps it as it is. So a file it
// names that lacks a root takes the proxy away from that client, and the bundle is still written.
// NODE_EXTRA_CA_CERTS too: it is the one way the root reaches a Node that does not read the
// system store. A file that cannot be read cannot show that it holds the root, and an empty
// GIT_SSL_CAINFO is the empty CA file git then reads, which fails every HTTPS request. Any
// SSL_CERT_DIR at all gets the bundle (the rows at the end say why).
func TestProxyRootsTrustedReadsTheTrustVariablesTheCommandHas(t *testing.T) {
	root, other := proxyTestRoot(t, "test proxy root"), proxyTestRoot(t, "test private authority")
	store := proxyTestPool(t, root)
	without := writeLoginFile(t, "private.pem", other)
	with := writeLoginFile(t, "both.pem", other+root)
	unreadable := filepath.Join(t.TempDir(), "not-there.pem")
	settings := proxyStoreSettings(t, writeLoginFile(t, "system.pem", root))

	for _, key := range airlockBundleVars {
		if proxyRootsTrusted(withSettings(settings, key+"="+without), root, store) {
			t.Errorf("%s naming a file without the root: proxyRootsTrusted = true, want false", key)
		}
		if got := proxyRootsTrusted(withSettings(settings, key+"="+with), root, store); !got {
			t.Errorf("%s naming a file that holds the root: proxyRootsTrusted = false, want true", key)
		}
	}
	if proxyRootsTrusted(withSettings(settings, "REQUESTS_CA_BUNDLE="+unreadable), root, store) {
		t.Errorf("REQUESTS_CA_BUNDLE naming a file that cannot be read: proxyRootsTrusted = true, want false")
	}
	if proxyRootsTrusted(withSettings(settings, "GIT_SSL_CAINFO="), root, store) {
		t.Errorf("an empty GIT_SSL_CAINFO, which git reads as an empty CA file: proxyRootsTrusted = true, want false")
	}
	// The last entry wins, as execve reads it.
	if proxyRootsTrusted(withSettings(settings, "CURL_CA_BUNDLE="+with, "CURL_CA_BUNDLE="+without), root, store) {
		t.Errorf("CURL_CA_BUNDLE set twice, the last naming a file without the root: proxyRootsTrusted = true, want false")
	}

	// SSL_CERT_DIR names a certificate directory that Node's system store (NODE_USE_SYSTEM_CA), uv
	// (UV_SYSTEM_CERTS) and httpx read in place of the system's, and with no bundle the command
	// keeps it. They do not read it alike: Node v24 takes the whole value as one path (a ':' list
	// is one directory that is not there) and an empty value as set, and httpx, through OpenSSL's
	// capath, finds a root only under its subject-hash name. No reading of the directories answers
	// for all three, so any SSL_CERT_DIR entry, even an empty one, gets the bundle, whose
	// SSL_CERT_FILE and NODE_EXTRA_CA_CERTS reach each of them.
	dirWithout := filepath.Dir(without)
	dirWith := filepath.Dir(writeLoginFile(t, "root.pem", root))
	for _, row := range []struct {
		name    string
		environ []string
	}{
		{name: "an empty directory", environ: []string{"SSL_CERT_DIR=" + t.TempDir()}},
		{name: "a directory without the root", environ: []string{"SSL_CERT_DIR=" + dirWithout}},
		{name: "a directory that holds the root", environ: []string{"SSL_CERT_DIR=" + dirWith}},
		{name: "a ':' list whose first directory holds the root", environ: []string{"SSL_CERT_DIR=" + dirWith + string(filepath.ListSeparator) + dirWithout}},
		{name: "set and empty", environ: []string{"SSL_CERT_DIR="}},
		{name: "set and empty after a directory", environ: []string{"SSL_CERT_DIR=" + dirWith, "SSL_CERT_DIR="}},
	} {
		if proxyRootsTrusted(withSettings(settings, row.environ...), root, store) {
			t.Errorf("SSL_CERT_DIR, %s (%q): proxyRootsTrusted = true, want false", row.name, row.environ)
		}
	}
}

// TestProxyRootsTrustedReadsTheWgetrcTheCommandHas: wget reads no trust variable, only its
// startup files, and with no bundle the command keeps them. A ca_directory there replaces the
// system store, so the last one, the one wget reads, must hold every root, and an empty one loads
// no directory and no system store at all; a ca_certificate adds to the store's trust and takes
// nothing away; and a wgetrc or a directory this process cannot read is one it cannot vouch for.
// wget ignores case, '_' and '-' in a command's name, and skips a line that opens with '#'.
func TestProxyRootsTrustedReadsTheWgetrcTheCommandHas(t *testing.T) {
	root, other := proxyTestRoot(t, "test proxy root"), proxyTestRoot(t, "test private authority")
	store := proxyTestPool(t, root)
	settings := proxyStoreSettings(t, writeLoginFile(t, "system.pem", root))
	without := writeLoginFile(t, "private.pem", other)
	dirWithout := filepath.Dir(writeLoginFile(t, "private.pem", other))
	dirWith := filepath.Dir(writeLoginFile(t, "root.pem", root))
	if err := os.WriteFile(filepath.Join(dirWith, "private.pem"), []byte(other), 0o600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(t.TempDir(), "not-there")
	for _, row := range []struct {
		name   string
		wgetrc string
		want   bool
	}{
		{name: "a wgetrc with no trust setting", wgetrc: "tries = 3\n", want: true},
		{name: "a ca_certificate without the root", wgetrc: "ca_certificate = " + without + "\n", want: true},
		{name: "a ca_directory that holds the root", wgetrc: "tries = 3\nca_directory = " + dirWith + "\n", want: true},
		{name: "a ca_directory without the root", wgetrc: "tries = 3\nca_directory = " + dirWithout + "\n"},
		{name: "the same setting spelt with dashes and capitals", wgetrc: "CA-Directory=" + dirWithout + "\n"},
		{name: "two ca_directory lines, the last without the root", wgetrc: "ca_directory = " + dirWith + "\nca_directory = " + dirWithout + "\n"},
		{name: "two ca_directory lines, the last with the root", wgetrc: "ca_directory = " + dirWithout + "\nca_directory = " + dirWith + "\n", want: true},
		{name: "a commented-out ca_directory", wgetrc: "# ca_directory = " + dirWithout + "\n", want: true},
		{name: "a ca_directory that cannot be read", wgetrc: "ca_directory = " + missing + "\n"},
		{name: "an empty ca_directory, which loads no directory and no system store", wgetrc: "ca_directory =\n"},
		{name: "an empty ca_directory after one that holds the root", wgetrc: "ca_directory = " + dirWith + "\nca_directory =   \n"},
		{name: "a ca_directory that holds the root after an empty one", wgetrc: "ca_directory =\nca_directory = " + dirWith + "\n", want: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			wgetrc := writeLoginFile(t, "wgetrc", row.wgetrc)
			if got := proxyRootsTrusted(withSettings(settings, "WGETRC="+wgetrc), root, store); got != row.want {
				t.Fatalf("proxyRootsTrusted with the wgetrc %q = %v, want %v", row.wgetrc, got, row.want)
			}
		})
	}
	if proxyRootsTrusted(withSettings(settings, "WGETRC="+missing), root, store) {
		t.Errorf("WGETRC naming a file that cannot be read: proxyRootsTrusted = true, want false")
	}
}

// TestProxyRootsTrustedReadsWgetsStartupFilesInWgetsOrder: wget reads the system wgetrc, which
// SYSTEM_WGETRC names when that file is there and the one built in (Debian's /etc/wgetrc)
// otherwise, and then the user's: the file WGETRC names, or ~/.wgetrc when WGETRC is unset or
// empty. A later ca_directory wins over an earlier one (measured with wget 1.25.0 and 1.21.4).
func TestProxyRootsTrustedReadsWgetsStartupFilesInWgetsOrder(t *testing.T) {
	root := proxyTestRoot(t, "test proxy root")
	store := proxyTestPool(t, root)
	settings := proxyStoreSettings(t, writeLoginFile(t, "system.pem", root))
	emptyDir := t.TempDir()
	dirWith := filepath.Dir(writeLoginFile(t, "root.pem", root))
	home := func(wgetrc string) string { return "HOME=" + filepath.Dir(writeLoginFile(t, ".wgetrc", wgetrc)) }
	noSetting := writeLoginFile(t, "plain-wgetrc", "tries = 3\n")
	system := func(wgetrc string) string { return "SYSTEM_WGETRC=" + writeLoginFile(t, "system-wgetrc", wgetrc) }
	for _, row := range []struct {
		name    string
		environ []string
		want    bool
	}{
		{name: "~/.wgetrc names an empty directory, WGETRC unset", environ: []string{home("ca_directory = " + emptyDir + "\n")}},
		{name: "~/.wgetrc has an empty ca_directory, WGETRC unset", environ: []string{home("ca_directory =\n")}},
		{name: "~/.wgetrc names a directory that holds the root, WGETRC unset", environ: []string{home("ca_directory = " + dirWith + "\n")}, want: true},
		{name: "~/.wgetrc has an empty ca_directory, WGETRC empty", environ: []string{home("ca_directory =\n"), "WGETRC="}},
		{name: "~/.wgetrc has an empty ca_directory, WGETRC names a file with no setting", environ: []string{home("ca_directory =\n"), "WGETRC=" + noSetting}, want: true},
		{name: "the system wgetrc names an empty directory, WGETRC has no setting", environ: []string{system("ca_directory = " + emptyDir + "\n"), "WGETRC=" + noSetting}},
		{name: "the system wgetrc names an empty directory, WGETRC one that holds the root", environ: []string{system("ca_directory = " + emptyDir + "\n"), "WGETRC=" + writeLoginFile(t, "wgetrc", "ca_directory = "+dirWith+"\n")}, want: true},
		{name: "the system wgetrc names a directory that holds the root, ~/.wgetrc an empty ca_directory", environ: []string{system("ca_directory = " + dirWith + "\n"), home("ca_directory =\n")}},
	} {
		if got := proxyRootsTrusted(withSettings(settings, row.environ...), root, store); got != row.want {
			t.Errorf("%s: proxyRootsTrusted = %v, want %v", row.name, got, row.want)
		}
	}

	// The built-in system wgetrc, read when SYSTEM_WGETRC is unset or names no file.
	old := systemWgetrcFile
	systemWgetrcFile = writeLoginFile(t, "etc-wgetrc", "ca_directory =\n")
	t.Cleanup(func() { systemWgetrcFile = old })
	if proxyRootsTrusted(withoutSetting(settings, "SYSTEM_WGETRC"), root, store) {
		t.Errorf("the built-in system wgetrc has an empty ca_directory, SYSTEM_WGETRC unset: proxyRootsTrusted = true, want false")
	}
	if proxyRootsTrusted(withSettings(settings, "SYSTEM_WGETRC="+filepath.Join(emptyDir, "missing")), root, store) {
		t.Errorf("the built-in system wgetrc has an empty ca_directory, SYSTEM_WGETRC names no file: proxyRootsTrusted = true, want false")
	}
	if !proxyRootsTrusted(settings, root, store) {
		t.Errorf("the built-in system wgetrc has an empty ca_directory, SYSTEM_WGETRC names a file with no setting: proxyRootsTrusted = false, want true")
	}
}

// TestHomeWgetrcIsTheFileWgetReads: wget's user startup file, where WGETRC names none, is
// $HOME/.wgetrc when HOME is set, even empty (wget joins the two with '/', so an empty HOME reads
// /.wgetrc), and the password database's home only when HOME is unset.
func TestHomeWgetrcIsTheFileWgetReads(t *testing.T) {
	for _, row := range []struct {
		name    string
		environ []string
		want    string
	}{
		{name: "HOME set", environ: []string{"HOME=/home/someone"}, want: "/home/someone/.wgetrc"},
		{name: "HOME set and empty", environ: []string{"HOME="}, want: "/.wgetrc"},
		{name: "HOME set empty after a value", environ: []string{"HOME=/home/someone", "HOME="}, want: "/.wgetrc"},
	} {
		if got := homeWgetrc(row.environ); got != row.want {
			t.Errorf("%s: homeWgetrc(%q) = %q, want %q", row.name, row.environ, got, row.want)
		}
	}
	if home := passwdHome(); home != "" {
		if got, want := homeWgetrc([]string{"PATH=/usr/bin"}), home+"/.wgetrc"; got != want {
			t.Errorf("HOME unset: homeWgetrc = %q, want the password database's home %q", got, want)
		}
	}
}

// TestProxyRootsTrustedReadsACertificateDirectoryInBounds: wget's ca_directory is read for its
// regular files alone, a symbolic link to one among them as Debian's /etc/ssl/certs holds, each up
// to proxyCertFileMaxBytes, and a directory of more than proxyCertDirMaxFiles entries, or more than
// proxyCertDirMaxBytes in all, is one this cannot vouch for. A subdirectory adds nothing, and a
// file over the bound is skipped.
func TestProxyRootsTrustedReadsACertificateDirectoryInBounds(t *testing.T) {
	root := proxyTestRoot(t, "test proxy root")
	store := proxyTestPool(t, root)
	settings := proxyStoreSettings(t, writeLoginFile(t, "system.pem", root))
	trusted := func(dir string) bool {
		return proxyRootsTrusted(withSettings(settings, "WGETRC="+writeLoginFile(t, "wgetrc", "ca_directory = "+dir+"\n")), root, store)
	}
	write := func(dir, name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	oversize := strings.Repeat("#", proxyCertFileMaxBytes) + "\n" + root

	linked := t.TempDir()
	target := writeLoginFile(t, "root.pem", root)
	if err := os.Symlink(target, filepath.Join(linked, "abcd1234.0")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(linked, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	if !trusted(linked) {
		t.Errorf("a directory holding a symbolic link to the root and a subdirectory: proxyRootsTrusted = false, want true")
	}

	big := t.TempDir()
	write(big, "big.pem", oversize)
	if trusted(big) {
		t.Errorf("a directory whose only root is in a file over %d bytes: proxyRootsTrusted = true, want false", proxyCertFileMaxBytes)
	}
	write(big, "root.pem", root)
	if !trusted(big) {
		t.Errorf("a directory holding the root beside a file over the bound: proxyRootsTrusted = false, want true")
	}

	crowded := t.TempDir()
	write(crowded, "root.pem", root)
	for i := range proxyCertDirMaxFiles {
		write(crowded, "empty-"+strconv.Itoa(i), "")
	}
	if trusted(crowded) {
		t.Errorf("a directory of more than %d entries, one holding the root: proxyRootsTrusted = true, want false", proxyCertDirMaxFiles)
	}

	// Under both bounds above, a directory of files just under proxyCertFileMaxBytes each still
	// adds up: past proxyCertDirMaxBytes in all, the directory is one this cannot vouch for, in
	// whichever order it lists the file that holds the root.
	heavy := t.TempDir()
	write(heavy, "root.pem", root)
	filler := strings.Repeat("#", proxyCertFileMaxBytes-1) + "\n"
	for i := range proxyCertDirMaxBytes/proxyCertFileMaxBytes + 1 {
		write(heavy, "filler-"+strconv.Itoa(i), filler)
	}
	if trusted(heavy) {
		t.Errorf("a directory of more than %d bytes in all, one file holding the root: proxyRootsTrusted = true, want false", proxyCertDirMaxBytes)
	}
}

// useSystemTrustFiles points systemTrustFiles, the system bundles prepareAirlock reads the store
// from, at paths until the test ends. A test that calls it swaps a package variable, so neither it
// nor its subtests run in parallel.
func useSystemTrustFiles(t *testing.T, paths ...string) {
	t.Helper()
	old := systemTrustFiles
	systemTrustFiles = paths
	t.Cleanup(func() { systemTrustFiles = old })
}

// TestPrepareAirlockLeavesTheTrustAloneWhereTheSystemStoreHoldsTheRoot runs prepareAirlock and
// airlockVars against a system bundle of the test's own, with and without the release's roots in
// it, for a command that carries the nine settings naming that bundle unless a row says
// otherwise. Where the bundle trusts every root, every setting is there, no trust variable or
// wgetrc of the command names a file or directory that lacks one, and the command has no
// SSL_CERT_DIR, no file is written and none of the trust variables or WGETRC is set, while the
// proxy still is. Every other row writes the bundle, holding every root, and points each trust
// variable and WGETRC at it. The store is the first of the system bundles that exists, as git and
// wget read it: a missing one is passed over, and one that is there but cannot be read is a store
// that trusts nothing, not a reason to read the next.
func TestPrepareAirlockLeavesTheTrustAloneWhereTheSystemStoreHoldsTheRoot(t *testing.T) {
	newRoot, oldRoot, private := proxyTestRoot(t, "test proxy root"), proxyTestRoot(t, "test old authority"), proxyTestRoot(t, "test private authority")
	withoutRoot := writeLoginFile(t, "private.pem", private)
	withRoot := writeLoginFile(t, "private-and-root.pem", private+newRoot)
	dirWithout := filepath.Dir(withoutRoot)
	dirWith := filepath.Dir(writeLoginFile(t, "root.pem", newRoot))
	wgetrcWithoutRoot := writeLoginFile(t, "wgetrc", "ca_directory = "+dirWithout+"\n")
	wgetrcEmptyDir := writeLoginFile(t, "wgetrc", "ca_directory =\n")
	homeEmptyDir := filepath.Dir(writeLoginFile(t, ".wgetrc", "ca_directory =\n"))
	missing := filepath.Join(t.TempDir(), "not-there.crt")
	unreadable := t.TempDir() // a directory: there, and not a file any client reads
	type row struct {
		name       string
		before     []string // system bundles listed ahead of the one holding store
		store      []string
		release    string
		unset      string // a setting the command does not carry
		environ    []string
		wantBundle bool
	}
	rows := []row{
		{name: "the store holds the root", store: []string{newRoot}, release: newRoot},
		{name: "the store lacks the root", store: []string{private}, release: newRoot, wantBundle: true},
		{name: "a two-root release and a store that holds one of them", store: []string{newRoot}, release: oldRoot + newRoot, wantBundle: true},
		{name: "a two-root release and a store that holds both", store: []string{newRoot, oldRoot}, release: oldRoot + newRoot},
		{name: "the command's CURL_CA_BUNDLE names a file without the root", store: []string{newRoot}, release: newRoot, environ: []string{"CURL_CA_BUNDLE=" + withoutRoot}, wantBundle: true},
		{name: "the command's CURL_CA_BUNDLE names a file that holds the root", store: []string{newRoot}, release: newRoot, environ: []string{"CURL_CA_BUNDLE=" + withRoot}},
		{name: "the command has NODE_USE_SYSTEM_CA=1 and no NODE_EXTRA_CA_CERTS", store: []string{newRoot}, release: newRoot, unset: "NODE_EXTRA_CA_CERTS", environ: []string{"NODE_USE_SYSTEM_CA=1"}, wantBundle: true},
		{name: "the command has an empty GIT_SSL_CAINFO", store: []string{newRoot}, release: newRoot, environ: []string{"GIT_SSL_CAINFO="}, wantBundle: true},
		{name: "the command's wgetrc names a ca_directory without the root", store: []string{newRoot}, release: newRoot, environ: []string{"WGETRC=" + wgetrcWithoutRoot}, wantBundle: true},
		{name: "the command's wgetrc has an empty ca_directory", store: []string{newRoot}, release: newRoot, environ: []string{"WGETRC=" + wgetrcEmptyDir}, wantBundle: true},
		{name: "the command's ~/.wgetrc has an empty ca_directory and WGETRC is unset", store: []string{newRoot}, release: newRoot, environ: []string{"HOME=" + homeEmptyDir}, wantBundle: true},
		{name: "the first system bundle is missing and the next holds the root", before: []string{missing}, store: []string{newRoot}, release: newRoot},
		{name: "the first system bundle is there and cannot be read", before: []string{unreadable}, store: []string{newRoot}, release: newRoot, wantBundle: true},
		// SSL_CERT_DIR, which Node reads as one path and httpx by hash link alone: any value gets
		// the bundle, whose NODE_EXTRA_CA_CERTS and SSL_CERT_FILE reach both.
		{name: "the command's SSL_CERT_DIR names an empty directory", store: []string{newRoot}, release: newRoot, environ: []string{"SSL_CERT_DIR=" + t.TempDir()}, wantBundle: true},
		{name: "the command's SSL_CERT_DIR names a directory without the root", store: []string{newRoot}, release: newRoot, environ: []string{"SSL_CERT_DIR=" + dirWithout}, wantBundle: true},
		{name: "the command's SSL_CERT_DIR names a directory that holds the root as root.pem, with no hash link", store: []string{newRoot}, release: newRoot, environ: []string{"SSL_CERT_DIR=" + dirWith}, wantBundle: true},
		{name: "the command's SSL_CERT_DIR is a list whose second directory holds the root", store: []string{newRoot}, release: newRoot, environ: []string{"SSL_CERT_DIR=" + dirWithout + ":" + dirWith}, wantBundle: true},
		{name: "the command's SSL_CERT_DIR is a list whose first directory holds the root", store: []string{newRoot}, release: newRoot, environ: []string{"SSL_CERT_DIR=" + dirWith + ":" + dirWithout}, wantBundle: true},
		{name: "the command's SSL_CERT_DIR is a list of empty entries around a directory that holds the root", store: []string{newRoot}, release: newRoot, environ: []string{"SSL_CERT_DIR=::" + dirWith + ":"}, wantBundle: true},
		{name: "the command's SSL_CERT_DIR is a ':' list ending in a missing directory", store: []string{newRoot}, release: newRoot, environ: []string{"SSL_CERT_DIR=" + dirWith + string(filepath.ListSeparator) + missing}, wantBundle: true},
		{name: "the command's SSL_CERT_DIR is set and empty", store: []string{newRoot}, release: newRoot, environ: []string{"SSL_CERT_DIR="}, wantBundle: true},
	}
	for _, key := range proxyStoreSettingNames {
		rows = append(rows, row{name: "the command has no " + key, store: []string{newRoot}, release: newRoot, unset: key, wantBundle: true})
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			system := writeLoginFile(t, "ca-certificates.crt", strings.Join(row.store, ""))
			useSystemTrustFiles(t, append(slices.Clone(row.before), system)...)
			runtimeDir := t.TempDir()
			environ := append([]string{"XDG_RUNTIME_DIR=" + runtimeDir}, proxyStoreSettings(t, system)...)
			if row.unset != "" {
				environ = withoutSetting(environ, row.unset)
			}
			environ = withSettings(environ, row.environ...)
			release := testRelease("AIRLOCK_ME", []string{"api.example.com"}, nil)
			release.CACertificate = row.release
			files, err := prepareAirlock(environ, release, "grant-one", nil)
			if err != nil {
				t.Fatalf("prepareAirlock: %v", err)
			}
			vars, err := airlockVars(environ, release, files)
			if err != nil {
				t.Fatalf("airlockVars: %v", err)
			}
			if vars["HTTPS_PROXY"] == "" || vars["NO_PROXY"] == "" {
				t.Errorf("the command's HTTPS_PROXY is %q and NO_PROXY %q, want both set whatever the store holds", vars["HTTPS_PROXY"], vars["NO_PROXY"])
			}
			if !row.wantBundle {
				if files.bundle != "" || files.wgetrc != "" {
					t.Errorf("prepareAirlock answered the files %q and %q, want none", files.bundle, files.wgetrc)
				}
				for _, key := range append(slices.Clone(airlockBundleVars), "WGETRC") {
					if value, set := vars[key]; set {
						t.Errorf("the command's %s is set to %q, want it left as the command has it", key, value)
					}
				}
				if _, err := os.Stat(filepath.Join(runtimeDir, "agent-secrets")); !os.IsNotExist(err) {
					t.Errorf("the runtime directory holds agent-secrets' files (stat: %v), want nothing written", err)
				}
				return
			}
			grantDir := grantDirOf(t, filepath.Join(runtimeDir, "agent-secrets", "airlock"), "grant-one", files.bundle)
			if filepath.Base(files.bundle) != "ca-bundle.pem" {
				t.Fatalf("the bundle is at %q, want the grant directory's ca-bundle.pem", files.bundle)
			}
			bundle, err := os.ReadFile(files.bundle)
			if err != nil {
				t.Fatalf("read the bundle: %v", err)
			}
			for _, root := range []string{newRoot, oldRoot} {
				if strings.Contains(row.release, root) && !strings.Contains(string(bundle), root) {
					t.Errorf("the bundle lacks a root the release carries:\n%s", bundle)
				}
			}
			for _, key := range airlockBundleVars {
				if vars[key] != files.bundle {
					t.Errorf("the command's %s is %q, want the bundle %q", key, vars[key], files.bundle)
				}
			}
			if want := filepath.Join(grantDir, "wgetrc"); vars["WGETRC"] != want {
				t.Errorf("the command's WGETRC is %q, want %q", vars["WGETRC"], want)
			}
		})
	}
}
