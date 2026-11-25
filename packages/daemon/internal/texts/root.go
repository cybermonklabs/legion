package texts

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Root is the extension root a launch of a folder loads before both of Legion's plugins.
type Root struct {
	// Dir holds agents/ and skills/ and no role part.
	Dir string
	// Touched are the skills whose files the folder holds, in order: the ones the settings layer
	// hides Legion's plugin copies of (WriteSettingsLayer).
	Touched []string
}

// BuildRoot builds the extension root of f in dest, a directory that is absent or empty. Its
// agents/ is every pi-legion agent with the folder's agents over them. Its skills/ is every
// pi-envoy skill, since pi-envoy gives that directory in place of its own, and each pi-legion skill
// whose files the folder holds, while pi-legion's other skills stay where the plugin keeps them;
// each is a copy of the plugin's skill directory with the folder's files over it, so a skill whose
// reference the folder replaces keeps its other files. Every file is a copy, never a link, so the
// root holds the bytes it was built from whatever later happens to the plugins or the folder. A
// folder file that replaces no plugin file is refused: CheckPlugins refuses it first, and a root
// holds no text Legion lacks.
func BuildRoot(f Folder, envoyRoot, legionRoot, dest string) (Root, error) {
	switch entries, err := os.ReadDir(dest); {
	case err == nil && len(entries) > 0:
		return Root{}, fmt.Errorf("build the texts root in %s: it already holds %s", dest, entries[0].Name())
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		return Root{}, fmt.Errorf("build the texts root in %s: %w", dest, err)
	}
	if err := copyDir(filepath.Join(legionRoot, "agents"), filepath.Join(dest, "agents"), nil); err != nil {
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
		if err := copyDir(src, filepath.Join(dest, "skills", skill.Name()), nil); err != nil {
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
		if err := copyDir(filepath.Join(skillsDir(plugin), skill), filepath.Join(dest, "skills", skill), nil); err != nil {
			return Root{}, err
		}
	}
	for _, p := range f.paths("agents/", "skills/") {
		file := filepath.Join(dest, filepath.FromSlash(p))
		if info, err := os.Lstat(file); err != nil || !info.Mode().IsRegular() {
			return Root{}, fmt.Errorf("build the texts root in %s: the folder %s holds %s, which replaces no file of Legion's plugins", dest, f.Dir, p)
		}
		if err := os.WriteFile(file, f.Files[p], 0o644); err != nil {
			return Root{}, fmt.Errorf("build the texts root in %s: %w", dest, err)
		}
	}
	return Root{Dir: dest, Touched: touched}, nil
}

// copyDir copies the directory src to dst, following each link in src to what it names, so dst
// holds regular files and directories only. open are the resolved directories the copy is inside,
// so a link back to one of them is refused rather than copied for ever.
func copyDir(src, dst string, open []string) error {
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
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return fmt.Errorf("copy %s: %w", src, err)
	}
	inside := append(open, resolved)
	for _, entry := range entries {
		from, to := filepath.Join(resolved, entry.Name()), filepath.Join(dst, entry.Name())
		info, err := os.Stat(from)
		if err != nil {
			return fmt.Errorf("copy %s: %w", from, err)
		}
		if info.IsDir() {
			err = copyDir(from, to, inside)
		} else {
			err = copyFile(from, to, info.Mode())
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// copyFile copies the regular file src to dst, a new file, keeping whether it is executable.
func copyFile(src, dst string, mode fs.FileMode) error {
	if !mode.IsRegular() {
		return fmt.Errorf("copy %s: it is not a regular file", src)
	}
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("copy %s: %w", src, err)
	}
	defer in.Close()
	perm := fs.FileMode(0o644)
	if mode&0o111 != 0 {
		perm = 0o755
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return fmt.Errorf("copy %s: %w", src, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return fmt.Errorf("copy %s to %s: %w", src, dst, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("copy %s to %s: %w", src, dst, err)
	}
	return nil
}
