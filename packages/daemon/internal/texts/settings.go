package texts

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/sjawhar/legion/daemon/internal/rolefile"
)

// HidingPatterns are the skills.ignoredSkills patterns that remove every copy of skill Oh My Pi
// offers under a collision name, and never the plain name the root's copy keeps: a collision name
// is `<namespace>/<skill>`, or `<namespace>/<skill>~<n>` where that name is taken too, so it holds
// exactly one `/`, which `*` does not cross, and a plain name holds none (Oh My Pi refuses a raw
// skill name with `/`).
func HidingPatterns(skill string) []string {
	return []string{"*/" + skill, "*/" + skill + "~*"}
}

// WriteSettingsLayer writes texts-settings.yml in root, the last settings layer of a launch of a
// folder, and returns its path in root.Dir. The file is made new through the root's directory as
// BuildRoot left it open, never through root.Dir's path (rolefile.CreateRegular), so a link a role
// put at the file's name, or in place of the root, is never written through. It sets
// skills.ignoredSkills to base, the value Oh My Pi reads without the layer (BaseIgnoredSkills),
// and after it the HidingPatterns of each of skills, the skills whose files the folder holds
// (Root.Touched): Oh My Pi's arrays are replaced whole by a higher layer, so a layer of the
// patterns alone would erase the operator's, a role's, an arm's or a repository's own list. It
// sets nothing else, so every other skills setting of a lower layer still holds.
func WriteSettingsLayer(root Root, base, skills []string) (string, error) {
	ignored := slices.Clone(base)
	if ignored == nil {
		ignored = []string{}
	}
	for _, skill := range skills {
		ignored = append(ignored, HidingPatterns(skill)...)
	}
	body, err := yaml.Marshal(map[string]map[string][]string{"skills": {"ignoredSkills": ignored}})
	if err != nil {
		return "", err
	}
	file := filepath.Join(root.Dir, SettingsLayerName)
	header := "# Written by Legion for a launch of an experiment arm's folder of texts: the ignored skills of\n" +
		"# the layers below, then the collision names of each skill the folder replaces.\n"
	out, err := rolefile.CreateRegular(int(root.dir.Fd()), SettingsLayerName, file, 0o644)
	if err != nil {
		return "", fmt.Errorf("write the texts settings layer %s: %w", file, err)
	}
	_, err = out.Write(append([]byte(header), body...))
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return "", fmt.Errorf("write the texts settings layer %s: %w", file, err)
	}
	return file, nil
}

// BaseIgnoredSkills is skills.ignoredSkills as Oh My Pi would read it for a launch without the
// texts settings layer: layers are the launch's PI_CONFIG_FILES in order, projectDir the directory
// it starts in, and agentDir its profile's agent directory. Oh My Pi merges its settings layer over
// layer and replaces an array whole (config/settings.ts, #deepMerge), so the value is the highest
// layer's that sets it: the last of layers that does, each a path relative to projectDir unless
// absolute; else the project's, .omp/config.yml over .omp/settings.json; else the profile's
// config.yml, or config.yaml where config.yml is absent; else none. A layer that sets it to a value
// Oh My Pi does not read as a list (null, or a skills that is no mapping in a layer that replaces
// the whole group) leaves Oh My Pi with its default, none. An overlay that is missing or malformed,
// and a project config.yml or profile file that is malformed, are errors, since Oh My Pi refuses to
// start on them; a malformed project settings.json Oh My Pi skips, and so does this.
func BaseIgnoredSkills(layers []string, projectDir, agentDir string) ([]string, error) {
	for i := len(layers) - 1; i >= 0; i-- {
		file := layers[i]
		if strings.HasPrefix(file, "~") {
			return nil, fmt.Errorf("PI_CONFIG_FILES names %s, whose home this launch cannot resolve", file)
		}
		if !filepath.IsAbs(file) {
			file = filepath.Join(projectDir, file)
		}
		raw, err := os.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("read the settings overlay %s: %w", file, err)
		}
		layer, err := yamlLayer(file, raw)
		if err != nil {
			return nil, err
		}
		if value, set, err := ignoredSkills(file, layer, false); set || err != nil {
			return value, err
		}
	}

	project := filepath.Join(projectDir, ".omp")
	if raw, found, err := readIfPresent(filepath.Join(project, "config.yml")); err != nil {
		return nil, err
	} else if found {
		layer, err := yamlLayer(filepath.Join(project, "config.yml"), raw)
		if err != nil {
			return nil, err
		}
		if value, set, err := ignoredSkills(filepath.Join(project, "config.yml"), layer, true); set || err != nil {
			return value, err
		}
	}
	if raw, found, err := readIfPresent(filepath.Join(project, "settings.json")); err != nil {
		return nil, err
	} else if found {
		var layer map[string]any
		if json.Unmarshal(raw, &layer) == nil {
			if value, set, err := ignoredSkills(filepath.Join(project, "settings.json"), layer, true); set || err != nil {
				return value, err
			}
		}
	}

	for _, name := range []string{"config.yml", "config.yaml"} {
		file := filepath.Join(agentDir, name)
		raw, found, err := readIfPresent(file)
		if err != nil {
			return nil, err
		}
		if !found {
			continue
		}
		layer, err := yamlLayer(file, raw)
		if err != nil {
			return nil, err
		}
		value, _, err := ignoredSkills(file, layer, false)
		return value, err
	}
	return nil, nil
}

// readIfPresent is file's bytes and true, or false where there is no such file.
func readIfPresent(file string) (raw []byte, found bool, err error) {
	raw, err = os.ReadFile(file)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read the settings %s: %w", file, err)
	}
	return raw, true, nil
}

// yamlLayer is the settings file at file, whose bytes are raw, as a mapping: an empty document is
// an empty one, and any other document that is no mapping is refused, as Oh My Pi refuses it.
func yamlLayer(file string, raw []byte) (map[string]any, error) {
	var doc any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse the settings %s: %w", file, err)
	}
	if doc == nil {
		return map[string]any{}, nil
	}
	layer, ok := doc.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("the settings %s hold no mapping at the document root", file)
	}
	return layer, nil
}

// ignoredSkills is the skills.ignoredSkills the settings layer read from file sets, and whether it
// sets one. A layer whose skills is no mapping replaces the whole group with nothing, unless it is a
// project layer, which Oh My Pi reads without such a value (dropSettingsGroupShadows). A value
// that is no list is ignored by Oh My Pi for its default, none; a list must hold strings alone.
func ignoredSkills(file string, layer map[string]any, project bool) ([]string, bool, error) {
	group, present := layer["skills"]
	if !present {
		return nil, false, nil
	}
	skills, ok := group.(map[string]any)
	if !ok {
		return nil, !project, nil
	}
	value, present := skills["ignoredSkills"]
	if !present {
		return nil, false, nil
	}
	list, ok := value.([]any)
	if !ok {
		return nil, true, nil
	}
	patterns := make([]string, 0, len(list))
	for _, item := range list {
		pattern, ok := item.(string)
		if !ok {
			return nil, true, fmt.Errorf("the settings %s set skills.ignoredSkills to a list holding %v, which is no pattern", file, item)
		}
		patterns = append(patterns, pattern)
	}
	return patterns, true, nil
}
