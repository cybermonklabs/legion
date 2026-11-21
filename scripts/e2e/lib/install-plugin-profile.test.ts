import { describe, expect, test } from "bun:test";
import {
  copyFileSync,
  existsSync,
  mkdirSync,
  readFileSync,
  realpathSync,
  writeFileSync,
} from "node:fs";
import { join } from "node:path";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";

// Which Oh My Pi install-plugin-profile.sh runs against the profile, and how. The script runs from a
// fake checkout (it finds the checkout from its own path), with a fake pack-plugin.sh that packs a
// manifest alone, the real lib/pinned-omp.sh and .github/scripts/omp-digest.sh, an .omp-pin.sha256
// recording the fake install's bin/omp, and a `mise` first on PATH that records every call, names a
// fake install for `mise where`, and answers `omp plugin list`. An `omp` wrapper sits on PATH
// beside it, as an operator's does: the installer must run the install's own bin/omp by its
// absolute path instead.
const root = join(import.meta.dir, "..", "..", "..");
const dir = temporaryDirectory("install-plugin-profile-test");
const repoPin = "github:sjawhar/oh-my-pi@18.12.2-sami.20261025-144204";
const installedOmp = "#!/bin/sh\nexit 0\n";

let runs = 0;
/** Runs the installer with `extra` arguments, on a fake install of the pin holding bin/omp and
 * each file of `beside`, which the checkout's digest does not name. */
function install(extra: string[], beside: string[] = []) {
  const run = join(dir, `run-${++runs}`);
  const checkout = join(run, "checkout");
  const lib = join(checkout, "scripts", "e2e", "lib");
  mkdirSync(lib, { recursive: true });
  copyFileSync(
    join(import.meta.dir, "install-plugin-profile.sh"),
    join(lib, "install-plugin-profile.sh")
  );
  writeFileSync(join(checkout, ".omp-pin"), `${repoPin}\n`);
  copyFileSync(join(import.meta.dir, "pinned-omp.sh"), join(lib, "pinned-omp.sh"));
  mkdirSync(join(checkout, ".github", "scripts"), { recursive: true });
  copyFileSync(
    join(root, ".github", "scripts", "omp-digest.sh"),
    join(checkout, ".github", "scripts", "omp-digest.sh")
  );
  const digest = new Bun.CryptoHasher("sha256").update(installedOmp).digest("hex");
  writeFileSync(join(checkout, ".omp-pin.sha256"), `${digest}  bin/omp\n`);
  writeFileSync(
    join(lib, "pack-plugin.sh"),
    `#!/usr/bin/env bash
set -euo pipefail
mkdir -p "$2/package"
printf '{"name":"@sjawhar/%s","version":"0.0.0-test"}\\n' "$1" >"$2/package/package.json"
tar czf "$2/plugin.tgz" -C "$2" package
printf '%s\\n' "$2/plugin.tgz"
`
  );
  const installDir = join(run, "mise-install");
  mkdirSync(join(installDir, "bin"), { recursive: true });
  writeFileSync(join(installDir, "bin", "omp"), installedOmp, { mode: 0o755 });
  for (const file of beside) writeFileSync(join(installDir, file), "#!/bin/sh\n", { mode: 0o755 });
  const log = join(run, "mise.log");
  const wrapperRan = join(run, "wrapper-ran");
  const bin = join(run, "bin");
  mkdirSync(bin);
  writeFileSync(
    join(bin, "mise"),
    `#!/usr/bin/env bash
printf '%s\\n' "$*" >>${JSON.stringify(log)}
case $1 in
where) printf '%s\\n' ${JSON.stringify(installDir)} ;;
x)
  # Run the command, as the real mise does, so an installer that named \`omp\` rather than the
  # build's own path would run the wrapper first on PATH and the test would see it.
  cmd=("\${@:4}")
  "\${cmd[@]}" >/dev/null 2>&1 || true
  if [ "\${cmd[1]:-} \${cmd[2]:-}" = "plugin list" ]; then
    printf '{"npm":[{"name":"@sjawhar/pi-envoy","version":"0.0.0-test","enabled":true,"path":"%s"}]}\\n' "$PWD"
  fi
  ;;
esac
`,
    { mode: 0o755 }
  );
  writeFileSync(join(bin, "omp"), `#!/bin/sh\ntouch ${JSON.stringify(wrapperRan)}\n`, {
    mode: 0o755,
  });
  const home = join(run, "omp-home");
  mkdirSync(join(home, ".omp"), { recursive: true });
  const dest = join(run, "pi-envoy");
  const result = Bun.spawnSync(
    [
      "bash",
      join(lib, "install-plugin-profile.sh"),
      "--package",
      "pi-envoy",
      "--profile",
      "legion-e2e-test",
      "--home",
      home,
      "--dest",
      dest,
      ...extra,
    ],
    { env: { PATH: `${bin}:${process.env.PATH ?? ""}`, HOME: join(run, "caller-home") } }
  );
  return {
    dest,
    omp: realpathSync(join(installDir, "bin", "omp")),
    calls: existsSync(log) ? readFileSync(log, "utf8").trim().split("\n") : [],
    wrapperRan: existsSync(wrapperRan),
    code: result.exitCode,
    stderr: result.stderr.toString(),
  };
}

describe("install-plugin-profile.sh's Oh My Pi", () => {
  test("runs the build --omp names, by its own bin/omp's absolute path, for the install and the verification", () => {
    const spec = "github:sjawhar/oh-my-pi@18.12.2-sami.20261026-004924";
    const r = install(["--omp", spec]);
    expect(r.stderr).toContain("enabled in OMP profile legion-e2e-test");
    expect(r.code).toBe(0);
    expect(r.calls).toEqual([
      `where ${spec}`,
      `where ${spec}`,
      `x ${spec} -- ${r.omp} plugin install ${r.dest}`,
      `x ${spec} -- ${r.omp} plugin list --json`,
    ]);
    // The `omp` on PATH, an operator's wrapper on a devbox, never runs.
    expect(r.wrapperRan).toBe(false);
  });

  test("runs .omp-pin's build when no --omp is given", () => {
    const r = install([]);
    expect(r.code).toBe(0);
    expect(r.calls).toEqual([
      `where ${repoPin}`,
      `where ${repoPin}`,
      `x ${repoPin} -- ${r.omp} plugin install ${r.dest}`,
      `x ${repoPin} -- ${r.omp} plugin list --json`,
    ]);
  });

  // Catches: an installer that runs .omp-pin's build before lib/pinned-omp.sh holds its install to
  // .omp-pin.sha256, here an install whose bin/omp is the recorded bytes with a kubectl beside it.
  test("refuses .omp-pin's build when its install holds a file the digest does not name, running nothing", () => {
    const r = install([], ["bin/kubectl"]);
    expect(r.code).toBe(1);
    expect(r.stderr).toContain("bin/kubectl: a regular file");
    expect(r.calls).toEqual([`where ${repoPin}`, `where ${repoPin}`]);
    expect(r.wrapperRan).toBe(false);
  });

  test("refuses an empty --omp, running nothing", () => {
    const r = install(["--omp", ""]);
    expect(r.code).toBe(2);
    expect(r.stderr).toContain("--omp needs a value");
    expect(r.calls).toEqual([]);
  });
});
