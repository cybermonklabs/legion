/**
 * The pinned Oh My Pi's own native addon, which the runner calls where a read's answer is the
 * addon's: the blocks of a read's block context (block-context.ts) and the structural summary of a
 * whole read of a code file (arms.ts's `summaryAnswer`). The pin is the tag
 * v18.18.6-sami.20261119-162325 of cybermonklabs/oh-my-pi, whose release extracts the addon from
 * its compiled binary. A later build of 18.18.6 reports the same release, and its addon is another
 * file, so the runner opens only the pin's: a file whose SHA-256 is one of PINNED_ADDON_DIGESTS,
 * on linux-x64, the one platform whose digests are known. It hashes the file once in a process.
 *
 * The file is the one the pin's loader opens (packages/natives/native/loader-state.js at the tag,
 * `getNativesDir` and `getAddonFilenames`): `pi_natives.<platform>-<arch>.node`, on x64 `-modern`
 * (a CPU with AVX2) or `-baseline` before `.node`, as PI_NATIVE_VARIANT says where it names one,
 * in `<natives dir>/18.18.6`, the natives directory being PI_NATIVES_DIR, else
 * `$OMP_DOWNLOAD_ROOT/natives`, else `$XDG_DATA_HOME/omp/natives` where `$XDG_DATA_HOME/omp`
 * exists, else `natives` under OMP_CONFIG_ROOT or `~/.omp`. Before those the runner tries the same
 * files in SKILL_CUTS_NATIVES_DIR (RUNNER_NATIVES_DIR), a natives directory of the runner's alone:
 * CI's pi-legion job names the pin's addon there (.github/actions/install-skill-cuts-omp), and not
 * in PI_NATIVES_DIR, which the later Oh My Pi the job's other tests run reads too. The runner takes
 * the first of those paths that is there, points the addon at `<natives dir>/grammars`, where Oh My
 * Pi installs the WebAssembly grammars it downloads, as the loader does, and throws where it has no
 * pinned addon: no such file, a file of another digest, or another platform, each reason naming the
 * file and the steps that give the pin's addon (PINNED_ADDON_STEPS). It never answers such a read
 * another way for want of the addon.
 *
 * The pin's own binary (`pinnedOmp`) is the file SKILL_CUTS_OMP names, opened only where its
 * SHA-256 is PINNED_OMP_DIGEST (the digest of the release archive's `bin/omp`), on linux-x64: the
 * runner runs it headless in its sandbox to answer a bash call whose output the pin's minimizer may
 * rewrite and a read of one of the pin's internal URLs (checkout.ts's `pinAnswer`). CI's install
 * action exports it (.github/actions/install-skill-cuts-omp). Where it is absent or another build's
 * the call is not answered, the reason naming the file and PINNED_OMP_STEPS.
 *
 * Some languages' grammars are WebAssembly modules Oh My Pi downloads the first time it needs one
 * (packages/coding-agent/src/utils/grammars.ts, `ensureGrammar`), before it looks for a window's
 * blocks and before it summarizes a file: with the grammar the answer is tree-sitter's, and where
 * the download failed the addon knows no tree. The runner cannot tell which the session had, so a
 * caller asks `missingGrammar` first and leaves such a read unanswered.
 */
import { createHash } from "node:crypto";
import { existsSync, readFileSync, realpathSync } from "node:fs";
import os from "node:os";
import path from "node:path";

/** The release of the pinned Oh My Pi's native addon (packages/natives/package.json). */
export const NATIVES_VERSION = "18.18.6";

/** The pin's tag and its linux-x64 release asset, from which the addon is extracted. The
 *  `skill-cuts-omp` row of .github/ci-tools.yaml, which CI installs the addon from, carries this
 *  tag without its `v` as its version (.github/scripts/check-ci-tools.sh holds the two equal). */
const PIN_TAG = "v18.18.6-sami.20261119-162325";
const PIN_ASSET = `omp-${PIN_TAG}-linux-x64.tar.gz`;

