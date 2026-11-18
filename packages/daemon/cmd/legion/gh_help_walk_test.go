package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// ghHelpWalkDir holds, for each gh release `legion gh` has run against, the help of every command
// it can read: one directory per release, named by its version, holding VERSION (`gh --version`'s
// first line), gh.txt (`gh --help`) and one file per command path with its words joined by "_".
// TestCaptureGhHelpWalk writes a directory; TestParseGhHelpReadsEveryCommandOfEachSavedGh reads
// every one.
const ghHelpWalkDir = "testdata/gh-help-walk"

// captureGhHelpWalk turns TestCaptureGhHelpWalk on. It is a flag of the test binary and not a
// variable, since TestMain clears every variable but the few fixtureenv keeps.
var captureGhHelpWalk = flag.Bool("capture-gh-help-walk", false, "save the help of every command of the gh LEGION_GH_PATH names")

// TestCaptureGhHelpWalk saves the help of every command of the gh LEGION_GH_PATH names into
// ghHelpWalkDir/<version>, walking it as `legion gh` walks a command line (ghWalk): gh's own help
// first, then each command a group's help lists, a group's own subcommands in turn, and no gh
// alias, extension or command that runs another program (ghRunsAnotherProgram), whose help runs
// it. A help topic is saved as gh prints it, and the parse test names it. gh runs with an empty
// configuration and data directory, as in a fresh pane, so it lists gh's one default alias (co)
// and no extension of this machine's. It runs only with -capture-gh-help-walk:
//
//	LEGION_GH_PATH=/path/to/gh go test -run '^TestCaptureGhHelpWalk$' ./cmd/legion/ -args -capture-gh-help-walk
func TestCaptureGhHelpWalk(t *testing.T) {
	if !*captureGhHelpWalk {
		t.Skip("pass -capture-gh-help-walk, with LEGION_GH_PATH naming a gh, to save that release's help")
	}
	gh := os.Getenv("LEGION_GH_PATH")
	if gh == "" {
		t.Fatal("LEGION_GH_PATH names no gh to save the help of")
	}
	clean := t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command(gh, args...)
		cmd.Env = append(os.Environ(), "GH_CONFIG_DIR="+filepath.Join(clean, "config"), "XDG_DATA_HOME="+filepath.Join(clean, "data"),
			"GH_NO_UPDATE_NOTIFIER=1", "NO_COLOR=1", "GH_PROMPT_DISABLED=1")
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("%s %s: %v", gh, strings.Join(args, " "), err)
		}
		return string(out)
	}
	version := strings.SplitN(run("--version"), "\n", 2)[0]
	fields := strings.Fields(version)
	if len(fields) < 3 || fields[0] != "gh" || fields[1] != "version" {
		t.Fatalf("%s --version printed %q", gh, version)
	}
	dir := filepath.Join(ghHelpWalkDir, fields[2])
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "VERSION"), []byte(version+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	saved := 0
	var walk func(path []string)
	walk = func(path []string) {
		text := run(append(slices.Clone(path), "--help")...)
		if err := os.WriteFile(filepath.Join(dir, ghHelpWalkFile(path)), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
		saved++
		cmd, err := parseGhHelp(path, text)
		if err != nil || !cmd.group {
			return
		}
		for _, name := range slices.Sorted(func(yield func(string) bool) {
			for name := range cmd.listed {
				if !yield(name) {
					return
				}
			}
		}) {
			if cmd.ghAliases[name] || cmd.extensions[name] || ghRunsAnotherProgram[strings.Join(append(slices.Clone(path), name), " ")] {
				continue
			}
			walk(append(slices.Clone(path), name))
		}
	}
	walk(nil)
	t.Logf("saved the help of %d commands of %s into %s", saved, version, dir)
}

// ghHelpWalkFile is the file a command path's help is saved in: gh.txt for gh's own, else the
// path's words joined by "_".
func ghHelpWalkFile(path []string) string {
	if len(path) == 0 {
		return "gh.txt"
	}
	return strings.Join(path, "_") + ".txt"
}

// savedGhHelpOf answers gh's help of path from the saved walk of the release version, as
// savedGhHelp answers from ghHelpDir: a path it has no file for prints the help of the nearest
// parent it has, and one with no such parent at all is an error. The directory is made absolute
// first, so a test that changes its working directory still reads it.
func savedGhHelpOf(t *testing.T, version string) func([]string) (string, error) {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join(ghHelpWalkDir, version))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "VERSION")); err != nil {
		t.Fatalf("no saved help of gh %s: %v", version, err)
	}
	return func(path []string) (string, error) {
		if len(path) == 0 {
			data, err := os.ReadFile(filepath.Join(dir, "gh.txt"))
			return string(data), err
		}
		for p := path; len(p) > 0; p = p[:len(p)-1] {
			if data, err := os.ReadFile(filepath.Join(dir, ghHelpWalkFile(p))); err == nil {
				return string(data), nil
			}
		}
		return "", fmt.Errorf("unknown command %q for \"gh\"", path[0])
	}
}

