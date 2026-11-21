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
// manifest alone, and a `mise` first on PATH that records every call, names a fake install for
// `mise where`, and answers `omp plugin list`. An `omp` wrapper sits on PATH beside it, as an
// operator's does: the installer must run the install's own bin/omp by its absolute path instead.
// Each run also holds an omp binary of its own, as a pod carries the pinned one with no mise, which
// records its calls and answers `omp plugin list` as the mise stand-in does.
const dir = temporaryDirectory("install-plugin-profile-test");
const repoPin = "github:sjawhar/oh-my-pi@18.12.2-sami.20261025-144204";

let runs = 0;
/** One run's fake checkout and stand-ins, which `install` runs the installer in. */
function fixture() {
  const run = join(dir, `run-${++runs}`);
  const checkout = join(run, "checkout");
  const lib = join(checkout, "scripts", "e2e", "lib");
  mkdirSync(lib, { recursive: true });
  copyFileSync(
    join(import.meta.dir, "install-plugin-profile.sh"),
    join(lib, "install-plugin-profile.sh")
  );
  writeFileSync(join(checkout, ".omp-pin"), `${repoPin}\n`);
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
  writeFileSync(join(installDir, "bin", "omp"), "#!/bin/sh\nexit 0\n", { mode: 0o755 });
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
  const binary = join(run, "pod", "omp");
  const binaryLog = join(run, "binary.log");
  mkdirSync(join(run, "pod"));
  writeFileSync(
    binary,
    `#!/usr/bin/env bash
printf '%s\\n' "$*" >>${JSON.stringify(binaryLog)}
if [ "$1 $2" = "plugin list" ]; then
  printf '{"npm":[{"name":"@sjawhar/pi-envoy","version":"0.0.0-test","enabled":true,"path":"%s"}]}\\n' "$PWD"
fi
`,
    { mode: 0o755 }
  );
  const home = join(run, "omp-home");
  mkdirSync(join(home, ".omp"), { recursive: true });
  return {
    run,
    lib,
    bin,
    home,
    dest: join(run, "pi-envoy"),
    installDir,
    log,
    wrapperRan,
    binary,
    binaryLog,
  };
}

/** The installer run with `extra` after its four required flags, in `f`'s checkout. */
function install(extra: string[], f = fixture()) {
  const result = Bun.spawnSync(
    [
      "bash",
      join(f.lib, "install-plugin-profile.sh"),
      "--package",
      "pi-envoy",
      "--profile",
      "legion-e2e-test",
      "--home",
      f.home,
      "--dest",
      f.dest,
      ...extra,
    ],
    { env: { PATH: `${f.bin}:${process.env.PATH ?? ""}`, HOME: join(f.run, "caller-home") } }
  );
  return {
    dest: f.dest,
    omp: realpathSync(join(f.installDir, "bin", "omp")),
    calls: existsSync(f.log) ? readFileSync(f.log, "utf8").trim().split("\n") : [],
    binaryCalls: existsSync(f.binaryLog)
      ? readFileSync(f.binaryLog, "utf8").trim().split("\n")
      : [],
    wrapperRan: existsSync(f.wrapperRan),
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

  test("refuses an empty --omp, running nothing", () => {
    const r = install(["--omp", ""]);
    expect(r.code).toBe(2);
    expect(r.stderr).toContain("--omp needs a value");
    expect(r.calls).toEqual([]);
  });

  // Catches: a pod's binary reached through mise, which a pod does not carry, and the wrapper on
  // PATH run in its place.
  test("runs the binary --omp-binary names, as it is and with no mise call, for the install and the verification", () => {
    const f = fixture();
    const r = install(["--omp-binary", f.binary], f);
    expect(r.stderr).toContain("enabled in OMP profile legion-e2e-test");
    expect(r.code).toBe(0);
    expect(r.calls).toEqual([]);
    expect(r.binaryCalls).toEqual([`plugin install ${r.dest}`, "plugin list --json"]);
    expect(r.wrapperRan).toBe(false);
  });

  // Catches: a verification failure under --omp-binary that names mise's pin, which that branch
  // never sets, so `set -u` ends the run on `pin: unbound variable` and the listing is lost.
  test("an --omp-binary whose plugin list omits the plugin fails naming the binary and the listing", () => {
    const f = fixture();
    const binary = join(f.run, "pod", "omp-without-plugin");
    writeFileSync(
      binary,
      `#!/usr/bin/env bash
if [ "$1 $2" = "plugin list" ]; then
  printf '{"npm":[]}\\n'
fi
`,
      { mode: 0o755 }
    );
    const r = install(["--omp-binary", binary], f);
    expect(r.stderr).not.toContain("unbound variable");
    expect(r.code).toBe(1);
    expect(r.stderr).toContain(`${realpathSync(binary)} plugin list`);
    expect(r.stderr).toContain("enabled: []");
    expect(r.calls).toEqual([]);
  });

  // Catches: an --omp read as a binary whenever it names an executable file, which is what let a
  // mistyped binary path fall through to `mise install <path>`: --omp means one thing.
  test("--omp is a mise tool spec even when it names an executable file", () => {
    const f = fixture();
    const r = install(["--omp", f.binary], f);
    expect(r.code).toBe(0);
    expect(r.calls).toEqual([
      `where ${f.binary}`,
      `where ${f.binary}`,
      `x ${f.binary} -- ${r.omp} plugin install ${r.dest}`,
      `x ${f.binary} -- ${r.omp} plugin list --json`,
    ]);
    expect(r.binaryCalls).toEqual([]);
  });

  // Catches: a path that names no executable file handed on to mise, which in a pod with no mise
  // fails without naming the path.
  test("refuses an --omp-binary that is not an executable file, naming it, and runs nothing", () => {
    const plain = join(dir, "omp-not-executable");
    writeFileSync(plain, "#!/bin/sh\nexit 0\n", { mode: 0o644 });
    const directory = join(dir, "omp-directory");
    mkdirSync(directory);
    const paths = [join(dir, "no-such-omp"), plain, directory];
    const seen = paths.map((path) => {
      const r = install(["--omp-binary", path]);
      return {
        code: r.code,
        refusal: r.stderr.split("\n")[0],
        calls: r.calls,
        binaryCalls: r.binaryCalls,
        unpacked: existsSync(r.dest),
      };
    });
    expect(seen).toEqual(
      paths.map((path) => ({
        code: 2,
        refusal: `install-plugin-profile: --omp-binary ${path} is not an executable file`,
        calls: [],
        binaryCalls: [],
        unpacked: false,
      }))
    );
  });

  test("refuses an empty --omp-binary, and --omp beside --omp-binary, running nothing", () => {
    const empty = install(["--omp-binary", ""]);
    expect(empty.code).toBe(2);
    expect(empty.stderr).toContain("--omp-binary needs a value");
    expect(empty.calls).toEqual([]);

    const f = fixture();
    const both = install(["--omp", repoPin, "--omp-binary", f.binary], f);
    expect(both.code).toBe(2);
    expect(both.stderr).toContain(
      "install-plugin-profile: --omp and --omp-binary each name the Oh My Pi to run; give one of them\n"
    );
    expect([both.calls, both.binaryCalls]).toEqual([[], []]);
  });
});
