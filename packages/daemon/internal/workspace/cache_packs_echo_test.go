package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
)

// packsEchoBearer is the bearer of the cases whose stand-in at the repository cache's address
// writes it back. It holds oauth2calltest.Token, the made-up credential the check's markers name.
const packsEchoBearer = "cache-" + oauth2calltest.Token + "-5d1e8a"

// packsEchoSignature is the signature in the query of every address the stand-in's manifest gives:
// letters alone, so a bucket's error code can be it, holding the check's marker eyJhbGci.
const packsEchoSignature = "eyJhbGci" + "SignedAddressQueryValue"

// packsEcho stands in at the repository cache's address and at its bucket's, both on loopback, for
// a stream of acme/widgets presenting bearer: /healthz answers ready and narrowed; /packs answers
// through packs, or with the bearer the manifest as JSON; /tar answers 503, so the stream fails
// after the bucket's attempt and its report says why the bucket was not used; and the bucket
// answers each download through download, or with the object's content. The manifest lists HEAD
// and one pack with its index, each with its size, its SHA-256 and an address at the bucket whose
// query holds packsEchoSignature.
type packsEcho struct {
	cache, bucket, bearer string
	manifest              PackManifest
	content               map[string][]byte
	packs, download       http.HandlerFunc
}

// newPacksEcho starts the stand-in, its bearer packsEchoBearer.
func newPacksEcho(t *testing.T) *packsEcho {
	t.Helper()
	p := &packsEcho{bearer: packsEchoBearer, content: map[string][]byte{}}
	bucket := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p.download != nil {
			p.download(w, r)
			return
		}
		body, ok := p.content[strings.TrimPrefix(r.URL.Path, "/"+testPacksBucket+"/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		_, _ = w.Write(body)
	}))
	t.Cleanup(bucket.Close)
	p.bucket = bucket.URL
	p.manifest = PackManifest{Bucket: testPacksBucket, Refs: []PackRef{{Path: "HEAD", Bytes: []byte("ref: refs/heads/main\n")}}}
	for _, extension := range []string{".pack", ".idx"} {
		p.put(testPacksPrefix+"pack-"+strings.Repeat("0", 40)+extension, []byte("the "+extension+" the bucket holds"))
	}
	cache := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(CacheRepositoryHeader, "acme/widgets")
		switch r.URL.Path {
		case "/healthz":
			_, _ = w.Write([]byte(`{"ready":true,"defaultBranchOnly":true}`))
		case "/packs":
			if p.packs != nil {
				p.packs(w, r)
				return
			}
			if r.Header.Get("Authorization") != "Bearer "+p.bearer {
				http.Error(w, "the repository cache's token is required", http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode(p.manifest)
		default:
			http.Error(w, "this stand-in streams no tar", http.StatusServiceUnavailable)
		}
	}))
	t.Cleanup(cache.Close)
	p.cache = cache.URL
	return p
}

// put lists the object name in the manifest with body as its content in the bucket.
func (p *packsEcho) put(name string, body []byte) {
	sum := sha256.Sum256(body)
	p.content[name] = body
	p.manifest.Objects = append(p.manifest.Objects, PackObject{Name: name, Size: int64(len(body)), SHA256: hex.EncodeToString(sum[:]), URL: p.address(name)})
}

// address is name's address at the bucket, its query packsEchoSignature.
func (p *packsEcho) address(name string) string {
	return p.bucket + "/" + testPacksBucket + "/" + name + "?X-Goog-Signature=" + packsEchoSignature
}

// echo is what the stand-in writes back where a case has it echo the ask: the Authorization the
// ask carried, and the pack's address, signature and all.
func (p *packsEcho) echo() string {
	return "Bearer " + p.bearer + " " + p.manifest.Objects[0].URL
}

