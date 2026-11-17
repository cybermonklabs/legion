"""How two checks run go over this repository and read what it prints: check-ci-filters.sh,
through ci_filter_coverage.py's Session, and check-legion-version-inputs.sh. Each check keeps its
own error handling and its own rules; what they share is the environment a go command runs under
and how its answers are read, so the two cannot come to list a closure under different ones.

A go command runs (run) with GOPROXY=off over a module cache of the caller's that starts empty:
nothing is downloaded, no go.work.sum is rewritten, and a cold runner gives the answer a warm
machine does. A third-party package then fails to load, which loses no package of this
repository, since no module outside it imports one (check-ci-filters.sh's header says why).

GOPROXY=off alone is not offline. A GONOPROXY that matches a module, or a GOPRIVATE when
GONOPROXY is unset (cmd/go/internal/cfg/cfg.go:481-483 in go1.27.3), puts a direct fetch of it,
"noproxy", before every proxy of the list (cmd/go/internal/modfetch/proxy.go:63-65), so an
inherited GOPRIVATE=example.org sends go to https://example.org for a module the empty cache
lacks. GONOPROXY=none matches no module, so the list is "off" alone; with no download, no
checksum database lookup follows either, so GOPRIVATE and GONOSUMDB need no value of their own.
And a variable set to the empty string is no override in go: cfg.Getenv reads the user's go/env
file for it instead (cfg.go:437-442), so GOFLAGS= and GOWORK= alone would still take a
`go env -w` value. GOENV=off leaves that file unread, so GOFLAGS is empty and no flag of the
machine changes what go lists, and GOWORK is empty, so go finds the go.work above the directory
as a plain build there does.

The toolchain is the one thing that empty cache cannot hold. GOTOOLCHAIN=auto satisfies a `go`
directive above the installed go by fetching that toolchain into GOMODCACHE, so every command
over the empty cache would try to fetch it again and GOPROXY=off would refuse: `toolchain not
available`, and every listing unknown. So toolchain() resolves the module's toolchain first,
under this machine's own cache and proxy (`go env GOROOT`), and run() starts that toolchain's own
binary under GOTOOLCHAIN=local.

Both checks import it from this directory, which their scripts put on PYTHONPATH; their harnesses
link it into each fixture beside the script under test.
"""

from __future__ import annotations

import json
import os
import subprocess
from pathlib import Path

# What every go command runs under, over the caller's empty module cache (the header says why each).
OFFLINE = {"GOPROXY": "off", "GONOPROXY": "none", "GOENV": "off", "GOFLAGS": "", "GOWORK": "",
           "GOTOOLCHAIN": "local"}


def toolchain(workdir) -> tuple[str | None, str]:
    """The go binary the module in `workdir` pins, resolved under this machine's own environment,
    and what `go env GOROOT` wrote to stderr: the binary is None when that names no toolchain
    holding a bin/go. With no go on PATH at all it raises FileNotFoundError."""
    found = subprocess.run(["go", "env", "GOROOT"], cwd=workdir, capture_output=True, text=True, check=False)
    root = found.stdout.strip()
    binary = Path(root) / "bin" / "go" if root else None
    return (str(binary) if found.returncode == 0 and binary and binary.is_file() else None), found.stderr


def run(binary, args, workdir, modcache, env=None) -> subprocess.CompletedProcess:
    """`binary args` in `workdir`, offline over the module cache `modcache`, with `env` (the
    caller's GOOS, GOARCH, CGO_ENABLED and the like) on top. The exit status is the caller's to
    judge."""
    environment = dict(os.environ, GOMODCACHE=str(modcache), **OFFLINE)
    environment.update(env or {})
    return subprocess.run([str(binary), *args], cwd=workdir, env=environment, capture_output=True, text=True,
                          check=False)


def decode_stream(text: str) -> list:
    """The JSON values of a `go list -json` or `go list -m -json` stream, in order."""
    decoder, index, out = json.JSONDecoder(), 0, []
    while True:
        while index < len(text) and text[index].isspace():
            index += 1
        if index >= len(text):
            return out
        obj, index = decoder.raw_decode(text, index)
        out.append(obj)


def relative(path, root: Path) -> str | None:
    """`path` relative to `root`, resolved, or None when it is outside it (GOROOT, the module
    cache)."""
    try:
        return Path(path).resolve().relative_to(root).as_posix()
    except ValueError:
        return None
