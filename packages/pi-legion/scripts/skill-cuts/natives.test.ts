import { expect, test } from "bun:test";
import { createHash } from "node:crypto";
import { mkdirSync, writeFileSync } from "node:fs";
import path from "node:path";
import { restoreEnvironment } from "@legion/test-fixtures/environment";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { pinnedAddon } from "./natives";

// The runner's pinned Oh My Pi native addon (natives.ts), on made-up files: none of these rows
// needs the real addon, and none opens one.

const scratch = temporaryDirectory("skill-cuts-natives-loader");
restoreEnvironment(["PI_NATIVES_DIR", "PI_NATIVE_VARIANT", "SKILL_CUTS_NATIVES_DIR"]);

/** The steps every refusal names. */
const STEPS =
  "to get the pin's addon, run gh release download v18.18.6-sami.20261119-162325 --repo cybermonklabs/oh-my-pi --pattern omp-v18.18.6-sami.20261119-162325-linux-x64.tar.gz, then tar -xzf omp-v18.18.6-sami.20261119-162325-linux-x64.tar.gz, then PI_NATIVES_DIR=<dir> omp-v18.18.6-sami.20261119-162325-linux-x64/bin/omp --help (which extracts it into <dir>/18.18.6), and run the runner with PI_NATIVES_DIR=<dir>";
/** The pin's two digests, as a refusal names them. */
const PINNED =
  "d47a5f57fae382072d43e4529bc8cd1a6eb7fea9765d903bf54b6b6d81950419 (modern) or 2a221317126cb5563837d3673f492fc67bca862d617269aa7de4e5d455fc1189 (baseline)";

/** `read`'s result in a process that reports the host `platform`-`arch`, whatever this one is;
 *  the process's own are put back before it returns. */
function onHost<T>(platform: string, arch: string, read: () => T): T {
  const own = {
    platform: Object.getOwnPropertyDescriptor(process, "platform"),
    arch: Object.getOwnPropertyDescriptor(process, "arch"),
  };
  Object.defineProperty(process, "platform", { value: platform, configurable: true });
  Object.defineProperty(process, "arch", { value: arch, configurable: true });
  try {
    return read();
  } finally {
    if (own.platform !== undefined) Object.defineProperty(process, "platform", own.platform);
    if (own.arch !== undefined) Object.defineProperty(process, "arch", own.arch);
  }
}

/** A natives directory `name` of the scratch directory, named by PI_NATIVES_DIR (with
 *  SKILL_CUTS_NATIVES_DIR unset), and the file the loader tries first there with
 *  PI_NATIVE_VARIANT=baseline. */
function nativesDir(name: string): { dir: string; baseline: string } {
  const dir = path.join(scratch, name);
  mkdirSync(path.join(dir, "18.18.6"), { recursive: true });
  process.env.PI_NATIVES_DIR = dir;
  process.env.PI_NATIVE_VARIANT = "baseline";
  delete process.env.SKILL_CUTS_NATIVES_DIR;
  return { dir, baseline: path.join(dir, "18.18.6", "pi_natives.linux-x64-baseline.node") };
}

// Catches: the runner opening a later build's addon of the same release (the pod's own Oh My Pi
// reports 18.18.6 too, and its addon's digest is another), or hashing a 123 MiB addon at each
// call.
test("a file of another digest where the pin's addon would be is refused, naming the file, its digest, the pin's and the steps; a process hashes it once", () => {
  const { baseline } = nativesDir("other-build");
  writeFileSync(baseline, "a made-up addon\n");
  const digest = createHash("sha256").update("a made-up addon\n").digest("hex");
  const why = `${baseline} is another build's: its SHA-256 is ${digest}, and the pin's is ${PINNED}; ${STEPS}`;
  expect(onHost("linux", "x64", pinnedAddon)).toEqual({ why });
  // Other bytes in the same file: the process keeps the digest it took.
  writeFileSync(baseline, "other made-up bytes\n");
  expect(onHost("linux", "x64", pinnedAddon)).toEqual({ why });
});