// fetch fills a fresh bare git directory from the stand-in with its bearer (StreamCache), and
// answers the report, which must say the cache's /tar was the source and why the bucket was not
// used.
func (p *packsEcho) fetch(t *testing.T) StreamReport {
	t.Helper()
	stream := CacheStream{Repo: ghrepo.MustParse("acme/widgets"), URL: p.cache, Token: p.bearer, Log: func(string) {}, Clock: newTestClock()}
	report, err := StreamCache(context.Background(), newLocalRunner(t), stream, filepath.Join(t.TempDir(), "widgets.git"))
	t.Logf("the clone's source: %q, because %q; the stream's failure: %v", report.Source, report.TarReason, err)
	if report.Source != SourceTar || report.TarReason == "" {
		t.Fatalf("StreamCache reported the source %q for %q, want /tar's with why the bucket was not used", report.Source, report.TarReason)
	}
	return report
}

// rawAnswer writes answer on the connection w's request came on, as whatever answers there can
// write it, and closes it.
func rawAnswer(t *testing.T, w http.ResponseWriter, answer string) {
	t.Helper()
	conn, _, err := w.(http.Hijacker).Hijack()
	if err != nil {
		t.Errorf("hijack the stand-in's answer: %v", err)
		return
	}
	defer conn.Close()
	_, _ = io.WriteString(conn, answer)
}

