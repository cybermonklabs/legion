/**
 * bun-cache-census.ts runs a command under strace and fails when any process it starts writes
 * Bun's runtime transpiler cache outside the cache of the user who runs it, or starts Bun under a
 * HOME or an XDG_CACHE_HOME of its own without BUN_RUNTIME_TRANSPILER_CACHE_PATH=0.
 *
 * Bun keeps that cache under `$XDG_CACHE_HOME/bun/@t@`, else `$HOME/.bun/install/cache/@t@`. A
 * test that gives a child a HOME of the test's own makes that cache empty, so the child writes
 * each module larger than 4 KB it transpiles into the test's directory, which the test then
 * removes: work no row checks, and stalling it on purpose reproduces a row's time-out. The census
 * sees every process the command starts, by system call, whatever API or helper started it, so it
 * names a test that a search of the source would miss.
 *
 * Two readings of one trace (`strace -ff -y -v`, execve and renameat only):
 *
 * - each `renameat` whose directory ends in `@t@`: Bun's commit of one cache file. One outside the
 *   runner's own caches (`$HOME/.bun/install/cache/@t@`, `$XDG_CACHE_HOME/bun/@t@`, and the path
 *   `BUN_RUNTIME_TRANSPILER_CACHE_PATH` names) is a violation, whatever program wrote it.
 * - each `execve` of a program named `bun`, with its environment: a HOME or XDG_CACHE_HOME other
 *   than the runner's without BUN_RUNTIME_TRANSPILER_CACHE_PATH=0 is a violation, even when that
 *   run wrote nothing (it transpiled only small modules this time), and a cache the child names
 *   with a path is one too. A compiled Bun program (omp) is seen by the first reading only.
 *
 * A trace that holds no execve at all is no clean result: the census could not see the command,
 * and says so with exit 2.
 *
 * Run: bun scripts/bun-cache-census.ts -- <command> [args...]
 * Exit: the command's own status when it failed; else 1 for a violation (each one printed), 0 when
 * clean, and 2 when strace is missing or its trace saw nothing.
 */
import { mkdtempSync, readdirSync, readFileSync, rmSync } from "node:fs";
import { homedir, tmpdir } from "node:os";
import { basename, join } from "node:path";

interface Finding {
  readonly pid: string;
  readonly what: string;
}

const CACHE_DIR = /^renameat\(\d+<([^>]*\/@t@)>/;
const EXECVE = /^execve\("([^"]*)"/;

function variable(call: string, name: string): string | undefined {
  return new RegExp(`"${name}=([^"]*)"`).exec(call)?.[1];
}

/** The runner's own cache directories, which a commit may land in. */
function ownCaches(env: NodeJS.ProcessEnv): string[] {
  const home = env.HOME ?? homedir();
  const caches = [join(home, ".bun", "install", "cache", "@t@")];
  if (env.XDG_CACHE_HOME) caches.push(join(env.XDG_CACHE_HOME, "bun", "@t@"));
  const named = env.BUN_RUNTIME_TRANSPILER_CACHE_PATH;
  if (named && named !== "0") caches.push(named);
  return caches;
}

/** A path with the run's TMPDIR shortened, so a finding names the test's directory prefix. */
function shown(path: string, tmp: string): string {
  return path.startsWith(`${tmp}/`) ? `$TMPDIR${path.slice(tmp.length)}` : path;
}

/** Each complete call of one per-process trace file, a call strace split in two joined again. */
function calls(text: string): string[] {
  const out: string[] = [];
  let pending: string | undefined;
  for (const line of text.split("\n")) {
    if (line.endsWith("<unfinished ...>")) {
      pending = line.slice(0, -"<unfinished ...>".length);
      continue;
    }
    const resumed = /^<\.\.\. \w+ resumed>(.*)$/.exec(line);
    if (resumed) {
      if (pending !== undefined) out.push(pending + resumed[1]);
      pending = undefined;
      continue;
    }
    if (line !== "") out.push(line);
  }
  return out;
}

