package api

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// A caller above this daemon's own contract is served the body this contract writes, on both
// sides, and the daemon says so in one warning naming the caller's contract, its own and the
// caller's address (servedContracts' `highest > DaemonAPIVersion`): a plugin newer than its daemon
// is a rollout out of order, and that line is what reports it. A pod whose newest process is above
// warns the same, since highest is what the comparison reads. A caller at this daemon's own
// contract is the edge and writes nothing, so the line is for a number above DaemonAPIVersion and
// not for DaemonAPIVersion itself.
func TestACallerAboveTheDaemonsOwnContractIsLogged(t *testing.T) {
	const caller = "192.0.2.1:41000"
	for _, tc := range []struct {
		name            string
		lowest, highest int
		warned          bool
	}{
		{name: "a caller above this daemon's own", lowest: DaemonAPIVersion() + 1, highest: DaemonAPIVersion() + 1, warned: true},
		{name: "a pod whose newest process is above this daemon's own", lowest: DaemonAPIVersion(), highest: DaemonAPIVersion() + 1, warned: true},
		{name: "a caller at this daemon's own", lowest: DaemonAPIVersion(), highest: DaemonAPIVersion()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			s := &server{log: slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))}
			request := httptest.NewRequest(http.MethodGet, "/legion/v1/state", nil)
			request.RemoteAddr = caller

			restore, strip := s.servedContracts(request, tc.lowest, tc.highest, "")

			if restore != DaemonAPIVersion() || strip != DaemonAPIVersion() {
				t.Errorf("a caller at %d to %d is served %d and %d, want this daemon's own %d on both sides", tc.lowest, tc.highest, restore, strip, DaemonAPIVersion())
			}
			var lines []map[string]any
			for line := range strings.Lines(logs.String()) {
				var entry map[string]any
				if err := json.Unmarshal([]byte(line), &entry); err != nil {
					t.Fatalf("decode the log line %q: %v", line, err)
				}
				lines = append(lines, entry)
			}
			if !tc.warned {
				if len(lines) != 0 {
					t.Errorf("a caller at this daemon's own contract logged %s, want no line: it is the edge, not above it", logs.String())
				}
				return
			}
			if len(lines) != 1 {
				t.Fatalf("logs %s carry %d lines, want the one warning a caller above this contract gets", logs.String(), len(lines))
			}
			want := map[string]any{
				"level": "WARN", "msg": "api: serving a caller above this daemon's own contract",
				"pluginContract": float64(tc.highest), "daemonApiVersion": float64(DaemonAPIVersion()), "caller": caller,
			}
			for key, value := range want {
				if lines[0][key] != value {
					t.Errorf("the warning's %s is %v, want %v; line %s", key, lines[0][key], value, logs.String())
				}
			}
		})
	}
}

// A number below one is no contract, in each place the daemon reads one. A caller whose header
// states 0 or a negative number states none (statedContract's `contract <= 0`), so it is placed by
// its pod, as a caller that states nothing is. A claim whose record holds 0 records none
// (recordsContract's `c.PluginContract != 0`), so a pod of such claims reports no contract and its
// callers are served this daemon's own body. The image probe's settings refuse a contract below one
// before anything is created (the sandbox probe's `p.Contract < 1`), which
// internal/runtime/sandbox's test of this name drives. One, the lowest number a contract can be,
// is a contract in each, which is the control: it shows where the line between the two is.
func TestANumberBelowOneIsNoContract(t *testing.T) {
	s := &server{log: slog.New(slog.NewTextHandler(io.Discard, nil)), callerPod: fixturePodOf}
	stating := func(header string) *http.Request {
		request := httptest.NewRequest(http.MethodGet, "/legion/v1/state", nil)
		request.RemoteAddr = statePodAddr + ":41000"
		if header != "" {
			request.Header.Set(pluginContractHeader, header)
		}
		return request
	}
	pod := podClaims(ServingFloor)
	byPod := s.contractsFor(stating(""), pod)
	if byPod.pod != statePodUID {
		t.Fatalf("a caller that states no contract is placed at %+v, want its pod %s: this proves nothing", byPod, statePodUID)
	}

	t.Run("a stated contract", func(t *testing.T) {
		for _, header := range []string{"0", "-1", "-46"} {
			if contract, stated := statedContract(stating(header)); stated || contract != 0 {
				t.Errorf("a header of %s states contract %d (%v), want none", header, contract, stated)
			}
			if placed := s.contractsFor(stating(header), pod); placed != byPod {
				t.Errorf("a caller stating %s is placed at %+v, want %+v, where its pod places a caller that states nothing", header, placed, byPod)
			}
		}
		if contract, stated := statedContract(stating("1")); !stated || contract != 1 {
			t.Errorf("a header of 1 states contract %d (%v), want 1", contract, stated)
		}
		if placed := s.contractsFor(stating("1"), pod); placed.pod != "" || placed.contract != 1 {
			t.Errorf("a caller stating 1 is placed at %+v, want by its header at 1 and by no pod", placed)
		}
	})

	t.Run("a recorded contract", func(t *testing.T) {
		none := []supervise.Claim{
			claimInPod("legion-legion-legion-208-planner", statePodUID, 0),
			claimInPod("legion-legion-legion-208-implementer", statePodUID, 0),
		}
		for _, c := range none {
			if recordsContract(c) {
				t.Errorf("a claim recording 0 records a contract: %+v", c)
			}
		}
		if lowest, highest, recorded := podContracts(none, statePodUID); lowest != 0 || highest != 0 || recorded != "" {
			t.Errorf("a pod whose claims record 0 reports %d, %d and %q, want nothing on all three", lowest, highest, recorded)
		}
		if placed := s.contractsFor(stating(""), none); placed != ownBody() {
			t.Errorf("the caller of a pod whose claims record 0 is placed at %+v, want this daemon's own body %+v", placed, ownBody())
		}
		one := claimInPod("legion-legion-legion-208-planner", statePodUID, 1)
		if !recordsContract(one) {
			t.Errorf("a claim recording 1 records no contract: %+v", one)
		}
		if lowest, highest, recorded := podContracts([]supervise.Claim{one}, statePodUID); lowest != 1 || highest != 1 || recorded != one.Token {
			t.Errorf("a pod whose claim records 1 reports %d, %d and %q, want 1, 1 and %s", lowest, highest, recorded, one.Token)
		}
	})
}
