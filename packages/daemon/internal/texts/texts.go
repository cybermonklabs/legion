// Package texts reads and checks a folder of texts an experiment arm names in place of Legion's
// own, and builds the extension root a launch of that arm loads before Legion's plugins. The
// daemon, both runtimes and the launcher import it.
//
// A folder mirrors the paths of Legion's texts: "roles/<p>" is the shared role prompt
// internal/prompts/roles/<p>, "go/<p>" the daemon's part internal/prompts/go/<p>,
// "agents/<name>.md" a pi-legion agent, and "skills/<skill>/<p>" a file of a skill as the plugins
// ship it (dist/skills/<skill>/<p>). Beside them sits one manifest.json, which records for each
// file the digest of Legion's text that it replaces. A folder replaces texts; it adds none and
// removes none.
package texts

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
)

const (
	// ManifestName is the manifest's name in the folder.
	ManifestName = "manifest.json"
	// RootVariable names the launch's extension root to pi-envoy, which reads dispatch-first and its
	// skills from it.
	RootVariable = "LEGION_TEXTS_ROOT"
	// SettingsLayerName is the last settings layer a launch of a folder names in PI_CONFIG_FILES,
	// written in its root (WriteSettingsLayer).
	SettingsLayerName = "texts-settings.yml"
)

// prefixes are the trees a folder's file may sit in.
var prefixes = []string{"roles/", "go/", "agents/", "skills/"}

// digestForm is a digest as the manifest records it.
var digestForm = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// Manifest is manifest.json: each file of the folder, by its path, with the digest of Legion's
// text it replaces ("sha256:<64 lower-case hex>").
type Manifest struct {
	Files map[string]string `json:"files"`
}

// Folder is a folder of texts as Read read it.
type Folder struct {
	// Dir is the folder as the caller named it.
	Dir string
	// Files are the bytes of every file the folder holds, by its path there, manifest.json among
	// them.
	Files    map[string][]byte
	Manifest Manifest
	// Fingerprint is the SHA-256, in hex, of one line `<path>\t<sha256 hex of its bytes>\n` for
	// each file, manifest.json included, in the order of their paths: any change to any file, and
	// only such a change, changes it.
	Fingerprint string
}

// Digest is body's digest as a manifest records it.
func Digest(body []byte) string {
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// paths are the folder's files under any of trees, in order.
func (f Folder) paths(trees ...string) []string {
	var paths []string
	for p := range f.Files {
		for _, tree := range trees {
			if strings.HasPrefix(p, tree) {
				paths = append(paths, p)
				break
			}
		}
	}
	slices.Sort(paths)
	return paths
}

// Read reads the folder at dir once, every file of it, and holds it to its manifest: every file
// listed, every listed file present, every path clean and under one of the four trees, and every
// digest of the manifest's form. It takes the layout of a Kubernetes ConfigMap or Secret volume,
// which shows the files as links into a timestamped directory beside a `..data` link: it skips
// every name that starts with `..`, follows a link whose target stays inside the folder, and
// refuses one that leaves it. It reads only regular files. A refusal is a *Fault.
func Read(dir string) (Folder, error) {
	refuse := func(file string, cause Cause, detail string) (Folder, error) {
		return Folder{}, &Fault{Dir: dir, Path: file, Cause: cause, Detail: detail}
	}
	info, err := os.Stat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return refuse("", Gone, "")
	case err != nil:
		return refuse("", Unreadable, err.Error())
	case !info.IsDir():
		return refuse("", Unreadable, "it is not a directory")
	}
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return refuse("", Unreadable, err.Error())
	}
	w := walker{root: root, files: map[string][]byte{}}
	if fault := w.walk(root, ""); fault != nil {
		fault.Dir = dir
		return Folder{}, fault
	}

	raw, ok := w.files[ManifestName]
	if !ok {
		return refuse(ManifestName, NoManifest, "")
	}
	manifest, detail := decodeManifest(raw)
	if detail != "" {
		return refuse(ManifestName, ManifestUnreadable, detail)
	}
	for _, p := range slices.Sorted(maps.Keys(manifest.Files)) {
		if !validPath(p) {
			return refuse(p, BadPath, "listed in "+ManifestName)
		}
		if digest := manifest.Files[p]; !digestForm.MatchString(digest) {
			return refuse(ManifestName, ManifestUnreadable, fmt.Sprintf("it records %q for %s, not sha256:<64 lower-case hex>", digest, p))
		}
	}
	for _, p := range slices.Sorted(maps.Keys(w.files)) {
		switch {
		case p == ManifestName:
		case !validPath(p):
			return refuse(p, BadPath, "")
		case manifest.Files[p] == "":
			return refuse(p, Unlisted, "")
		}
	}
	for _, p := range slices.Sorted(maps.Keys(manifest.Files)) {
		if _, ok := w.files[p]; !ok {
			return refuse(p, Missing, "")
		}
	}
	return Folder{Dir: dir, Files: w.files, Manifest: manifest, Fingerprint: fingerprint(w.files)}, nil
}

