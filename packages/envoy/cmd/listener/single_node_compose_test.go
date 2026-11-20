package main

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The single-node compose file: read is the path from this package's directory, path names the
// same file from the repository root for a failure read in CI.
var (
	singleNodeComposeRead = filepath.Join("..", "..", "deploy", "compose", "single-node.compose.yml")
	singleNodeComposePath = "packages/envoy/deploy/compose/single-node.compose.yml"
)

// composeService is the part of a Compose service this check reads.
type composeService struct {
	Command   []string `yaml:"command"`
	DependsOn map[string]struct {
		Condition string `yaml:"condition"`
	} `yaml:"depends_on"`
	Environment map[string]string `yaml:"environment"`
	Healthcheck struct {
		Test []string `yaml:"test"`
	} `yaml:"healthcheck"`
}

// TestTheSingleNodeComposeFileMakesItsListenersBucketsBeforeItStarts holds the single-node compose
// file to the start this listener makes. No listener creates a bucket it copies, and a start that
// finds one missing waits with every route answering 503 (awaitCopiedBuckets), so on a fresh
// nats-data volume the file's listener would never serve unless something made the buckets
// first. The file does that with a one-shot `envoy-infra buckets create` service the listener
// depends on with service_completed_successfully; this check holds that one-shot to the buckets
// this listener copies (listenerCopiedBuckets, with the GitHub webhook route exactly when the
// file's listener mounts it), to the NATS server the listener connects to, and the listener's
// health check to a curl that fails on the 503 "waiting" a held start answers, so `up --wait`
// fails rather than passes a listener that serves nothing.
func TestTheSingleNodeComposeFileMakesItsListenersBucketsBeforeItStarts(t *testing.T) {
	raw, err := os.ReadFile(singleNodeComposeRead)
	if err != nil {
		t.Fatalf("read %s: %v", singleNodeComposePath, err)
	}
	var file struct {
		Services map[string]composeService `yaml:"services"`
	}
	if err := yaml.Unmarshal(raw, &file); err != nil {
		t.Fatalf("parse %s: %v", singleNodeComposePath, err)
	}
	listener, ok := file.Services["listener"]
	if !ok {
		t.Fatalf("%s has no listener service; services: %v", singleNodeComposePath, slices.Sorted(maps.Keys(file.Services)))
	}

	var makers []string
	for name, dependency := range listener.DependsOn {
		if dependency.Condition == "service_completed_successfully" {
			makers = append(makers, name)
		}
	}
	if len(makers) != 1 {
		t.Fatalf("%s: the listener depends with service_completed_successfully on %v, want exactly one service, the one-shot that makes the buckets it copies: a start on a fresh volume finds them missing and waits for ever, every route answering 503", singleNodeComposePath, makers)
	}
	maker := file.Services[makers[0]]
	if len(maker.Command) < 2 || maker.Command[0] != "buckets" || maker.Command[1] != "create" {
		t.Fatalf("%s: the listener's one-shot %q runs %q, want `buckets create ...` (envoy-infra buckets create)", singleNodeComposePath, makers[0], maker.Command)
	}
	var made []string
	nats := ""
	for _, arg := range maker.Command[2:] {
		if bucket, ok := strings.CutPrefix(arg, "--bucket="); ok {
			made = append(made, bucket)
		}
		if url, ok := strings.CutPrefix(arg, "--nats="); ok {
			nats = url
		}
	}
	slices.Sort(made)
	github := slices.Contains(strings.Split(listener.Environment["ENVOY_WEBHOOKS"], ","), "github")
	want := slices.Sorted(slices.Values(listenerCopiedBuckets(github)))
	if !slices.Equal(made, want) {
		t.Errorf("%s: the one-shot %q creates %v, want %v, the buckets the file's listener copies (listenerCopiedBuckets(%t)): a bucket left out holds the start, and one more puts a writer the listener does not run into the proof", singleNodeComposePath, makers[0], made, want, github)
	}
	if nats == "" || nats != listener.Environment["NATS_URLS"] {
		t.Errorf("%s: the one-shot %q makes the buckets on --nats=%q, want the listener's NATS_URLS %q", singleNodeComposePath, makers[0], nats, listener.Environment["NATS_URLS"])
	}

	check := listener.Healthcheck.Test
	failsOn503 := slices.ContainsFunc(check, func(arg string) bool {
		return strings.HasPrefix(arg, "-") && !strings.HasPrefix(arg, "--") && strings.Contains(arg, "f")
	})
	if !slices.Contains(check, "curl") || !failsOn503 || !strings.HasSuffix(check[len(check)-1], "/healthz") {
		t.Errorf("%s: the listener's health check is %q, want curl -f against /healthz: a start held for a missing bucket answers 503 \"waiting\", which only a curl that fails on a 5xx reads as unhealthy", singleNodeComposePath, check)
	}
}
