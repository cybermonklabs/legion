package main

import (
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
)

// The tests in this file read the expansion's own Go names (jj_expand.go): its parser of the read
// of the aliases, against reads captured from jj's releases, and its list of built-in commands,
// against the pod's wrapper's and jj's own. The tests of the expansion through each form of
// Legion's jj are in jj_expand_test.go, which names nothing the change adds.

// capturedConfigLists are the reads of the aliases (jjAliasRead) captured from jj's releases, each
// in a scratch repository whose configuration holds one shape:
// testdata/jj-config-list/<release>/<shape>.txt holds on its first line the command that made it,
// then that command's standard output, then a line `--- stderr`, then its standard error, then a
// line `--- exit <status>`.
const capturedConfigLists = "testdata/jj-config-list"

// capturedConfigList is one capture's command, standard output, standard error and exit status:
// what the command printed (capturedRun), cut at its line `--- stderr`.
func capturedConfigList(t *testing.T, path string) (command, stdout, stderr string, code int) {
	t.Helper()
	command, printed, code := capturedRun(t, path)
	stdout, stderr, ok := strings.Cut(printed, "--- stderr\n")
	if !ok {
		t.Fatalf("%s holds no line `--- stderr` before its standard error", path)
	}
	return command, stdout, stderr, code
}

// capturedAliases is what the parser must give of every capture that it reads without an error:
// jj's own default aliases, which --include-defaults lists.
var capturedAliases = map[string][]string{"b": {"bookmark"}, "ci": {"commit"}, "desc": {"describe"}, "st": {"status"}}

// capturedShape is what the parser must give of a capture of one shape: aliases, each alias the
// shape defines with the words it expands to, beside jj's defaults (capturedAliases); unexpandable,
// an alias that alias answers jjUnexpandable for; defaults, ui.default-command; values, each
// value of a table-form alias, in JSON, by its name; builtins, the names of jj's warnings; and
// failed, whether the read is jjUnexpandable itself.
type capturedShape struct {
	aliases      map[string][]string
	unexpandable string
	defaults     []string
	values       map[string]string
	builtins     []string
	failed       bool
}

// capturedShapes are the shapes each release's captures hold, by file name.
var capturedShapes = map[string]capturedShape{
	"lists":          {aliases: map[string][]string{"p": {"git", "push"}, "pp": {"p"}, "l": {"log", "-r", "@", "--no-graph"}}},
	"string":         {unexpandable: "x"},
	"numbers":        {unexpandable: "x"},
	"table":          {aliases: map[string][]string{"x": {"git", "push", "-b", "b"}}, values: map[string]string{"aliases.x.definition": `["git","push","-b","b"]`}},
	"table-disabled": {unexpandable: "x", values: map[string]string{"aliases.x.definition": `["git","push","-b","b"]`, "aliases.x.enabled": "false"}},
	"scoped":         {aliases: map[string][]string{"s": {"git", "push", "-b", "b"}}},
	"default-list":   {defaults: []string{"git", "push", "-b", "b"}},
	"default-string": {defaults: []string{"p"}},
	"builtin":        {aliases: map[string][]string{"log": {"status"}}, builtins: []string{"log"}},
	"unparsable":     {failed: true},
}

