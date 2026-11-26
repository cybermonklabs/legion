package texts_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/texts"
)

// The ignored skills below the texts layer are what Oh My Pi reads without it, layered as Oh My
// Pi layers them (config/settings.ts at the .omp-pin tag): a higher layer's array replaces a lower
// one's whole, so the value is the highest layer's that sets it. The PI_CONFIG_FILES overlays are
// highest, the last that sets it first, a relative one read from the project; then the project's
// .omp/config.yml over its .omp/settings.json; then the profile's config.yml, or its config.yaml
// where config.yml is absent. A layer that sets the key to null, or replaces the whole skills
// group with no mapping, leaves Oh My Pi with its default, none; a project layer's skills that is
// no mapping Oh My Pi drops instead. A file Oh My Pi refuses to start on is an error; a project
// settings.json it cannot parse it skips.
func TestBaseIgnoredSkillsFollowsOhMyPisLayers(t *testing.T) {
	list := func(patterns ...string) string {
		return "skills:\n  ignoredSkills: [" + strings.Join(patterns, ", ") + "]\n"
	}
	other := "skills:\n  customDirectories: [/elsewhere]\n"
	for _, tc := range []struct {
		name string
		// files are the files of the case, by where they sit: "overlay-<n>.yml" (named in
		// PI_CONFIG_FILES in that order, absolute), "relative.yml" (named relative to the project),
		// "project/.omp/<file>" and "agent/<file>".
		files    map[string]string
		overlays []string
		want     []string
		err      string
		// missing is a case whose error is the one for an absent file (fs.ErrNotExist).
		missing bool
	}{
		{name: "no layer sets it", want: nil},
		{name: "the profile's config.yml", files: map[string]string{"agent/config.yml": list("global")}, want: []string{"global"}},
		{name: "the profile's config.yaml", files: map[string]string{"agent/config.yaml": list("global-yaml")}, want: []string{"global-yaml"}},
		{name: "a config.yml that does not set it hides the config.yaml",
			files: map[string]string{"agent/config.yml": other, "agent/config.yaml": list("global-yaml")}, want: nil},
		{name: "the project's settings.json over the profile",
			files: map[string]string{"agent/config.yml": list("global"), "project/.omp/settings.json": `{"skills": {"ignoredSkills": ["project-json"]}}`},
			want:  []string{"project-json"}},
		{name: "the project's config.yml over its settings.json",
			files: map[string]string{"agent/config.yml": list("global"), "project/.omp/settings.json": `{"skills": {"ignoredSkills": ["project-json"]}}`, "project/.omp/config.yml": list("project-yml")},
			want:  []string{"project-yml"}},
		{name: "a project config.yml that does not set it",
			files: map[string]string{"project/.omp/settings.json": `{"skills": {"ignoredSkills": ["project-json"]}}`, "project/.omp/config.yml": other},
			want:  []string{"project-json"}},
		{name: "a project skills that is no mapping is dropped",
			files: map[string]string{"agent/config.yml": list("global"), "project/.omp/config.yml": "skills: off\n"},
			want:  []string{"global"}},
		{name: "a project settings.json Oh My Pi cannot parse is skipped",
			files: map[string]string{"agent/config.yml": list("global"), "project/.omp/settings.json": `{"skills":`},
			want:  []string{"global"}},
		{name: "an overlay over the project",
			files:    map[string]string{"project/.omp/config.yml": list("project-yml"), "overlay-1.yml": list("operator")},
			overlays: []string{"overlay-1.yml"}, want: []string{"operator"}},
		{name: "the last overlay that sets it",
			files:    map[string]string{"overlay-1.yml": list("operator"), "overlay-2.yml": list("role", "arm-*"), "overlay-3.yml": other},
			overlays: []string{"overlay-1.yml", "overlay-2.yml", "overlay-3.yml"}, want: []string{"role", "arm-*"}},
		{name: "an overlay named relative to the project",
			files:    map[string]string{"project/relative.yml": list("relative"), "agent/config.yml": list("global")},
			overlays: []string{"relative.yml"}, want: []string{"relative"}},
		{name: "an overlay that sets it to null",
			files:    map[string]string{"agent/config.yml": list("global"), "overlay-1.yml": "skills:\n  ignoredSkills: null\n"},
			overlays: []string{"overlay-1.yml"}, want: nil},
		{name: "an overlay that sets it to an empty list",
			files:    map[string]string{"agent/config.yml": list("global"), "overlay-1.yml": list()},
			overlays: []string{"overlay-1.yml"}, want: []string{}},
		{name: "an overlay whose skills is no mapping",
			files:    map[string]string{"agent/config.yml": list("global"), "overlay-1.yml": "skills: null\n"},
			overlays: []string{"overlay-1.yml"}, want: nil},
		{name: "an empty overlay", files: map[string]string{"agent/config.yml": list("global"), "overlay-1.yml": ""},
			overlays: []string{"overlay-1.yml"}, want: []string{"global"}},
		{name: "a missing overlay", overlays: []string{"overlay-1.yml"}, err: "overlay-1.yml", missing: true},
		{name: "a malformed overlay", files: map[string]string{"overlay-1.yml": "skills: [\n"}, overlays: []string{"overlay-1.yml"}, err: "overlay-1.yml"},
		{name: "an overlay that is no mapping", files: map[string]string{"overlay-1.yml": "- a\n"}, overlays: []string{"overlay-1.yml"}, err: "overlay-1.yml"},
		{name: "a list holding no pattern", files: map[string]string{"overlay-1.yml": "skills:\n  ignoredSkills: [{a: 1}]\n"}, overlays: []string{"overlay-1.yml"}, err: "overlay-1.yml"},
		{name: "a malformed project config.yml", files: map[string]string{"project/.omp/config.yml": "skills: [\n"}, err: "config.yml"},
		{name: "a malformed profile config.yml", files: map[string]string{"agent/config.yml": "skills: [\n"}, err: "config.yml"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The case's files are served from a map under a directory that holds none of them, so a
			// read that does not go through the reader finds nothing.
			base := t.TempDir()
			project, agent := filepath.Join(base, "project"), filepath.Join(base, "agent")
			served := map[string]string{}
			for name, body := range tc.files {
				served[filepath.Join(base, filepath.FromSlash(name))] = body
			}
			read := func(path string) ([]byte, error) {
				body, ok := served[path]
				if !ok {
					return nil, &fs.PathError{Op: "open", Path: path, Err: fs.ErrNotExist}
				}
				return []byte(body), nil
			}
			var layers []string
			for _, overlay := range tc.overlays {
				if overlay == "relative.yml" {
					layers = append(layers, overlay)
				} else {
					layers = append(layers, filepath.Join(base, overlay))
				}
			}
			got, err := texts.BaseIgnoredSkills(layers, project, agent, read)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("BaseIgnoredSkills = %q, %v; want an error naming %s", got, err, tc.err)
				}
				if tc.missing && !errors.Is(err, fs.ErrNotExist) {
					t.Errorf("BaseIgnoredSkills = %v; want an error that is fs.ErrNotExist", err)
				}
				return
			}
			if err != nil || !slices.Equal(got, tc.want) {
				t.Errorf("BaseIgnoredSkills = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

// Every role of an issue pod writes the workspace a launch starts in, so a role can make each
// settings file there anything: a fifo, whose plain open waits for a writer for as long as none
// comes, or a link to /dev/zero, which a plain read holds whole in memory until its reader, the
// pod's launcher or the tmux daemon, is gone. Read with ReadSettingsFile, the reader both pass,
// either is an error naming the file, answered at once: at the project's .omp/config.yml, at its
// .omp/settings.json, and as an overlay named relative to the project. A link there to a regular
// file is read, as the links of an operator's ConfigMap volume are (the negative control). Each
// answer is waited for with no span of its own (testwait.Receive), so a read that waits fails by
// name before the test binary's deadline, and a read that is merely slow passes.
func TestBaseIgnoredSkillsReadsNoFifoOrDeviceARoleLeavesInTheWorkspace(t *testing.T) {
	for _, place := range []struct {
		name, file string
		overlays   []string
	}{
		{"the project's config.yml", ".omp/config.yml", nil},
		{"the project's settings.json", ".omp/settings.json", nil},
		{"an overlay named relative to the project", "relative.yml", []string{"relative.yml"}},
	} {
		for _, kind := range []struct {
			name    string
			plant   func(t *testing.T, path string)
			refused bool
		}{
			{"a fifo", func(t *testing.T, path string) {
				if err := syscall.Mkfifo(path, 0o644); err != nil {
					t.Fatal(err)
				}
			}, true},
			{"a link to /dev/zero", func(t *testing.T, path string) {
				if err := os.Symlink("/dev/zero", path); err != nil {
					t.Fatal(err)
				}
			}, true},
			{"a link to a regular file", func(t *testing.T, path string) {
				target := filepath.Join(t.TempDir(), "settings")
				writeFile(t, target, `{"skills": {"ignoredSkills": ["linked"]}}`)
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			}, false},
		} {
			t.Run(kind.name+" as "+place.name, func(t *testing.T) {
				base := t.TempDir()
				project, agent := filepath.Join(base, "project"), filepath.Join(base, "agent")
				path := filepath.Join(project, filepath.FromSlash(place.file))
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				kind.plant(t, path)
				type answer struct {
					ignored []string
					err     error
				}
				done := make(chan answer, 1)
				go func() {
					ignored, err := texts.BaseIgnoredSkills(place.overlays, project, agent, texts.ReadSettingsFile)
					done <- answer{ignored, err}
				}()
				got := testwait.Receive(t, "BaseIgnoredSkills over "+path, done)
				if kind.refused {
					if got.err == nil || !strings.Contains(got.err.Error(), path) {
						t.Fatalf("BaseIgnoredSkills over %s = %q, %v; want an error naming it", path, got.ignored, got.err)
					}
					return
				}
				if got.err != nil || !slices.Equal(got.ignored, []string{"linked"}) {
					t.Fatalf("BaseIgnoredSkills over a link at %s = %q, %v; want [linked], the file it names", path, got.ignored, got.err)
				}
			})
		}
	}
}

// The texts layer, the last of PI_CONFIG_FILES, sets skills.ignoredSkills to the value below it and
// the collision patterns of each replaced skill after it, so read as Oh My Pi layers it the value
// keeps the operator's and the arm's own list; and it sets nothing else, so every other skills
// setting of a lower layer still holds under Oh My Pi's deep merge.
func TestTheSettingsLayerKeepsTheIgnoredSkillsBelowIt(t *testing.T) {
	base := t.TempDir()
	project, agent, root := filepath.Join(base, "project"), filepath.Join(base, "agent"), filepath.Join(base, "root")
	for _, dir := range []string{project, agent} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	envoy, legion := plugins(t)
	built, err := texts.BuildRoot(texts.Folder{}, envoy, legion, base, "root")
	if err != nil {
		t.Fatal(err)
	}
	defer built.Close()
	writeFile(t, filepath.Join(agent, "config.yml"), "skills:\n  ignoredSkills: [global]\n")
	operator, arm := filepath.Join(base, "operator.yml"), filepath.Join(base, "arm.yml")
	writeFile(t, operator, "skills:\n  ignoredSkills: [operator-*]\n  customDirectories: [/operator/skills]\n")
	writeFile(t, arm, "modelRoles:\n  default: standin/model\n")
	layers := []string{operator, arm}

	below, err := texts.BaseIgnoredSkills(layers, project, agent, os.ReadFile)
	if err != nil || !slices.Equal(below, []string{"operator-*"}) {
		t.Fatalf("BaseIgnoredSkills = %q, %v; want the operator's [operator-*]", below, err)
	}
	layer, err := texts.WriteSettingsLayer(built, below, []string{"dispatch-first", "legion-worker"})
	if err != nil {
		t.Fatal(err)
	}
	if layer != filepath.Join(root, texts.SettingsLayerName) {
		t.Errorf("WriteSettingsLayer wrote %s, want %s", layer, filepath.Join(root, texts.SettingsLayerName))
	}
	want := []string{"operator-*", "*/dispatch-first", "*/dispatch-first~*", "*/legion-worker", "*/legion-worker~*"}
	if got, err := texts.BaseIgnoredSkills(append(slices.Clone(layers), layer), project, agent, os.ReadFile); err != nil || !slices.Equal(got, want) {
		t.Errorf("with the texts layer last, Oh My Pi reads skills.ignoredSkills as %q, %v; want %q", got, err, want)
	}
	body, err := os.ReadFile(layer)
	if err != nil {
		t.Fatal(err)
	}
	var settings map[string]map[string]any
	if err := yaml.Unmarshal(body, &settings); err != nil {
		t.Fatalf("the texts layer is no YAML Oh My Pi reads: %v\n%s", err, body)
	}
	if len(settings) != 1 || len(settings["skills"]) != 1 || settings["skills"]["ignoredSkills"] == nil {
		t.Errorf("the texts layer sets %v, want skills.ignoredSkills alone, so the operator's customDirectories still holds", settings)
	}

	// With nothing below and nothing replaced, the layer still sets an empty list rather than
	// null, so the profile's own list below it does not show through.
	bare, err := texts.BuildRoot(texts.Folder{}, envoy, legion, t.TempDir(), "root")
	if err != nil {
		t.Fatal(err)
	}
	defer bare.Close()
	empty, err := texts.WriteSettingsLayer(bare, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := texts.BaseIgnoredSkills([]string{empty}, project, agent, os.ReadFile); err != nil || len(got) != 0 {
		t.Errorf("an empty texts layer over the profile's [global] reads as %q, %v; want an empty list", got, err)
	}
}
