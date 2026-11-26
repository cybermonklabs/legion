package docs

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	gws "github.com/gorilla/websocket"
	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/testcleanup"
)

// A document socket's 101 carries the Dispatch-Private-Projects value the process's middleware set
// on the writer, beside the process header. ygo's upgrade writes the 101 on the hijacked
// connection and reads no header of the writer's, so the value reaches it only through the
// connection the service's Hijack hands out (upgradeHeadersWriter).
func TestADocumentSocketsUpgradeSaysWhetherAnyProjectIsPrivate(t *testing.T) {
	t.Parallel()
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "before")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(access.PrivateProjectsHeader, "1")
		service.ServeHTTP(w, r)
	}))
	testcleanup.Add(t, server.Close)
	address := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws/doc/" + artifactID + "?schema_version=" + strconv.Itoa(pmdoc.SchemaVersion())
	connection, answer, err := gws.DefaultDialer.Dial(address, http.Header{"X-Dispatch-User": {"alice"}})
	if err != nil {
		t.Fatalf("open the document socket: %v (%v)", err, answer)
	}
	testcleanup.Add(t, func() { _ = connection.Close() })
	if got := answer.Header.Values(access.PrivateProjectsHeader); len(got) != 1 || got[0] != "1" {
		t.Fatalf("the document socket's upgrade answered %d with %s %q, want it once as 1; header %v", answer.StatusCode, access.PrivateProjectsHeader, got, answer.Header)
	}
	if service.hostname != "" && answer.Header.Get(ProcessHeader) != service.hostname {
		t.Fatalf("the upgrade lost its process header: %v", answer.Header)
	}
}