// The expansion's parser places every read of the aliases captured from jj 0.38.0, 0.45.1-sami and
// 0.46.0, each made by the expansion's own read (jjAliasRead), whose command is each capture's
// first line, and each release holds every shape. Of each read it gives each alias the shape
// defines as a list of strings, or as a table's definition, as the words it expands to, and jj's
// own default aliases beside them; ui.default-command as a list, as one string, and as `log` where
// nothing sets it; each value of a table-form alias, its definition and its enabled = false; the
// name each warning of an alias named for a built-in command names, in either release's spelling;
// and jjUnexpandable for an alias written as a string, a list of numbers, a table-form alias with
// enabled = false, and a configuration that does not parse.
func TestTheAliasReadParserPlacesEveryCapturedRead(t *testing.T) {
	t.Parallel()
	want := "jj " + strings.Join(jjAliasRead[:len(jjAliasRead)-1], " ") + " '" + jjAliasRead[len(jjAliasRead)-1] + "'"
	releases, err := os.ReadDir(capturedConfigLists)
	if err != nil {
		t.Fatal(err)
	}
	if len(releases) != 3 {
		t.Errorf("%s holds %d releases, want 3: 0.38.0, 0.45.1-sami and 0.46.0", capturedConfigLists, len(releases))
	}
	for _, release := range releases {
		files, err := filepath.Glob(filepath.Join(capturedConfigLists, release.Name(), "*.txt"))
		if err != nil {
			t.Fatal(err)
		}
		var shapes []string
		for _, path := range files {
			shapes = append(shapes, strings.TrimSuffix(filepath.Base(path), ".txt"))
		}
		slices.Sort(shapes)
		if all := slices.Sorted(maps.Keys(capturedShapes)); !slices.Equal(shapes, all) {
			t.Errorf("%s holds the shapes %q, want each of %q", release.Name(), shapes, all)
		}
		for _, path := range files {
			t.Run(release.Name()+"/"+filepath.Base(path), func(t *testing.T) {
				t.Parallel()
				shape, known := capturedShapes[strings.TrimSuffix(filepath.Base(path), ".txt")]
				if !known {
					t.Fatalf("%s is no shape this test knows", path)
				}
				command, stdout, stderr, code := capturedConfigList(t, path)
				if command != want {
					t.Errorf("the capture's command is\n%s\nwant the expansion's own read\n%s", command, want)
				}
				a, err := parseJJAliases(stdout, stderr, code)
				var unexpandable jjUnexpandable
				if shape.failed {
					if !errors.As(err, &unexpandable) || unexpandable.what != "the command" || !strings.Contains(unexpandable.why, "Config error: ") {
						t.Errorf("parseJJAliases answered %v, want that it cannot expand the command, naming jj's Config error", err)
					}
					return
				}
				if err != nil {
					t.Fatalf("parseJJAliases answered %v", err)
				}
				for name, words := range capturedAliases {
					if got, ok, err := a.alias(name); !ok || err != nil || !slices.Equal(got, words) {
						t.Errorf("alias(%q) is %q, %v, %v, want jj's default %q", name, got, ok, err, words)
					}
				}
				for name, words := range shape.aliases {
					if got, ok, err := a.alias(name); !ok || err != nil || !slices.Equal(got, words) {
						t.Errorf("alias(%q) is %q, %v, %v, want %q", name, got, ok, err, words)
					}
				}
				if shape.unexpandable != "" {
					if _, _, err := a.alias(shape.unexpandable); !errors.As(err, &unexpandable) || !strings.HasPrefix(unexpandable.what, "aliases."+shape.unexpandable) {
						t.Errorf("alias(%q) answered %v, want that it cannot expand aliases.%s", shape.unexpandable, err, shape.unexpandable)
					}
				}
				if _, ok, err := a.alias("nosuchalias"); ok || err != nil {
					t.Errorf("alias(%q) is %v, %v, want no alias", "nosuchalias", ok, err)
				}
				defaults := shape.defaults
				if defaults == nil {
					defaults = []string{"log"}
				}
				if got, err := a.defaultCommand(); err != nil || !slices.Equal(got, defaults) {
					t.Errorf("defaultCommand() is %q, %v, want %q", got, err, defaults)
				}
				for name, value := range shape.values {
					if got := string(a.values[name]); got != value {
						t.Errorf("the value of %s is %s, want %s", name, got, value)
					}
				}
				if got := slices.Sorted(maps.Keys(a.builtins)); !slices.Equal(got, shape.builtins) {
					t.Errorf("the names of jj's warnings are %q, want %q", got, shape.builtins)
				}
			})
		}
	}
}

// The pane's list of built-in commands is the pod's wrapper's: jjBuiltins holds the names of the
// builtins= line of packages/daemon/docker/jj, in the same order.
func TestTheBuiltinListIsTheImageWrappers(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(filepath.Join("..", "..", "docker", "jj"))
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		if names, ok := strings.CutPrefix(line, "builtins='"); ok {
			if got := strings.Fields(strings.TrimSuffix(names, "'")); !slices.Equal(got, jjBuiltins) {
				t.Errorf("the pod's wrapper's builtins= line names %q, want jjBuiltins, %q", got, jjBuiltins)
			}
			return
		}
	}
	t.Fatal("packages/daemon/docker/jj holds no builtins= line")
}