// ghHelpSection is every line of text's section named name (`FLAGS`, `HELP TOPICS`), its heading
// a line of its own at the margin, up to the next such heading.
func ghHelpSection(text, name string) []string {
	var lines []string
	in := false
	for _, line := range strings.Split(text, "\n") {
		if line != "" && line[0] != ' ' && line[0] != '\t' {
			in = strings.TrimSpace(line) == name
			continue
		}
		if in {
			lines = append(lines, line)
		}
	}
	return lines
}

// Every gh release saved in ghHelpWalkDir is read whole: each option line of every command's
// FLAGS and INHERITED FLAGS sections is one addFlag reads, gh 2.104.0's `--name[=true|false]`
// among them, as an option taking no value; every command's help parses as a group or a leaf, and
// the only help refused is a help topic its group lists, which names no usage; and every command a
// group lists, but a gh alias, an extension and a command that runs another program, has its help
// saved, so the walk left nothing out. The counts are asserted, so a directory that held nothing
// fails rather than passing on an empty set.
func TestParseGhHelpReadsEveryCommandOfEachSavedGh(t *testing.T) {
	versions, err := os.ReadDir(ghHelpWalkDir)
	if err != nil {
		t.Fatal(err)
	}
	read := 0
	for _, version := range versions {
		if !version.IsDir() {
			continue
		}
		read++
		t.Run(version.Name(), func(t *testing.T) {
			dir := filepath.Join(ghHelpWalkDir, version.Name())
			stamp, err := os.ReadFile(filepath.Join(dir, "VERSION"))
			if err != nil || !strings.HasPrefix(string(stamp), "gh version "+version.Name()+" ") {
				t.Fatalf("%s/VERSION is %q (%v), want gh version %s", dir, stamp, err, version.Name())
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			helps := map[string]string{}
			for _, entry := range entries {
				if name := entry.Name(); strings.HasSuffix(name, ".txt") {
					data, err := os.ReadFile(filepath.Join(dir, name))
					if err != nil {
						t.Fatal(err)
					}
					helps[strings.TrimSuffix(name, ".txt")] = string(data)
				}
			}
			pathOf := func(key string) []string {
				if key == "gh" {
					return nil
				}
				return strings.Split(key, "_")
			}
			topics := map[string]bool{}
			for key, text := range helps {
				for _, line := range ghHelpSection(text, "HELP TOPICS") {
					if listed := ghListedCommand.FindStringSubmatch(line); listed != nil {
						topics[ghHelpWalkFile(append(slices.Clone(pathOf(key)), listed[1]))] = true
					}
				}
			}
			var flagLines, booleanMarked, groups, leaves, refusedTopics int
			for key, text := range helps {
				path := pathOf(key)
				for _, section := range []string{"FLAGS", "INHERITED FLAGS"} {
					for _, line := range ghHelpSection(text, section) {
						if !strings.HasPrefix(strings.TrimSpace(line), "-") {
							continue
						}
						flagLines++
						if strings.Contains(line, "[=true|false]") {
							booleanMarked++
						}
						scratch := ghCommand{flags: map[string]ghFlag{}, shorts: map[byte]string{}}
						if err := scratch.addFlag(line); err != nil {
							t.Errorf("gh %s's %s: %v", strings.Join(path, " "), section, err)
							continue
						}
						for name, flag := range scratch.flags {
							if strings.Contains(line, "--"+name+"[=true|false]") && (flag.value || flag.optional) {
								t.Errorf("gh %s's %q reads as %+v, want an option taking no value", strings.Join(path, " "), strings.TrimSpace(line), flag)
							}
						}
					}
				}
				cmd, err := parseGhHelp(path, text)
				switch {
				case err == nil && cmd.group:
					groups++
					for name := range cmd.listed {
						child := append(slices.Clone(path), name)
						if cmd.ghAliases[name] || cmd.extensions[name] || ghRunsAnotherProgram[strings.Join(child, " ")] {
							continue
						}
						if _, ok := helps[strings.TrimSuffix(ghHelpWalkFile(child), ".txt")]; !ok {
							t.Errorf("gh %s lists %q, whose help is not saved", strings.Join(path, " "), name)
						}
					}
				case err == nil:
					leaves++
				case topics[ghHelpWalkFile(path)] && strings.Contains(err.Error(), "names no usage"):
					refusedTopics++
				default:
					t.Errorf("parseGhHelp(%q): %v", path, err)
				}
			}
			t.Logf("%s: %d helps, %d groups, %d leaves, %d help topics refused for naming no usage, %d option lines, %d marked [=true|false]",
				strings.TrimSpace(string(stamp)), len(helps), groups, leaves, refusedTopics, flagLines, booleanMarked)
			if groups < 20 || leaves < 150 || flagLines < 1000 || refusedTopics == 0 {
				t.Errorf("%s holds %d groups, %d leaves, %d option lines and %d help topics, want gh's whole command tree", dir, groups, leaves, flagLines, refusedTopics)
			}
		})
	}
	if read < 2 {
		t.Errorf("%s holds %d gh releases, want the one the worker image pins and the one CI's runner image carries", ghHelpWalkDir, read)
	}
}
