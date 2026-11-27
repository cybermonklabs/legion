// packages/envoy/cmd/cipher/proxy_trust_store_vars_test.go
//
// The decision to write no trust bundle rests on the machine's system trust store. x509.SystemCertPool
// reads that store through SSL_CERT_FILE and SSL_CERT_DIR where the command's environment sets
// them, but git's HTTPS transport (Debian's libcurl built on GnuTLS) and wget (GnuTLS) read
// neither variable: they read the system bundle alone, and GIT_SSL_CAINFO and the wgetrc that
// WGETRC names. So a command whose own SSL_CERT_FILE or SSL_CERT_DIR names the proxy's root, on a
// machine whose system store lacks it, still needs the bundle. Without it, git and wget refuse the
// proxy's certificate where, with the bundle, GIT_SSL_CAINFO and WGETRC carried the root to them.
package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// storeVarsChildInput is what TestProxyTrustStoreVarsChild reads: the environment the command has,
// which is also the child process's own environment, as os.Environ is in cmdExec.
type storeVarsChildInput struct {
	Environ       []string `json:"environ"`
	CACertificate string   `json:"ca_certificate"`
}

type storeVarsChildOutput struct {
	Bundle string            `json:"bundle"`
	Vars   map[string]string `json:"vars"`
	Error  string            `json:"error"`
}

// TestProxyTrustStoreVarsChild is the child process of runStoreVarsChild, skipped in every other
// run: x509.SystemCertPool reads SSL_CERT_FILE and SSL_CERT_DIR once per process.
func TestProxyTrustStoreVarsChild(t *testing.T) {
	inputPath := os.Getenv("AGENT_SECRETS_STORE_VARS_CHILD_INPUT")
	if inputPath == "" {
		t.Skip("subprocess of runStoreVarsChild")
	}
	data, err := os.ReadFile(inputPath)
	if err != nil {
		t.Fatalf("read the input: %v", err)
	}
	var in storeVarsChildInput
	if err := json.Unmarshal(data, &in); err != nil {
		t.Fatalf("parse the input: %v", err)
	}
	release := testRelease("AIRLOCK_ME", []string{"api.example.com"}, nil)
	release.CACertificate = in.CACertificate
	var out storeVarsChildOutput
	files, err := prepareAirlock(in.Environ, release, "grant-one", nil)
	if err == nil {
		out.Bundle = files.bundle
		out.Vars, err = airlockVars(in.Environ, release, files, releaseNames(nil, release))
	}
	if err != nil {
		out.Error = err.Error()
	}
	answer, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("encode the output: %v", err)
	}
	if err := os.WriteFile(os.Getenv("AGENT_SECRETS_STORE_VARS_CHILD_OUTPUT"), answer, 0o600); err != nil {
		t.Fatalf("write the output: %v", err)
	}
}

// runStoreVarsChild runs TestProxyTrustStoreVarsChild with environ as both the command's
// environment and the process's own, on this machine's real system store: this process's
// SSL_CERT_FILE and SSL_CERT_DIR are not passed on, so only environ's can name a store.
func runStoreVarsChild(t *testing.T, caPEM string, environ []string) storeVarsChildOutput {
	t.Helper()
	scratch := t.TempDir()
	input, err := json.Marshal(storeVarsChildInput{Environ: environ, CACertificate: caPEM})
	if err != nil {
		t.Fatal(err)
	}
	inputPath, outputPath := filepath.Join(scratch, "input.json"), filepath.Join(scratch, "output.json")
	if err := os.WriteFile(inputPath, input, 0o600); err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var env []string
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "SSL_CERT_FILE=") && !strings.HasPrefix(e, "SSL_CERT_DIR=") {
			env = append(env, e)
		}
	}
	cmd := exec.Command(self, "-test.run", "^TestProxyTrustStoreVarsChild$", "-test.count", "1")
	cmd.Env = append(append(env, environ...),
		"AGENT_SECRETS_STORE_VARS_CHILD_INPUT="+inputPath,
		"AGENT_SECRETS_STORE_VARS_CHILD_OUTPUT="+outputPath)
	if combined, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the child process: %v\n%s", err, combined)
	}
	data, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("the child process wrote no answer: %v", err)
	}
	var out storeVarsChildOutput
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("parse the child's answer %q: %v", data, err)
	}
	if out.Error != "" {
		t.Fatalf("the child process: %s", out.Error)
	}
	return out
}

// TestPrepareAirlockReadsTheStoreWithNoVariableWhereEveryClientSettingIsSet: the machine's own
// system bundle lacks a root made for this test, and the command carries the nine settings, the
// eight file settings naming one file that holds that bundle's roots plus the root, SSL_CERT_FILE
// among them. x509.SystemCertPool would read the root through SSL_CERT_FILE, but git and wget read
// the system bundle alone, so the bundle must be written, with GIT_SSL_CAINFO and WGETRC naming
// it. The command carries all nine because one missing setting gets the bundle whatever the store
// holds, which would hide a store read through SSL_CERT_FILE.
func TestPrepareAirlockReadsTheStoreWithNoVariableWhereEveryClientSettingIsSet(t *testing.T) {
	root := proxyTestRoot(t, "test proxy root for the store with every setting")
	system := ""
	for _, path := range systemTrustFiles {
		if data, err := os.ReadFile(path); err == nil {
			system = string(data)
			break
		}
	}
	if strings.Contains(system, strings.TrimSpace(root)) {
		t.Fatalf("the system bundle already holds the root made for this test")
	}
	environ := append([]string{"XDG_RUNTIME_DIR=" + t.TempDir()}, proxyStoreSettings(t, writeLoginFile(t, "system-and-root.pem", system+root))...)
	out := runStoreVarsChild(t, root, environ)
	t.Logf("the child answered: %+v", out)
	if out.Bundle == "" {
		t.Fatalf("prepareAirlock wrote no bundle, though the system bundle lacks the root and only the command's settings name it; git and wget read the system bundle alone")
	}
	for _, key := range []string{"GIT_SSL_CAINFO", "CURL_CA_BUNDLE", "SSL_CERT_FILE", "NODE_EXTRA_CA_CERTS"} {
		if out.Vars[key] != out.Bundle {
			t.Errorf("the command's %s is %q, want the bundle %q", key, out.Vars[key], out.Bundle)
		}
	}
	if out.Vars["WGETRC"] == "" {
		t.Errorf("the command's WGETRC is not set; wget reads the bundle from no other place")
	}
}
