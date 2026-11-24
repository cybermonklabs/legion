package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// tsCensus runs scripts/redact-gate/ts-census.ts on dir: the TypeScript half of the call census,
// and the byte range of each function of a changed file, which a Bun row's binding reads.
func (g *gate) tsCensus(dir string) census {
	c := census{}
	files, err := g.changed(dir, func(n string) bool {
		return strings.HasPrefix(n, "packages/") && strings.Contains(n, "/src/") && strings.HasSuffix(n, ".ts") && !strings.HasSuffix(n, ".test.ts") && !strings.Contains(n, "/__tests__/") && !strings.HasPrefix(n, "packages/test-fixtures/")
	})
	if err != nil {
		c.Unknown = append(c.Unknown, "TypeScript: "+err.Error())
		return c
	}
	if len(files) == 0 {
		return c
	}
	request := map[string]any{"root": dir, "files": files, "rule": g.inv.Gate.TSRule}
	data, _ := json.Marshal(request)
	// The snapshot's own script, run where the snapshot's node_modules are, so `typescript`
	// resolves to the workspace's pinned compiler. From a tree with no node_modules Bun would
	// install the latest one instead, whose default `types` differ, and every file would be
	// UNKNOWN for a reason that is not the code's.
	cmd := exec.Command("bun", "--no-install", filepath.Join(dir, "packages/envoy/scripts/redact-gate/ts-census.ts"))
	cmd.Dir = dir
	cmd.Stdin = bytes.NewReader(data)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		c.Unknown = append(c.Unknown, fmt.Sprintf("TypeScript: the census did not run: %v: %s", err, lastLines(stderr.String(), 5)))
		return c
	}
	if err := json.Unmarshal(out, &c); err != nil {
		c.Unknown = append(c.Unknown, "TypeScript: the census answered no JSON: "+err.Error())
	}
	return c
}

// tsStructural runs scripts/redact-gate/ts-structural.ts on dir: the TypeScript box and holder
// censuses, over every production file of each workspace package that can name the box (each
// that depends on the rule's package, and that package).
func (g *gate) tsStructural(dir string) census {
	c := census{}
	box := g.inv.Gate.Box
	home := strings.Join(strings.SplitN(box.TSFile, "/", 3)[:2], "/") // packages/<name>
	manifest, err := os.ReadFile(filepath.Join(dir, home, "package.json"))
	var self struct {
		Name string `json:"name"`
	}
	if err != nil || json.Unmarshal(manifest, &self) != nil || self.Name == "" {
		c.Unknown = append(c.Unknown, "TypeScript: the rule's package "+home+" has no readable package.json")
		return c
	}
	packages := []string{home}
	entries, err := os.ReadDir(filepath.Join(dir, "packages"))
	if err != nil {
		c.Unknown = append(c.Unknown, "TypeScript: "+err.Error())
		return c
	}
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(dir, "packages", e.Name(), "package.json"))
		if err != nil {
			continue
		}
		var p struct {
			Dependencies    map[string]string `json:"dependencies"`
			DevDependencies map[string]string `json:"devDependencies"`
		}
		if json.Unmarshal(data, &p) != nil {
			c.Unknown = append(c.Unknown, "TypeScript: packages/"+e.Name()+"/package.json cannot be parsed")
			continue
		}
		if _, ok := p.Dependencies[self.Name]; ok {
			packages = append(packages, "packages/"+e.Name())
		} else if _, ok := p.DevDependencies[self.Name]; ok {
			packages = append(packages, "packages/"+e.Name())
		}
	}
	sort.Strings(packages[1:])
	request := map[string]any{
		"root":     dir,
		"box":      map[string]any{"file": box.TSFile, "class": box.TSClass, "rule": box.TSRule, "comparisons": []string{"startsWith", "endsWith", "includes"}, "parses": orEmpty(box.TSParses)},
		"packages": packages,
	}
	data, _ := json.Marshal(request)
	cmd := exec.Command("bun", "--no-install", filepath.Join(dir, "packages/envoy/scripts/redact-gate/ts-structural.ts"))
	cmd.Dir = dir
	cmd.Stdin = bytes.NewReader(data)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		c.Unknown = append(c.Unknown, fmt.Sprintf("TypeScript: the structural census did not run: %v: %s", err, lastLines(stderr.String(), 5)))
		return c
	}
	if err := json.Unmarshal(out, &c); err != nil {
		c.Unknown = append(c.Unknown, "TypeScript: the structural census answered no JSON: "+err.Error())
	}
	return c
}

// orEmpty is list, or an empty list for none, so the request's JSON holds an array.
func orEmpty(list []string) []string {
	if list == nil {
		return []string{}
	}
	return list
}
