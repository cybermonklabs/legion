package leakguardtest

import (
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/githubtest"
)

// Releases is a stand-in for the two GitHub REST endpoints a read of one release asset makes, as
// the daemon reads the leak check's index of sandbox lines: the release by its tag,
// `GET /repos/<repository>/releases/tags/<tag>`, whose answer lists the release's assets, and the
// asset by the id that list gives it, `GET /repos/<repository>/releases/assets/<id>`. The asset
// route answers the asset's bytes to a request that asks for application/octet-stream, as GitHub
// streams them, and the asset's JSON to any other, as GitHub does
// (https://docs.github.com/en/rest/releases/assets#get-a-release-asset). Every request must carry
// the stand-in's bearer token (githubtest.Spec's Token); a route of no release, and a release of
// another tag, fail the test.
//
// The repository and the tag are the caller's, since this package imports the guard not at all
// (the package comment): the guard's tests hand it the guard's own repository.
type Releases struct {
	// URL is the stand-in's REST root, which a reader takes for GitHub's.
	URL string

	t          *testing.T
	repository string
	tag        string

	mu     sync.Mutex
	lastID int64
	asset  *publishedAsset
	asked  []Asked
}

// publishedAsset is the release's one asset: its id, its name, the bytes it holds and when it was
// published.
type publishedAsset struct {
	id        int64
	name      string
	body      []byte
	published time.Time
}

// Asked is one request the stand-in was made: its path, and the two headers a read of an asset
// sets.
type Asked struct {
	Path          string
	Authorization string
	Accept        string
}

// firstAssetID is the id of the first asset the stand-in publishes, a number of the size GitHub's
// asset ids have.
const firstAssetID = 312000001

// ServeReleases starts the stand-in for repository (`<owner>/<name>`) and its release of tag,
// admitting only the bearer token, and closes it when the test ends. Until the first Publish the
// release does not exist, and the release route answers GitHub's 404, as before the first build.
func ServeReleases(t *testing.T, repository, tag, token string) *Releases {
	t.Helper()
	r := &Releases{t: t, repository: repository, tag: tag, lastID: firstAssetID - 1}
	r.URL = githubtest.Server(t, githubtest.Spec{Token: token, Record: r.record, Handle: r.handle})
	return r
}

// Publish makes name, holding body, the release's one asset, under an id no asset of the stand-in
// had before, and answers that id. That is what a publish leaves on GitHub: the new upload, the
// removal of the old asset and the rename of the new one give the asset a new id. Publishing a
// name a reader does not ask for is the moment between the removal and the rename, when the
// release holds the new upload alone.
func (r *Releases) Publish(name string, body []byte) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lastID++
	r.asset = &publishedAsset{id: r.lastID, name: name, body: slices.Clone(body), published: time.Now().UTC().Truncate(time.Second)}
	return r.lastID
}

// Asked is every request the stand-in was made, in the order it was made them, the ones it
// refused included.
func (r *Releases) Asked() []Asked {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.asked)
}

func (r *Releases) record(request *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.asked = append(r.asked, Asked{
		Path: request.URL.Path, Authorization: request.Header.Get("Authorization"), Accept: request.Header.Get("Accept"),
	})
}

func (r *Releases) handle(w http.ResponseWriter, request *http.Request) {
	route, routed := strings.CutPrefix(request.URL.Path, "/repos/"+r.repository+"/releases/")
	if !routed || request.Method != http.MethodGet {
		githubtest.Unexpected(r.t, w, request)
		return
	}
	r.mu.Lock()
	asset := r.asset
	r.mu.Unlock()
	if tag, isTag := strings.CutPrefix(route, "tags/"); isTag {
		switch {
		case tag != r.tag:
			githubtest.Unexpected(r.t, w, request)
		case asset == nil:
			notFound(w, "https://docs.github.com/rest/releases/releases#get-a-release-by-tag-name")
		default:
			r.answer(w, r.release(asset))
		}
		return
	}
	id, isAsset := strings.CutPrefix(route, "assets/")
	if !isAsset {
		githubtest.Unexpected(r.t, w, request)
		return
	}
	if asset == nil || id != strconv.FormatInt(asset.id, 10) {
		notFound(w, "https://docs.github.com/rest/releases/assets#get-a-release-asset")
		return
	}
	if request.Header.Get("Accept") != "application/octet-stream" {
		r.answer(w, r.assetJSON(asset))
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = w.Write(asset.body)
}

// release is GitHub's answer for the release that holds asset: a prerelease that is never the
// latest, as the index's release is published.
func (r *Releases) release(asset *publishedAsset) map[string]any {
	stamp := asset.published.Format(time.RFC3339)
	return map[string]any{
		"url":              r.URL + "/repos/" + r.repository + "/releases/270000001",
		"assets_url":       r.URL + "/repos/" + r.repository + "/releases/270000001/assets",
		"html_url":         "https://github.com/" + r.repository + "/releases/tag/" + r.tag,
		"id":               270000001,
		"node_id":          "RE_kwDOStandIn",
		"tag_name":         r.tag,
		"target_commitish": "main",
		"name":             r.tag,
		"body":             "",
		"draft":            false,
		"prerelease":       true,
		"created_at":       stamp,
		"published_at":     stamp,
		"tarball_url":      r.URL + "/repos/" + r.repository + "/tarball/" + r.tag,
		"zipball_url":      r.URL + "/repos/" + r.repository + "/zipball/" + r.tag,
		"assets":           []any{r.assetJSON(asset)},
	}
}

// assetJSON is GitHub's JSON of asset, which the release lists and which the asset route answers
// a request that does not ask for the bytes.
func (r *Releases) assetJSON(asset *publishedAsset) map[string]any {
	id := strconv.FormatInt(asset.id, 10)
	stamp := asset.published.Format(time.RFC3339)
	return map[string]any{
		"url":                  r.URL + "/repos/" + r.repository + "/releases/assets/" + id,
		"browser_download_url": "https://github.com/" + r.repository + "/releases/download/" + r.tag + "/" + asset.name,
		"id":                   asset.id,
		"node_id":              "RA_kwDOStandIn" + id,
		"name":                 asset.name,
		"label":                "",
		"state":                "uploaded",
		"content_type":         "application/octet-stream",
		"size":                 len(asset.body),
		"download_count":       0,
		"created_at":           stamp,
		"updated_at":           stamp,
	}
}

// answer writes value as the JSON body of a 200; the content type is githubtest's.
func (r *Releases) answer(w http.ResponseWriter, value any) {
	if err := json.NewEncoder(w).Encode(value); err != nil {
		r.t.Errorf("the release stand-in could not write its answer: %v", err)
	}
}

// notFound is GitHub's 404 for a release or an asset it does not hold, its keys in GitHub's order.
func notFound(w http.ResponseWriter, documentation string) {
	w.WriteHeader(http.StatusNotFound)
	body, _ := json.Marshal(struct {
		Message       string `json:"message"`
		Documentation string `json:"documentation_url"`
		Status        string `json:"status"`
	}{"Not Found", documentation, "404"})
	_, _ = w.Write(body)
}
