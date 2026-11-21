//go:build unix

// packages/envoy/cmd/cipher/pod_projects_test.go
//
// `enroll --pod` names the pod's Dispatch projects as `register` and `enroll --helper` name a
// session's: AGENT_SECRETS_PROJECTS, or --projects over it. The broker takes the list only from a
// pod that acts for a person (a person's pod), checked against that person, and refuses it from
// any other pod (PROJECTS_NEED_A_PERSON), so the command sends exactly what the session names.
package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipherclient"
)

// projectsPodBroker is a broker that takes the token login and the enrollment of `enroll --pod`
// and records the enrollment's body whole, its projects included, which servePodBroker reads as
// strings alone.
func projectsPodBroker(t *testing.T) (*httptest.Server, func() []map[string]any) {
	t.Helper()
	var mu sync.Mutex
	var enrollments []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/launcher-credentials/token":
			_ = json.NewEncoder(w).Encode(map[string]any{"credential_id": "credential-for-the-pod", "expires_at": time.Now().Add(time.Hour)})
		case "/v1/enrollments":
			body := map[string]any{}
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Errorf("the enrollment's body is not JSON: %v", err)
			}
			mu.Lock()
			enrollments = append(enrollments, body)
			mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"enrollment_id": "enrollment-of-the-pod", "lease_expires_at": time.Now().Add(time.Hour), "slot": nil})
		default:
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{"code": "NOT_FOUND", "error": r.URL.Path})
		}
	}))
	t.Cleanup(srv.Close)
	return srv, func() []map[string]any {
		mu.Lock()
		defer mu.Unlock()
		return append([]map[string]any(nil), enrollments...)
	}
}

// TestEnrollPodNamesTheProjectsItsSessionNames: enroll --pod sends the projects
// AGENT_SECRETS_PROJECTS lists, sorted; --projects names them over the variable; and with neither
// the body carries no projects field at all, as before. A list that is no list is refused before
// the broker is asked, naming the setting and never the value.
func TestEnrollPodNamesTheProjectsItsSessionNames(t *testing.T) {
	tokenFile := writePodToken(t, fakePodToken(t, testPodAccount, testPodUID))
	for _, c := range []struct {
		name     string
		variable string
		args     []string
		want     []string
	}{
		{"the variable", "PUB PRIV", nil, []string{"PRIV", "PUB"}},
		{"--projects over the variable", "PUB PRIV", []string{"--projects", "CORE"}, []string{"CORE"}},
		{"neither", "", nil, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			broker, enrollments := projectsPodBroker(t)
			t.Setenv("AGENT_SECRETS_URL", broker.URL)
			t.Setenv(cipherclient.ProjectsVar, c.variable)
			args := append([]string{"--pod", "--service", testPodService, "--pod-token-file", tokenFile, "--out", t.TempDir()}, c.args...)
			var stdout, stderr bytes.Buffer
			if code := cmdEnroll(args, &stdout, &stderr); code != 0 {
				t.Fatalf("enroll %q under %s=%q: exit %d, stderr %q", args, cipherclient.ProjectsVar, c.variable, code, stderr.String())
			}
			got := enrollments()
			if len(got) != 1 {
				t.Fatalf("the broker took %d enrollments, want 1", len(got))
			}
			sent, present := got[0]["projects"]
			if c.want == nil {
				if present {
					t.Fatalf("the enrollment sent projects %v, want no projects field", sent)
				}
				return
			}
			var names []string
			for _, v := range asList(t, sent) {
				names = append(names, v.(string))
			}
			if !slices.Equal(names, c.want) {
				t.Fatalf("the enrollment sent projects %v, want %v", sent, c.want)
			}
		})
	}

	const secret = "hunter2pw-pod-projects-4c1a"
	for _, c := range []struct {
		name, variable, says string
		args                 []string
	}{
		{"--projects", "", "--projects", []string{"--projects", strings.ToLower(secret)}},
		{"the variable", "PRIV " + strings.ToLower(secret), cipherclient.ProjectsVar, nil},
	} {
		t.Run("a list that is no list in "+c.name, func(t *testing.T) {
			broker, enrollments := projectsPodBroker(t)
			t.Setenv("AGENT_SECRETS_URL", broker.URL)
			t.Setenv(cipherclient.ProjectsVar, c.variable)
			args := append([]string{"--pod", "--service", testPodService, "--pod-token-file", tokenFile, "--out", t.TempDir()}, c.args...)
			var stdout, stderr bytes.Buffer
			if code := cmdEnroll(args, &stdout, &stderr); code != exitUsageError || !strings.Contains(stderr.String(), c.says) || strings.Contains(stderr.String(), secret) {
				t.Fatalf("enroll %q: exit %d, stderr %q; want a usage error naming %s and not the value", args, code, stderr.String(), c.says)
			}
			if n := len(enrollments()); n != 0 {
				t.Fatalf("a list that is no list reached the broker: %d enrollments", n)
			}
		})
	}
}

// TestEnrollPodNamesTheSettingARefusedListCameFrom: the broker refuses a list from any pod but a
// person's 400 PROJECTS_NEED_A_PERSON, in words of its own that do not say where the command read
// the list. So the line enroll --pod prints for that refusal names the setting: the variable when
// no --projects was given, saying to unset it for a service's pod, and --projects over the
// variable, saying to enrol without it. Each row names its own setting and not the other's, and
// the line never prints the list.
func TestEnrollPodNamesTheSettingARefusedListCameFrom(t *testing.T) {
	tokenFile := writePodToken(t, fakePodToken(t, testPodAccount, testPodUID))
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/launcher-credentials/token":
			_ = json.NewEncoder(w).Encode(map[string]any{"credential_id": "credential-for-the-pod", "expires_at": time.Now().Add(time.Hour)})
		case "/v1/enrollments":
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"code": "PROJECTS_NEED_A_PERSON", "error": "only a session or a pod that acts for a person names projects"})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(broker.Close)
	const variableList, flagList = "QZV7K3", "QZW8M4"
	for _, c := range []struct {
		name    string
		args    []string
		says    []string
		notSays string
	}{
		{"the variable", nil, []string{"came from " + cipherclient.ProjectsVar, "unset it"}, "--projects"},
		{"--projects over the variable", []string{"--projects", flagList}, []string{"came from --projects", "without --projects"}, cipherclient.ProjectsVar},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("AGENT_SECRETS_URL", broker.URL)
			t.Setenv(cipherclient.ProjectsVar, variableList)
			args := append([]string{"--pod", "--service", testPodService, "--pod-token-file", tokenFile, "--out", t.TempDir()}, c.args...)
			var stdout, stderr bytes.Buffer
			code := cmdEnroll(args, &stdout, &stderr)
			line := stderr.String()
			if code != 1 || !strings.Contains(line, "PROJECTS_NEED_A_PERSON") || strings.Contains(line, c.notSays) || strings.Contains(line, variableList) || strings.Contains(line, flagList) {
				t.Fatalf("enroll %q refused PROJECTS_NEED_A_PERSON: exit %d, stderr %q; want exit 1 naming the code, not %s, and no list", args, code, line, c.notSays)
			}
			for _, want := range c.says {
				if !strings.Contains(line, want) {
					t.Errorf("enroll %q refused PROJECTS_NEED_A_PERSON: stderr %q; want it to say %q", args, line, want)
				}
			}
		})
	}
}

// asList is v as a JSON array, failing t when it is none.
func asList(t *testing.T, v any) []any {
	t.Helper()
	list, ok := v.([]any)
	if !ok {
		t.Fatalf("projects = %#v, want a JSON array", v)
	}
	return list
}