export function readTrace(
  directory: string,
  env: NodeJS.ProcessEnv
): { execs: number; bunExecs: number; commits: number; findings: Finding[] } {
  const tmp = env.TMPDIR ?? tmpdir();
  const caches = ownCaches(env);
  const runnerHome = env.HOME ?? homedir();
  const runnerXdg = env.XDG_CACHE_HOME;
  const findings: Finding[] = [];
  const seen = new Set<string>();
  let execs = 0;
  let bunExecs = 0;
  let commits = 0;
  for (const name of readdirSync(directory)) {
    const pid = name.slice(name.lastIndexOf(".") + 1);
    for (const call of calls(readFileSync(join(directory, name), "utf-8"))) {
      if (!/ = 0$/.test(call)) continue;
      const exec = EXECVE.exec(call);
      if (exec) {
        execs++;
        if (basename(exec[1] ?? "") !== "bun") continue;
        bunExecs++;
        const home = variable(call, "HOME");
        const xdg = variable(call, "XDG_CACHE_HOME");
        const off = variable(call, "BUN_RUNTIME_TRANSPILER_CACHE_PATH");
        // A HOME or XDG_CACHE_HOME the child lacks is no move: Bun then uses the runner's own.
        const movedXdg = xdg !== undefined && xdg !== runnerXdg;
        const movedHome = home !== undefined && home !== runnerHome;
        if ((movedXdg || movedHome) && off !== "0") {
          const where = movedXdg
            ? `XDG_CACHE_HOME=${shown(xdg ?? "", tmp)}`
            : `HOME=${shown(home ?? "", tmp)}`;
          const what = `a Bun process started under ${where} without BUN_RUNTIME_TRANSPILER_CACHE_PATH=0`;
          if (!seen.has(what)) findings.push({ pid, what });
          seen.add(what);
        }
        continue;
      }
      const commit = CACHE_DIR.exec(call);
      if (commit) {
        commits++;
        const dir = commit[1] ?? "";
        if (caches.some((own) => dir === own || dir.startsWith(`${own}/`))) continue;
        const what = `a transpiler cache file was written in ${shown(dir, tmp)}`;
        if (!seen.has(what)) findings.push({ pid, what });
        seen.add(what);
      }
    }
  }
  return { execs, bunExecs, commits, findings };
}

if (import.meta.main) {
  // Bun removed the `--` after the script from Bun.argv when measured; one is dropped if present.
  const given = Bun.argv.slice(2);
  const command = given[0] === "--" ? given.slice(1) : given;
  if (command.length === 0) {
    console.error("usage: bun scripts/bun-cache-census.ts -- <command> [args...]");
    process.exit(2);
  }
  if (Bun.which("strace") === null) {
    console.error("bun-cache-census: strace is not on PATH, so nothing was traced");
    process.exit(2);
  }
  const directory = mkdtempSync(join(tmpdir(), "bun-cache-census-"));
  let status: number;
  try {
    const traced = Bun.spawnSync(
      [
        "strace",
        "-f",
        "-ff",
        "-qq",
        "--seccomp-bpf",
        "-y",
        "-v",
        "-s",
        "512",
        "-e",
        "trace=execve,renameat",
        "-o",
        join(directory, "trace"),
        ...command,
      ],
      { stdin: "inherit", stdout: "inherit", stderr: "inherit" }
    );
    const { execs, bunExecs, commits, findings } = readTrace(directory, process.env);
    console.error(
      `bun-cache-census: ${execs} programs started, ${bunExecs} of them Bun, ${commits} cache files written, ${findings.length} violations`
    );
    for (const { pid, what } of findings)
      console.error(`bun-cache-census: process ${pid}: ${what}`);
    if (execs === 0) {
      console.error(
        "bun-cache-census: the trace holds no started program, so the census saw nothing"
      );
      status = 2;
    } else if (traced.exitCode !== 0) status = traced.exitCode ?? 1;
    else status = findings.length > 0 ? 1 : 0;
  } finally {
    rmSync(directory, { recursive: true, force: true });
  }
  process.exit(status);
}