// The pane's lists of jj's global options are the pod's wrapper's: in command_word of
// packages/daemon/docker/jj, the case that sets takes=1 names jjWordValues, and the case of flags
// in front of `-h* | --help` names jjWordFlags, each in the same order. A flag one list skips and
// the other does not would leave one form's line of `jj <flag> git push` with no command word, and
// that form's push with no author step.
func TestTheGlobalOptionListsAreTheImageWrappers(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(filepath.Join("..", "..", "docker", "jj"))
	if err != nil {
		t.Fatal(err)
	}
	_, body, ok := strings.Cut(string(data), "\ncommand_word() {\n")
	if !ok {
		t.Fatal("packages/daemon/docker/jj holds no command_word function")
	}
	body, _, _ = strings.Cut(body, "\n}\n")
	var values, flags []string
	previous := ""
	for line := range strings.SplitSeq(body, "\n") {
		line = strings.TrimSpace(line)
		if pattern, ok := strings.CutSuffix(line, ") takes=1 ;;"); ok && values == nil {
			values = strings.Split(pattern, " | ")
		}
		if strings.HasPrefix(line, "-h* | --help") && flags == nil {
			if pattern, ok := strings.CutSuffix(previous, ") ;;"); ok {
				flags = strings.Split(pattern, " | ")
			}
		}
		previous = line
	}
	if !slices.Equal(values, jjWordValues) {
		t.Errorf("command_word's options that take a value are %q, want jjWordValues, %q", values, jjWordValues)
	}
	if !slices.Equal(flags, jjWordFlags) {
		t.Errorf("command_word's flags are %q, want jjWordFlags, %q", flags, jjWordFlags)
	}
}

// The pane's list of built-in commands is jj's own: with an alias named for each name of
// jjBuiltins and for each command `jj help` lists, in the user's configuration of a scratch home,
// the names that `jj config list` warns `Cannot define an alias that overrides the built-in
// command` for are jjBuiltins under jj 0.45.1-sami, and jjBuiltins but arrange and converge under
// jj 0.38.0, which has neither.
func TestTheBuiltinListIsJJs(t *testing.T) {
	t.Parallel()
	real, home := rolePushRealJJ(t), t.TempDir()
	env := append([]string{"PATH=/usr/bin:/bin", "JJ_USER=legion-test", "JJ_EMAIL=legion-test@example.invalid"}, fixtureenv.Home(home)...)
	run := func(args ...string) (stdout, stderr string) {
		t.Helper()
		code, stdout, stderr := trailerRun(t, home, env, "", real, args...)
		if code != 0 {
			t.Fatalf("jj %q exited %d:\n%s", args, code, stderr)
		}
		return stdout, stderr
	}
	version, _ := run("--version")
	help, _ := run("help")
	names := slices.Clone(jjBuiltins)
	in := false
	for line := range strings.SplitSeq(help, "\n") {
		switch {
		case line == "Commands:":
			in = true
		case in && strings.HasPrefix(line, "  ") && !strings.HasPrefix(line, "   "):
			if name := strings.Fields(line)[0]; !slices.Contains(names, name) {
				names = append(names, name)
			}
		case in && line != "":
			in = false
		}
	}
	var config strings.Builder
	config.WriteString("[aliases]\n")
	for _, name := range names {
		config.WriteString(name + " = [\"log\"]\n")
	}
	var jjConfig string
	for _, entry := range env {
		if value, ok := strings.CutPrefix(entry, "JJ_CONFIG="); ok {
			jjConfig = value
		}
	}
	if err := os.WriteFile(jjConfig, []byte(config.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	_, stderr := run("--ignore-working-copy", "config", "list", "-T", `""`)
	var warned []string
	for _, match := range jjOverridesBuiltin.FindAllStringSubmatch(stderr, -1) {
		warned = append(warned, match[1])
	}
	want := slices.Clone(jjBuiltins)
	if strings.HasPrefix(version, "jj 0.38.") {
		want = slices.DeleteFunc(want, func(name string) bool { return name == "arrange" || name == "converge" })
	}
	slices.Sort(warned)
	slices.Sort(want)
	if !slices.Equal(warned, want) {
		t.Errorf("%s warns of an alias named for the built-in commands %q, want %q", strings.TrimSpace(version), warned, want)
	}
}
