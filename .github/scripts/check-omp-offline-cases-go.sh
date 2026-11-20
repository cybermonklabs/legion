# shellcheck shell=bash
# shellcheck disable=SC2154 # check-omp-offline.test.sh initializes the shared harness variables.

# Cases of check-omp-offline.sh's Go reading: a door and an exec of the binary with no helper, a
# door through a constant, the reason and how far up it reaches, a start spelled in prose, the
# binary under any name (LookPath, a joined or literal path, a function or method that returns it,
# env, bash -c, a parameter, LookupEnv), the Cmd's own environment, a helper apart from the start,
# exec.CommandContext and the starts inside testbin, and the environment an assignment or a closure
# gives a start. Sourced by check-omp-offline.test.sh, whose fixtures, run_check, named and counters
# they use; not run on its own.

echo "case: a Go test that starts Oh My Pi without the helper is refused, door and exec alike"
root=$(fixture go_bare)
put "$root" packages/daemon/internal/podsafety/bare_test.go <<'EOF'
package podsafety

func TestBare(t *testing.T) {
	omp := testbin.OMP(t)
	cmd := exec.Command(omp, "models", "list")
	cmd.Env = []string{"HOME=" + t.TempDir(), "PATH=/usr/bin:/bin"}
}
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the door" "$(contains "$out" 'bare_test.go:4: a test takes the real Oh My Pi (testbin.OMP)')"
check "names the exec" "$(contains "$out" 'bare_test.go:5: a test starts Oh My Pi')"
check "annotates the line" "$(contains "$out" '::error file=packages/daemon/internal/podsafety/bare_test.go,line=4::')"
check "passes the helped test beside it" "$(is "$(contains "$out" 'helped_test.go')" false)"

echo "case: an exec of a field named omp, in a function apart from the door, is refused"
root=$(fixture go_field)
put "$root" packages/daemon/internal/daemon/live_test.go <<'EOF'
package daemon

func (r *live) paneOmp(args ...string) *exec.Cmd {
	cmd := exec.Command(r.omp, args...)
	cmd.Env = []string{"HOME=" + r.home}
	return cmd
}
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the exec" "$(contains "$out" 'live_test.go:4: a test starts Oh My Pi')"

echo "case: a read of LEGION_TEST_OMP through a constant is a door"
root=$(fixture go_getenv)
put "$root" packages/daemon/internal/daemon/vars.go <<'EOF'
package daemon

const ompVariable = "LEGION_TEST_OMP"
EOF
put "$root" packages/daemon/internal/daemon/getenv_test.go <<'EOF'
package daemon

func TestGetenv(t *testing.T) {
	binary := os.Getenv(ompVariable)
	run(binary)
}
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the read" "$(contains "$out" 'getenv_test.go:4: a test reads LEGION_TEST_OMP')"

echo "case: the reasoned form of a Go start passes"
root=$(fixture go_reasoned)
put "$root" packages/daemon/internal/daemon/install_test.go <<'EOF'
package daemon

func installPlugins(t *testing.T, omp string, environ []string) {
	// omp-offline: environ is the caller's, which testbin.OMPEnviron makes.
	install := exec.Command(omp, "plugin", "install", "dir")
	install.Env = environ
}
EOF
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "counts the reason" "$(contains "$out" 'offline helper (3) or carries a reason (1)')"

echo "case: a reason six lines up is too far"
root=$(fixture go_distant)
put "$root" packages/daemon/internal/daemon/distant_test.go <<'EOF'
package daemon

func installPlugins(t *testing.T, omp string, environ []string) {
	// omp-offline: environ is the caller's, which testbin.OMPEnviron makes.
	_ = 1
	_ = 2
	_ = 3
	_ = 4
	_ = 5
	install := exec.Command(omp, "plugin", "install", "dir")
	install.Env = environ
}
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the exec" "$(contains "$out" 'distant_test.go:10:')"

echo "case: a Go reason on the start's own line, or five lines up, passes"
root=$(fixture go_reason_reach)
put "$root" packages/daemon/internal/daemon/reach_test.go <<'EOF'
package daemon

func installPlugins(t *testing.T, omp string, environ []string) {
	same := exec.Command(omp, "plugin", "install", "dir") // omp-offline: environ is the caller's, which testbin.OMPEnviron makes.
	same.Env = environ
	// omp-offline: environ is the caller's, which testbin.OMPEnviron makes.
	_ = 1
	_ = 2
	_ = 3
	_ = 4
	five := exec.Command(omp, "plugin", "install", "dir")
	five.Env = environ
}
EOF
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "passes the reason on its own line" "$(is "$(contains "$out" 'reach_test.go:4:')" false)"
check "passes the reason five lines up" "$(is "$(contains "$out" 'reach_test.go:11:')" false)"
check "counts both reasons" "$(contains "$out" 'offline helper (3) or carries a reason (2)')"

echo "case: a Go start spelled in a comment or a string starts nothing"
root=$(fixture go_prose)
put "$root" packages/daemon/internal/daemon/prose_test.go <<'EOF'
package daemon

