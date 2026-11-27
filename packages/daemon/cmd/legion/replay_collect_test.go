package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// `legion replay collect` prints each ancestor on a Dispatch parent chain whose events Dispatch did
// not give, which window.json names in missingAncestors (spec R10: the record's missing input is
// reported). The window's one line names ACME-1, which stands under ACMEAUX-1, which stands under
// ACMEAUX-2; the Dispatch the command reads serves the events of ACME-1 and ACMEAUX-1, lists no
// issue of ACME, holds an empty event log, and answers that it has no ACMEAUX-2.
func TestReplayCollectPrintsEachAncestorDispatchDoesNotShow(t *testing.T) {
	filed := func(id int, key, parent string) string {
		project, _, _ := strings.Cut(key, "-")
		return fmt.Sprintf(`{"id":%d,"issue_key":%q,"artifact_id":null,"project":%q,"seq":1,"type":"issue.created",`+
			`"actor":{"kind":"user","id":"acme-person"},"notify":true,"created_at":"2001-03-05T08:30:00Z",`+
			`"payload":{"key":%q,"project":%q,"status":"todo","parent":%q,"blocked_by":[],"priority":null}}`, id, key, project, key, project, parent)
	}
	served := map[string]string{"ACME-1": filed(1, "ACME-1", "ACMEAUX-1"), "ACMEAUX-1": filed(2, "ACMEAUX-1", "ACMEAUX-2")}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		key, events := strings.CutPrefix(r.URL.Path, "/api/v1/issues/")
		key, events = strings.CutSuffix(key, "/events")
		switch {
		case r.Method != http.MethodGet:
		case r.URL.Path == "/api/v1/events/log":
			fmt.Fprint(w, "[]")
			return
		case r.URL.Path == "/api/v1/issues":
			fmt.Fprintf(w, `{"issues":[],"total":0,"limit":%s,"offset":0}`, r.URL.Query().Get("limit"))
			return
		case events && served[key] != "":
			fmt.Fprintf(w, "[%s]", served[key])
			return
		}
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"code":"ISSUE_NOT_FOUND","error":"issue not found"}`)
	}))
	t.Cleanup(server.Close)
	t.Setenv("DISPATCH_URL", server.URL)
	dir := t.TempDir()
	logs, token := filepath.Join(dir, "daemon.log"), filepath.Join(dir, "dispatch-token")
	line := `{"time":"2001-03-05T09:30:00Z","level":"INFO","msg":"admission: slot taken","issue":"ACME-1","slots":1,"cap":1}`
	if err := os.WriteFile(logs, []byte(line+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(token, []byte("a-test-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := legionRun(t, "replay", "collect", "--project", "ACME", "--from", "2001-03-05T09:00:00Z", "--to", "2001-03-05T10:00:00Z",
		"--before", "1h", "--log", logs, "--out", filepath.Join(dir, "window"), "--dispatch-token-file", token, "--collector", record.MainRule.Commit)
	want := "legion replay collect: window.json names the ancestors on a Dispatch parent chain that Dispatch does not show (missingAncestors): ACMEAUX-2\n"
	if code != 0 || !strings.Contains(stdout, want) {
		t.Errorf("legion replay collect = %d, stderr %q, stdout:\n%s\nwant 0 and the line\n%s", code, stderr, stdout, want)
	}
}

// `legion replay collect-git` refuses, as a usage error that reads and writes nothing, any two of
// its four runs of a part alone (--github-only, --git-only, --fetch-only, --merge-only); the git
// phase alone, its fetching alone and its merging alone each with a GitHub token's variable,
// GitHub's API, a reserve or a rate; both phases with no token's variable; a memory cap of no
// bytes; a --parallel below 1; and a --parallel above 1 with --github-only or --fetch-only, which
// read and merge nothing with git.
func TestReplayCollectGitRefusesThePhasesFlagsThatExcludeEachOther(t *testing.T) {
	t.Parallel()
	window := t.TempDir()
	base := []string{"replay", "collect-git", "--repo", "acme/widgets", "--branch", "main", "--out", window}
	type refusal struct {
		args   []string
		stderr string
	}
	cases := []refusal{
		{[]string{"--github-token-env", "GITHUB_TOKEN", "--github-only", "--git-only"}, "--github-only and --git-only exclude each other"},
		{[]string{"--github-token-env", "GITHUB_TOKEN", "--github-only", "--fetch-only"}, "--github-only and --fetch-only exclude each other"},
		{[]string{"--github-token-env", "GITHUB_TOKEN", "--merge-only", "--github-only"}, "--github-only and --merge-only exclude each other"},
		{[]string{"--git-only", "--fetch-only"}, "--git-only and --fetch-only exclude each other"},
		{[]string{"--merge-only", "--git-only"}, "--git-only and --merge-only exclude each other"},
		{[]string{"--fetch-only", "--merge-only"}, "--fetch-only and --merge-only exclude each other"},
		{nil, "--github-token-env is required but with --git-only, --fetch-only or --merge-only"},
		{[]string{"--git-only", "--memory-cap", "0"}, "--memory-cap 0"},
		{[]string{"--git-only", "--parallel", "0"}, "--parallel 0: at least one git command runs at a time"},
		{[]string{"--github-token-env", "GITHUB_TOKEN", "--github-only", "--parallel", "2"},
			"--github-only runs none of the git phase's reads and merges, so it takes no --parallel above 1"},
		{[]string{"--fetch-only", "--parallel", "2"}, "--fetch-only runs none of the git phase's reads and merges, so it takes no --parallel above 1"},
	}
	for _, run := range []string{"--git-only", "--fetch-only", "--merge-only"} {
		for _, share := range [][]string{{"--github-token-env", "GITHUB_TOKEN"}, {"--github-api", "https://api.github.com"}, {"--github-reserve", "10"},
			{"--github-rate", "1"}} {
			cases = append(cases, refusal{append([]string{run}, share...), run + " reads no GitHub API"})
		}
	}
	for _, tc := range cases {
		args := append(slices.Clone(base), tc.args...)
		code, stdout, stderr := legionRun(t, args...)
		if code != 2 || stdout != "" || !strings.Contains(stderr, tc.stderr) {
			t.Errorf("legion %s = %d, stdout %q, stderr %q; want 2 and stderr holding %q", strings.Join(args, " "), code, stdout, stderr, tc.stderr)
		}
	}
	if entries, err := os.ReadDir(window); err != nil || len(entries) != 0 {
		t.Errorf("the refused runs left %v in the window (%v), want nothing", entries, err)
	}
}

// `legion replay collect-git --help` names its four runs of a part alone on its usage line, which
// exclude each other, and says where each half of the git phase runs: the fetching where the git
// credential is, the clone's directory copied, and the merging where none is; and that the file
// does not depend on --parallel, though git's memory grows with it.
func TestReplayCollectGitHelpNamesTheFourRunsAndTheTwoMachines(t *testing.T) {
	t.Parallel()
	code, stdout, stderr := legionRun(t, "replay", "collect-git", "--help")
	if code != 0 || stderr != "" {
		t.Fatalf("legion replay collect-git --help = %d, stderr %q", code, stderr)
	}
	for _, want := range []string{"[--github-only | --git-only | --fetch-only | --merge-only]", "--fetch-only", "--merge-only",
		"copy the clone's directory", "do not depend on --parallel", "memory grows with --parallel"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("legion replay collect-git --help prints\n%s\nwant it to hold %q", stdout, want)
		}
	}
}

// Each command that reads GitHub refuses, as a usage error that reads and writes nothing, a share
// of the quota no reader takes: a reserve below zero, a rate below zero, and a rate slower than one
// request an hour.
func TestReplayGitHubCommandsRefuseAShareOfTheQuotaNoReaderTakes(t *testing.T) {
	t.Parallel()
	window := t.TempDir()
	for _, command := range [][]string{
		{"replay", "collect-merges", "--repo", "acme/widgets", "--branch", "main", "--from", "2001-03-05T09:00:00Z", "--to", "2001-03-05T10:00:00Z",
			"--github-token-env", "GITHUB_TOKEN", "--out", window},
		{"replay", "collect-queue", "--repo", "acme/widgets", "--github-token-env", "GITHUB_TOKEN", "--out", window},
		{"replay", "collect-git", "--repo", "acme/widgets", "--branch", "main", "--github-token-env", "GITHUB_TOKEN", "--out", window},
	} {
		for _, share := range [][]string{{"--github-reserve", "-1"}, {"--github-rate", "-0.5"}, {"--github-rate", "0.0001"}} {
			args := append(slices.Clone(command), share...)
			code, stdout, stderr := legionRun(t, args...)
			if code != 2 || stdout != "" || !strings.Contains(stderr, "--github-reserve") {
				t.Errorf("legion %s = %d, stdout %q, stderr %q; want 2 and stderr naming the share refused", strings.Join(args, " "), code, stdout, stderr)
			}
		}
	}
	if entries, err := os.ReadDir(window); err != nil || len(entries) != 0 {
		t.Errorf("the refused runs left %v in the window (%v), want nothing", entries, err)
	}
}