// decodeManifest is raw read as a manifest, or what is wrong with it: one JSON object holding
// `files` and no other key, an object of strings, and nothing after it.
func decodeManifest(raw []byte) (Manifest, string) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	var top map[string]json.RawMessage
	if err := dec.Decode(&top); err != nil {
		return Manifest{}, err.Error()
	}
	if top == nil {
		return Manifest{}, "it is not a JSON object"
	}
	if _, err := dec.Token(); err != io.EOF {
		return Manifest{}, "it holds more than the one JSON object"
	}
	for _, key := range slices.Sorted(maps.Keys(top)) {
		if key != "files" {
			return Manifest{}, fmt.Sprintf("it holds the unknown key %q", key)
		}
	}
	member, ok := top["files"]
	if !ok {
		return Manifest{}, "it holds no files object"
	}
	var files map[string]string
	if err := json.Unmarshal(member, &files); err != nil {
		return Manifest{}, "its files: " + err.Error()
	}
	if files == nil {
		return Manifest{}, "its files is null, not an object"
	}
	return Manifest{Files: files}, ""
}

// validPath is whether p is a path a folder's file may have: relative, written with `/`, clean,
// with no element that Read would skip (`..*`), and under one of the four trees.
func validPath(p string) bool {
	if !fs.ValidPath(p) || path.Clean(p) != p || strings.Contains(p, `\`) {
		return false
	}
	for _, element := range strings.Split(p, "/") {
		if strings.HasPrefix(element, "..") {
			return false
		}
	}
	return slices.ContainsFunc(prefixes, func(prefix string) bool { return strings.HasPrefix(p, prefix) })
}

// fingerprint is Folder.Fingerprint of files.
func fingerprint(files map[string][]byte) string {
	sum := sha256.New()
	for _, p := range slices.Sorted(maps.Keys(files)) {
		file := sha256.Sum256(files[p])
		fmt.Fprintf(sum, "%s\t%s\n", p, hex.EncodeToString(file[:]))
	}
	return hex.EncodeToString(sum.Sum(nil))
}

// walker reads every file below root, the folder with its links resolved.
type walker struct {
	root  string
	files map[string][]byte
	// open are the directories the walk is inside, by their resolved path, so a link back to one
	// of them is refused rather than walked for ever.
	open []string
}

// walk reads the directory dir, whose path in the folder is at ("" for the folder itself).
func (w *walker) walk(dir, at string) *Fault {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return &Fault{Path: at, Cause: Unreadable, Detail: err.Error()}
	}
	w.open = append(w.open, dir)
	defer func() { w.open = w.open[:len(w.open)-1] }()
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, "..") {
			continue
		}
		file, full := path.Join(at, name), filepath.Join(dir, name)
		if entry.Type()&fs.ModeSymlink != 0 {
			target, err := filepath.EvalSymlinks(full)
			if err != nil {
				return &Fault{Path: file, Cause: Unreadable, Detail: err.Error()}
			}
			if rel, err := filepath.Rel(w.root, target); err != nil || !filepath.IsLocal(rel) {
				return &Fault{Path: file, Cause: LinkEscapes, Detail: "it leads to " + target}
			}
			full = target
		}
		info, err := os.Stat(full)
		if err != nil {
			return &Fault{Path: file, Cause: Unreadable, Detail: err.Error()}
		}
		if info.IsDir() {
			if slices.Contains(w.open, full) {
				return &Fault{Path: file, Cause: Unreadable, Detail: "it is a link to a directory that holds it"}
			}
			if fault := w.walk(full, file); fault != nil {
				return fault
			}
			continue
		}
		body, err := readRegular(full)
		if err != nil {
			cause := Unreadable
			if file == ManifestName {
				cause = ManifestUnreadable
			}
			return &Fault{Path: file, Cause: cause, Detail: err.Error()}
		}
		w.files[file] = body
	}
	return nil
}

// readRegular is the bytes of the regular file at file. The open does not wait for a writer, so a
// named pipe is refused at once rather than read.
func readRegular(file string) ([]byte, error) {
	f, err := os.OpenFile(file, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", file)
	}
	return io.ReadAll(f)
}

// WriteManifest writes dir/manifest.json recording digests, each a path of the folder with the
// digest of Legion's text it replaces (Digest). It is how a test or a proof cuts a folder.
func WriteManifest(dir string, digests map[string]string) error {
	body, err := json.MarshalIndent(Manifest{Files: digests}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, ManifestName), append(body, '\n'), 0o644)
}
