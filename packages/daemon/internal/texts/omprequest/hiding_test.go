package omprequest

import (
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/texts"
)

// The texts layer hides the plugin copies of each skill the folder replaces with
// texts.HidingPatterns, which Oh My Pi matches with Bun.Glob, whose `*`, like Go's path.Match's,
// crosses no `/` (the real Oh My Pi subtest TestATextsRootOnTheRealOhMyPi holds the two to agreement).
// Held to every skill name the captured requests list: each collision name, `<namespace>/<skill>`
// and the `~<n>` form Oh My Pi gives a namespaced name that is itself taken, is matched by its own
// skill's patterns and by no other skill's, and no plain name is matched by any skill's patterns,
// so a hidden skill's root copy, which keeps the plain name, is never hidden. Beside the alias
// the first fixture shows, each plain name gets the collision names Oh My Pi would give it in the
// namespace that alias has, so names that share a prefix (dispatch, dispatch-first,
// dispatch-writing-specs) are held apart. The patterns are the ones the second fixture was
// captured with (testdata/README.md), which removed the alias there.
func TestTheHidingPatternsMatchEveryCollisionNameAndNoPlainName(t *testing.T) {
	if got, want := texts.HidingPatterns("legion-worker"), []string{"*/legion-worker", "*/legion-worker~*"}; !slices.Equal(got, want) {
		t.Fatalf("HidingPatterns(legion-worker) = %q, want %q, the layer the second fixture was captured with", got, want)
	}
	files, err := filepath.Glob(filepath.Join("testdata", "requests", "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no captured requests under testdata/requests: %v", err)
	}
	var plain, collisions []string
	for _, file := range files {
		body, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		request, err := Read(body)
		if err != nil {
			t.Fatalf("Read(%s) = %v", file, err)
		}
		for _, skill := range request.Skills {
			switch strings.Count(skill.Name, "/") {
			case 0:
				if !slices.Contains(plain, skill.Name) {
					plain = append(plain, skill.Name)
				}
			case 1:
				collisions = append(collisions, skill.Name)
			default:
				t.Errorf("%s lists the skill %q, with more than one `/`: no collision name Oh My Pi gives has more", file, skill.Name)
			}
		}
	}
	if len(collisions) == 0 {
		t.Fatal("no captured request lists a collision name: the first fixture's pi-legion/legion-worker alias is what this test reads")
	}
	// The collision names of every plain name, and the `~<n>` form of each, which no case of the
	// fixtures made.
	namespace := strings.SplitN(collisions[0], "/", 2)[0]
	for _, name := range plain {
		collisions = append(collisions, namespace+"/"+name)
	}
	for _, name := range slices.Clone(collisions) {
		collisions = append(collisions, name+"~2")
	}
	for _, name := range collisions {
		namespaced := strings.SplitN(name, "/", 2)[1]
		own, _, _ := strings.Cut(namespaced, "~")
		for _, skill := range plain {
			matched := false
			for _, pattern := range texts.HidingPatterns(skill) {
				if ok, err := path.Match(pattern, name); err != nil {
					t.Fatalf("path.Match(%q, %q) = %v", pattern, name, err)
				} else if ok {
					matched = true
				}
			}
			if matched != (skill == own) {
				t.Errorf("HidingPatterns(%s) match the collision name %s = %t, want %t: a skill's patterns hide its own collision names and no other skill's", skill, name, matched, skill == own)
			}
		}
	}
	for _, skill := range plain {
		for _, pattern := range texts.HidingPatterns(skill) {
			for _, name := range plain {
				if ok, _ := path.Match(pattern, name); ok {
					t.Errorf("the pattern %q of %s hides the plain name %s, which the root's copy keeps", pattern, skill, name)
				}
			}
		}
	}
}
