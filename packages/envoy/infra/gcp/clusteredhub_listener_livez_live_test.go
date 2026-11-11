//go:build natslive

package gcp_test

import (
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/testnats"
)

// The other half of decision 15 (dispatch://INFRA-2739/artifact/open-items-md): a probe path has
// to be a path the listener's own binary serves. The listener mounts /metrics, /healthz, each
// webhook and /v1 and nothing else (packages/envoy/cmd/listener/main.go:957-977), so GET /livez
// answers 404 today. A startup or liveness probe of this pod is curl -sS with no -f, which exits
// 0 on a 404, so the pod would stay up on a probe that cannot report anything at all. The answer
// is counted, so a read that made no request fails here rather than passing on an empty set.
func TestClusteredNatsHubListenerAnswersLivez(t *testing.T) {
	binary := buildLiveListener(t)
	ports := newLoopbackPorts()
	_, natsURL := testnats.Start(t)
	process, port := startLiveListener(t, "the listener", binary, natsURL, ports)

	answers := 0
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	status, body := liveListenerAnswer(t, addr, "/livez", clusteredHubListenerLiveHost)
	answers++

	require.Equal(t, 1, answers, "one answer was read from /livez")
	require.Less(t, status, http.StatusBadRequest,
		"GET /livez answered %d: %s\n%s", status, strings.TrimSpace(body), process.Output.String())
	t.Logf("%d answer(s) read; GET /livez at %s: %d %s", answers, addr, status, strings.TrimSpace(body))
}
