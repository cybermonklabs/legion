# shellcheck shell=bash
# shellcheck disable=SC2154 # check-go-offline.test.sh initializes the shared harness variables.

# Cases of check-go-offline.sh's rule 1 in Python test files: a bare site, the offline and the
# reasoned form, and one planted case for each Python form the check's header names. Sourced by
# check-go-offline.test.sh, whose fixtures, run_check and counters they use; not run on its own.

echo "case: a Python test that runs go list -deps with no rule is refused"
root=$(fixture py-bare)
put "$root" tools/test_deps.py <<'EOF'
import subprocess


def test_deps():
    subprocess.run(["go", "list", "-deps", "./..."], check=True)
EOF
run_check "$root"
refused tools/test_deps.py 5

echo "case: the Python test with \"GOPROXY\": \"off\" in the run's environment passes"
root=$(fixture py-offline)
put "$root" tools/test_deps.py <<'EOF'
import os
import subprocess


def test_deps():
    subprocess.run(["go", "list", "-deps", "./..."], check=True,
                   env={**os.environ, "GOPROXY": "off"})
EOF
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "counts the command as offline" "$(contains "$out" 'runs with GOPROXY=off (1), carries a reason (0)')"

echo "case: the Python test with a reason on the line above passes"
root=$(fixture py-reasoned)
put "$root" tools/test_deps.py <<'EOF'
import subprocess


def test_deps():
    # go-offline: it lists the standard library, which reads no module.
    subprocess.run(["go", "list", "-deps", "std"], check=True)
EOF
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "counts the command as reasoned" "$(contains "$out" 'runs with GOPROXY=off (0), carries a reason (1)')"

echo "case: Python: an argv Biome-style, one element a line"
refuses py-multiline tools/test_multiline.py 6 <<'EOF'
import subprocess


def test_deps():
    subprocess.run(
        [
            "go",
            "list",
            "-deps",
        ]
    )
EOF

echo "case: Python: os.system, subprocess's string, shell=True and an f-string"
refuses py-system tools/test_system.py 5 <<'EOF'
import os


def test_deps():
    os.system("go list -deps ./...")
EOF
refuses py-string tools/test_string.py 5 <<'EOF'
import subprocess


def test_deps():
    subprocess.run("go list -deps ./...")
EOF
refuses py-shell tools/test_shell.py 5 <<'EOF'
import subprocess


def test_build():
    subprocess.run("cd sub && go build ./...", shell=True)
EOF
refuses py-fstring tools/test_fstring.py 5 <<'EOF'
import subprocess


def test_deps(pkg):
    subprocess.run(f"go list -deps {pkg}", shell=True)
EOF

echo "case: Python: GOPROXY off before a ** spread is overridden; dict(os.environ, GOPROXY=\"off\") is offline"
refuses py-before-spread tools/test_spread.py 5 <<'EOF'
import os, subprocess


def test_deps():
    subprocess.run(["go", "list"], env={"GOPROXY": "off", **os.environ})
EOF
passes_offline py-dict tools/test_dict.py <<'EOF'
import os, subprocess


def test_deps():
    subprocess.run(["go", "list"], env=dict(os.environ, GOPROXY="off"))
EOF

echo "case: Python: a shell's -c in an argv, go after env, and a go shutil.which found"
refuses py-bash-c tools/test_bash.py 5 <<'EOF'
import subprocess


def test_build():
    subprocess.run(["bash", "-c", "go build ./..."])
EOF
refuses py-env-argv tools/test_env.py 5 <<'EOF'
import subprocess


def test_build():
    subprocess.run(["env", "GOFLAGS=-mod=mod", "go", "build"])
EOF
refuses py-which tools/test_which.py 6 <<'EOF'
import shutil, subprocess


def test_build():
    go = shutil.which("go")
    subprocess.run([go, "build", "./..."])
EOF

echo "case: Python: os.popen of a go command, and the go binary as a path.join of a last \"go\""
refuses py-popen tools/test_popen.py 5 <<'EOF'
import os


def test_deps():
    os.popen("go list -deps ./...").read()
EOF
refuses py-join tools/test_join.py 5 <<'EOF'
import os, subprocess


def test_build(goroot):
    subprocess.run([os.path.join(goroot, "bin", "go"), "build", "./..."])
EOF

echo "case: Python: go after timeout, flock or exec in an argv"
refuses py-timeout tools/test_timeout.py 5 <<'EOF'
import subprocess


def test_deps():
    subprocess.run(["timeout", "600", "go", "test", "./..."])
EOF
refuses py-flock tools/test_flock.py 5 <<'EOF'
import subprocess


def test_build():
    subprocess.run(["flock", "/tmp/lock", "exec", "go", "build", "./..."])
EOF

echo "case: Python: an argv tuple, a call's own arguments, and a shutil.which(\"go\") nothing here starts"
refuses py-tuple tools/test_tuple.py 5 <<'EOF'
import subprocess


def test_build():
    subprocess.run(("go", "build", "./..."))
EOF
refuses py-call-args tools/test_args.py 5 <<'EOF'
import asyncio


async def test_build():
    await asyncio.create_subprocess_exec("go", "build", "./...")
EOF
refuses py-which-alone tools/test_alone.py 5 <<'EOF'
import shutil


def go_binary():
    return shutil.which("go")
EOF

echo "case: Python: shlex.split of a go command is read as shell, in the environment of the call it is given to"
refuses py-shlex tools/test_shlex.py 5 <<'EOF'
import shlex, subprocess


def test_build():
    subprocess.run(shlex.split("go build ./..."))
EOF
passes_offline py-shlex-offline tools/test_shlex.py <<'EOF'
import os, shlex, subprocess


def test_build():
    subprocess.run(shlex.split("go build ./..."), env={**os.environ, "GOPROXY": "off"})
EOF

echo "case: Python: a name bound in the module, read in a function"
refuses py-module-name tools/test_module.py 6 <<'EOF'
import subprocess

GO = "go"

def test_build():
    subprocess.run([GO, "build", "./..."])
EOF
passes_offline py-module-env tools/test_module.py <<'EOF'
import os, subprocess

ENV = {**os.environ, "GOPROXY": "off"}

def test_build():
    subprocess.run(["go", "build", "./..."], env=ENV)
EOF

echo "case: Python: a file its ast module cannot parse is refused, since none of its go commands is seen"
root=$(fixture py-unparsable)
put "$root" tools/test_broken.py <<'EOF'
import subprocess

def test_build(:
    subprocess.run(["go", "build", "./..."])
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the file" "$(contains "$out" 'tools/test_broken.py:1: a Python file this check cannot parse')"
