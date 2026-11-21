package main

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/helper"
	"github.com/sjawhar/envoy/internal/cipherclient"
)

// recordingHelper listens at a fresh socket, answers every op as enrolled, and records each
// request it got. Its register answer names this test's parent as the session's root: register
// without --exec attaches, and prints the helper's answer only when the root it names is above the
// caller (isAncestor, which reads /proc, so this file is Linux's alone, as hostmode_linux_test.go
// is).
func recordingHelper(t *testing.T) (string, func() []helper.Request) {
	t.Helper()
	sock := filepath.Join(shortTempDir(t), "h.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	var mu sync.Mutex
	var got []helper.Request
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			line, _ := bufio.NewReader(conn).ReadBytes('\n')
			var req helper.Request
			_ = json.Unmarshal(line, &req)
			mu.Lock()
			got = append(got, req)
			mu.Unlock()
			data, _ := json.Marshal(helper.Response{OK: true, State: "enrolled", RuntimeID: "rt", EnrollmentID: "e", RootPID: os.Getppid()})
			_, _ = conn.Write(append(data, '\n'))
			_ = conn.Close()
		}
	}()
	return sock, func() []helper.Request {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(got)
	}
}

// TestRegisterReadsAgentSecretsProjects: register and enroll --helper name the session's projects
// to the helper from AGENT_SECRETS_PROJECTS, sorted, and --projects overrides the variable; a list
// that is no list is refused before the helper is asked.
func TestRegisterReadsAgentSecretsProjects(t *testing.T) {
	stubLauncher(t, 0, errNoLauncher)
	sock, requests := recordingHelper(t)
	t.Setenv("AGENT_SECRETS_HELPER_SOCK", sock)
	t.Setenv(cipherclient.ProjectsVar, "PUB PRIV")
	t.Setenv("AGENT_SECRETS_KEY_DIR", t.TempDir())
	var out, errOut strings.Builder
	if code := cmdRegister(nil, &out, &errOut); code != 0 {
		t.Fatalf("register: exit %d stderr %q", code, errOut.String())
	}
	if code := cmdRegister([]string{"--projects", "CORE"}, &out, &errOut); code != 0 {
		t.Fatalf("register --projects CORE: exit %d stderr %q", code, errOut.String())
	}
	if code := cmdEnroll([]string{"--helper", "--runtime-id", "box:1", "--thumbprint", "tp"}, &out, &errOut); code != 0 {
		t.Fatalf("enroll --helper: exit %d stderr %q", code, errOut.String())
	}
	got := requests()
	if len(got) != 3 {
		t.Fatalf("the helper got %d requests, want 3: %+v", len(got), got)
	}
	for i, want := range [][]string{{"PRIV", "PUB"}, {"CORE"}, {"PRIV", "PUB"}} {
		if !slices.Equal(got[i].Projects, want) {
			t.Errorf("request %d (%s) names projects %v, want %v", i, got[i].Op, got[i].Projects, want)
		}
	}
	errOut.Reset()
	if code := cmdRegister([]string{"--projects", "core"}, &out, &errOut); code != exitUsageError || !strings.Contains(errOut.String(), "--projects") {
		t.Fatalf("register --projects core: exit %d stderr %q; want a usage error naming --projects", code, errOut.String())
	}
	t.Setenv(cipherclient.ProjectsVar, "PRIV PRIV")
	errOut.Reset()
	if code := cmdRegister(nil, &out, &errOut); code != exitUsageError || !strings.Contains(errOut.String(), cipherclient.ProjectsVar) {
		t.Fatalf("register under %s=PRIV PRIV: exit %d stderr %q; want a usage error naming the variable", cipherclient.ProjectsVar, code, errOut.String())
	}
	if n := len(requests()); n != 3 {
		t.Fatalf("a refused list reached the helper: it got %d requests, want 3", n)
	}
}