/** The variable naming the runner's own natives directory, tried before the loader's. */
const RUNNER_NATIVES_DIR = "SKILL_CUTS_NATIVES_DIR";

/** The SHA-256 of each linux-x64 addon the pin's binary extracts (its `bin/omp --help` with
 *  PI_NATIVES_DIR set): the `-modern` one, and the `-baseline` one it extracts with
 *  PI_NATIVE_VARIANT=baseline or on a CPU without AVX2. */
const PINNED_ADDON_DIGESTS: Readonly<Record<string, string>> = {
  d47a5f57fae382072d43e4529bc8cd1a6eb7fea9765d903bf54b6b6d81950419: "modern",
  "2a221317126cb5563837d3673f492fc67bca862d617269aa7de4e5d455fc1189": "baseline",
};

/** How to get the pin's addon, which every refusal names. */
const PINNED_ADDON_STEPS = `to get the pin's addon, run gh release download ${PIN_TAG} --repo cybermonklabs/oh-my-pi --pattern ${PIN_ASSET}, then tar -xzf ${PIN_ASSET}, then PI_NATIVES_DIR=<dir> omp-${PIN_TAG}-linux-x64/bin/omp --help (which extracts it into <dir>/${NATIVES_VERSION}), and run the runner with PI_NATIVES_DIR=<dir>`;

/** The variable naming the pin's own `omp` binary (`pinnedOmp`): CI's pi-legion job names the one
 *  it unpacks there (.github/actions/install-skill-cuts-omp). */
const RUNNER_OMP = "SKILL_CUTS_OMP";

/** The SHA-256 of the pin's linux-x64 binary, `bin/omp` of PIN_ASSET (measured with sha256sum on
 *  the unpacked release asset, 2026-11-25). */
const PINNED_OMP_DIGEST = "8cbeb1d5852f93e215f76a0907426b70fd25c9565baf8d021d2d6eefcfbe8e9b";

/** How to get the pin's binary, which every refusal of it names. */
const PINNED_OMP_STEPS = `to get the pin's binary, run gh release download ${PIN_TAG} --repo cybermonklabs/oh-my-pi --pattern ${PIN_ASSET}, then tar -xzf ${PIN_ASSET}, and run the runner with ${RUNNER_OMP}=<dir>/omp-${PIN_TAG}-linux-x64/bin/omp`;

/** One span of the addon's structural summary of a text (crates/pi-natives' `SummarySegment`):
 *  `kept` (its lines' text) or `elided`, its lines 1-based and inclusive. */
export interface SummarySegment {
  readonly kind: string;
  readonly startLine: number;
  readonly endLine: number;
  readonly text?: string;
}

/** The addon's structural summary of a text (`SummaryResult`): whether tree-sitter parsed it
 *  with no syntax error, whether a span was elided, and its spans in source order. */
export interface CodeSummary {
  readonly parsed: boolean;
  readonly elided: boolean;
  readonly segments: readonly SummarySegment[];
}

/** The exports of the addon the runner calls (crates/pi-natives/src/block.rs, summary.rs and
 *  grammars). */
interface Natives {
  readonly __ompSetGrammarDir: (dir: string) => void;
  readonly enclosingBlockBoundaries: (options: {
    code: string;
    path?: string;
    ranges: { startLine: number; endLine: number }[];
  }) => number[] | null;
  readonly wasmGrammarFor: (query: {
    path?: string;
  }) => { language: string; file: string; installed: boolean } | null;
  readonly summarizeCode: (options: {
    code: string;
    path?: string;
    minBodyLines?: number;
    minCommentLines?: number;
    unfoldUntilLines?: number;
    unfoldLimitLines?: number;
  }) => CodeSummary;
}

let loaded: Natives | undefined;

/** The SHA-256 of each file this process hashed, by its real path. */
const digests = new Map<string, string>();

/** An absolute directory from an override variable (`~` expanded), or undefined for a relative
 *  or empty value (loader-state.js's `resolveAbsoluteDir`). */
