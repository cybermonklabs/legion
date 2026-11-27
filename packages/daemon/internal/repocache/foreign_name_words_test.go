package repocache

import (
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
	"github.com/sjawhar/legion/daemon/internal/repocache/fakegcs"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// TestAForeignObjectNamePrintsNoBearerItEchoes: whatever answers in Cloud Storage's place answers
// the cache's list with an object under the repository's prefix whose name quotes the bearer the
// list carried, a name the cache does not write. The cache's log line of it (listBucket) holds no
// run of the bearer: it counts such objects and names none. The twin shows the list hands the name
// back as answered. Captures repocache.foreign-name.
func TestAForeignObjectNamePrintsNoBearerItEchoes(t *testing.T) {
	answer := func(w http.ResponseWriter, r *http.Request, route string) bool {
		if route != "list" {
			return false
		}
		object := answeredObject(testPrefix+"notes-"+echoedBearer(r), 1, 0, "")
		standInJSON(w, http.StatusOK, map[string]any{"kind": "storage#objects", "items": []any{object}})
		return true
	}

	t.Run("twin", func(t *testing.T) {
		b, err := googleStandIn(t, fakegcs.DefaultEmail, answer)
		if err != nil {
			t.Fatalf("open the bucket against the stand-in: %v", err)
		}
		_, ignored, err := b.list(testwait.Context(t))
		if err != nil || len(ignored) != 1 || !strings.Contains(ignored[0], wordsBearer) {
			t.Fatalf("list answered %q ignored (%v), not the one name holding the bearer, so the row cannot see one printed", ignored, err)
		}
	})

	t.Run("log", func(t *testing.T) {
		b, err := googleStandIn(t, fakegcs.DefaultEmail, answer)
		if err != nil {
			t.Fatalf("open the bucket against the stand-in: %v", err)
		}
		var (
			mu    sync.Mutex
			lines []string
		)
		c := &cache{opts: Options{Bucket: testBucket, Log: func(line string) {
			mu.Lock()
			defer mu.Unlock()
			lines = append(lines, line)
		}}, store: b, foreign: map[string]bool{}}
		c.listBucket(testwait.Context(t))
		mu.Lock()
		defer mu.Unlock()
		oauth2calltest.RequireNoPiece(t, "repocache.foreign-name", strings.Join(lines, "\n"), wordsBearer)
		if len(lines) != 1 || !strings.Contains(lines[0], "no pack or index the cache writes") {
			t.Errorf("the list logged %q, not one line of the object that is no pack", lines)
		}
	})
}
