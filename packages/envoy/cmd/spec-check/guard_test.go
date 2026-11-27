package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// guardHit is the end of guard's line for each place that it names, after "spec-check:
// <path>:<line>".
const guardHit = ": this line names the command that compares the files with the approved spec, and no CI job runs that command (R2). If the line only mentions the command, for example in a comment, a quoted text, a pattern or a name, reword it.\n"

// testGuard runs guard over the CI-file fixtures of testdata/ci-files and over trees it makes: it
// names each place of a CI file that names the command that compares the files with the approved
// spec (R2: "No CI job compares the files"), and an instrument that read no workflow does not
// pass.
func testGuard(t *testing.T, binary string) {
	t.Run("guard passes CI files that do not name the comparison", func(t *testing.T) {
		t.Parallel()
		root := ciFiles(t, "clean")
		expect(t, runBinary(t, binary, "", "guard", "--root", root), result{stdout: "spec-check: " + root + ": no line of 4 CI files names the command that compares the files\n"})
	})
	t.Run("guard names the planted comparison of a workflow, an action and a script", func(t *testing.T) {
		t.Parallel()
		root := ciFiles(t, "planted")
		expect(t, runBinary(t, binary, "", "guard", "--root", root), result{
			code:   1,
			stdout: "spec-check: " + root + ": 3 lines of CI files name the command that compares the files\n",
			stderr: "spec-check: " + filepath.Join(root, ".github", "actions", "compare", "action.yml") + ":9" + guardHit +
				"spec-check: " + filepath.Join(root, ".github", "workflows", "compare.yaml") + ":13" + guardHit +
				"spec-check: " + filepath.Join(root, "scripts", "compare.sh") + ":4" + guardHit,
		})
	})
	t.Run("guard names one line in the singular", func(t *testing.T) {
		t.Parallel()
		root, workflow := workflowTree(t, "      - run: |\n          spec-check \\\n            compare --dir .dispatch/spec --approved a.json\n")
		expect(t, runBinary(t, binary, "", "guard", "--root", root), named(root, workflow, 6))
	})
	// A quote, and a line break between the two words, are ignored: each row names the line that
	// holds spec-check.
	t.Run("guard names a quoted subcommand and a value over two lines", func(t *testing.T) {
		t.Parallel()
		for _, row := range []struct {
			name, step string
			line       int
		}{
			{"a quoted subcommand", "      - run: spec-check \"compare\" --dir .dispatch/spec --approved a.json\n", 5},
			{"a plain scalar over two lines", "      - run: spec-check\n          compare --dir .dispatch/spec --approved a.json\n", 5},
			{"a folded scalar over two lines", "      - run: >-\n          spec-check\n          compare --dir .dispatch/spec --approved a.json\n", 6},
		} {
			t.Run(row.name, func(t *testing.T) {
				t.Parallel()
				root, workflow := workflowTree(t, row.step)
				expect(t, runBinary(t, binary, "", "guard", "--root", root), named(root, workflow, row.line))
			})
		}
	})
	// sudo -n and docker run start the command in a CI step past the refusal of compare. The guard
	// names such a line in a workflow when the command's name and compare stand next to each other,
	// which is the part of R2 that covers them ("guard passes each form outside its claim" holds the
	// --entrypoint form, where the image stands between the two).
	t.Run("guard names a sudo or a container step that runs the comparison", func(t *testing.T) {
		t.Parallel()
		root, workflow := workflowTree(t, "      - run: sudo -n spec-check compare --dir .dispatch/spec --approved a.json\n"+
			"      - run: docker run --rm -v \"$PWD:/w\" -w /w -v \"$RUNNER_TEMP/spec-check:/usr/local/bin/spec-check:ro\" debian:bookworm-slim spec-check compare --dir .dispatch/spec --approved a.json\n")
		expect(t, runBinary(t, binary, "", "guard", "--root", root), named(root, workflow, 5, 6))
	})
	// A $ before a quote is ignored with the quote, so $'compare' is named behind env -u, which
	// takes the step past the refusal of compare. A subcommand that an escape builds, such as
	// $'com\x70are', is not found: compare's refusal stops it in a step that keeps GITHUB_ACTIONS.
	t.Run("guard names a $ quote and not a word that an escape builds", func(t *testing.T) {
		t.Parallel()
		root, workflow := workflowTree(t, "      - run: spec-check compare --dir .dispatch/spec --approved a.json\n"+
			"      - run: env -u GITHUB_ACTIONS spec-check $'compare' --dir .dispatch/spec --approved a.json\n"+
			"      - run: env -u GITHUB_ACTIONS spec-check $'com\\x70are' --dir .dispatch/spec --approved a.json\n"+
			"      - run: spec-check $'tree' --dir .dispatch/spec\n")
		expect(t, runBinary(t, binary, "", "guard", "--root", root), named(root, workflow, 5, 6))
	})
	// A backslash and the line break after it are ignored, with the indentation after them, so a
	// $ and its quote that they split, or a quoted word that they split, is named. A word that an
	// escape builds is not.
	t.Run("guard names a subcommand that a line continuation splits", func(t *testing.T) {
		t.Parallel()
		root, workflow := workflowTree(t, "      - run: |\n"+
			"          env -u GITHUB_ACTIONS spec-check $\\\n"+
			"          'com\\x70are' --dir .dispatch/spec --approved a.json\n"+
			"      - run: |\n"+
			"          env -u GITHUB_ACTIONS spec-check $\\\n"+
			"          \"compare\" --dir .dispatch/spec --approved a.json\n"+
			"      - run: |\n"+
			"          spec-check 'com\\\n"+
			"          pare' --dir .dispatch/spec\n")
		expect(t, runBinary(t, binary, "", "guard", "--root", root), named(root, workflow, 9, 12))
	})
	// A quote in a comment or in a here-document changes nothing for a rule that reads text, so the
	// audit's two inputs, which run the split comparison behind env -u, are named. The plain
	// comparison is named, and the same comment before spec-check tree is not.
	t.Run("guard names a comparison after a quote in a comment or a here-document", func(t *testing.T) {
		t.Parallel()
		root, workflow := workflowTree(t, "      - run: |\n"+
			"          # \" \\\n"+
			"          : \"it's\"; env -u GITHUB_ACTIONS spec-check com\\\n"+
			"          pare --dir .dispatch/spec --approved a.json\n"+
			"      - run: |\n"+
			"          cat <<'END\\'\n"+
			"          \" \\\n"+
			"          END\\\n"+
			"          : \"it's\"; env -u GITHUB_ACTIONS spec-check com\\\n"+
			"          pare --dir .dispatch/spec --approved a.json\n"+
			"      - run: spec-check compare --dir .dispatch/spec --approved a.json\n"+
			"      - run: |\n"+
			"          # \" \\\n"+
			"          : \"it's\"; env -u GITHUB_ACTIONS spec-check tree --dir .dispatch/spec\n")
		expect(t, runBinary(t, binary, "", "guard", "--root", root), named(root, workflow, 7, 13, 15))
	})
	// A quoted text that no shell runs is named too, and the line tells its writer to reword it.
	t.Run("guard tells the writer to reword a line that no shell runs", func(t *testing.T) {
		t.Parallel()
		root, workflow := workflowTree(t, "      - run: echo 'run spec-check compare by hand'\n")
		expect(t, runBinary(t, binary, "", "guard", "--root", root), named(root, workflow, 5))
	})
	// A line that only mentions the command is named too: a step that greps for the two words or
	// echoes them, which a shell runs, and a needs list or a key, which holds names. The guard's
	// lines say that such a line names the command, which is true of each, and not that it holds
	// the command, and they tell the author to reword a line that only mentions it.
	t.Run("guard tells the author to reword a line that only mentions the command", func(t *testing.T) {
		t.Parallel()
		mention := regexp.MustCompile(`only mentions the command.{0,120}reword`)
		for _, row := range []struct {
			name, steps string
			line        int
		}{
			{"a grep step", `      - run: "! grep -rn 'spec-check compare' .github/workflows"` + "\n", 5},
			{"an echo step", `      - run: echo "spec-check compare runs in Dispatch, not in CI"` + "\n", 5},
			{"a needs list", "      - run: echo ok\n  ci-ok:\n    needs: [lint, spec-check, compare]\n    steps:\n      - run: echo ok\n", 7},
			{"a key above compare", "      - run: echo ok\n    env:\n      spec-check:\n        compare: false\n", 7},
		} {
			t.Run(row.name, func(t *testing.T) {
				t.Parallel()
				root, workflow := workflowTree(t, row.steps)
				got := runBinary(t, binary, "", "guard", "--root", root)
				expect(t, got, named(root, workflow, row.line))
				if strings.Contains(got.stdout+got.stderr, "holds the command") || !mention.MatchString(got.stderr) {
					t.Errorf("the guard's lines say that this line holds the command, or give no way past a line that only mentions it:\nstdout %q\nstderr %q", got.stdout, got.stderr)
				}
			})
		}
	})
	t.Run("guard does not pass when it reads no workflow", func(t *testing.T) {
		t.Parallel()
		root := ciFiles(t, "no-workflow")
		expect(t, runBinary(t, binary, "", "guard", "--root", root), result{code: 1, stderr: "spec-check: " + root + ": read no workflow\n"})
		// A workflows directory that holds only an empty directory, beside an action: no workflow.
		empty := t.TempDir()
		for _, dir := range []string{filepath.Join(".github", "workflows", "nested"), filepath.Join(".github", "actions", "spec")} {
			if err := os.MkdirAll(filepath.Join(empty, dir), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		writeFile(t, filepath.Join(empty, ".github", "actions", "spec", "action.yml"), []byte("runs:\n  using: composite\n"))
		expect(t, runBinary(t, binary, "", "guard", "--root", empty), result{code: 1, stderr: "spec-check: " + empty + ": read no workflow\n"})
	})
	// A word that only holds the command's name is another name, so lines whose job, key, tool or
	// word only holds spec-check (wordsThatOnlyHoldTheName), in a script, pass the guard.
	t.Run("guard passes a word that only holds the command's name", func(t *testing.T) {
		t.Parallel()
		root, _ := workflowTree(t, "      - run: echo ok\n")
		if err := os.Mkdir(filepath.Join(root, "scripts"), 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(root, "scripts", "a.sh"), []byte(strings.Join(wordsThatOnlyHoldTheName, "\n")+"\n"))
		expect(t, runBinary(t, binary, "", "guard", "--root", root), result{stdout: "spec-check: " + root + ": no line of 2 CI files names the command that compares the files\n"})
	})
	// A word after the command's name that only holds compare, such as a job, another subcommand,
	// a script or a path (wordsThatOnlyHoldCompare), is another word, so its line passes the guard.
	t.Run("guard passes a word that only holds compare", func(t *testing.T) {
		t.Parallel()
		root, _ := workflowTree(t, "      - run: echo ok\n")
		if err := os.Mkdir(filepath.Join(root, "scripts"), 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(root, "scripts", "a.sh"), []byte(strings.Join(wordsThatOnlyHoldCompare, "\n")+"\n"))
		expect(t, runBinary(t, binary, "", "guard", "--root", root), result{stdout: "spec-check: " + root + ": no line of 2 CI files names the command that compares the files\n"})
	})
	// Each other byte after compare ends the word: the comparison at the end of a line, and before a
	// space, a quote, ), ;, | or &, is named.
	t.Run("guard names compare before each byte that ends a word", func(t *testing.T) {
		t.Parallel()
		root, workflow := workflowTree(t, "      - run: spec-check compare\n"+
			"      - run: spec-check compare --dir .dispatch/spec --approved a.json\n"+
			"      - run: bash -c \"spec-check compare\"\n"+
			"      - run: (spec-check compare)\n"+
			"      - run: spec-check compare; echo done\n"+
			"      - run: spec-check compare| tee out\n"+
			"      - run: spec-check compare& wait\n")
		expect(t, runBinary(t, binary, "", "guard", "--root", root), named(root, workflow, 5, 6, 7, 8, 9, 10, 11))
	})
	// A double-quoted YAML value, and a string of JavaScript or Python, turn \n and \t into a line
	// break and a tab, so the shell runs the comparison: after a backslash, n stands for white space,
	// before the command's name and after compare.
	t.Run("guard names the comparison beside an escape in a quoted text", func(t *testing.T) {
		t.Parallel()
		root, workflow := workflowTree(t, `      - run: "set -e\nspec-check compare --dir .dispatch/spec --approved a.json"`+"\n"+
			`      - run: "spec-check compare\n--dir .dispatch/spec --approved a.json"`+"\n")
		if err := os.Mkdir(filepath.Join(root, "scripts"), 0o755); err != nil {
			t.Fatal(err)
		}
		script := filepath.Join(root, "scripts", "a.ts")
		writeFile(t, script, []byte(`execSync("set -e\nspec-check compare --dir a");`+"\n"))
		expect(t, runBinary(t, binary, "", "guard", "--root", root), result{
			code:   1,
			stdout: "spec-check: " + root + ": 3 lines of CI files name the command that compares the files\n",
			stderr: "spec-check: " + workflow + ":5" + guardHit + "spec-check: " + workflow + ":6" + guardHit + "spec-check: " + script + ":1" + guardHit,
		})
	})
	// The two bounds read the file's own bytes, so the review's two Python lines, which split() turns
	// into a list, are named: the quote after compare ends the word, and the . after the quote makes
	// no other word of it. After compare, the byte after a backslash takes its place, so an escaped
	// space ends the word, and so does \r, which PowerShell reads as a line break and Bash does not.
	t.Run("guard names compare before a quote, an escaped space or \\r", func(t *testing.T) {
		t.Parallel()
		root, workflow := workflowTree(t, `      - run: "env -u GITHUB_ACTIONS spec-check compare\ --dir .dispatch/spec --approved approved.json"`+"\n"+
			`      - run: "spec-check compare\r--dir .dispatch/spec --approved a.json"`+"\n")
		if err := os.MkdirAll(filepath.Join(root, ".github", "scripts"), 0o755); err != nil {
			t.Fatal(err)
		}
		script := filepath.Join(root, ".github", "scripts", "a.py")
		writeFile(t, script, []byte(`cmd = "spec-check compare".split() + ["--dir", d, "--approved", a]`+"\n"+
			`subprocess.run('spec-check compare'.split() + args)`+"\n"))
		expect(t, runBinary(t, binary, "", "guard", "--root", root), result{
			code:   1,
			stdout: "spec-check: " + root + ": 4 lines of CI files name the command that compares the files\n",
			stderr: "spec-check: " + script + ":1" + guardHit + "spec-check: " + script + ":2" + guardHit +
				"spec-check: " + workflow + ":5" + guardHit + "spec-check: " + workflow + ":6" + guardHit,
		})
	})
	// The guard names a line only when the command's name and compare stand next to each other. Each
	// form outside that claim passes it: another word between the two (docker run's --entrypoint), a
	// line break and a - between them (a block list of a command), an expansion that builds compare
	// (a command substitution, a glob, a brace sequence, printf -v), and an alias.
	t.Run("guard passes each form outside its claim", func(t *testing.T) {
		t.Parallel()
		root, _ := workflowTree(t, "      - run: docker run --rm --entrypoint spec-check debian:bookworm-slim compare --dir .dispatch/spec --approved a.json\n"+
			"      - run: spec-check $(echo compare) --dir .dispatch/spec --approved a.json\n"+
			"      - run: spec-check `echo compare` --dir .dispatch/spec --approved a.json\n"+
			"      - run: spec-check compar[e] --dir .dispatch/spec --approved a.json\n"+
			"      - run: spec-check compar{e..e} --dir .dispatch/spec --approved a.json\n"+
			"      - run: printf -v sub %s compare && spec-check \"$sub\" --dir .dispatch/spec --approved a.json\n"+
			"      - run: |\n          shopt -s expand_aliases\n          alias sc=spec-check\n          sc compare --dir .dispatch/spec --approved a.json\n")
		if err := os.Mkdir(filepath.Join(root, "scripts"), 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(root, "scripts", "compose.yaml"), []byte("services:\n  spec:\n    image: debian:bookworm-slim\n    command:\n      - spec-check\n      - compare\n"))
		expect(t, runBinary(t, binary, "", "guard", "--root", root), result{stdout: "spec-check: " + root + ": no line of 2 CI files names the command that compares the files\n"})
	})
	// The guard follows no symbolic link: a scripts directory that is a link to another directory,
	// and a workflow that is a link to a file, each holding the comparison, pass it, though a runner
	// follows both.
	t.Run("guard follows no symbolic link", func(t *testing.T) {
		t.Parallel()
		root, _ := workflowTree(t, "      - run: echo ok\n")
		if err := os.Mkdir(filepath.Join(root, "linked"), 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(root, "linked", "a.sh"), []byte("spec-check compare --dir .dispatch/spec --approved a.json\n"))
		for target, link := range map[string]string{
			"linked": filepath.Join(root, "scripts"),
			filepath.Join("..", "..", "linked", "a.sh"): filepath.Join(root, ".github", "workflows", "linked.yaml"),
		} {
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
		}
		expect(t, runBinary(t, binary, "", "guard", "--root", root), result{stdout: "spec-check: " + root + ": no line of 1 CI file names the command that compares the files\n"})
	})
	t.Run("guard names a relative root by its absolute path", func(t *testing.T) {
		t.Parallel()
		root := ciFiles(t, "clean")
		expect(t, runBinary(t, binary, filepath.Dir(root), "guard", "--root", "clean"), result{stdout: "spec-check: " + root + ": no line of 4 CI files names the command that compares the files\n"})
	})
	t.Run("guard exits 2 on a root it cannot read", func(t *testing.T) {
		t.Parallel()
		missing := filepath.Join(t.TempDir(), "missing")
		expect(t, runBinary(t, binary, "", "guard", "--root", missing), result{code: 2, stderr: "spec-check: " + missing + ": does not exist\n"})
		file := filepath.Join(t.TempDir(), "file")
		writeFile(t, file, []byte("A file.\n"))
		expect(t, runBinary(t, binary, "", "guard", "--root", file), result{code: 2, stderr: "spec-check: " + file + ": is not a directory\n"})
	})
	t.Run("guard exits 2 on a CI directory it cannot read", func(t *testing.T) {
		t.Parallel()
		// Root reads each directory, so root cannot make the state; a CI job runs as another user.
		if os.Geteuid() == 0 {
			skipOutsideCI(t, "root reads a directory of mode 000")
		}
		root := t.TempDir()
		workflows := filepath.Join(root, ".github", "workflows")
		if err := os.MkdirAll(workflows, 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(workflows, "spec.yaml"), []byte("on: pull_request\n"))
		locked := filepath.Join(root, "scripts")
		if err := os.Mkdir(locked, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
		expect(t, runBinary(t, binary, "", "guard", "--root", root), result{
			code:   2,
			stderr: "spec-check: " + locked + ": cannot be read: open " + locked + ": permission denied\n",
		})
	})
}

// workflowTree is a repository root that holds one workflow, .github/workflows/spec.yaml, whose
// one job's steps follow its head from line 5 on, and the workflow's path.
func workflowTree(t *testing.T, steps string) (root, workflow string) {
	t.Helper()
	root = t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".github", "workflows"), 0o755); err != nil {
		t.Fatal(err)
	}
	workflow = filepath.Join(root, ".github", "workflows", "spec.yaml")
	writeFile(t, workflow, []byte("on: pull_request\njobs:\n  spec:\n    steps:\n"+steps))
	return root, workflow
}

// named is guard's result over root when it names each of lines of workflow.
func named(root, workflow string, lines ...int) result {
	var stderr strings.Builder
	for _, n := range lines {
		fmt.Fprintf(&stderr, "spec-check: %s:%d%s", workflow, n, guardHit)
	}
	verb := "name"
	if len(lines) == 1 {
		verb = "names"
	}
	return result{
		code:   1,
		stdout: fmt.Sprintf("spec-check: %s: %s of CI files %s the command that compares the files\n", root, count(len(lines), "line"), verb),
		stderr: stderr.String(),
	}
}

// ciFiles is the absolute path of testdata/ci-files/<name>, the CI directories of a repository as
// guard reads them.
func ciFiles(t *testing.T, name string) string {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("testdata", "ci-files", name))
	if err != nil {
		t.Fatal(err)
	}
	return dir
}
