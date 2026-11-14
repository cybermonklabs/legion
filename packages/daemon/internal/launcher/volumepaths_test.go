package launcher

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The four roots the trimmer may delete entries of, in the order it sweeps them, and what one
// entry of each is. The list is pinned here because it is a decision and not a derivation: each
// root was chosen for what the pod writes there and each rule for who names the entries, and a
// root added or a rule widened — $TMPDIR swept by immediate child, say, which is where a role's
// clones and databases live — is the edit that deletes a role's work. A test in
// internal/runtime/sandbox holds the same two lists to the renderer's paths, which is the other
// half: this case says what may be deleted, that one says nothing on the volume was forgotten.
func TestTheTrimmerSweepsTheFourCacheRootsInOrder(t *testing.T) {
	want := []ReclaimableRoot{
		{Path: "/legion/scratch/tmp", Entry: GoBuildTrees},
		{Path: "/legion/go/build", Entry: GoCacheEntries},
		{Path: "/legion/scratch/cache", Entry: ImmediateChildren, Except: []string{"ms-playwright", "legion-claude-code"}},
		{Path: "/legion/scratch/bun/install/cache", Entry: ImmediateChildren},
	}
	if len(ReclaimableRoots) != len(want) {
		t.Fatalf("the trimmer holds %d reclaimable roots, want the %d of this case: %+v", len(ReclaimableRoots), len(want), ReclaimableRoots)
	}
	for at, root := range ReclaimableRoots {
		if root.Path != want[at].Path || root.Entry != want[at].Entry || !slices.Equal(root.Except, want[at].Except) {
			t.Errorf("reclaimable root %d is %+v, want %+v", at, root, want[at])
		}
	}
}

// Every path of either list is an absolute, clean path on the pod's volume, and every exception
// is one base name. The trimmer joins an exception to its root and compares a listed entry's
// name against it, and it deletes by the path it built, so a path that is not clean, a path that
// is not on the volume, or an exception carrying a separator would each be a deletion somewhere
// the trimmer never meant to reach.
func TestEveryTrimmerPathIsAnAbsoluteCleanPathOfThePodsVolume(t *testing.T) {
	if VolumeRoot != "/legion" {
		t.Fatalf("VolumeRoot is %q, want the issue volume's mount point /legion (sandbox.TreeRoot)", VolumeRoot)
	}
	examined := 0
	check := func(what, path string) {
		examined++
		switch {
		case !filepath.IsAbs(path):
			t.Errorf("%s %q is not absolute", what, path)
		case filepath.Clean(path) != path:
			t.Errorf("%s %q is not clean: it would be deleted as %q", what, path, filepath.Clean(path))
		case !strings.HasPrefix(path, VolumeRoot+"/"):
			t.Errorf("%s %q is not on the pod's volume %s", what, path, VolumeRoot)
		}
	}
	for _, root := range ReclaimableRoots {
		check("the reclaimable root", root.Path)
		for _, name := range root.Except {
			examined++
			if name == "" || strings.ContainsRune(name, filepath.Separator) || name != filepath.Base(name) {
				t.Errorf("the exception %q of %s is not one base name", name, root.Path)
			}
		}
	}
	for _, path := range ProtectedPaths {
		check("the protected path", path)
	}
	if examined == 0 {
		t.Fatal("the case examined no path at all: both lists are empty")
	}
	if want := len(ReclaimableRoots) + len(ProtectedPaths) + 2; examined != want {
		t.Errorf("the case examined %d paths, want the %d of the two lists and the cache home's two exceptions", examined, want)
	}
}

// The two lists never overlap and never nest the wrong way: no reclaimable root is a protected
// path or lies under one, since the trimmer would then delete inside a directory the other list
// says it must not touch; and no reclaimable root lies under another, since one entry would then
// be judged by two rules. What does nest is a protected path under a reclaimable root, and only
// as that root's own exception: the Playwright registry and the Claude Code npm cache under the
// cache home. Each exception is named in both places, so a reader of either list alone sees them
// kept.
func TestTheProtectedPathsAndTheReclaimableRootsNestOnlyAsExceptions(t *testing.T) {
	under := func(path, ancestor string) bool { return strings.HasPrefix(path, ancestor+"/") }
	pairs := 0
	for at, root := range ReclaimableRoots {
		for _, other := range ReclaimableRoots[at+1:] {
			pairs++
			if root.Path == other.Path || under(root.Path, other.Path) || under(other.Path, root.Path) {
				t.Errorf("the reclaimable roots %s and %s are one inside the other, so an entry would be judged by two rules", root.Path, other.Path)
			}
		}
		for _, protected := range ProtectedPaths {
			pairs++
			if root.Path == protected || under(root.Path, protected) {
				t.Errorf("the reclaimable root %s is protected by %s, so the trimmer would delete inside a path the other list keeps", root.Path, protected)
			}
			if !under(protected, root.Path) {
				continue
			}
			name, _, nested := strings.Cut(strings.TrimPrefix(protected, root.Path+"/"), "/")
			if nested || !slices.Contains(root.Except, name) {
				t.Errorf("the protected path %s lies under the reclaimable root %s without being one of its exceptions %v", protected, root.Path, root.Except)
			}
		}
		for _, name := range root.Except {
			pairs++
			if kept := root.Path + "/" + name; !slices.Contains(ProtectedPaths, kept) {
				t.Errorf("the exception %s of %s is not in ProtectedPaths, where a reader of that list alone would look for it", kept, root.Path)
			}
		}
	}
	if pairs == 0 {
		t.Fatal("the case compared no pair of paths at all")
	}
}
