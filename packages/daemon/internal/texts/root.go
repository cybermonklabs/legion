package texts

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/sjawhar/legion/daemon/internal/rolefile"
)

// Root is the extension root a launch of a folder loads before both of Legion's plugins.
type Root struct {
	// Dir holds agents/ and skills/ and no role part.
	Dir string
	// Touched are the skills whose files the folder holds, in order: the ones the settings layer
	// hides Legion's plugin copies of (WriteSettingsLayer).
	Touched []string
	// dir is the directory BuildRoot made at Dir, open until Close: the settings layer is written
	// through it, never through Dir's path, which a role may have pointed elsewhere since.
	dir *os.File
}

// Close closes the root's directory, which BuildRoot leaves open for WriteSettingsLayer.
func (r Root) Close() error { return r.dir.Close() }

// BuildRoot builds the extension root of f at rel, a clean slash-separated path under base, and
// answers it with its directory open (Close). base is a directory its caller trusts, and must not
// be a link itself; each element of rel is made where it is absent, and the root, rel's last
// element, must be absent or empty. Its agents/ is every pi-legion agent with the folder's agents
// in place of theirs. Its skills/ is every pi-envoy skill, since pi-envoy gives that directory in
// place of its own, and each pi-legion skill whose files the folder holds, while pi-legion's other
// skills stay where the plugin keeps them; each is a copy of the plugin's skill directory with the
// folder's files in place of the plugin's at the same paths, so a skill whose reference the folder
// replaces keeps its other files. Every file is a copy, never a link, so the root holds the bytes
// it was built from whatever later happens to the plugins or the folder. A folder file that
// replaces no plugin file is refused: CheckPlugins refuses it first, and a root holds no text
// Legion lacks.
//
// The root is made through directory descriptors alone, since a role can write where it is built:
// base is opened as rolefile.RemoveTree opens a parent, and each directory below it is made or
// opened, and each file made, under the descriptor of the directory that holds it
// (rolefile.MakeDirectory, rolefile.CreateRegular). So the build makes and writes nothing where a
// link points, whenever a role put it there: a link at base, at an element of rel, or at a path in
// the root the build has yet to make is refused in words naming that path and what is there. The
// plugins' own directories, which the image owns, are read following their links.
func BuildRoot(f Folder, envoyRoot, legionRoot, base, rel string) (Root, error) {
	base = filepath.Clean(base)
	parent, err := rolefile.OpenDirectory(unix.AT_FDCWD, base, base)
	if err != nil {
		return Root{}, fmt.Errorf("build the texts root in %s: %w", filepath.Join(base, filepath.FromSlash(rel)), err)
	}
	defer unix.Close(parent)
	return BuildRootAt(f, envoyRoot, legionRoot, parent, base, rel)
}

// BuildRootAt is BuildRoot under the directory dirfd, which its caller holds open, having reached
// it following no link, and which is base as the errors and the root's Dir print it: base's path
// is never opened, so whatever a role has done to that path since, the root is made under the
// directory dirfd is. dirfd stays open, the caller's to close.
func BuildRootAt(f Folder, envoyRoot, legionRoot string, dirfd int, base, rel string) (_ Root, err error) {
	dest := filepath.Join(base, filepath.FromSlash(rel))
	dir, err := openRoot(dirfd, base, rel, dest)
	if err != nil {
		return Root{}, fmt.Errorf("build the texts root in %s: %w", dest, err)
	}
	defer func() {
		if err != nil {
			_ = dir.Close()
		}
	}()
	root := int(dir.Fd())
	c := &rootCopy{dest: dest, folder: f.Dir, pending: map[string][]byte{}}
	for _, p := range f.paths("agents/", "skills/") {
		c.pending[p] = f.Files[p]
	}
	if err := c.copyDir(filepath.Join(legionRoot, "agents"), root, "", "agents", nil); err != nil {
		return Root{}, err
	}
	envoySkills, err := os.ReadDir(skillsDir(envoyRoot))
	if err != nil {
		return Root{}, fmt.Errorf("build the texts root in %s: read pi-envoy's skills: %w", dest, err)
	}
	for _, skill := range envoySkills {
		src := filepath.Join(skillsDir(envoyRoot), skill.Name())
		if info, err := os.Stat(src); err != nil || !info.IsDir() {
			continue
		}
		if err := c.copyDir(src, root, "", "skills/"+skill.Name(), nil); err != nil {
			return Root{}, err
		}
	}
	var touched []string
	for _, p := range f.paths("skills/") {
		skill, _, _ := strings.Cut(strings.TrimPrefix(p, "skills/"), "/")
		if !slices.Contains(touched, skill) {
			touched = append(touched, skill)
		}
	}
	for _, skill := range touched {
		plugin, ok := skillPlugin(skill, envoyRoot, legionRoot)
		if !ok {
			return Root{}, fmt.Errorf("build the texts root in %s: neither plugin ships the skill %s that the folder %s replaces files of", dest, skill, f.Dir)
		}
		if plugin == envoyRoot {
			continue
		}
		if err := c.copyDir(filepath.Join(skillsDir(plugin), skill), root, "", "skills/"+skill, nil); err != nil {
			return Root{}, err
		}
	}
	for _, p := range f.paths("agents/", "skills/") {
		if _, left := c.pending[p]; left {
			return Root{}, fmt.Errorf("build the texts root in %s: the folder %s holds %s, which replaces no file of Legion's plugins", dest, f.Dir, p)
		}
	}
	return Root{Dir: dest, Touched: touched, dir: dir}, nil
}