// A test once ran omp := testbin.OMP(t) and exec.Command(omp, "models", "list") with no proxy.
func TestProse(t *testing.T) {
	message := `exec.Command(omp, "models") and testbin.OMP(t)`
	other := "exec.Command(omp, \"models\")"
	t.Log(message, other)
}
EOF
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "counts no start of its own" "$(contains "$out" '3 starts of Oh My Pi in 5 test files read')"

echo "case: a Go exec of the binary under any name: LookPath, a joined path, a literal path, a function or method that returns it, env, bash -c"
root=$(fixture go_values)
put "$root" packages/daemon/internal/podsafety/pinned.go <<'EOF'
package podsafety

// pinnedBinary is the binary LEGION_TEST_OMP names.
func pinnedBinary() string {
	return os.Getenv("LEGION_TEST_OMP")
}

type rig struct{}

func (rig) binary() string {
	return os.Getenv("LEGION_OMP_PATH")
}
EOF
put "$root" packages/daemon/internal/podsafety/values_test.go <<'EOF'
package podsafety

func TestValues(t *testing.T) {
	found, err := exec.LookPath("omp")
	if err != nil {
		t.Skip(err)
	}
	home := t.TempDir()
	byPath := exec.Command(found, "models", "list")
	byPath.Env = []string{"HOME=" + home}
	joined := exec.Command(filepath.Join(home, ".local", "bin", "omp"), "models")
	joined.Env = []string{"HOME=" + home}
	returned := exec.Command(pinnedBinary(), "models")
	returned.Env = []string{"HOME=" + home}
	var r rig
	method := exec.Command(r.binary(), "models")
	method.Env = []string{"HOME=" + home}
	wrapped := exec.Command("env", "HOME="+home, found, "models")
	wrapped.Env = []string{"HOME=" + home}
	run := func(program string) *exec.Cmd {
		return exec.Command(program, "models")
	}
	_ = run(found)
	looked, _ := os.LookupEnv("LEGION_TEST_OMP")
	byLookup := exec.Command(looked, "models")
	byLookup.Env = []string{"HOME=" + home}
}
EOF
put "$root" packages/daemon/internal/podsafety/literal_test.go <<'EOF'
package podsafety

func TestLiteral(t *testing.T) {
	home := t.TempDir()
	literal := exec.Command("/usr/local/bin/omp", "models")
	literal.Env = []string{"HOME=" + home}
	shell := exec.Command("bash", "-c", "omp models list")
	shell.Env = []string{"HOME=" + home}
}
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names LookPath as a door" "$(contains "$out" 'values_test.go:4: a test resolves the real Oh My Pi (exec.LookPath)')"
check "names the exec of LookPath's" "$(named values_test.go 9)"
check "names the joined path" "$(named values_test.go 11)"
check "names the function's, from another file of the package" "$(named values_test.go 13)"
check "names the method's, which reads LEGION_OMP_PATH" "$(named values_test.go 16)"
check "names it after env" "$(named values_test.go 18)"
check "names it as a parameter the closure's call is given" "$(named values_test.go 21)"
check "names os.LookupEnv of LEGION_TEST_OMP as a door" "$(contains "$out" 'values_test.go:24: a test reads LEGION_TEST_OMP')"
check "names the exec of LookupEnv's value" "$(named values_test.go 25)"
check "names the literal path, in a file with no door" "$(named literal_test.go 5)"
check "names bash -c, in a file with no door" "$(named literal_test.go 7)"
check "does not read the non-test file's Getenv as a door" "$(is "$(contains "$out" 'pinned.go')" false)"

echo "case: a Go exec's own Cmd's environment decides, not a helper elsewhere in its function"
root=$(fixture go_own_env)
put "$root" packages/daemon/internal/podsafety/own_test.go <<'EOF'
package podsafety

func TestOwn(t *testing.T) {
	omp := testbin.OMP(t)
	offline := testbin.OMPOffline(t)
	held := exec.Command(omp, "models", "list")
	held.Env = []string{"HOME=" + t.TempDir(), offline}
	offline = "PATH=/usr/bin"
	bare := exec.Command(omp, "config", "get", "x")
	bare.Env = []string{"HOME=" + t.TempDir()}
	_ = testbin.OMPOffline(t)
	extended := exec.Command(omp, "models")
	extended.Env = testbin.OMPEnviron(t, t.TempDir())
	extended.Env = append(extended.Env, "X=1")
}
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the exec whose Env holds no helper" "$(named own_test.go 9)"
check "passes the exec whose Env names the helper's entry, set again only after it" "$(is "$(named own_test.go 6)" false)"
check "passes the exec whose Env appends to the helper's" "$(is "$(named own_test.go 12)" false)"
check "passes the door, in a function that calls the helper" "$(is "$(named own_test.go 4)" false)"
check "and nothing else" "$(contains "$out" '^1 problems over')"

echo "case: a binary from a helper that calls OMPOffline is held to its own exec's environment"
root=$(fixture go_split)
put "$root" packages/daemon/internal/podsafety/split_test.go <<'EOF'
package podsafety

