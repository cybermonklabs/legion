package leakguard

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/rolefile"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"golang.org/x/sys/unix"
)

// Where the leak check's index of sandbox lines is published, and where it is held. The builder
// publishes the index as IndexAsset, the one asset of the release IndexRelease of the check's
// repository (CheckSlug): a prerelease whose tag is no leak-check-v* tag. The check reads it at
// IndexFile in IndexDir under the passwd home of the account it runs as, and downloads a new one
// there itself where the one it holds is an hour old or unreadable, unless IndexPushedMarker is
// beside it. In a pod the daemon reads the release (IndexSource) and pushes each index to the pod's
// launcher, which installs it in that folder (InstallIndex) and writes the marker when it starts
// (MarkIndexPushed), so no check in the pod downloads.
const (
	IndexRelease      = "leak-check-index"
	IndexAsset        = "sandbox-lines.idx"
	IndexDir          = ".cache/public-leak-check"
	IndexFile         = "sandbox-lines.idx"
	IndexPushedMarker = "sandbox-lines.idx.pushed"
)

// maxIndexBytes, 16 MiB, is the largest index the daemon reads from GitHub and a launcher holds:
// the most a launcher_leak_check_index sequence carries, so an index the daemon reads is one it
// can push.
const maxIndexBytes = shimwire.MaxLeakCheckIndexBytes

// maxReleaseBytes is how much of GitHub's answer for the index's release a read takes. The answer
// lists one asset and is a few KiB, so a MiB is far above what it holds and still bounds what one
// read may take of the daemon's memory.
const maxReleaseBytes = 1 << 20

// indexLine1 is line 1 of an index of format 1, the one format there is.
const indexLine1 = "PUBLIC-LEAK-CHECK-INDEX 1\n"

