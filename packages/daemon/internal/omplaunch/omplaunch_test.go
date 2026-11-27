package omplaunch

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// writeExecutable makes an executable file for the resolution tests.
func writeExecutable(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// writeMise serves the configured tool's install root. Its test keeps an executable named omp
// earlier on PATH, so ResolveInvocation must not resolve through ordinary command lookup. It runs
// `x <tool> -- <program> <args…>` as mise runs it under a `_.path` entry of the operator's config:
// it writes its pid to <path>.pid, puts the directory shims beside it first on PATH, and execs
// the program.
func writeMise(t *testing.T, path, tool, install string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = where ] && [ \"$2\" = \"" + tool + "\" ]; then\n" +
		"  printf '%s\\n' \"" + install + "\"\n" +
		"  exit 0\n" +
		"fi\n" +
		"if [ \"$1\" = x ] && [ \"$2\" = \"" + tool + "\" ] && [ \"$3\" = -- ]; then\n" +
		"  shift 3\n" +
		"  echo \"$$\" >\"" + path + ".pid\"\n" +
		"  PATH=\"" + filepath.Join(filepath.Dir(path), "shims") + ":$PATH\"\n" +
		"  exec \"$@\"\n" +
		"fi\n" +
		"exit 1\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

// ResolveInvocation is the shipped resolveOmpInvocation (environment.ts:312-341): an absolute
// LEGION_OMP_PATH is resolved and used directly; otherwise the configured invocation must be
// `mise x <tool> -- omp`, run with mise pinned to its resolved absolute path, so mise still
// activates the tool inside the pane, and with the configured tool's own omp after the shell that
// puts the launch's first PATH entry back in front (keepPathHead).
func TestResolveInvocation(t *testing.T) {
	dir := t.TempDir()
	realOmp := filepath.Join(dir, "installs", "omp-18", "bin", "omp")
	writeExecutable(t, realOmp)
	linkedOmp := filepath.Join(dir, "bin", "omp")
	if err := os.MkdirAll(filepath.Dir(linkedOmp), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realOmp, linkedOmp); err != nil {
		t.Fatal(err)
	}
	mise := filepath.Join(dir, "mise dir", "mise")
	writeMise(t, mise, "github:sjawhar/oh-my-pi@18.1.21", filepath.Dir(filepath.Dir(realOmp)))
	notExecutable := filepath.Join(dir, "plain")
	if err := os.WriteFile(notExecutable, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	const pinned = "mise x github:sjawhar/oh-my-pi@18.1.21 -- omp"
	const pathHeadFirst = `/bin/sh -c 'PATH=$1:$PATH && shift && exec "$@"' sh "${PATH%%:*}" `

	for _, tc := range []struct {
		name       string
		invocation string
		env        map[string]string
		want       string
		refusal    string
	}{
		{
			name:       "LEGION_OMP_PATH names an executable: its real path, used directly",
			invocation: pinned,
			env:        map[string]string{"LEGION_OMP_PATH": linkedOmp},
			want:       realOmp,
		},
		{
			name:       "LEGION_OMP_PATH wins over any invocation shape",
			invocation: "omp",
			env:        map[string]string{"LEGION_OMP_PATH": linkedOmp},
			want:       realOmp,
		},
		{
			name:       "LEGION_OMP_PATH with no invocation configured",
			invocation: "",
			env:        map[string]string{"LEGION_OMP_PATH": linkedOmp},
			want:       realOmp,
		},
		{
			// The shipped daemon falls back to its pinned default; the Go daemon has no copy of
			// the pin, so it names the key instead.
			name:       "no invocation and no LEGION_OMP_PATH",
			invocation: "",
			env:        map[string]string{"PATH": filepath.Dir(mise)},
			refusal:    "omp_invocation is not set: set it to 'mise x <tool> -- omp', or set LEGION_OMP_PATH to an absolute executable path",
		},
		{
			name:       "LEGION_OMP_PATH relative",
			invocation: pinned,
			env:        map[string]string{"LEGION_OMP_PATH": "bin/omp"},
			refusal:    "LEGION_OMP_PATH must be an absolute executable path",
		},
		{
			name:       "LEGION_OMP_PATH not executable",
			invocation: pinned,
			env:        map[string]string{"LEGION_OMP_PATH": notExecutable},
			refusal:    "LEGION_OMP_PATH is not an executable: " + notExecutable,
		},
		{
			name:       "mise resolves the configured tool's binary, not an omp earlier on PATH",
			invocation: pinned,
			env:        map[string]string{"LEGION_MISE_PATH": mise, "PATH": filepath.Dir(linkedOmp)},
			want:       "'" + mise + "' x github:sjawhar/oh-my-pi@18.1.21 -- " + pathHeadFirst + realOmp,
		},
		{
			name:       "mise resolves the configured tool's binary through PATH",
			invocation: pinned,
			env:        map[string]string{"PATH": filepath.Dir(linkedOmp) + ":" + filepath.Dir(mise)},
			want:       "'" + mise + "' x github:sjawhar/oh-my-pi@18.1.21 -- " + pathHeadFirst + realOmp,
		},
		{
			name:       "any other invocation",
			invocation: "omp --mode rpc",
			env:        map[string]string{"PATH": filepath.Dir(mise)},
			refusal:    "OMP invocation must be 'mise x <tool> -- omp'. Set LEGION_OMP_PATH to an absolute executable path.",
		},
		{
			name:       "no mise anywhere",
			invocation: pinned,
			env:        map[string]string{"PATH": "/nonexistent"},
			refusal:    "Missing required daemon tool: mise (set LEGION_MISE_PATH to an absolute executable path)",
		},
		{
			name:       "LEGION_MISE_PATH relative",
			invocation: pinned,
			env:        map[string]string{"LEGION_MISE_PATH": "mise"},
			refusal:    "LEGION_MISE_PATH must be an absolute executable path",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveInvocation(tc.invocation, func(name string) string { return tc.env[name] })
			if tc.refusal != "" {
				if err == nil || err.Error() != tc.refusal {
					t.Fatalf("got (%q, %v), want refusal %q", got, err, tc.refusal)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveInvocation: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// The program mise starts from the resolved fragment is OMP itself, which the shell's exec leaves
// in the process mise ran in, with the first entry of the PATH the launch ran under back in front
// of the directory mise put first, and the rest of the PATH as mise made it. That entry holds a
// space and a `$`, and stays the one directory it names.
func TestResolvedMiseInvocationKeepsTheLaunchsFirstPathEntryFirst(t *testing.T) {
	dir := t.TempDir()
	omp := filepath.Join(dir, "installs", "omp-18", "bin", "omp")
	record := filepath.Join(dir, "record")
	if err := os.MkdirAll(filepath.Dir(omp), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(omp, []byte("#!/bin/sh\nprintf '%s\\n' \"$$\" \"$PATH\" \"$*\" >\""+record+"\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	mise := filepath.Join(dir, "mise")
	writeMise(t, mise, "github:sjawhar/oh-my-pi@18.1.21", filepath.Dir(filepath.Dir(omp)))
	invocation, err := ResolveInvocation("mise x github:sjawhar/oh-my-pi@18.1.21 -- omp", func(name string) string {
		return map[string]string{"LEGION_MISE_PATH": mise}[name]
	})
	if err != nil {
		t.Fatalf("ResolveInvocation: %v", err)
	}
	launcher := filepath.Join(dir, "state $dir", "bin")
	launch := exec.Command("/bin/sh", "-c", invocation+" --mode rpc --append-system-prompt '/state/a prompt.md'")
	launch.Env = []string{"PATH=" + launcher + ":/usr/bin:/bin"}
	if output, err := launch.CombinedOutput(); err != nil {
		t.Fatalf("%s: %v\n%s", invocation, err, output)
	}
	pid, err := os.ReadFile(mise + ".pid")
	if err != nil {
		t.Fatalf("mise x never ran: %v", err)
	}
	seen, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("OMP never ran: %v", err)
	}
	want := strings.Join([]string{
		strings.TrimSpace(string(pid)),
		launcher + ":" + filepath.Join(dir, "shims") + ":" + launcher + ":/usr/bin:/bin",
		"--mode rpc --append-system-prompt /state/a prompt.md",
	}, "\n") + "\n"
	if string(seen) != want {
		t.Errorf("OMP recorded its pid, PATH and arguments as\n%s\nwant (mise's pid, the launch's first PATH entry before mise's PATH, the launch's arguments)\n%s", seen, want)
	}
}

// The one --append-system-prompt word every pane's OMP gets is the path of the file its prompt was
// written to, one shell word however the path is spelled, and never the text or a `$(cat …)` the
// shell would expand into one argument the size of the prompt.
func TestSystemPromptArgument(t *testing.T) {
	for _, tc := range []struct {
		name, path, want string
	}{
		{name: "a plain path", path: "/state/secrets/legion-legion-legion-42-tester-system-prompt.md",
			want: "--append-system-prompt /state/secrets/legion-legion-legion-42-tester-system-prompt.md"},
		{name: "a path the shell would split or expand", path: "/state dir/$HOME/system-prompt.md",
			want: `--append-system-prompt '/state dir/$HOME/system-prompt.md'`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := SystemPromptArgument(tc.path); got != tc.want {
				t.Errorf("SystemPromptArgument(%q) = %s, want %s", tc.path, got, tc.want)
			}
		})
	}
}

// WithPrefix quotes each prefix element on its own and leaves the invocation — already a
// shell fragment — as it is (runtime-tmux.ts:112-118).
func TestWithPrefix(t *testing.T) {
	invocation := "/usr/bin/mise x 'github:sjawhar/oh-my-pi@18' -- omp"
	if got := WithPrefix(nil, invocation); got != invocation {
		t.Errorf("no prefix: got %q", got)
	}
	got := WithPrefix([]string{"env", "OMP_PROFILE=legion", "--", "nice", "-n 5"}, invocation)
	if want := "env 'OMP_PROFILE=legion' -- nice '-n 5' " + invocation; got != want {
		t.Errorf("prefix: got %q, want %q", got, want)
	}
}

// PrefixMasked takes every word of the prefix of MaskedPrefixWord characters or more out of a
// launch's output a line at a time, the program included, and the value alone of a NAME=value
// word, wherever each stands; a shorter word stays, a line that names the variable with another
// value stays, and a line that still holds a 10-character run of an argument or a value, folded or
// cut, is not printed. A program word with no command's shape (a token) is held to the same way,
// so its cut copy and its upper-case copy are not printed either. The credentials are made up: a
// token pasted as the program, a token an env word sets, and a proxy URL's password.
func TestPrefixMaskedTakesOutEveryWordAndValue(t *testing.T) {
	const program, value = "madeUpProgramToken0123", "madeUpEnvValue0123456"
	proxy := "https://legion:madeUpProxyPass0123" + "@proxy.example"
	prefix := []string{program, "OMP_TOKEN=" + value, "--", proxy}
	text := "sh: 1: " + program + ": not found; OMP_TOKEN=" + value + " (" + value + "x); -- kept; " + proxy + "\n" +
		"OMP_TOKEN=other is set\n" +
		"the token MADEUPENVVALUE0123456 is refused\n" +
		"cut: madeUpProgramTo…\n" +
		"MADEUPPROGRAMTOKEN0123 refused"
	want := "sh: 1: <prefix word 0>: not found; <prefix word 1> (<prefix word 1>x); -- kept; <prefix word 3>\n" +
		"OMP_TOKEN=other is set\n" +
		"(a line of 42 bytes that holds part of the launch prefix, not printed)\n" +
		"(a line of 23 bytes that holds part of the launch prefix, not printed)\n" +
		"(a line of 30 bytes that holds part of the launch prefix, not printed)"
	if got := PrefixMasked(text, prefix); got != want {
		t.Fatalf("PrefixMasked = %q, want %q", got, want)
	}
	if got := PrefixMasked("nothing of it", nil); got != "nothing of it" {
		t.Fatalf("PrefixMasked with no prefix = %q, want the text as it was", got)
	}
}

// A prefix word holding "=" that sets no variable is a word whole, whose runs a line may not hold:
// a key handed to a wrapper with its base64 padding (`<token>==`), and a `--key=<token>` flag. The
// wrapper echoes each token without what surrounded it. The tokens are made up.
func TestPrefixMaskedReadsOnlyAVariableAsANameValueWord(t *testing.T) {
	for _, tc := range []struct{ name, word, token string }{
		{"a base64 key and its padding", "madeUpKeyToken0123456789==", "madeUpKeyToken0123456789"},
		{"a base64 key holding a / and a +", "madeUp+Key/Token0123456789=", "madeUp+Key/Token0123456789"},
		{"a flag with its value", "--key=madeUpFlagToken0123456789", "madeUpFlagToken0123456789"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := PrefixMasked("keywrap: key "+tc.token+" rejected", []string{"keywrap", "--key", tc.word})
			if strings.Contains(got, tc.token[:10]) {
				t.Fatalf("PrefixMasked = %q, which holds part of the key", got)
			}
		})
	}
	// A variable set to a value is still read as one: the line naming the variable is kept.
	const value = "madeUpEnvValue0123456"
	if got := PrefixMasked("OMP_TOKEN is set", []string{"env", "OMP_TOKEN=" + value}); got != "OMP_TOKEN is set" {
		t.Fatalf("PrefixMasked = %q, want the line naming the variable kept", got)
	}
}

// The program is taken out as an exact copy and by its base name, and by its base name's runs only
// when that has no command's shape: a line that shares a directory with it is kept, a check's JSON
// among them, while a shell's refusal naming it either way is masked, and a cut copy of a token
// pasted as the program is not printed. The token is made up.
func TestPrefixMaskedKeepsALineSharingTheProgramsDirectory(t *testing.T) {
	for _, tc := range []struct{ name, program, text, want string }{
		{"a library beside the wrapper", "/usr/local/bin/proxywrap",
			"error: cannot read /usr/local/lib/libproxy.so.1", "error: cannot read /usr/local/lib/libproxy.so.1"},
		{"a check's JSON", "/usr/local/bin/omp-proxy-wrapper",
			`{"python":"/usr/local/lib/python3.12"}`, `{"python":"/usr/local/lib/python3.12"}`},
		{"the program by its path", "/usr/local/bin/proxywrap",
			"sh: 1: /usr/local/bin/proxywrap: not found", "sh: 1: <prefix word 0>: not found"},
		{"the program by its base name", "/usr/local/bin/proxywrap",
			"proxywrap: cannot reach the proxy", "<prefix word 0>: cannot reach the proxy"},
		{"a command name's cut copy", "/usr/local/bin/omp-proxy-wrapper",
			"omp-proxy-wr… exited 1", "omp-proxy-wr… exited 1"},
		{"a token program's cut copy", "/opt/bin/madeUpProgramToken0123",
			"sh: madeUpProgramTo: cut", "(a line of 24 bytes that holds part of the launch prefix, not printed)"},
		{"a line beside a token program", "/opt/bin/madeUpProgramToken0123",
			"error: cannot read /opt/bin/wrapper.conf", "error: cannot read /opt/bin/wrapper.conf"},
		{"a segmented token program's cut copy", "a1b2-c3d4-e5f6-g7h8-i9j0",
			"sh: a1b2-c3d4-e5f6: not found", "(a line of 29 bytes that holds part of the launch prefix, not printed)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := PrefixMasked(tc.text, []string{tc.program, "--"}); got != tc.want {
				t.Fatalf("PrefixMasked = %q, want %q", got, tc.want)
			}
		})
	}
}

// PrefixShown names the prefix's program, without its directory, and how many arguments follow,
// never an argument: a made-up token set with env, and a made-up proxy URL with a password, are
// the arguments here. A first word that is a NAME=value word shows as one, and one that has no
// command's shape, a made-up token pasted where the program goes, bare or as a path's last part,
// shows as ProgramNotShown. WithPrefixShown puts those words before the invocation, which holds no
// credential.
func TestPrefixShownNamesNoArgument(t *testing.T) {
	const token = "madeUpPrefixToken0123456789"
	invocation := "/usr/bin/mise x 'github:sjawhar/oh-my-pi@18' -- omp"
	for _, tc := range []struct {
		name   string
		prefix []string
		want   []string
	}{
		{"none", nil, nil},
		{"a program alone", []string{"/usr/bin/nice"}, []string{"nice"}},
		{"env setting a token", []string{"/usr/bin/env", "OMP_TOKEN=" + token}, []string{"env", "(1 argument, not shown)"}},
		{"a proxy wrapper handed a URL", []string{"proxywrap", "--upstream", "https://legion:" + token + "@proxy.example", "--"}, []string{"proxywrap", "(3 arguments, not shown)"}},
		{"a NAME=value first word", []string{"OMP_TOKEN=" + token, "omp-wrapper"}, []string{"(a NAME=value word)", "(1 argument, not shown)"}},
		{"a versioned program", []string{"/usr/local/bin/python3.12", "-m", "wrap"}, []string{"python3.12", "(2 arguments, not shown)"}},
		{"a token where the program goes", []string{token, "--flag"}, []string{ProgramNotShown, "(1 argument, not shown)"}},
		{"a path ending in a token", []string{"/opt/bin/" + token}, []string{ProgramNotShown}},
		{"a hex key where the program goes", []string{"deadbeefcafe4bad"}, []string{ProgramNotShown}},
		{"a base64 key and its padding where the program goes", []string{token + "=="}, []string{ProgramNotShown}},
		// A made-up token written in segments that each hold a digit, with no long run: it has no
		// command's shape. A compiler's name with a digit in two of its segments keeps one.
		{"a token in segments where the program goes", []string{"a1b2-c3d4-e5f6-g7h8-i9j0", "--x"}, []string{ProgramNotShown, "(1 argument, not shown)"}},
		{"a cross compiler", []string{"/usr/bin/aarch64-linux-gnu-gcc-12"}, []string{"aarch64-linux-gnu-gcc-12"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := PrefixShown(tc.prefix)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("PrefixShown(%q) = %q, want %q", tc.prefix, got, tc.want)
			}
			shown := WithPrefixShown(tc.prefix, invocation)
			if want := strings.TrimSpace(strings.Join(tc.want, " ") + " " + invocation); shown != want {
				t.Fatalf("WithPrefixShown = %q, want %q", shown, want)
			}
			for i := 0; i+10 <= len(token); i++ {
				if strings.Contains(shown, token[i:i+10]) {
					t.Fatalf("WithPrefixShown = %q, which holds %q of the token", shown, token[i:i+10])
				}
			}
		})
	}
}