// Catches: a read whose answer needs the addon answered another way where the runner has none, or
// a refusal that does not say how to get the pin's.
test("no file where the pin's loader looks is refused, naming each path it tried and the steps", () => {
  const { dir } = nativesDir("none");
  expect(onHost("linux", "x64", pinnedAddon)).toEqual({
    why: `none is at ${dir}/18.18.6/pi_natives.linux-x64-baseline.node, ${dir}/18.18.6/pi_natives.linux-x64.node; ${STEPS}`,
  });
});

// Catches: an addon opened on a platform whose pinned addon's digest the runner does not know.
test("another platform is refused, naming the file the pin's loader would open there and the steps", () => {
  const { dir } = nativesDir("arm");
  writeFileSync(path.join(dir, "18.18.6", "pi_natives.linux-arm64.node"), "a made-up addon\n");
  expect(onHost("linux", "arm64", pinnedAddon)).toEqual({
    why: `it is known by its SHA-256 on linux-x64 alone, and this host is linux-arm64, so the runner opens none of ${dir}/18.18.6/pi_natives.linux-arm64.node; on a linux-x64 host, ${STEPS}`,
  });
});

// Catches: CI's job naming the pin's addon only in PI_NATIVES_DIR, which the later Oh My Pi its
// other tests run reads too, or the runner's own directory read after the loader's, so a file
// there is never the one opened.
test("the runner's own natives directory, SKILL_CUTS_NATIVES_DIR, is tried before the pin loader's", () => {
  const { dir } = nativesDir("loader");
  const own = path.join(scratch, "runner-own");
  mkdirSync(path.join(own, "18.18.6"), { recursive: true });
  process.env.SKILL_CUTS_NATIVES_DIR = own;
  expect(onHost("linux", "x64", pinnedAddon)).toEqual({
    why: `none is at ${own}/18.18.6/pi_natives.linux-x64-baseline.node, ${own}/18.18.6/pi_natives.linux-x64.node, ${dir}/18.18.6/pi_natives.linux-x64-baseline.node, ${dir}/18.18.6/pi_natives.linux-x64.node; ${STEPS}`,
  });
  const file = path.join(own, "18.18.6", "pi_natives.linux-x64-baseline.node");
  writeFileSync(file, "a runner's made-up addon\n");
  writeFileSync(path.join(dir, "18.18.6", "pi_natives.linux-x64-baseline.node"), "another\n");
  const digest = createHash("sha256").update("a runner's made-up addon\n").digest("hex");
  expect(onHost("linux", "x64", pinnedAddon)).toEqual({
    why: `${file} is another build's: its SHA-256 is ${digest}, and the pin's is ${PINNED}; ${STEPS}`,
  });
});

// Catches: the runner's open of the addon skipping the check (a refusal the loader states and
// the open does not keep).
test("the runner's open of the addon refuses with the same reason, before it opens anything", async () => {
  const { baseline } = nativesDir("open");
  writeFileSync(baseline, "a made-up addon\n");
  const digest = createHash("sha256").update("a made-up addon\n").digest("hex");
  // A process of its own, which has opened no addon yet; it reports linux-x64 whatever its host.
  const child = Bun.spawn(
    [
      process.execPath,
      "-e",
      [
        `import { natives } from ${JSON.stringify(path.join(import.meta.dir, "natives.ts"))};`,
        'Object.defineProperty(process, "platform", { value: "linux", configurable: true });',
        'Object.defineProperty(process, "arch", { value: "x64", configurable: true });',
        'try { natives(); console.log("opened"); } catch (error) { console.log(error.message); }',
      ].join("\n"),
    ],
    { env: { ...process.env }, stdin: "ignore", stdout: "pipe", stderr: "pipe" }
  );
  const [stdout, stderr, code] = await Promise.all([
    new Response(child.stdout).text(),
    new Response(child.stderr).text(),
    child.exited,
  ]);
  expect({ code, stderr, stdout }).toEqual({
    code: 0,
    stderr: "",
    stdout: `a read's answer needs the pinned Oh My Pi's native addon: ${baseline} is another build's: its SHA-256 is ${digest}, and the pin's is ${PINNED}; ${STEPS}\n`,
  });
}, 30_000);
