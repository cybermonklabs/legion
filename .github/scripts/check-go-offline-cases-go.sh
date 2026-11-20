# shellcheck shell=bash
# shellcheck disable=SC2154 # check-go-offline.test.sh initializes the shared harness variables.

# Cases of check-go-offline.sh's rule 1 in Go test files: a bare site, the offline and the reasoned
# form, and one planted case for each Go form the check's header names. Sourced by
# check-go-offline.test.sh, whose fixtures, run_check and counters they use; not run on its own.
# The Go plants need not compile: the check reads text.

echo "case: a Go test that runs go list -deps with no rule is refused, with an annotation"
root=$(fixture go-bare)
put "$root" packages/app/deps_test.go <<'EOF'
package app

import (
	"os/exec"
	"testing"
)

func TestDeps(t *testing.T) {
	if err := exec.Command("go", "list", "-deps", "./...").Run(); err != nil {
		t.Fatal(err)
	}
}
EOF
run_check "$root"
refused packages/app/deps_test.go 9
check "counts it against the total" "$(contains "$out" '1 problems over the 1 go commands')"

echo "case: the Go test with GOPROXY=off in the command's environment passes"
root=$(fixture go-offline)
put "$root" packages/app/deps_test.go <<'EOF'
package app

import (
	"os"
	"os/exec"
	"testing"
)

func TestDeps(t *testing.T) {
	cmd := exec.Command("go", "list", "-deps", "./...")
	cmd.Env = append(os.Environ(), "GOPROXY=off")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
}
EOF
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "counts the command as offline" "$(contains "$out" 'every one of the 1 go commands .* runs with GOPROXY=off (1), carries a reason (0)')"

echo "case: the Go test with a reason on the line above passes"
root=$(fixture go-reasoned)
put "$root" packages/app/deps_test.go <<'EOF'
package app

import (
	"os/exec"
	"testing"
)

func TestDeps(t *testing.T) {
	// go-offline: it lists the standard library, which reads no module.
	if err := exec.Command("go", "list", "-deps", "std").Run(); err != nil {
		t.Fatal(err)
	}
}
EOF
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "counts the command as reasoned" "$(contains "$out" 'runs with GOPROXY=off (0), carries a reason (1)')"

echo "case: a reason six lines above the Go command is too far"
root=$(fixture go-distant)
put "$root" packages/app/deps_test.go <<'EOF'
package app

import (
	"os/exec"
	"testing"
)

// go-offline: it lists the standard library, which reads no module.
func TestDeps(t *testing.T) {
	t.Helper()
	t.Parallel()
	t.Log("listing")
	t.Log("the standard library")
	if err := exec.Command("go", "list", "-deps", "std").Run(); err != nil {
		t.Fatal(err)
	}
}
EOF
run_check "$root"
refused packages/app/deps_test.go 14

echo "case: Go: an argv Biome writes one element a line is one site"
refuses go-biome packages/app/biome_test.go 4 <<'EOF'
package app

func TestDeps(t *testing.T) {
	list := exec.Command(
		"go",
		"list",
		"-deps",
		"./...",
	)
	_ = list.Run()
}
EOF

echo "case: Go: exec.CommandContext whose context is a call"
refuses go-context packages/app/context_test.go 4 <<'EOF'
package app

func TestDeps(t *testing.T) {
	_ = exec.CommandContext(ctxOf(t), "go", "build", "./...").Run()
}
EOF

echo "case: Go: a command does not borrow the GOPROXY=off of the next one's Env"
refuses go-neighbour packages/app/neighbour_test.go 4 2 <<'EOF'
package app

func TestDeps(t *testing.T) {
	list := exec.Command("go", "list", "-deps", "./...")
	_ = list.Run()
	build := exec.Command("go", "build", "./...")
	build.Env = append(os.Environ(), "GOPROXY=off")
	_ = build.Run()
}
EOF

echo "case: Go: the go binary as filepath.Join(runtime.GOROOT(), \"bin\", \"go\")"
refuses go-goroot packages/app/goroot_test.go 4 <<'EOF'
package app

func TestDeps(t *testing.T) {
	_ = exec.Command(filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "./...").Run()
}
EOF

echo "case: Go: the go binary as a literal path ending /go"
refuses go-path packages/app/path_test.go 4 <<'EOF'
package app

func TestDeps(t *testing.T) {
	_ = exec.Command("/usr/local/go/bin/go", "build", "./...").Run()
}
EOF

echo "case: Go: the go binary exec.LookPath found, started in the same function"
refuses go-lookpath packages/app/lookpath_test.go 8 <<'EOF'
package app

func TestDeps(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	_ = exec.Command(goBin, "build", "./...").Run()
}
EOF

echo "case: Go: an exec.LookPath(\"go\") nothing here starts resolves the go a later line starts"
refuses go-lookpath-alone packages/app/resolve_test.go 4 <<'EOF'
package app

func goBinary(t *testing.T) string {
	path, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	return path
}
EOF

echo "case: Go: a go bound to a variable in the same function"
refuses go-variable packages/app/variable_test.go 5 <<'EOF'
package app

func TestDeps(t *testing.T) {
	goBin := "go"
	_ = exec.Command(goBin, "build", "./...").Run()
}
EOF

echo "case: Go: a shell's -c whose script runs go"
refuses go-bash-c packages/app/bash_test.go 4 <<'EOF'
package app

