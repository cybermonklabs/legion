package contracts

//ci:reads packages/envoy/scripts/fixtures/github/* TestGithubEnvelopeGoldens
//ci:reads packages/contracts/fixtures/github-envelopes/** TestGithubEnvelopeGoldens os.ReadFile each golden, TestGithubEnvelopeGoldens filepath.WalkDir the directory at every depth

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

var updateGoldens = flag.Bool("update", false, "update golden files")

type githubEnvelopeGolden struct {
	Topic          string `json:"topic"`
	PayloadSummary string `json:"payload_summary"`
	Payload        any    `json:"payload"`
}

func TestGithubEnvelopeGoldens(t *testing.T) {
	fixtures := []struct {
		name  string
		event string
	}{
		{name: "pull-request-opened", event: "pull_request"},
		// A dot in a repository's name is one subject segment's `_`, so the name stays one segment.
		{name: "pull-request-opened-dotted-repository", event: "pull_request"},
		{name: "pull-request-synchronize", event: "pull_request"},
		{name: "pull-request-closed-merged", event: "pull_request"},
		{name: "issue-comment-created", event: "issue_comment"},
		{name: "issue-comment-pr-created", event: "issue_comment"},
		{name: "issue-comment-pr-legion-footer", event: "issue_comment"},
		{name: "pull-request-review-submitted", event: "pull_request_review"},
		{name: "pull-request-review-comment-created", event: "pull_request_review_comment"},
		{name: "push", event: "push"},
		// A finished Actions job on the default branch, of a run with no pull request, on its run's
		// job topic.
		{name: "check-run-actions-job-without-pr", event: "check_run"},
		// A completed run of a workflow on the default branch, of a run with no pull request (a real
		// sjawhar/legion release.yaml push run on main), on its workflow's topic: the Legion daemon
		// wakes a production check from it when the project names the workflow a deploy.
		{name: "workflow-run-completed", event: "workflow_run"},
		// A sub-issue added to an issue, on its parent's topic.
		{name: "sub-issues-added", event: "sub_issues"},
	}

	for _, fixture := range fixtures {
		t.Run(fixture.name, func(t *testing.T) {
			rawPath := filepath.Join("..", "..", "scripts", "fixtures", "github", fixture.name+".json")
			raw, err := os.ReadFile(rawPath)
			if err != nil {
				t.Fatalf("read raw fixture: %v", err)
			}

			body := map[string]any{}
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Fatalf("decode raw fixture: %v", err)
			}
			envelope := GithubEnvelope(GithubEnvelopeInput{
				Event:    fixture.event,
				Delivery: "golden-" + fixture.name,
				Body:     body,
			})
			payload := any(nil)
			if err := json.Unmarshal([]byte(envelope.Payload), &payload); err != nil {
				t.Fatalf("decode normalized payload: %v", err)
			}
			got := githubEnvelopeGolden{
				Topic:          envelope.Topic,
				PayloadSummary: envelope.PayloadSummary,
				Payload:        payload,
			}

			// The golden is json.MarshalIndent's output and a newline, byte for byte: biome.json's
			// formatter excludes the directory, as it does the other generated JSON, and the bytes
			// check below holds the file to what -update writes, so neither a formatter nor a hand
			// edit moves it.
			goldenPath := filepath.Join("..", "..", "..", "contracts", "fixtures", "github-envelopes", fixture.name+".json")
			encoded, err := json.MarshalIndent(got, "", "  ")
			if err != nil {
				t.Fatalf("encode golden: %v", err)
			}
			encoded = append(encoded, '\n')
			if *updateGoldens {
				if err := os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
					t.Fatalf("create golden directory: %v", err)
				}
				if err := os.WriteFile(goldenPath, encoded, 0o644); err != nil {
					t.Fatalf("write golden: %v", err)
				}
			}

			goldenBytes, err := os.ReadFile(goldenPath)
			if err != nil {
				t.Fatalf("read golden: %v", err)
			}
			want := githubEnvelopeGolden{}
			if err := json.Unmarshal(goldenBytes, &want); err != nil {
				t.Fatalf("decode golden: %v", err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("normalized envelope mismatch\ngot:  %#v\nwant: %#v", got, want)
			}
			if string(goldenBytes) != string(encoded) {
				t.Fatalf("%s holds the envelope but not the bytes -update writes (json.MarshalIndent and a newline); write it again with `go test -run TestGithubEnvelopeGoldens ./internal/contracts/ -update`", goldenPath)
			}
		})
	}

	// The directory holds the goldens this list writes and nothing else, at any depth and of any
	// name or kind. biome.json's formatter excludes the whole directory, and two other tests read
	// its .json files as goldens (packages/daemon/internal/intake/intake_test.go and normalize_test.go
	// here), so anything this test does not write would sit there with no formatter and no check.
	written := map[string]bool{}
	for _, fixture := range fixtures {
		written[fixture.name+".json"] = true
	}
	goldenDir := filepath.Join("..", "..", "..", "contracts", "fixtures", "github-envelopes")
	listed := 0
	err := filepath.WalkDir(goldenDir, func(path string, _ os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == goldenDir {
			return nil
		}
		listed++
		name, err := filepath.Rel(goldenDir, path)
		if err != nil {
			return err
		}
		if name = filepath.ToSlash(name); !written[name] {
			t.Errorf("%s holds %s, which this test's list does not write: add it to the list, or remove it", goldenDir, name)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", goldenDir, err)
	}
	if listed == 0 {
		t.Fatalf("%s holds no entry, so this check read nothing", goldenDir)
	}
}