// asServed is all net/http gives of a GET of address, presenting bearer where it is not empty: the
// status line, the body and the failure of its ask or of its read, as they came.
func asServed(t *testing.T, address, bearer string) string {
	t.Helper()
	ask, err := http.NewRequest(http.MethodGet, address, nil)
	if err != nil {
		t.Fatal(err)
	}
	if bearer != "" {
		ask.Header.Set("Authorization", "Bearer "+bearer)
	}
	response, err := http.DefaultClient.Do(ask)
	if err != nil {
		return err.Error()
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	return fmt.Sprintf("%s %s %v", response.Status, body, err)
}

// A stand-in at the repository cache's address holds the bearer the /packs ask carried, and the
// signed addresses its manifest gives, and whatever answers at those addresses holds the signed
// query its download carried: either can write them back. The cache's: in /packs' status line
// (packs-status); in a trailer of its body that does not parse, which net/http's error of the read
// quotes (packs-read); and in a member of its manifest that the stream refuses - the bucket
// (bucket), a ref that is no path inside the clone (ref-path), one listed twice (ref-twice), one
// that is another's directory (ref-directory), an object of no pack's name (object-name), an
// object's SHA-256 (object-sha256) - or a ref it takes whose write fails, its name too long for the
// volume (write-ref). The bucket's: in a download's status line (download-status), its error's
// code (download-code), a status line that does not parse, which net/http's error of the ask quotes
// (download-unanswered), and a trailer of its body that does not parse (download-read). The reason
// the report gives for filling the clone from /tar, which `workspace-init provision` prints
// (logCacheSource), holds no run of the bearer or of the signature, and still names what failed.
// Each case first shows what net/http or the volume gives of the stand-in's words holds one.
// Captures workspace.packs-echo/<case>.
func TestABucketAttemptsReasonPrintsNoCredentialTheCacheOrTheBucketEchoes(t *testing.T) {
	// refEcho is a ref a clone takes that holds the bearer and the signature.
	refEcho := "refs/heads/" + packsEchoBearer + "?X-Goog-Signature=" + packsEchoSignature
	// longRef is one whose last segment is longer than a file's name may be.
	longRef := "refs/heads/" + strings.Repeat(packsEchoBearer+"-", 12) + "?X-Goog-Signature=" + packsEchoSignature
	member := func(edit func(p *packsEcho, m *PackManifest)) func(t *testing.T, p *packsEcho) string {
		return func(t *testing.T, p *packsEcho) string {
			edit(p, &p.manifest)
			return asServed(t, p.cache+"/packs", p.bearer)
		}
	}
	// downloads makes every download answer through answer, and answers what net/http gives of the
	// pack's.
	downloads := func(answer func(t *testing.T, p *packsEcho, w http.ResponseWriter, r *http.Request)) func(t *testing.T, p *packsEcho) string {
		return func(t *testing.T, p *packsEcho) string {
			p.download = func(w http.ResponseWriter, r *http.Request) { answer(t, p, w, r) }
			return asServed(t, p.manifest.Objects[0].URL, "")
		}
	}
	for _, tc := range []struct {
		name string
		// fault makes the case's echo once the stand-in is built, and answers what shows it.
		fault func(t *testing.T, p *packsEcho) string
		// says is what the reason holds, before and after the echo is redacted.
		says func(p *packsEcho) string
	}{
		{"packs-status", func(t *testing.T, p *packsEcho) string {
			p.packs = func(w http.ResponseWriter, _ *http.Request) {
				rawAnswer(t, w, "HTTP/1.1 503 "+p.echo()+"\r\n"+CacheRepositoryHeader+": acme/widgets\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
			}
			return asServed(t, p.cache+"/packs", p.bearer)
		}, func(p *packsEcho) string { return reasonUnavailable + ": GET " + p.cache + "/packs answered 503 " }},
		{"packs-read", func(t *testing.T, p *packsEcho) string {
			// A trailer line with no colon does not parse: net/http takes one with a colon as a
			// header, spaces before the colon and all.
			p.packs = func(w http.ResponseWriter, _ *http.Request) {
				rawAnswer(t, w, "HTTP/1.1 200 OK\r\n"+CacheRepositoryHeader+": acme/widgets\r\nTransfer-Encoding: chunked\r\nConnection: close\r\n\r\n"+
					"2\r\n{}\r\n0\r\nBearer "+p.bearer+" "+packsEchoSignature+"\r\n\r\n")
			}
			return asServed(t, p.cache+"/packs", p.bearer)
		}, func(p *packsEcho) string { return reasonUnavailable + ": read GET " + p.cache + "/packs: " }},
		{"bucket", member(func(p *packsEcho, m *PackManifest) { m.Bucket = p.echo() }),
			func(*packsEcho) string { return reasonInvalid + `: it names the bucket "` }},
		{"ref-path", member(func(p *packsEcho, m *PackManifest) { m.Refs = append(m.Refs, PackRef{Path: p.echo()}) }),
			func(*packsEcho) string { return reasonInvalid + `: it lists the ref "` }},
		{"ref-twice", member(func(_ *packsEcho, m *PackManifest) {
			m.Refs = append(m.Refs, PackRef{Path: refEcho}, PackRef{Path: refEcho})
		}), func(*packsEcho) string { return `" twice` }},
		{"ref-directory", member(func(_ *packsEcho, m *PackManifest) {
			m.Refs = append(m.Refs, PackRef{Path: refEcho}, PackRef{Path: refEcho + "/tip"})
		}), func(*packsEcho) string { return `", which is the directory of the ref "` }},
		{"object-name", member(func(p *packsEcho, m *PackManifest) { m.Objects[0].Name = p.echo() }),
			func(*packsEcho) string { return "\", which is no pack or index of acme/widgets" }},
		{"object-sha256", member(func(p *packsEcho, m *PackManifest) { m.Objects[0].SHA256 = p.echo() }),
			func(*packsEcho) string { return "\", which is not 64 lower-case hex digits" }},
		{"write-ref", func(t *testing.T, p *packsEcho) string {
			p.manifest.Refs = append(p.manifest.Refs, PackRef{Path: longRef, Bytes: []byte("0123456789abcdef0123456789abcdef01234567\n")})
			volume := t.TempDir()
			if err := os.MkdirAll(filepath.Join(volume, "refs", "heads"), 0o755); err != nil {
				t.Fatal(err)
			}
			err := os.WriteFile(filepath.Join(volume, filepath.FromSlash(longRef)), nil, 0o644)
			if err == nil {
				t.Fatalf("the volume took a file named %q, so the case's write cannot fail", longRef)
			}
			return err.Error()
		}, func(*packsEcho) string { return reasonWrite + ": the ref refs/heads/" }},
		{"download-status", downloads(func(t *testing.T, p *packsEcho, w http.ResponseWriter, r *http.Request) {
			rawAnswer(t, w, "HTTP/1.1 403 Bearer "+p.bearer+" http://"+r.Host+r.URL.RequestURI()+"\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
		}), func(*packsEcho) string { return ": the bucket answered 403 " }},
		{"download-code", downloads(func(_ *testing.T, _ *packsEcho, w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, "<?xml version='1.0' encoding='UTF-8'?><Error><Code>"+r.URL.Query().Get("X-Goog-Signature")+"</Code></Error>")
		}), func(*packsEcho) string { return ": the bucket answered 403 Forbidden (" }},
		{"download-unanswered", downloads(func(t *testing.T, p *packsEcho, w http.ResponseWriter, r *http.Request) {
			rawAnswer(t, w, p.bearer+"|"+r.URL.Query().Get("X-Goog-Signature")+"\r\n\r\n")
		}), func(*packsEcho) string { return reasonDownload + ": " + testPacksPrefix + "pack-" }},
		{"download-read", downloads(func(t *testing.T, p *packsEcho, w http.ResponseWriter, r *http.Request) {
			body := p.content[strings.TrimPrefix(r.URL.Path, "/"+testPacksBucket+"/")]
			rawAnswer(t, w, fmt.Sprintf("HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\nConnection: close\r\n\r\n%x\r\n%s\r\n0\r\nBearer %s %s\r\n\r\n",
				len(body), body, p.bearer, r.URL.Query().Get("X-Goog-Signature")))
		}), func(*packsEcho) string { return reasonDownload + ": " + testPacksPrefix + "pack-" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newPacksEcho(t)
			if served := tc.fault(t, p); oauth2calltest.Piece(served, p.bearer) == "" && oauth2calltest.Piece(served, packsEchoSignature) == "" {
				t.Fatalf("what the stand-in's answer gives holds no run of the bearer or the signature, so the case cannot see a leak: %q", served)
			}
			reason := p.fetch(t).TarReason
			oauth2calltest.RequireNoPiece(t, "workspace.packs-echo/"+tc.name, reason, p.bearer, packsEchoSignature)
			if says := tc.says(p); !strings.Contains(reason, says) {
				t.Errorf("the reason is %q, want it to name what failed (%q)", reason, says)
			}
		})
	}
}

// The repository cache's bearer is 64 hex digits, and the shapes a manifest's bucket, object names
// and SHA-256s are held to take hex digits: a stand-in at the cache's address can spell the bearer
// whole in a bucket's name (bucket), a pack's name (name) or a SHA-256 (sha256), each of which a
// fetch would print as it came, in the reason it gives or the line naming each object it
// downloaded. The fetch refuses such a manifest naming the member by its place, and the reason
// holds no run of the bearer. The bearer holds none of the check's markers, so no capture reads
// these cases: a run of it is told by oauth2calltest.Piece.
func TestAManifestMemberThatSpellsTheCachesBearerIsRefusedByItsPlace(t *testing.T) {
	sum := sha256.Sum256([]byte("the cache's bearer"))
	bearer := hex.EncodeToString(sum[:])
	const holding = "holding a run of a credential the ask of /packs carried"
	for _, tc := range []struct {
		name  string
		edit  func(p *packsEcho)
		says  string
		named string
	}{
		{"bucket", func(p *packsEcho) { p.manifest.Bucket = bearer[:32] + "." + bearer[32:] }, "it names a bucket " + holding, ""},
		{"name", func(p *packsEcho) {
			objects := p.manifest.Objects
			p.manifest.Objects = nil
			for _, object := range objects {
				p.put(testPacksPrefix+"pack-"+bearer+filepath.Ext(object.Name), p.content[object.Name])
			}
		}, "it lists object 1 with a name " + holding, ""},
		{"sha256", func(p *packsEcho) { p.manifest.Objects[0].SHA256 = bearer }, " with a SHA-256 " + holding, ".pack"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newPacksEcho(t)
			p.bearer = bearer
			tc.edit(p)
			reason := p.fetch(t).TarReason
			if piece := oauth2calltest.Piece(reason, bearer); piece != "" {
				t.Errorf("the reason %q shows %q, a run of the cache's bearer", reason, piece)
			}
			if !strings.HasPrefix(reason, reasonInvalid+": ") || !strings.Contains(reason, tc.says) || !strings.Contains(reason, tc.named) {
				t.Errorf("the reason is %q, want the manifest refused naming the member by its place (%q)", reason, tc.says)
			}
		})
	}
}
