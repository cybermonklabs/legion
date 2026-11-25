package main

import (
	"bytes"
	"context"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/prompts"
	"github.com/sjawhar/legion/daemon/internal/runtime/sandbox"
	"github.com/sjawhar/legion/daemon/internal/texts"
)

// textsArmKey is the key of the arm every one-folder configuration below and make-cases.sh's
// stands under: projects.DEMO's experiment `shorter`, its arm `folder`.
const textsArmKey = "projects.DEMO.experiments.shorter.variants.folder"

// skipUnderTreeRoot skips a case that mounts a folder under dir where dir is under the directory
// every Legion pod mounts for itself (sandbox.TreeRoot), as an issue pod's TMPDIR is: the check
// refuses an operator's mount there before it reads any folder, so the case can show nothing.
func skipUnderTreeRoot(t *testing.T, dir string) {
	t.Helper()
	if strings.HasPrefix(dir, sandbox.TreeRoot+"/") {
		t.Skipf("the case mounts a folder under %s, which is under %s: every Legion pod mounts %s for itself, so the check refuses an operator's mount there; run with a TMPDIR outside it",
			dir, sandbox.TreeRoot, sandbox.TreeRoot)
	}
}

// `legion config check` over each configuration make-cases.sh writes, the one writer of them for
// this test and for the check run by hand: the six it refuses, each refusal naming its cause and
// what it is about (the folder, the file, the role, or both experiments), and the two it accepts,
// a good folder under kubernetes inside the pods' mounts and two experiments whose folders reach
// the reviewer and the architect. The two it accepts mount a folder under the test's own
// temporary directory, and are skipped where that is under /legion (skipUnderTreeRoot).
func TestConfigCheckRefusesEachBadTextsFolder(t *testing.T) {
	legionState(t)
	dir := t.TempDir()
	if out, err := exec.Command("bash", filepath.Join("testdata", "texts", "make-cases.sh"), dir).CombinedOutput(); err != nil {
		t.Fatalf("make-cases.sh %s: %v\n%s", dir, err, out)
	}
	// says is, for each case, the parts of the refusal it names; none for a case the check accepts.
	says := map[string][]string{
		"gone":                        {textsArmKey + ".texts: texts folder " + dir + "/gone/texts: the folder does not exist [gone]"},
		"no-manifest":                 {textsArmKey + ".texts: texts folder " + dir + "/no-manifest/texts: manifest.json: the folder holds no manifest.json [no-manifest]"},
		"outside-mounts":              {textsArmKey + ".texts " + dir + "/outside-mounts/texts is under no runtime.kubernetes.pod.volume_mounts mount_path: "},
		"unreached-part":              {textsArmKey + ".texts: texts folder " + dir + "/unreached-part/texts: roles/planner.md: no role the experiment reaches reads Legion's text at this path [unread]"},
		"claude-role":                 {textsArmKey + " names texts, and worker_backends.reviewer is claude, which reads no folder of texts: "},
		"two-folders-one-role":        {"projects.DEMO.experiments.first-texts and projects.DEMO.experiments.second-texts each give a folder of texts to the reviewer, "},
		"inside-mounts":               nil,
		"two-folders-different-roles": nil,
	}
	written, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(written))
	for _, entry := range written {
		names = append(names, entry.Name())
	}
	if want := slices.Sorted(maps.Keys(says)); !slices.Equal(names, want) {
		t.Fatalf("make-cases.sh wrote %q, want the cases %q", names, want)
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			if says[name] == nil {
				skipUnderTreeRoot(t, dir)
			}
			var out, errb bytes.Buffer
			code := run(context.Background(), []string{"legion", "config", "check", "--config", filepath.Join(dir, name, "legion.yaml")}, &out, &errb)
			t.Logf("legion config check exited %d\nstdout:\n%s\nstderr:\n%s", code, out.String(), errb.String())
			if says[name] == nil {
				if code != 0 || !strings.HasPrefix(out.String(), "Config OK: project=DEMO ") || errb.Len() != 0 {
					t.Fatalf("legion config check = %d, stdout %q, stderr %q; want 0 and the Config OK line", code, out.String(), errb.String())
				}
				return
			}
			refusal := errb.String()
			if code != 1 || out.Len() != 0 || !strings.HasPrefix(refusal, "legion config check: ") {
				t.Fatalf("legion config check = %d, stdout %q, stderr %q; want 1 and one refusal", code, out.String(), refusal)
			}
			for _, part := range says[name] {
				if !strings.Contains(refusal, part) {
					t.Errorf("the refusal %q does not say %q", refusal, part)
				}
			}
		})
	}
}