function absoluteDir(value: string | undefined): string | undefined {
  let dir = value?.trim();
  if (!dir) return undefined;
  if (dir === "~") dir = os.homedir();
  else if (dir.startsWith("~/")) dir = os.homedir() + dir.slice(1);
  return path.isAbsolute(dir) ? path.normalize(dir) : undefined;
}

/** The directory Oh My Pi keeps its natives in (loader-state.js's `getNativesDir`). */
function nativesDir(): string {
  const override = absoluteDir(process.env.PI_NATIVES_DIR);
  if (override !== undefined) return override;
  const downloadRoot = absoluteDir(process.env.OMP_DOWNLOAD_ROOT);
  if (downloadRoot !== undefined) return path.join(downloadRoot, "natives");
  const xdgDataHome = process.env.XDG_DATA_HOME;
  if (xdgDataHome && existsSync(path.join(xdgDataHome, "omp"))) {
    return path.join(xdgDataHome, "omp", "natives");
  }
  const root = absoluteDir(process.env.OMP_CONFIG_ROOT) ?? path.join(os.homedir(), ".omp");
  return path.join(root, "natives");
}

/** Whether this x64 CPU runs the `modern` addon (loader-state.js's `detectAvx2Support`). */
function hasAvx2(): boolean {
  if (process.platform === "linux") {
    try {
      return /\bavx2\b/i.test(readFileSync("/proc/cpuinfo", "utf8"));
    } catch {
      return false;
    }
  }
  if (process.platform === "darwin") {
    for (const sysctl of ["/usr/sbin/sysctl", "sysctl"]) {
      for (const key of ["machdep.cpu.leaf7_features", "machdep.cpu.features"]) {
        try {
          const run = Bun.spawnSync([sysctl, "-n", key], { stdout: "pipe", stderr: "pipe" });
          if (run.exitCode === 0 && /\bAVX2\b/i.test(run.stdout.toString())) return true;
        } catch {
          // The next probe answers.
        }
      }
    }
  }
  return false;
}

/** The addon's file names, in the order the loader tries them (`getAddonFilenames`). */
function addonFilenames(): string[] {
  const tag = `${process.platform}-${process.arch}`;
  const plain = `pi_natives.${tag}.node`;
  if (process.arch !== "x64") return [plain];
  const asked = process.env.PI_NATIVE_VARIANT;
  const modern = asked === "modern" || asked === "baseline" ? asked === "modern" : hasAvx2();
  const baseline = `pi_natives.${tag}-baseline.node`;
  return modern ? [`pi_natives.${tag}-modern.node`, baseline, plain] : [baseline, plain];
}

/** The paths the runner tries for the pin's native addon, in its order (see the file's header):
 *  each of the loader's file names in RUNNER_NATIVES_DIR, then in the loader's natives directory. */
function nativeAddonPaths(): string[] {
  const names = addonFilenames();
  const dirs = [absoluteDir(process.env[RUNNER_NATIVES_DIR]), nativesDir()];
  const unique = [...new Set(dirs.filter((dir) => dir !== undefined))];
  return unique.flatMap((dir) => names.map((name) => path.join(dir, NATIVES_VERSION, name)));
}

/** The SHA-256 of `file`, hashed once in this process however many paths lead to it. */
function digestOf(file: string): string {
  const real = realpathSync(file);
  let digest = digests.get(real);
  if (digest === undefined) {
    digest = createHash("sha256").update(readFileSync(real)).digest("hex");
    digests.set(real, digest);
  }
  return digest;
}

/**
 * The pinned Oh My Pi's native addon that the runner would open (see the file's header): the first
 * path the pin's loader tries that holds a file, where that file's SHA-256 is one of
 * PINNED_ADDON_DIGESTS; else why there is none, naming the file or the paths tried, and
 * PINNED_ADDON_STEPS.
 */