func pinned(t *testing.T) string {
	_ = testbin.OMPOffline(t)
	return testbin.OMP(t)
}

func TestSplit(t *testing.T) {
	bin := pinned(t)
	cmd := exec.Command(bin, "models", "list")
	cmd.Env = []string{"HOME=" + t.TempDir()}
}
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the exec" "$(named split_test.go 10)"
check "passes the door, in the helper that calls OMPOffline" "$(is "$(named split_test.go 5)" false)"

echo "case: a Go exec.CommandContext, a function of the package that qualifies the call and a start inside testbin are read; a door passes by a helper in its function's closure, or around its closure"
root=$(fixture go_more)
put "$root" packages/daemon/internal/rigs/rigs.go <<'EOF'
package rigs

func Binary() string {
	return os.Getenv("LEGION_TEST_OMP")
}
EOF
put "$root" packages/daemon/internal/podsafety/more_test.go <<'EOF'
package podsafety

func TestContext(t *testing.T) {
	ctx := context.Background()
	omp := testbin.OMP(t)
	bare := exec.CommandContext(ctx, omp, "models", "list")
	bare.Env = []string{"HOME=" + t.TempDir()}
	helped := exec.CommandContext(ctx, omp, "models", "list")
	helped.Env = testbin.OMPEnviron(t, t.TempDir())
	fromRigs := exec.Command(rigs.Binary(), "models")
	fromRigs.Env = []string{"HOME=" + t.TempDir()}
}

func TestSubtest(t *testing.T) {
	omp := testbin.OMP(t)
	t.Run("models", func(t *testing.T) {
		cmd := exec.Command(omp, "models", "list")
		cmd.Env = testbin.OMPEnviron(t, t.TempDir())
	})
}

func TestOuter(t *testing.T) {
	environ := testbin.OMPEnviron(t, t.TempDir())
	t.Run("models", func(t *testing.T) {
		cmd := exec.Command(testbin.OMP(t), "models", "list")
		cmd.Env = environ
	})
}
EOF
put "$root" packages/daemon/internal/testbin/run.go <<'EOF'
package testbin

func runBare(t *testing.T) {
	cmd := exec.Command(OMP(t), "models", "list")
	cmd.Env = []string{"HOME=" + t.TempDir()}
}

func runHelped(t *testing.T) {
	cmd := exec.Command(OMP(t), "models", "list")
	cmd.Env = append([]string{"HOME=" + t.TempDir()}, OMPOffline(t))
}
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the bare exec.CommandContext" "$(named more_test.go 6)"
check "passes the exec.CommandContext under the helper" "$(is "$(named more_test.go 8)" false)"
check "names the binary from a function of the package that qualifies the call" "$(named more_test.go 10)"
check "passes the door whose function calls the helper in a closure" "$(is "$(named more_test.go 15)" false)"
check "passes the door in a closure whose function calls the helper" "$(is "$(named more_test.go 25)" false)"
check "names the bare start inside testbin" "$(named testbin/run.go 4)"
check "passes the start inside testbin under its own OMPOffline" "$(is "$(named testbin/run.go 9)" false)"
check "counts them" "$(contains "$out" '3 problems over the 13 starts of Oh My Pi in 6 test files read (Go 12, TypeScript 1)')"

echo "case: the nearest assignment before a start decides its environment"
root=$(fixture go_nearest)
put "$root" packages/daemon/internal/podsafety/nearest_test.go <<'EOF'
package podsafety

func TestNearest(t *testing.T) {
	omp := testbin.OMP(t)
	env := []string{"HOME=" + t.TempDir()}
	env = append(env, testbin.OMPOffline(t))
	cmd := exec.Command(omp, "models")
	cmd.Env = env
}
EOF
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "counts the door and the exec under the helper" "$(contains "$out" 'offline helper (5) or carries a reason (0)')"

echo "case: the environment a closure is given is followed to its every call"
root=$(fixture go_param)
put "$root" packages/daemon/internal/podsafety/param_test.go <<'EOF'
package podsafety

func TestParam(t *testing.T) {
	omp := testbin.OMP(t)
	bare := []string{"HOME=" + t.TempDir(), testbin.OMPOffline(t)}
	run := func(env []string) {
		cmd := exec.Command(omp, "models", "list")
		cmd.Env = env
	}
	run(bare)
	run(append(bare, "X=1"))
}
EOF
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "counts the door and the exec under the helper" "$(contains "$out" 'offline helper (5) or carries a reason (0)')"
root=$(fixture go_param_bare)
put "$root" packages/daemon/internal/podsafety/param_test.go <<'EOF'
package podsafety

func TestParam(t *testing.T) {
	omp := testbin.OMP(t)
	bare := []string{"HOME=" + t.TempDir(), testbin.OMPOffline(t)}
	run := func(env []string) {
		cmd := exec.Command(omp, "models", "list")
		cmd.Env = env
	}
	run(bare)
	run([]string{"HOME=" + t.TempDir()})
}
EOF
run_check "$root"
check "fails when one call gives it no helper" "$(is "$status" 1)"
check "names the exec" "$(named param_test.go 7)"