// textsFolder writes a folder of texts in a directory of the test's own and answers its path: a
// text of its own at each of written, Legion's paths or others, and a manifest listing each of
// listed, Legion's paths, with the digest of Legion's text at it, as make-cases.sh cuts one.
func textsFolder(t *testing.T, written, listed []string) string {
	t.Helper()
	folder := filepath.Join(t.TempDir(), "texts")
	for _, path := range written {
		file := filepath.Join(folder, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte("This folder's text in place of Legion's "+path+".\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	digests := map[string]string{}
	for _, path := range listed {
		body, err := fs.ReadFile(prompts.Texts(), path)
		if err != nil {
			t.Fatalf("read Legion's %s: %v", path, err)
		}
		digests[path] = texts.Digest(body)
	}
	if err := texts.WriteManifest(folder, digests); err != nil {
		t.Fatal(err)
	}
	return folder
}

// textsArmLines are refusalFile's lines of projects.DEMO declaring the experiment `shorter`, of
// the roles roles (every issue role where it is ""), whose arm `folder` names folder beside a
// control.
func textsArmLines(roles, folder string) string {
	if roles != "" {
		roles = ", roles: " + roles
	}
	return "    experiments: {shorter: {mode: random" + roles + ", variants: [{name: folder, texts: " + folder + "}, {name: control}]}}\n"
}

// reviewerParts are the reviewer's two role parts, the parts of the good folder.
var reviewerParts = []string{"roles/reviewer.md", "go/reviewer.md"}

// textsRefusal is the refusal of a configuration whose arm reaches the reviewer and names folder.
func textsRefusal(t *testing.T, folder string) string {
	t.Helper()
	return refusalFile{project: textsArmLines("[reviewer]", folder)}.refuse(t)
}

// textsRefusals is every refusal `legion config check` makes of an arm's folder of texts, the
// loader's and the boot's, by what it refuses: each cause boot refuses a folder for (texts.Cause),
// by its word, and each rule of the loader, by its name, each with a fixture that makes the check
// print it. TestEveryTextsRefusalIsInTheTroubleshootingTable holds every one to a row of the
// troubleshooting page's table, and the table's own test drives each row through one of them.
var textsRefusals = map[string]func(t *testing.T) string{
	string(texts.Gone): func(t *testing.T) string {
		return textsRefusal(t, filepath.Join(t.TempDir(), "absent"))
	},
	string(texts.Unreadable): func(t *testing.T) string {
		file := filepath.Join(t.TempDir(), "texts")
		if err := os.WriteFile(file, []byte("a file in place of the folder\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return textsRefusal(t, file)
	},
	string(texts.NoManifest): func(t *testing.T) string {
		folder := textsFolder(t, reviewerParts, reviewerParts)
		if err := os.Remove(filepath.Join(folder, texts.ManifestName)); err != nil {
			t.Fatal(err)
		}
		return textsRefusal(t, folder)
	},
	string(texts.ManifestUnreadable): func(t *testing.T) string {
		folder := textsFolder(t, reviewerParts, reviewerParts)
		if err := os.WriteFile(filepath.Join(folder, texts.ManifestName), []byte(`{"files": `), 0o644); err != nil {
			t.Fatal(err)
		}
		return textsRefusal(t, folder)
	},
	string(texts.Unlisted): func(t *testing.T) string {
		return textsRefusal(t, textsFolder(t, append(slices.Clone(reviewerParts), "roles/planner.md"), reviewerParts))
	},
	string(texts.Missing): func(t *testing.T) string {
		return textsRefusal(t, textsFolder(t, reviewerParts[:1], reviewerParts))
	},
	string(texts.BadPath): func(t *testing.T) string {
		return textsRefusal(t, textsFolder(t, append(slices.Clone(reviewerParts), "notes.md"), reviewerParts))
	},
	string(texts.LinkEscapes): func(t *testing.T) string {
		folder := textsFolder(t, reviewerParts[1:], reviewerParts)
		outside := filepath.Join(t.TempDir(), "reviewer.md")
		if err := os.WriteFile(outside, []byte("A reviewer outside the folder.\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(folder, "roles"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(folder, "roles", "reviewer.md")); err != nil {
			t.Fatal(err)
		}
		return textsRefusal(t, folder)
	},
	string(texts.Unread): func(t *testing.T) string {
		parts := append(slices.Clone(reviewerParts), "roles/planner.md")
		return textsRefusal(t, textsFolder(t, parts, parts))
	},
	string(texts.TwoFolders): func(t *testing.T) string {
		return refusalFile{project: "    experiments:\n" +
			"      first-texts: {mode: random, roles: [reviewer], variants: [{name: folder, texts: /etc/legion-texts/first}, {name: control}]}\n" +
			"      second-texts: {mode: random, roles: [implementer, reviewer], variants: [{name: folder, texts: /etc/legion-texts/second}, {name: control}]}\n"}.refuse(t)
	},
	"a relative path": func(t *testing.T) string {
		return textsRefusal(t, "shorter")
	},
	"an unclean path": func(t *testing.T) string {
		return textsRefusal(t, "/etc/legion-texts/../shorter")
	},
	"a role on claude": func(t *testing.T) string {
		return refusalFile{top: "worker_backends: {reviewer: claude}\n", project: textsArmLines("[reviewer]", "/etc/legion-texts/shorter")}.refuse(t)
	},
	"a folder under no mount": func(t *testing.T) string {
		return refusalFile{kubernetes: true, project: textsArmLines("[reviewer]", "/etc/legion-texts/shorter")}.refuse(t)
	},
	"a folder under a sub_path mount": func(t *testing.T) string {
		return refusalFile{kubernetes: true, project: textsArmLines("[reviewer]", "/etc/legion-texts/shorter"), pod: "    pod:\n" +
			"      volumes: [{name: operator-texts, config_map: {name: legion-operator-texts}}]\n" +
			"      volume_mounts: [{volume: operator-texts, mount_path: /etc/legion-texts/shorter, sub_path: shorter}]\n"}.refuse(t)
	},
	"a file no projected item carries": func(t *testing.T) string {
		folder := textsFolder(t, reviewerParts, reviewerParts)
		skipUnderTreeRoot(t, folder)
		return refusalFile{kubernetes: true, project: textsArmLines("[reviewer]", folder), pod: "    pod:\n" +
			"      volumes: [{name: operator-texts, config_map: {name: legion-operator-texts, items: [{key: manifest.json, path: manifest.json}, {key: roles-reviewer.md, path: roles/reviewer.md}]}}]\n" +
			"      volume_mounts: [{volume: operator-texts, mount_path: " + folder + "}]\n"}.refuse(t)
	},
}

// textsLaunchCauses are the causes of texts.Cause neither the loader nor the boot refuses a folder
// for, each with why: a launch of the folder's arm finds each and falls back to Legion's texts.
var textsLaunchCauses = map[texts.Cause]string{
	texts.DigestDiffers:       "a folder cut from another release of Legion's texts, from which each launch falls back, so a release never stops a deployment whose folders were cut from the one before it",
	texts.NoLegionText:        "the same: a path the release the folder was cut from had and this one does not",
	texts.NameDiffers:         "the runtime's to read, against the plugins the launch loads, which boot does not hold",
	texts.UnknownAgent:        "the names the worker image ships are the plugin gate's or the image probe's to read, after the configuration's reads",
	texts.UnknownSkill:        "the same as an unknown agent",
	texts.NamesUnread:         "a boot whose plugin gate or image probe read no names, which only a launch then meets",
	texts.LauncherCannotCheck: "the launcher of a pod kept from before this release, which only a launch into that pod meets",
	texts.InstallsPlugins:     "a launch that installs Legion's plugins after its start, which only that launch is",
	texts.Changed:             "a folder edited between the daemon's read and the launcher's, which only a launch meets",
}

// Every refusal `legion config check` makes of an arm's folder of texts is a row of the
// troubleshooting page's refusal table, the rows TestTheTroubleshootingTableHoldsEveryRefusalToAMessageTheLoaderProduces
// reads (troubleshootingExtract), matched as that test matches a row: each cause of texts.Cause is
// either one the loader or the boot refuses, with a fixture in textsRefusals, or one only a launch
// meets, in textsLaunchCauses with why, and not both; and each fixture's refusal matches a row's
// text whole. A fixture for one of boot's causes is held to name it, as the refusal's last word.
// A cause added with no row, or a refusal reworded with no row, fails here.
func TestEveryTextsRefusalIsInTheTroubleshootingTable(t *testing.T) {
	for _, cause := range texts.Causes {
		_, refused := textsRefusals[string(cause)]
		reason, launch := textsLaunchCauses[cause]
		switch {
		case refused && launch:
			t.Errorf("the cause %s is in both textsRefusals and textsLaunchCauses: the check refuses it, or a launch alone meets it", cause)
		case !refused && !launch:
			t.Errorf("the cause %s is in neither textsRefusals nor textsLaunchCauses: give it a fixture the check refuses, or say why a launch alone meets it", cause)
		case launch && reason == "":
			t.Errorf("the cause %s names no reason a launch alone meets it", cause)
		}
	}
	var rows []*regexp.Regexp
	for _, row := range troubleshootingRows(t) {
		for _, text := range refusalTexts(row) {
			rows = append(rows, refusalPattern(t, text))
		}
	}
	for _, name := range slices.Sorted(maps.Keys(textsRefusals)) {
		t.Run(name, func(t *testing.T) {
			says := textsRefusals[name](t)
			if slices.Contains(texts.Causes, texts.Cause(name)) && texts.Cause(name) != texts.TwoFolders && !strings.HasSuffix(says, " ["+name+"]") {
				t.Errorf("the refusal %q does not name the cause %s", says, name)
			}
			if !slices.ContainsFunc(rows, func(row *regexp.Regexp) bool { return row.MatchString(says) }) {
				t.Errorf("the refusal %q matches no row of the troubleshooting table: add the row, and to refusalFixtures the fixture that drives it", says)
			}
		})
	}
}
