package repocache

import (
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
	"github.com/sjawhar/legion/daemon/internal/repocache/fakegcs"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// TestAnObjectTheBucketAnswersPrintsNoBearerItEchoes: whatever answers in Cloud Storage's place
// answers put's read of an object already there with attributes that quote the bearer the read
// carried: as the object's sha256 metadata, beside the local file's size and CRC32C
// (sha256-metadata), or as the object's name, beside another size (object-name). put refuses the
// object, and its error, which the cache logs, holds no run of the bearer: it names the object by
// the name the cache asked for, and a sha256 metadata that is not the local file's by that alone,
// never by what the bucket answered (storedObject.differs). The twin shows the Storage client hands
// back both attributes as answered, the bearer in each. Captures repocache.answered-object/<case>.
func TestAnObjectTheBucketAnswersPrintsNoBearerItEchoes(t *testing.T) {
	content := randomContent(t, 16<<10)
	name := packObject(content, "pack")
	local := writeLocal(t, content)
	answering := func(object func(bearer string) map[string]any) func(http.ResponseWriter, *http.Request, string) bool {
		return func(w http.ResponseWriter, r *http.Request, route string) bool {
			if route != "read" {
				return false
			}
			standInJSON(w, http.StatusOK, object(echoedBearer(r)))
			return true
		}
	}

	t.Run("twin", func(t *testing.T) {
		b, err := googleStandIn(t, fakegcs.DefaultEmail, answering(func(bearer string) map[string]any {
			return answeredObject(testPrefix+"echo-"+bearer, local.Size, local.CRC32C, "echo-"+bearer)
		}))
		if err != nil {
			t.Fatalf("open the bucket against the stand-in: %v", err)
		}
		attrs, err := b.handle.Object(name).Attrs(testwait.Context(t))
		if err != nil {
			t.Fatalf("read the object's attributes: %v", err)
		}
		if !strings.Contains(attrs.Name, wordsBearer) || !strings.Contains(attrs.Metadata[sha256Metadata], wordsBearer) {
			t.Fatalf("the Storage client answered the name %q and the sha256 metadata %q, not both holding the bearer, so the rows cannot see one printed", attrs.Name, attrs.Metadata[sha256Metadata])
		}
	})

	for _, c := range []struct {
		name, field string
		object      func(bearer string) map[string]any
	}{
		{"sha256-metadata", "sha256 metadata", func(bearer string) map[string]any {
			return answeredObject(name, local.Size, local.CRC32C, "echo-"+bearer)
		}},
		{"object-name", "size", func(bearer string) map[string]any {
			return answeredObject(testPrefix+"echo-"+bearer, local.Size+1, local.CRC32C, local.SHA256)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			b, err := googleStandIn(t, fakegcs.DefaultEmail, answering(c.object))
			if err != nil {
				t.Fatalf("open the bucket against the stand-in: %v", err)
			}
			_, err = b.put(testwait.Context(t), name, local)
			if err == nil {
				t.Fatal("put took the object the stand-in answered")
			}
			oauth2calltest.RequireNoPiece(t, "repocache.answered-object/"+c.name, err.Error(), wordsBearer)
			if !strings.Contains(err.Error(), name) || !strings.Contains(err.Error(), "another "+c.field) {
				t.Errorf("put's refusal %q names neither %s nor its %s", err, name, c.field)
			}
		})
	}
}
