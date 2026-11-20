package mcpbridge

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cmdtest"
)

func buildMockBinary(t *testing.T) string {
	t.Helper()
	return cmdtest.BuildCommand(t, cmdtest.Command{Name: "mock_mcp_server", Target: writeMockServer(t.TempDir())})
}

func TestManagedServer_StartAndStop(t *testing.T) {
	bin := buildMockBinary(t)

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	os.WriteFile(cfgPath, []byte(mockServerConfig(bin)), 0644)

	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	// The server calls onNotify on a goroutine of its own, so the URI comes back over a channel.
	notified := make(chan string, 1)
	s := NewManagedServer(cfg.Servers[0], func(uri string) {
		select {
		case notified <- uri:
		default:
		}
	})

	if err := s.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer s.Stop()

	if s.State() != StateReady {
		t.Fatalf("expected ready state, got %d", s.State())
	}

	// The mock server sends a notification after subscribe.
	select {
	case uri := <-notified:
		if uri != "whatsapp://messages/15551234567/5551234567@s.whatsapp.net" {
			t.Fatalf("unexpected notified URI: %s", uri)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("expected a notification within 5s")
	}
}

func TestManagedServer_ReadResource(t *testing.T) {
	bin := buildMockBinary(t)

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	os.WriteFile(cfgPath, []byte(mockServerConfig(bin)), 0644)

	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	s := NewManagedServer(cfg.Servers[0], nil)
	if err := s.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer s.Stop()

	contents, err := s.ReadResource("whatsapp://messages/15551234567/5551234567@s.whatsapp.net")
	if err != nil {
		t.Fatalf("ReadResource: %v", err)
	}
	if len(contents) != 1 {
		t.Fatalf("expected 1 content, got %d", len(contents))
	}
	if contents[0].Text != "Hello from mock" {
		t.Fatalf("unexpected text: %s", contents[0].Text)
	}
}

func TestManagedServer_StopSetsStateDead(t *testing.T) {
	bin := buildMockBinary(t)

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	os.WriteFile(cfgPath, []byte(mockServerConfig(bin)), 0644)

	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	s := NewManagedServer(cfg.Servers[0], nil)
	if err := s.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	s.Stop()

	if s.State() != StateDead {
		t.Fatalf("expected dead state after stop, got %d", s.State())
	}
}

// TestManagedServer_AStartAfterAStopRunsNoProcess: a Stop that comes before Start leaves Start
// nothing to start. The bridge registers a server before it starts it (Bridge.startServer), so a
// bridge's stop that comes between the two stops a server that has no process yet, and Start must
// then answer errStopped and run none. Without that refusal Start would run the server after the
// stop: its handshake's failure would kill it again, but only in a process that is still there to
// run that failure, and envoy-mcp returns as soon as its stop of the bridge does.
func TestManagedServer_AStartAfterAStopRunsNoProcess(t *testing.T) {
	s := NewManagedServer(ServerConfig{Name: "stopped", Command: []string{"sleep", "275"}}, nil)
	s.Stop()
	if err := s.Start(); !errors.Is(err, errStopped) {
		t.Errorf("Start after Stop = %v; want errStopped", err)
	}
	if s.cmd != nil {
		t.Errorf("Start after Stop ran the server's command (pid %d); want no process", s.cmd.Process.Pid)
	}
}