// openRoot is the directory at rel under the directory dirfd, which is base, dest, made where it
// is absent, for a build: each element of rel is made or opened under the one before it
// (makeDirs), so a link at any of them is refused. A root that already holds anything is refused,
// naming the first entry and what it is.
func openRoot(dirfd int, base, rel, dest string) (*os.File, error) {
	if !fs.ValidPath(rel) || rel == "." {
		return nil, fmt.Errorf("%q is no clean relative path under %s", rel, base)
	}
	fd, err := makeDirs(dirfd, base, rel)
	if err != nil {
		return nil, err
	}
	dir := os.NewFile(uintptr(fd), dest)
	entries, err := dir.ReadDir(1)
	switch {
	case len(entries) > 0:
		err = fmt.Errorf("it already holds %s, a %s", filepath.Join(dest, entries[0].Name()), rolefile.Kind(entries[0].Type()))
	case errors.Is(err, io.EOF):
		return dir, nil
	}
	_ = dir.Close()
	return nil, err
}

// makeDirs is a descriptor of the directory at rel, a slash-separated path under the directory
// dirfd at at, made where it is absent: each element is made or opened under the descriptor of the
// one before it (rolefile.MakeDirectory), so a link at any of them is refused rather than followed.
// dirfd stays open, and the descriptor answered is the caller's to close.
func makeDirs(dirfd int, at, rel string) (int, error) {
	fd := dirfd
	for _, name := range strings.Split(rel, "/") {
		at = filepath.Join(at, name)
		next, err := rolefile.MakeDirectory(fd, name, at, 0o755)
		if fd != dirfd {
			_ = unix.Close(fd)
		}
		if err != nil {
			return -1, err
		}
		fd = next
	}
	return fd, nil
}

// rootCopy is one build's copy of the plugins into its root at dest: folder is the folder of
// texts, and pending its agent and skill files the copy has yet to write, by their paths there,
// which are their paths in the root too.
type rootCopy struct {
	dest, folder string
	pending      map[string][]byte
}

// path is the root's entry at rel, a slash-separated path under it, as the errors print it.
func (c *rootCopy) path(rel string) string { return filepath.Join(c.dest, filepath.FromSlash(rel)) }

// copyDir copies the directory src to name, a slash-separated path under the directory parent,
// which is under in the root, following each link in src to what it names, so the root holds
// regular files and directories only. Each directory is made, and each file created, under the
// descriptor of the directory that holds it (makeDirs, copyFile). open are the resolved
// directories the copy is inside, so a link back to one of them is refused rather than copied for
// ever.
func (c *rootCopy) copyDir(src string, parent int, under, name string, open []string) error {
	resolved, err := filepath.EvalSymlinks(src)
	if err != nil {
		return fmt.Errorf("copy %s: %w", src, err)
	}
	if slices.Contains(open, resolved) {
		return fmt.Errorf("copy %s: it is a link to a directory that holds it", src)
	}
	entries, err := os.ReadDir(resolved)
	if err != nil {
		return fmt.Errorf("copy %s: %w", src, err)
	}
	dir, err := makeDirs(parent, c.path(under), name)
	if err != nil {
		return fmt.Errorf("copy %s: %w", src, err)
	}
	defer unix.Close(dir)
	rel := path.Join(under, name)
	inside := append(open, resolved)
	for _, entry := range entries {
		from := filepath.Join(resolved, entry.Name())
		info, err := os.Stat(from)
		if err != nil {
			return fmt.Errorf("copy %s: %w", from, err)
		}
		if info.IsDir() {
			err = c.copyDir(from, dir, rel, entry.Name(), inside)
		} else {
			err = c.copyFile(from, info.Mode(), dir, path.Join(rel, entry.Name()))
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// copyFile makes rel, a new file of the root in the directory dir, holding the bytes of src, a
// regular file whose mode is mode, or the folder's file at rel where the folder holds one, and
// keeping whether src is executable. The file is created under dir, and whatever is at its name
// already is refused (rolefile.CreateRegular).
func (c *rootCopy) copyFile(src string, mode fs.FileMode, dir int, rel string) error {
	if !mode.IsRegular() {
		return fmt.Errorf("copy %s: it is not a regular file", src)
	}
	var in io.Reader
	if body, over := c.pending[rel]; over {
		src, in = filepath.Join(c.folder, filepath.FromSlash(rel)), bytes.NewReader(body)
	} else {
		file, err := os.Open(src)
		if err != nil {
			return fmt.Errorf("copy %s: %w", src, err)
		}
		defer file.Close()
		in = file
	}
	perm := uint32(0o644)
	if mode&0o111 != 0 {
		perm = 0o755
	}
	dst := c.path(rel)
	out, err := rolefile.CreateRegular(dir, path.Base(rel), dst, perm)
	if err != nil {
		return fmt.Errorf("copy %s: %w", src, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return fmt.Errorf("copy %s to %s: %w", src, dst, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("copy %s to %s: %w", src, dst, err)
	}
	delete(c.pending, rel)
	return nil
}