export function pinnedAddon(): { readonly file: string } | { readonly why: string } {
  const tried = nativeAddonPaths();
  const platform = `${process.platform}-${process.arch}`;
  if (platform !== "linux-x64") {
    return {
      why: `it is known by its SHA-256 on linux-x64 alone, and this host is ${platform}, so the runner opens none of ${tried.join(", ")}; on a linux-x64 host, ${PINNED_ADDON_STEPS}`,
    };
  }
  const file = tried.find((each) => existsSync(each));
  if (file === undefined) {
    return { why: `none is at ${tried.join(", ")}; ${PINNED_ADDON_STEPS}` };
  }
  const digest = digestOf(file);
  if (PINNED_ADDON_DIGESTS[digest] === undefined) {
    const pinned = Object.entries(PINNED_ADDON_DIGESTS).map(
      ([each, variant]) => `${each} (${variant})`
    );
    return {
      why: `${file} is another build's: its SHA-256 is ${digest}, and the pin's is ${pinned.join(" or ")}; ${PINNED_ADDON_STEPS}`,
    };
  }
  return { file };
}

/**
 * The pinned Oh My Pi's own binary, which checkout.ts runs headless to answer a call the pin's
 * own tools shape (its shell minimizer's rewrite, a read of one of its internal URLs): the file
 * RUNNER_OMP names, its real path, where its SHA-256 is PINNED_OMP_DIGEST, on linux-x64, the one
 * platform whose digest is known; else why there is none, naming the file and PINNED_OMP_STEPS.
 * It hashes a file once in a process, as `pinnedAddon` does.
 */
export function pinnedOmp(): { readonly file: string } | { readonly why: string } {
  const platform = `${process.platform}-${process.arch}`;
  if (platform !== "linux-x64") {
    return {
      why: `it is known by its SHA-256 on linux-x64 alone, and this host is ${platform}; on a linux-x64 host, ${PINNED_OMP_STEPS}`,
    };
  }
  const given = process.env[RUNNER_OMP];
  if (given === undefined || given === "") {
    return { why: `${RUNNER_OMP} names no file; ${PINNED_OMP_STEPS}` };
  }
  if (!existsSync(given)) {
    return { why: `${given}, which ${RUNNER_OMP} names, is not there; ${PINNED_OMP_STEPS}` };
  }
  const digest = digestOf(given);
  if (digest !== PINNED_OMP_DIGEST) {
    return {
      why: `${given} is another build's: its SHA-256 is ${digest}, and the pin's is ${PINNED_OMP_DIGEST}; ${PINNED_OMP_STEPS}`,
    };
  }
  return { file: realpathSync(given) };
}

/** The pinned Oh My Pi's native addon, opened once (see the file's header). Throws, with
 *  `pinnedAddon`'s reason, where there is none. */
export function natives(): Natives {
  if (loaded !== undefined) return loaded;
  const found = pinnedAddon();
  if ("why" in found) {
    throw new Error(`a read's answer needs the pinned Oh My Pi's native addon: ${found.why}`);
  }
  const addon = { exports: {} as Natives };
  process.dlopen(addon, found.file);
  // The natives directory, which holds the release's directory and the grammars beside it.
  addon.exports.__ompSetGrammarDir(path.join(path.dirname(path.dirname(found.file)), "grammars"));
  loaded = addon.exports;
  return loaded;
}

/**
 * The WebAssembly grammar the pin would download before it looks for the blocks of a window of the
 * file `file`, or before it summarizes it (its name picks the language), when it is not installed:
 * its language and file name; undefined when the file's language is linked into the addon, is
 * none the addon knows, or has its grammar installed (block-context.ts's `warmBlockContext` and
 * read-summary.ts's `trySummarize`, grammars.ts's `ensureGrammar` at the pin). Ask it only where
 * the pin would download: of a window that does not show every line (block-context.ts's
 * `spansCoverEveryLine`), or of a file the pin would summarize (arms.ts's `summaryAnswer`).
 */
export function missingGrammar(file: string): { language: string; file: string } | undefined {
  const info = natives().wasmGrammarFor({ path: file });
  return info === null || info.installed ? undefined : { language: info.language, file: info.file };
}