func TestDeps(t *testing.T) {
	_ = exec.Command("bash", "-c", "cd sub && go build ./...").Run()
}
EOF

echo "case: Go: go after env in an argv, with its own settings"
refuses go-env-argv packages/app/env_test.go 4 <<'EOF'
package app

func TestDeps(t *testing.T) {
	_ = exec.Command("env", "GOFLAGS=-mod=mod", "go", "build", "./...").Run()
}
EOF
passes_offline go-env-argv-offline packages/app/env_test.go <<'EOF'
package app

func TestDeps(t *testing.T) {
	_ = exec.Command("env", "GOPROXY=off", "go", "build", "./...").Run()
}
EOF

echo "case: Go: a \"GOPROXY=off\" in a trailing comment sets nothing"
refuses go-comment packages/app/comment_test.go 4 <<'EOF'
package app

func TestDeps(t *testing.T) {
	build := exec.Command("go", "build", "./...") // "GOPROXY=off"
	_ = build.Run()
}
EOF

echo "case: Go: GOPROXY=off before the inherited environment, or before a later GOPROXY, is overridden"
refuses go-before-environ packages/app/before_test.go 4 <<'EOF'
package app

func TestDeps(t *testing.T) {
	build := exec.Command("go", "build", "./...")
	build.Env = append([]string{"GOPROXY=off"}, os.Environ()...)
	_ = build.Run()
}
EOF
refuses go-later-goproxy packages/app/later_test.go 4 <<'EOF'
package app

func TestDeps(t *testing.T) {
	build := exec.Command("go", "build", "./...")
	build.Env = append(os.Environ(), "GOPROXY=off", "GOPROXY=https://proxy.golang.org")
	_ = build.Run()
}
EOF

echo "case: Go: an Env bound to a variable that ends in GOPROXY=off is offline"
passes_offline go-bound-env packages/app/bound_test.go <<'EOF'
package app

func TestDeps(t *testing.T) {
	env := append(os.Environ(), "GOPROXY=off")
	build := exec.Command("go", "build", "./...")
	build.Env = env
	_ = build.Run()
}
EOF

echo "case: Go: the go binary as a + of strings that ends /go"
refuses go-concat packages/app/concat_test.go 4 <<'EOF'
package app

func TestDeps(t *testing.T) {
	_ = exec.Command(runtime.GOROOT()+"/bin/go", "build", "./...").Run()
}
EOF

echo "case: Go: env -i in an argv clears the Env's GOPROXY=off"
refuses go-env-i packages/app/clear_test.go 4 <<'EOF'
package app

func TestDeps(t *testing.T) {
	build := exec.Command("env", "-i", "HOME=/tmp", "go", "build", "./...")
	build.Env = append(os.Environ(), "GOPROXY=off")
	_ = build.Run()
}
EOF

echo "case: Go: a shell's -c among its other options (bash -ec)"
refuses go-bash-ec packages/app/bashec_test.go 4 <<'EOF'
package app

func TestDeps(t *testing.T) {
	_ = exec.Command("bash", "-ec", "go build ./...").Run()
}
EOF

echo "case: Go: os/exec imported under another name, and as a dot import"
refuses go-exec-alias packages/app/alias_test.go 9 <<'EOF'
package app

import (
	osexec "os/exec"
	"testing"
)

func TestDeps(t *testing.T) {
	_ = osexec.Command("go", "build", "./...").Run()
}
EOF
refuses go-exec-dot packages/app/dot_test.go 5 <<'EOF'
package app

import . "os/exec"

func TestDeps(t *testing.T) { _ = Command("go", "build", "./...").Run() }
EOF

echo "case: Go: go after timeout, nice, command, nohup, time or flock in an argv, with their operands"
refuses go-timeout packages/app/timeout_test.go 4 <<'EOF'
package app

func TestDeps(t *testing.T) {
	_ = exec.Command("timeout", "600", "go", "test", "./...").Run()
}
EOF
refuses go-nice packages/app/nice_test.go 4 <<'EOF'
package app

func TestDeps(t *testing.T) {
	_ = exec.Command("nice", "-n", "5", "command", "go", "build", "./...").Run()
}
EOF
refuses go-nohup packages/app/nohup_test.go 4 <<'EOF'
package app

func TestDeps(t *testing.T) {
	_ = exec.Command("nohup", "time", "flock", "/tmp/lock", "exec", "go", "build", "./...").Run()
}
EOF

echo "case: Go: a go bound to a package variable"
refuses go-package-var packages/app/package_test.go 5 <<'EOF'
package app

var goBin = "go"

func TestDeps(t *testing.T) { _ = exec.Command(goBin, "build", "./...").Run() }
EOF

echo "case: Go: a filepath.Join(..., \"bin\", \"go\") nothing here starts resolves the go a later line starts"
refuses go-join-alone packages/app/join_test.go 4 <<'EOF'
package app

func goBinary() string {
	return filepath.Join(runtime.GOROOT(), "bin", "go")
}
EOF

echo "case: Go: a \"GOPROXY=off\" no command is given sets nothing"
refuses go-loose-string packages/app/loose_test.go 5 <<'EOF'
package app

func TestDeps(t *testing.T) {
	_ = "GOPROXY=off"
	_ = exec.Command("go", "build", "./...").Run()
}
EOF
