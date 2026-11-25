package testbin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// OMPFrame is the part of an Oh My Pi RPC frame a real-binary test reads: the answer to a
// `get_state` request names the session's model under `data`, and an assistant message names the
// model that answered it under `message`. Every other member of a frame is one encoding/json
// drops.
type OMPFrame struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	ID      string `json:"id"`
	Data    struct {
		Model struct {
			ID       string `json:"id"`
			Provider string `json:"provider"`
		} `json:"model"`
	} `json:"data"`
	Message struct {
		Provider string `json:"provider"`
		Model    string `json:"model"`
	} `json:"message"`
}

// WriteOMPProfile writes the `legion` profile a real-binary test runs Oh My Pi under — models.yml
// and config.yml, the two files the operator's ConfigMap holds in a pod
// (deploy/kubernetes/operator-route) — and returns that profile's agent directory.
func WriteOMPProfile(t *testing.T, home, models, config string) string {
	t.Helper()
	agent := filepath.Join(home, ".omp", "profiles", "legion", "agent")
	if err := os.MkdirAll(agent, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{"models.yml": models, "config.yml": config} {
		if err := os.WriteFile(filepath.Join(agent, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return agent
}

// OMPEnviron is the whole environment a real-binary test's Oh My Pi runs under: the test's own
// home and profile and nothing of the machine's, so neither a PI_CONFIG_FILES an operator's pod
// exports nor any credential it carries reaches the measurement.
func OMPEnviron(home string) []string {
	return []string{"HOME=" + home, "OMP_PROFILE=legion", "PATH=/usr/local/bin:/usr/bin:/bin", "AWS_EC2_METADATA_DISABLED=true"}
}

// AnthropicAnswer writes one short streaming answer in the shape an `anthropic-messages` provider
// gives: the model it answers as, and one text block.
func AnthropicAnswer(w http.ResponseWriter, model, text string) {
	w.Header().Set("content-type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	for _, event := range []map[string]any{
		{"type": "message_start", "message": map[string]any{
			"id": "msg_1", "type": "message", "role": "assistant", "model": model, "content": []any{},
			"stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]any{"input_tokens": 1, "output_tokens": 1},
		}},
		{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}},
		{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": text}},
		{"type": "content_block_stop", "index": 0},
		{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil},
			"usage": map[string]any{"output_tokens": 1}},
		{"type": "message_stop"},
	} {
		body, err := json.Marshal(event)
		if err != nil {
			panic(err)
		}
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event["type"], body)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}
}

// StateModel is the model the answer to one `get_state` request names, as `<provider>/<id>`.
func StateModel(t *testing.T, stdout string) string {
	t.Helper()
	for _, line := range strings.Split(stdout, "\n") {
		var frame OMPFrame
		if json.Unmarshal([]byte(line), &frame) != nil || frame.Type != "response" || frame.Command != "get_state" {
			continue
		}
		return frame.Data.Model.Provider + "/" + frame.Data.Model.ID
	}
	t.Fatalf("no answer to get_state in:\n%s", stdout)
	return ""
}

// SessionFiles are the session files under a profile's sessions directory, which Oh My Pi keeps
// in a directory named for the working directory the session ran in: the regular files whose name
// ends in `.jsonl`. On Linux the pinned build also keeps, beside each session file it has written,
// a lock sidecar `.<session file>.lock.os` that it never removes (`#osGatePath` in
// packages/coding-agent/src/session/session-storage.ts at the pinned tag: the lock is held on the
// file's inode, so the file stays), and no other file of the directory is a session.
func SessionFiles(t *testing.T, sessions string) []string {
	t.Helper()
	var files []string
	if err := filepath.WalkDir(sessions, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type().IsRegular() && strings.HasSuffix(entry.Name(), ".jsonl") {
			files = append(files, path)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	slices.Sort(files)
	return files
}

// ModelChanges are the models a session file's model-change records name, in the order the
// session wrote them.
func ModelChanges(t *testing.T, sessionFile string) []string {
	t.Helper()
	contents, err := os.ReadFile(sessionFile)
	if err != nil {
		t.Fatal(err)
	}
	var models []string
	for _, line := range bytes.Split(contents, []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var record struct {
			Type  string `json:"type"`
			Model string `json:"model"`
		}
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatalf("parse session record %q: %v", line, err)
		}
		if record.Type == "model_change" {
			models = append(models, record.Model)
		}
	}
	return models
}