// indexBuiltAt is the shape of the header's built_at, the UTC time YYYY-MM-DDTHH:MM:SSZ, which the
// check refuses any other shape of. time.Parse alone would take a one-digit hour.
var indexBuiltAt = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$`)

// indexBuiltAtLayout is that shape as a layout of the time package, which reads and writes it.
const indexBuiltAtLayout = "2006-01-02T15:04:05Z"

// indexMaxAhead is how far after the clock an index's build time may be: the check's own
// INDEX_MAX_AHEAD, 600 s. The check reads an index built further ahead as a header it cannot read,
// since such a time would keep the index alive past the 6 hours after which it reads an index
// UNKNOWN. So InstallIndex neither installs such an index nor keeps one against a later index: one
// held would read UNKNOWN in every role of the pod, and keep out every index pushed after it,
// until the clock came within this bound of its build time.
const indexMaxAhead = 10 * time.Minute

// Index is an index of sandbox lines ParseIndex read: Body the file whole, SHA256 the digest of its
// list its header holds, and BuiltAt the build time its header holds.
type Index struct {
	Body    []byte
	SHA256  string
	BuiltAt time.Time
}

// ParseIndex reads body as an index of format 1, as the check's writer lays one out: line 1, then
// line 2, one ASCII JSON object followed by spaces up to an LF whose next byte is at a multiple of
// 8, then the list, `entries` unsigned 64-bit little-endian integers. Of the header it reads the
// keys format (1), built_at, entries and sha256, and it holds the list to entries and to sha256.
// It reads no other key, the order of the list, or the build time's age: the check judges those
// where it reads the index.
//
// Each error is a sentence of what is wrong with the file, in the check's own words where the
// check names the same case.
func ParseIndex(body []byte) (Index, error) {
	rest, found := bytes.CutPrefix(body, []byte(indexLine1))
	if !found {
		return Index{}, fmt.Errorf("the index's first line is not %q", strings.TrimSuffix(indexLine1, "\n"))
	}
	end := bytes.IndexByte(rest, '\n')
	if end < 0 {
		return Index{}, errors.New("the index has no header line")
	}
	listAt := len(indexLine1) + end + 1
	if listAt%8 != 0 {
		return Index{}, fmt.Errorf("the index's header line ends at byte %d, not at a multiple of 8", listAt)
	}
	line := rest[:end]
	for _, b := range line {
		if b >= 0x80 {
			return Index{}, errors.New("the index's header is not ASCII")
		}
	}
	var header map[string]json.RawMessage
	if err := json.Unmarshal(line, &header); err != nil || header == nil {
		return Index{}, errors.New("the index's header is not a JSON object")
	}
	var format int
	var builtAt, digest string
	var entries int64
	for _, key := range []struct {
		name string
		into any
	}{{"format", &format}, {"built_at", &builtAt}, {"entries", &entries}, {"sha256", &digest}} {
		if err := headerKey(header, key.name, key.into); err != nil {
			return Index{}, err
		}
	}
	if format != 1 {
		return Index{}, fmt.Errorf("the index is of format %d, not 1", format)
	}
	at, err := time.Parse(indexBuiltAtLayout, builtAt)
	if err != nil || !indexBuiltAt.MatchString(builtAt) {
		return Index{}, fmt.Errorf("the index's built_at %q is not a UTC time YYYY-MM-DDTHH:MM:SSZ", builtAt)
	}
	list := body[listAt:]
	if len(list)%8 != 0 {
		return Index{}, fmt.Errorf("the index's list is %d bytes, which is no whole number of 8-byte entries", len(list))
	}
	if int64(len(list)/8) != entries {
		return Index{}, fmt.Errorf("the index's list holds %d entries and its header says %d", len(list)/8, entries)
	}
	if sum := sha256.Sum256(list); hex.EncodeToString(sum[:]) != digest {
		return Index{}, errors.New("the sha256 of the index's list is not its header's")
	}
	return Index{Body: body, SHA256: digest, BuiltAt: at}, nil
}

// headerKey decodes the header's key name into into, refusing a header without it and a value not
// of into's type. JSON's null is no value of any type, though Go would decode it into one as
// nothing at all.
func headerKey(header map[string]json.RawMessage, name string, into any) error {
	raw, there := header[name]
	if !there {
		return fmt.Errorf("the index's header has no %s", name)
	}
	if string(raw) == "null" || json.Unmarshal(raw, into) != nil {
		return fmt.Errorf("the index's header holds the %s %s, which is not of its type", name, raw)
	}
	return nil
}

// IndexSource is the daemon's reader of the leak check's index of sandbox lines: the asset
// IndexAsset of the release IndexRelease of the check's repository (CheckSlug), read over GitHub's
// REST API with a read-only token of the daemon's own, as Source reads the check itself. It runs
// no gh and reads no gh configuration, and it keeps nothing, so one IndexSource may be read from at
// once by any number of callers.
type IndexSource struct {
	// Token mints the read-only token the read is made with. Its error's text is what happened,
	// which a refused read carries.
	Token func(context.Context) (string, error)
	// API is GitHub's REST root; empty is https://api.github.com.
	API string
	// Forget, when set, drops what Token holds of the token it minted, so the next mint is a new
	// token: the read calls it where GitHub answered either of its requests 401, a token GitHub no
	// longer takes. Nil is a Token that holds nothing.
	Forget func()
	// Client makes the read; nil is http.DefaultClient. Its redirect policy is Go's own, which
	// follows GitHub's redirect of an asset to its storage host and drops the Authorization header
	// on the way to another host.
	Client *http.Client
}

// Read answers the index GitHub publishes now. Each call reads both endpoints, the release by its
// tag for the asset's id (`GET repos/<CheckSlug>/releases/tags/<IndexRelease>`) and then the
// asset's bytes (`GET repos/<CheckSlug>/releases/assets/<id>`, `Accept: application/octet-stream`),
// and keeps nothing.
//
// It does not use Source's cache (R10). Source keeps the read of a tag for the life of the
// process because a tag of the check is trusted not to move; this release keeps its one tag while
// the builder replaces its asset every hour, and R10 asks that the daemon read it at least once an
// hour and push each new index to every pod. A kept read would push one index for the daemon's
// life, and the check in each pod would read it UNKNOWN six hours after its build. So the daemon
// decides how often to read, and each read asks GitHub.
//
// The token goes into the two requests' Authorization header and nowhere else. A 401 from either
// request calls Forget. The whole read, the mint included, runs under leakCheckReadBound. A
// release that does not list IndexAsset, which is how it reads between the removal of the old
// asset and the rename of the new upload, an answer over maxIndexBytes, and an asset that is no
// index (ParseIndex) are each refused. Every error is one sentence, naming the asset, the release
// and the repository, then why.
func (s *IndexSource) Read(ctx context.Context) (Index, error) {
	ctx, cancel := context.WithTimeout(ctx, leakCheckReadBound)
	defer cancel()
	index, err := s.read(ctx)
	if err != nil {
		return Index{}, fmt.Errorf("the daemon could not read %s of the release %s of %s: %w", IndexAsset, IndexRelease, CheckSlug, err)
	}
	return index, nil
}

// read is Read's two requests, under the bound ctx carries.
func (s *IndexSource) read(ctx context.Context) (Index, error) {
	token, err := s.Token(ctx)
	if err != nil {
		return Index{}, err
	}
	repository := githubrest.RepositoryAPI(s.API, CheckRepository)
	answer, err := s.get(ctx, token, "the release", repository+"/releases/tags/"+IndexRelease, "application/vnd.github+json", maxReleaseBytes)
	if err != nil {
		return Index{}, err
	}
	var release struct {
		Assets []struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		} `json:"assets"`
	}
	if err := json.Unmarshal(answer, &release); err != nil {
		return Index{}, fmt.Errorf("GitHub's answer for the release is no release: %w", err)
	}
	for _, asset := range release.Assets {
		if asset.Name != IndexAsset {
			continue
		}
		body, err := s.get(ctx, token, "the asset", repository+"/releases/assets/"+strconv.FormatInt(asset.ID, 10), "application/octet-stream", maxIndexBytes)
		if err != nil {
			return Index{}, err
		}
		return ParseIndex(body)
	}
	return Index{}, fmt.Errorf("the release holds no asset %s", IndexAsset)
}

// get answers the body of GitHub's 200 to a GET of target, what, made with token and accepting
// accept, or why there is none: the transport's error, GitHub's status with the first line of its
// answer, or an answer over limit bytes, which is refused rather than read on.
func (s *IndexSource) get(ctx context.Context, token, what, target, accept string, limit int) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", accept)
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	client := s.Client
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		if response.StatusCode == http.StatusUnauthorized && s.Forget != nil {
			s.Forget()
		}
		refusal, _ := io.ReadAll(io.LimitReader(response.Body, refusalBodyBytes))
		why := fmt.Sprintf("GitHub answered the read of %s with HTTP %d", what, response.StatusCode)
		if strings.TrimSpace(string(refusal)) != "" {
			why += ": " + firstLine(string(refusal))
		}
		return nil, errors.New(why)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, int64(limit)+1))
	if err != nil {
		return nil, err
	}
	if len(body) > limit {
		return nil, fmt.Errorf("GitHub's answer for %s is over the %d MiB this daemon reads", what, limit>>20)
	}
	return body, nil
}

// InstallIndex installs body, an index (ParseIndex), as IndexFile in dir, the folder the check
// reads it from, and answers whether it replaced what dir held there. It reads the clock once, and
// refuses body where it was built more than indexMaxAhead after that reading, before it writes
// anything. Otherwise it makes dir where it is absent, mode 0755, and writes body to a new file
// beside the index, `.sandbox-lines.idx.<random>.new`, mode 0644, synced to disk. It then renames
// that file over IndexFile where what is there is no index, an index built more than
// indexMaxAhead after the clock's reading, or an index built before body's: a reader finds the old
// file or the new one, whole. An index there built at body's time or later, and not ahead of the
// clock past that bound, is kept, so of two calls made one after the other, a push that arrives
// late never puts an older index back.
//
// The folder is one every role of the pod reads and the check in a role writes, so what is at
// IndexFile is read as a role's file is read (rolefile.Read): a symlink, a fifo, anything else that
// is no regular file, and a file over maxIndexBytes are no index, never followed or waited on, and
// the rename replaces them. The new file is removed on every path but the rename, and each error
// names dir and what failed.
func InstallIndex(dir string, body []byte) (replaced bool, err error) {
	defer func() {
		if err != nil {
			err = fmt.Errorf("the leak check index could not be installed in %s: %w", dir, err)
		}
	}()
	now := time.Now()
	index, err := ParseIndex(body)
	if err != nil {
		return false, err
	}
	latest := now.Add(indexMaxAhead)
	if index.BuiltAt.After(latest) {
		return false, fmt.Errorf("the index's built_at %s is more than %d minutes ahead of this machine's clock, %s",
			index.BuiltAt.UTC().Format(indexBuiltAtLayout), int(indexMaxAhead/time.Minute), now.UTC().Format(indexBuiltAtLayout))
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, err
	}
	staged, err := stageIndex(dir, body)
	if err != nil {
		return false, err
	}
	installed := false
	defer func() {
		if !installed {
			_ = os.Remove(staged)
		}
	}()
	path := filepath.Join(dir, IndexFile)
	if held, err := rolefile.Read(path, maxIndexBytes); err == nil {
		if kept, err := ParseIndex(held); err == nil && !kept.BuiltAt.Before(index.BuiltAt) && !kept.BuiltAt.After(latest) {
			return false, nil
		}
	}
	if err := os.Rename(staged, path); err != nil {
		return false, err
	}
	installed = true
	return true, nil
}

// stageIndex writes body to a new file of a random name beside the index in dir, the name the
// check's own download gives its new file, syncs it, and answers its path. The file is opened
// O_EXCL, so a name something already holds fails the write rather than being written through, and
// it is removed where the write fails.
func stageIndex(dir string, body []byte) (string, error) {
	var random [8]byte
	_, _ = rand.Read(random[:])
	path := filepath.Join(dir, "."+IndexFile+"."+hex.EncodeToString(random[:])+".new")
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return "", err
	}
	_, err = file.Write(body)
	if err == nil {
		err = file.Sync()
	}
	if closed := file.Close(); err == nil {
		err = closed
	}
	if err != nil {
		_ = os.Remove(path)
		return "", err
	}
	return path, nil
}

// MarkIndexPushed writes IndexPushedMarker, an empty regular file, in dir, making dir where it is
// absent, mode 0755. The pod's launcher writes it when it starts: a check that finds the marker
// never downloads the index, which the daemon pushes instead. A marker already there is left as
// it is, and what is at its name is opened as a role's file is (rolefile.OpenRegular): a symlink
// there is refused, never written through.
func MarkIndexPushed(dir string) (err error) {
	defer func() {
		if err != nil {
			err = fmt.Errorf("the leak check index's pushed marker could not be written in %s: %w", dir, err)
		}
	}()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(dir, IndexPushedMarker)
	file, err := rolefile.OpenRegular(unix.AT_FDCWD, path, path, unix.O_WRONLY|unix.O_CREAT, 0o644)
	if err != nil {
		return err
	}
	return file.Close()
}
