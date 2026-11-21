package main

import (
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipherclient"
)

// TestTheRegisterAndEnrollSynopsesNameProjects: the usage line of register, and of each half of
// enroll, names --projects, the option that names a session's Dispatch projects. A person's pod
// names its projects as the person's session does, so enroll's --pod half names it too.
func TestTheRegisterAndEnrollSynopsesNameProjects(t *testing.T) {
	if synopsis := lookupCommand("register").synopsis; !strings.Contains(synopsis, "[--projects ") {
		t.Errorf("register's synopsis %q names no --projects", synopsis)
	}
	synopsis := lookupCommand("enroll").synopsis
	helperHalf, podHalf, found := strings.Cut(synopsis, " | ")
	if !found || !strings.Contains(helperHalf, "[--projects ") || !strings.Contains(podHalf, "[--projects ") {
		t.Errorf("enroll's synopsis %q: want --projects in its --helper half and in its --pod half", synopsis)
	}
}

// TestADenialForAnUnnamedProjectSaysHowToNameIt: a request the broker denies project-not-named
// ends with the line that says to start the session with AGENT_SECRETS_PROJECTS naming the
// project, in the request form and the exec form; the line names no project, and a denial for
// any other reason adds nothing.
func TestADenialForAnUnnamedProjectSaysHowToNameIt(t *testing.T) {
	binary := buildAgentSecrets(t)
	broker, _ := fakeBroker(t)
	defer broker.Close()
	keyDir := newKeyDir(t)
	for _, args := range [][]string{
		{"request", "PROJECT_DENIED"},
		{"PROJECT_DENIED", "--", "sh", "-c", "echo ran-the-child"},
	} {
		stdout, stderr, exit := runAgentSecrets(t, binary, broker.URL, keyDir, nil, args...)
		if exit != exitDenied || !strings.Contains(stderr, projectNotNamedHint+"\n") || strings.Contains(stdout, "ran-the-child") {
			t.Errorf("%q = exit %d stdout %q stderr %q; want exit %d and the line %q", args, exit, stdout, stderr, exitDenied, projectNotNamedHint)
		}
	}
	for _, args := range [][]string{{"request", "DETAIL_DENIED"}, {"request", "DENY_ME"}} {
		_, stderr, _ := runAgentSecrets(t, binary, broker.URL, keyDir, nil, args...)
		if strings.Contains(stderr, cipherclient.ProjectsVar) {
			t.Errorf("%q printed the project line for a denial of another reason: %q", args, stderr)
		}
	}
}
