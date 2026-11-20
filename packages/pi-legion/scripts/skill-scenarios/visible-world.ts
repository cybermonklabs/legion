#!/usr/bin/env bun
// visible-world.ts: every place in one run's world where its agent could read the name of its
// scenario, its case or its label, the word `ablate`, another run's id, or the path of a label's
// checkout, of the map or of any other rig work directory's tree. A run measures what its label
// removes only when its agent cannot learn which label it runs under, nor which case it is in.
//
//   bun visible-world.ts <run dir> [--live]
//
// <run dir> is a run's directory, <work>/runs/<id> (rig.sh's layout). The names come from the map,
// <work>/maps: each link <work>/maps/runs/<scenario>[-<case>]-<label>-<n> points at a run's
// directory, and each link <work>/maps/profiles/<label> at a label's profile.
//
// What it reads, the world the run's agent sees:
//   env        the run's pane.env, the agent's whole environment (launch runs it under `env -i`)
//   command    launch.sh and system-args, the agent's command line
//   tree       the path and the bytes of every file under the run's directory (its cwd, HOME,
//              TMPDIR, stand-ins and fixtures), and the target of every symbolic link there
//   transcript the files under <run>/sessions: what the agent read and wrote
//   plugins    the path and the bytes of every file under each place a link of the run points at
//              outside it (the label's packed plugins, the tools' installs, the native module
//              cache), read where the host holds what the agent's view shows there; a place the
//              run's view file holds nothing at is not read
//   git        the commits, their messages and the refs of each git repository under the run
//   view       every path name in the agent's whole view but under /proc, /sys and /dev: a walk
//              (find) inside bwrap with the view file the agent ran under (<work>/views/<id>), or
//              with --live a walk of the running agent's own root (/proc/<pid>/root). Each name is
//              matched against the terms, and against a map, run or profile tree of a rig work
//              directory (<w>/maps/runs, <w>/runs/<16 hex digits>, <w>/profiles/<16 hex digits>)
//              that is not this run's own. A fixed-string filter (grep) of the terms' words passes
//              the names on, so a walk of hundreds of millions of names keeps find's pace. A hit
//              names the shortest path that holds its match: a tree that matches is one hit.
// and with --live, while the run's agent runs:
//   process    the environment and command line of each process that carries the run's
//              SKILL_SCENARIO_RUN
//   visible    the command line of each process the agent's own /proc shows, the names under
//              <work> as the agent's mount namespace shows them (/proc/<pid>/root), whether its
//              /tmp is its run's own tmp (the machine's is every run's), the names in its /var/tmp
//              and /dev/shm, and whether each label's checkout is empty in its view
//   dispatch   the run's Dispatch database (pg_dump of the database its dispatch.json names), the
//              store its `dispatch` commands read
//
// A hit is a channel, a place, the term's kind (`own` for the run's own scenario, case or label),
// the term and the text around it. A label or case that is a plain word (`head`, `facts`) is
// searched only in the shapes a rig name gives it (`profiles/head`, `skills-head`, `-head-3`), so
// prose that says "head" is no hit. Exit 0: no hit. Exit 1: hits. Exit 2: a usage error. Exit 3:
// no hit, and a channel could not see what it should have (no file under the run, no agent
// process with --live, a view walk that did not finish, an agent root or a database it could not
// read): it says UNKNOWN rather than clean.
import { execFileSync } from "node:child_process";
import type { Dirent } from "node:fs";
import {
  existsSync,
  mkdtempSync,
  readdirSync,
  readFileSync,
  readlinkSync,
  realpathSync,
  rmSync,
  statSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { CASED_RUNS } from "./score";

interface Term {
  readonly kind: string;
  readonly own: boolean;
  readonly pattern: RegExp;
  readonly text: string;
  /** Fixed strings, lower case, one of which each match holds: the view walk's prefilter. */
  readonly literals: readonly string[];
}
interface Hit {
  readonly channel: string;
  readonly where: string;
  readonly term: Term;
  readonly context: string;
}
/** One link of <work>/maps/runs, read into its fields. */
interface MappedRun {
  readonly name: string;
  readonly target: string;
  readonly scenario: string;
  readonly case: string;
  readonly label: string;
}
interface RunMap {
  readonly own: MappedRun | undefined;
  readonly runs: MappedRun[];
  readonly labels: string[];
  readonly checkouts: { label: string; checkout: string }[];
  readonly maps: string;
}

const MAX_FILE = 64 * 1024 * 1024;
const escaped = (text: string) => text.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
/** A name that holds no hyphen, digit or capital is a plain word, searched only in a rig name's shapes. */
const plainWord = (name: string) => /^[a-z]+$/.test(name);

/** The run's own fields and every name the map holds, from the links under <work>/maps. */
function mapOf(work: string, runDir: string): RunMap {
  const runsLinks = path.join(work, "maps", "runs");
  const profileLinks = path.join(work, "maps", "profiles");
  const labels = existsSync(profileLinks) ? readdirSync(profileLinks) : [];
  const runReal = realpathSync(runDir);
  const runs = (existsSync(runsLinks) ? readdirSync(runsLinks) : []).map((name) => {
    const link = path.join(runsLinks, name);
    let target = "";
    try {
      target = realpathSync(link);
    } catch {
      // A link whose run was removed points at nothing.
    }
    // The case of a run of each scenario the scorer reads a case of, by the same patterns.
    const cased = Object.values(CASED_RUNS)
      .map((pattern) => pattern.exec(name))
      .find((hit) => hit !== null);
    const label =
      labels
        .filter((one) => new RegExp(`-${escaped(one)}-\\d+$`).test(name))
        .sort((a, b) => b.length - a.length)[0] ?? "";
    const prefix = label === "" ? name : name.slice(0, name.lastIndexOf(`-${label}-`));
    const runCase = cased?.[1] ?? "";
    const scenario = runCase === "" ? prefix : prefix.slice(0, -(runCase.length + 1));
    return { name, target, scenario, case: runCase, label };
  });
  const own = runs.find((run) => run.target === runReal);
  const checkouts = labels.flatMap((label) => {
    const file = path.join(profileLinks, label, "checkout");
    return existsSync(file) ? [{ label, checkout: readFileSync(file, "utf8").trim() }] : [];
  });
  return { own, runs, labels, checkouts, maps: path.join(work, "maps") };
}

/** `word` between two separators, `-` or `_`, as fixed strings: the literals of `[-_]word[-_]`. */
const separated = (word: string) =>
  ["-", "_"].flatMap((before) => ["-", "_"].map((after) => `${before}${word}${after}`));

function termsOf(map: RunMap): Term[] {
  const own = map.own;
  const terms: Term[] = [];
  const add = (
    kind: string,
    isOwn: boolean,
    text: string,
    pattern: RegExp,
    literals: readonly string[] = [text]
  ) => {
    if (!terms.some((term) => term.pattern.source === pattern.source))
      terms.push({
        kind,
        own: isOwn,
        text,
        pattern,
        literals: literals.map((literal) => literal.toLowerCase()),
      });
  };
  for (const scenario of new Set(map.runs.map((run) => run.scenario))) {
    if (scenario === "") continue;
    const isOwn = scenario === own?.scenario;
    if (plainWord(scenario)) {
      add("scenario", isOwn, `runs/${scenario}-`, new RegExp(`runs/${escaped(scenario)}-`, "g"));
      add("scenario", isOwn, `run_${scenario}_`, new RegExp(`run_${escaped(scenario)}_`, "g"));
    } else add("scenario", isOwn, scenario, new RegExp(escaped(scenario), "g"));
  }
  for (const runCase of new Set(map.runs.map((run) => run.case))) {
    if (runCase === "") continue;
    const isOwn = runCase === own?.case;
    if (plainWord(runCase))
      add(
        "case",
        isOwn,
        `-${runCase}-`,
        new RegExp(`[-_]${escaped(runCase)}[-_]`, "g"),
        separated(runCase)
      );
    else add("case", isOwn, runCase, new RegExp(escaped(runCase), "g"));
  }
  for (const label of map.labels) {
    const isOwn = label === own?.label;
    const squashed = label.replace(/-/g, "");
    add("label", isOwn, `profiles/${label}`, new RegExp(`profiles/${escaped(label)}\\b`, "g"));
    add("label", isOwn, `skills-${label}`, new RegExp(`skills-${escaped(label)}\\b`, "g"));
    add("label", isOwn, `skills${squashed}r`, new RegExp(`skills${escaped(squashed)}r\\d`, "g"));
    add(
      "label",
      isOwn,
      `-${label}-<n>`,
      new RegExp(`[-_]${escaped(label)}[-_]\\d`, "g"),
      separated(label)
    );
    if (!plainWord(label)) add("label", isOwn, label, new RegExp(escaped(label), "g"));
  }
  add("word", false, "ablate", /ablate/gi);
  // Another run's directory name, 16 random hex digits: what the run is called nowhere but there.
  for (const run of map.runs)
    if (run.target !== "" && run.target !== own?.target) {
      const id = path.basename(run.target);
      add("run", false, id, new RegExp(escaped(id), "g"));
    }
  for (const { label, checkout } of map.checkouts)
    add("checkout", label === own?.label, checkout, new RegExp(escaped(checkout), "g"));
  add("map", false, map.maps, new RegExp(escaped(map.maps), "g"));
  add("case-file", false, "case.txt", /(^|\/)case\.txt$/g);
  return terms;
}

const hits: Hit[] = [];
const counts = new Map<string, number>();
const count = (channel: string, n = 1) => counts.set(channel, (counts.get(channel) ?? 0) + n);
/** Each place a channel could not see, printed as UNKNOWN: with no hit, the exit is 3. */
const unknowns: string[] = [];

function search(channel: string, where: string, text: string, terms: Term[]) {
  for (const term of terms) {
    if (term.kind === "case-file") continue;
    term.pattern.lastIndex = 0;
    for (const match of text.matchAll(term.pattern)) {
      const at = match.index ?? 0;
      const context = text
        .slice(Math.max(0, at - 60), at + match[0].length + 60)
        .replace(/[^\x20-\x7e]/g, " ");
      hits.push({ channel, where, term, context });
      if (hits.filter((hit) => hit.where === where && hit.term === term).length >= 3) break;
    }
  }
}

function searchPath(channel: string, file: string, terms: Term[]) {
  search(channel, `${file} (its path)`, file, terms);
  for (const term of terms)
    if (term.kind === "case-file" && /(^|\/)case\.txt$/.test(file))
      hits.push({ channel, where: file, term, context: "a file named case.txt" });
}

function readText(file: string): string | undefined {
  try {
    if (statSync(file).size > MAX_FILE) {
      count("skipped (over 64 MiB)");
      return undefined;
    }
    return readFileSync(file).toString("latin1");
  } catch {
    count("unreadable");
    return undefined;
  }
}

/** Every file under `dir`, without following a link; each link's target is returned apart. */
function walk(dir: string, files: string[], links: string[]) {
  let entries: Dirent[];
  try {
    entries = readdirSync(dir, { withFileTypes: true });
  } catch {
    count("unreadable");
    return;
  }
  for (const entry of entries) {
    const full = path.join(dir, entry.name);
    if (entry.isSymbolicLink()) links.push(full);
    else if (entry.isDirectory()) walk(full, files, links);
    else if (entry.isFile()) files.push(full);
  }
}

function gitRepositories(files: string[]): string[] {
  const repos = new Set<string>();
  for (const file of files) {
    const base = path.basename(file);
    if (base === "HEAD" && existsSync(path.join(path.dirname(file), "objects")))
      repos.add(path.dirname(file));
  }
  return [...repos];
}

/** One mount of a bubblewrap view, in the order bwrap makes them: a bind of `source` (a host path),
 *  a symbolic link to `link`, or an empty one of its own (a tmpfs, /dev, /proc). */
interface Mount {
  readonly dest: string;
  readonly source?: string;
  readonly link?: string;
}

/** The mounts of a view file (<work>/views/<id>, NUL-separated bwrap arguments). */
function mountsOf(file: string): Mount[] {
  const args = readFileSync(file, "latin1").split("\0").filter(Boolean);
  const mounts: Mount[] = [];
  for (let at = 0; at < args.length; ) {
    const option = args[at] ?? "";
    if (["--bind", "--ro-bind", "--dev-bind", "--symlink"].includes(option)) {
      const [from = "", dest = ""] = args.slice(at + 1, at + 3);
      mounts.push(option === "--symlink" ? { dest, link: from } : { dest, source: from });
      at += 3;
    } else if (["--tmpfs", "--dev", "--proc"].includes(option)) {
      mounts.push({ dest: args[at + 1] ?? "" });
      at += 2;
    } else at += option === "--chdir" ? 2 : 1;
  }
  return mounts;
}

/** Where the host holds `place`, a path of the agent's view: under the source of the last mount
 *  that covers it, through the view's own links; undefined where the view holds nothing there or
 *  an empty mount of its own. */
function hostPath(place: string, mounts: readonly Mount[]): string | undefined {
  let at = place;
  for (let hops = 0; hops < 8; hops++) {
    const mount = mounts.findLast(
      ({ dest }) => dest === "/" || at === dest || at.startsWith(`${dest}/`)
    );
    if (mount === undefined) return undefined;
    const rest = mount.dest === "/" ? at : at.slice(mount.dest.length);
    if (mount.source !== undefined) return path.join(mount.source, rest);
    if (mount.link === undefined) return undefined;
    at = path.join(path.resolve(path.dirname(mount.dest), mount.link), rest);
  }
  return undefined;
}

function staticWorld(runDir: string, terms: Term[], mounts: readonly Mount[] | undefined) {
  const files: string[] = [];
  const links: string[] = [];
  walk(runDir, files, links);
  for (const name of ["pane.env"]) {
    const file = path.join(runDir, name);
    if (existsSync(file)) {
      search("env", file, readFileSync(file, "utf8"), terms);
      count("env", readFileSync(file, "utf8").split("\n").filter(Boolean).length);
    }
  }
  for (const name of ["launch.sh", "system-args", "prompt.txt"]) {
    const file = path.join(runDir, name);
    if (existsSync(file)) {
      search("command", file, readFileSync(file, "utf8"), terms);
      count("command");
    }
  }
  const sessions = path.join(runDir, "sessions");
  for (const file of files) {
    const channel = file.startsWith(`${sessions}/`) ? "transcript" : "tree";
    searchPath(channel, file, terms);
    const text = readText(file);
    if (text !== undefined) search(channel, file, text, terms);
    count(channel);
  }
  const outside = new Set<string>();
  for (const link of links) {
    let target = "";
    try {
      target = readlinkSync(link);
    } catch {
      continue;
    }
    searchPath("tree", link, terms);
    search("tree", `${link} (its target)`, target, terms);
    count("links");
    const resolved = path.resolve(path.dirname(link), target);
    if (!resolved.startsWith(`${runDir}/`)) outside.add(resolved);
  }
  // The label's plugins and every other place a link of the run points at outside it, read where
  // the host holds what the agent's view shows there. A place the view does not hold is no part
  // of the agent's world, and is not read: a link to the operator's home, which mise leaves in a
  // run's state when it runs there with the operator's config, would read all of it.
  if (mounts === undefined && outside.size > 0)
    unknowns.push(
      `plugins: no view file, so none of ${outside.size} places links point at is read`
    );
  for (const place of outside) {
    if (mounts === undefined) break;
    const host = hostPath(place, mounts);
    if (host === undefined || !existsSync(host)) {
      count("links to nothing the view holds");
      continue;
    }
    const inner: string[] = [];
    if (statSync(host).isDirectory()) walk(realpathSync(host), inner, []);
    else inner.push(host);
    for (const file of inner) {
      searchPath("plugins", file, terms);
      const text = readText(file);
      if (text !== undefined) search("plugins", file, text, terms);
      count("plugins");
    }
  }
  for (const repo of gitRepositories(files)) {
    try {
      const log = execFileSync(
        "git",
        ["-C", repo, "log", "--all", "--format=%H %an <%ae> %cn <%ce>%n%B"],
        { encoding: "utf8", maxBuffer: 64 * 1024 * 1024 }
      );
      const refs = execFileSync("git", ["-C", repo, "for-each-ref", "--format=%(refname)"], {
        encoding: "utf8",
      });
      search("git", `${repo} (log)`, log, terms);
      search("git", `${repo} (refs)`, refs, terms);
      count("git");
    } catch {
      count("unreadable");
    }
  }
  return files.length;
}

/** A rig work directory's own trees in a path: its map (<w>/maps/runs, profiles or built-from),
 *  and a run's or a label's directory (<w>/runs/<id>, <w>/profiles/<id>, <id> 16 hex digits). */
const WORK_TREE =
  /\/maps(?=\/(?:runs|profiles|built-from)(?:\/|$))|\/(?:runs|profiles)\/[0-9a-f]{16}(?=\/|$)/g;
const WORK_TERM: Term = {
  kind: "work",
  own: false,
  text: "a rig work directory's map, run or profile",
  pattern: WORK_TREE,
  literals: ["/maps/", "/runs/", "/profiles/"],
};

/** The walk: every name of the view, each with a leading /, as find prints it with -printf
 *  '/%P\0', through a fixed-string filter of the literals (case-insensitive, in the C locale), on
 *  stdout; find's own count of names (one NUL each, counted on a FIFO beside the filter), its
 *  errors and each stage's status in the directory $1. $2 is `view`, a walk inside bwrap with the
 *  view file $3, or `root`, a walk under the directory $3 (an agent's /proc/<pid>/root). */
const WALK = `set -o pipefail
out=$1 mode=$2 where=$3
if [ "$mode" = view ]; then
  walk() { bwrap --args 3 -- find / \\( -path /proc -o -path /sys -o -path /dev \\) -prune -o -printf '/%P\\0' 3<"$where"; }
else
  walk() { find "$where/" \\( -path "$where/proc" -o -path "$where/sys" -o -path "$where/dev" \\) -prune -o -printf '/%P\\0'; }
fi
mkfifo "$out/names"
tr -dc '\\000' <"$out/names" | wc -c >"$out/count" &
counter=$!
walk 2>"$out/errors" | tee "$out/names" | LC_ALL=C grep -zFi -f "$out/literals"
printf '%s\\n' "\${PIPESTATUS[@]}" >"$out/status"
wait "$counter"
`;

/** find's own words for a name the walk could not enter, which the agent cannot read either, or
 *  that went between the listing and the read. Any other line on its stderr, a bwrap error among
 *  them, means the walk did not see the whole view. */
const WALK_NOTE =
  /^find: .*: (Permission denied|No such file or directory)$|^find: File system loop detected;/;

/**
 * The view channel: every name of the agent's whole view, walked as `source` says, matched
 * against each term and against another rig work directory's tree (WORK_TREE), but for the run's
 * own trees, `own`. A hit names the shortest path that holds its match, once per term. The walk
 * must reach the run's own directory, which every view holds: a walk that did not, or did not
 * finish, is UNKNOWN.
 */
async function viewWorld(
  runDir: string,
  terms: Term[],
  own: readonly string[],
  source: { readonly mode: "view" | "root"; readonly where: string }
) {
  const out = mkdtempSync(path.join(tmpdir(), "visible-world-"));
  try {
    const literals = new Set(WORK_TERM.literals);
    for (const term of terms) for (const literal of term.literals) literals.add(literal);
    writeFileSync(path.join(out, "literals"), `${[...literals].join("\n")}\n`);
    const walk = Bun.spawn(["bash", "-c", WALK, "-", out, source.mode, source.where], {
      stdout: "pipe",
      stderr: "pipe",
    });
    const matchers = [...terms, WORK_TERM];
    const seen = new Set<string>();
    let candidates = 0;
    let reachedRun = false;
    const consider = (name: string) => {
      candidates++;
      if (name === runDir) reachedRun = true;
      const lower = name.toLowerCase();
      for (const term of matchers) {
        if (!term.literals.some((literal) => lower.includes(literal))) continue;
        for (const match of name.matchAll(term.pattern)) {
          const at = match.index ?? 0;
          const end = at + match[0].length;
          const cut = term === WORK_TERM ? end : name.indexOf("/", end);
          const where = cut === -1 ? name : name.slice(0, cut);
          if (term === WORK_TERM && own.includes(where)) continue;
          const key = `${term.kind}\0${term.text}\0${where}`;
          if (seen.has(key)) continue;
          seen.add(key);
          const context = where.slice(Math.max(0, at - 60), end + 60).replace(/[^\x20-\x7e]/g, " ");
          hits.push({ channel: "view", where, term, context });
        }
      }
    };
    let rest = Buffer.alloc(0);
    for await (const chunk of walk.stdout) {
      const buffer = rest.length === 0 ? Buffer.from(chunk) : Buffer.concat([rest, chunk]);
      let start = 0;
      for (let end = buffer.indexOf(0, start); end !== -1; end = buffer.indexOf(0, start)) {
        consider(buffer.toString("latin1", start, end));
        start = end + 1;
      }
      rest = buffer.subarray(start);
    }
    const shell = (await new Response(walk.stderr).text()).trim();
    const exit = await walk.exited;
    const read = (name: string) => {
      try {
        return readFileSync(path.join(out, name), "utf8");
      } catch {
        return "";
      }
    };
    const [walked = "", tee = "", filter = ""] = read("status").trim().split("\n");
    const names = Number(read("count").trim() || "0");
    const errors = read("errors").split("\n").filter(Boolean);
    const other = errors.filter((line) => !WALK_NOTE.test(line));
    count("view names", names);
    count("view names past the filter", candidates);
    count("view names it could not enter", errors.length - other.length);
    const why =
      exit !== 0
        ? `the walk's shell exited ${exit}${shell ? `: ${shell.split("\n")[0]}` : ""}`
        : !["0", "1"].includes(walked)
          ? `the walk exited ${walked || "with no status"}`
          : tee !== "0"
            ? `its tee exited ${tee}`
            : !["0", "1"].includes(filter)
              ? `its filter exited ${filter}`
              : other.length > 0
                ? `the walk said: ${other[0]}`
                : !reachedRun
                  ? `the walk of ${names} names never reached the run's own directory ${runDir}`
                  : "";
    if (why !== "") unknowns.push(`view (${source.mode} ${source.where}): ${why}`);
    else
      console.log(
        `view: walked all ${names} names of the agent's view (${source.mode} ${source.where}), ${errors.length - other.length} it could not enter`
      );
  } finally {
    rmSync(out, { recursive: true, force: true });
  }
}

/** The live channels; returns how many processes carry the run's SKILL_SCENARIO_RUN, and the pid
 *  of its agent among them. */
function liveWorld(
  runDir: string,
  work: string,
  terms: Term[],
  map: RunMap
): { agents: number; agent: string | undefined } {
  const marker = `SKILL_SCENARIO_RUN=${runDir}\0`;
  const pids = readdirSync("/proc").filter((pid) => {
    if (!/^\d+$/.test(pid)) return false;
    try {
      return readFileSync(`/proc/${pid}/environ`).toString("latin1").includes(marker);
    } catch {
      return false;
    }
  });
  for (const pid of pids) {
    try {
      search(
        "process",
        `pid ${pid} environ`,
        readFileSync(`/proc/${pid}/environ`).toString("latin1"),
        terms
      );
      search(
        "process",
        `pid ${pid} cmdline`,
        readFileSync(`/proc/${pid}/cmdline`).toString("latin1"),
        terms
      );
      count("process");
    } catch {
      count("unreadable");
    }
  }
  const agent = pids.find((pid) => {
    try {
      return readFileSync(`/proc/${pid}/cmdline`, "latin1").includes("omp");
    } catch {
      return false;
    }
  });
  if (agent !== undefined) {
    const root = `/proc/${agent}/root`;
    try {
      for (const pid of readdirSync(`${root}/proc`).filter((one) => /^\d+$/.test(one))) {
        try {
          search(
            "visible",
            `its /proc/${pid}/cmdline`,
            readFileSync(`${root}/proc/${pid}/cmdline`).toString("latin1"),
            terms
          );
          count("visible processes");
        } catch {
          // A process that ended between the listing and the read.
        }
      }
    } catch {
      unknowns.push(`visible: cannot read ${root}/proc`);
    }
    const names: string[] = [];
    const list = (dir: string, depth: number) => {
      let entries: string[];
      try {
        entries = readdirSync(`${root}${dir}`);
      } catch {
        return;
      }
      for (const name of entries) {
        const full = path.join(dir, name);
        names.push(full);
        search("visible", `${full} (under its <work>)`, full, terms);
        if (depth > 0 && full !== runDir) {
          try {
            if (statSync(`${root}${full}`).isDirectory()) list(full, depth - 1);
          } catch {
            // Gone, or a socket.
          }
        }
      }
    };
    list(work, 2);
    count("visible work entries", names.length);
    console.log(
      `its <work> shows: ${names.filter((name) => name.split("/").length <= work.split("/").length + 2).join(" ")}`
    );
    if (existsSync(`${root}${work}/maps`))
      hits.push({
        channel: "visible",
        where: `${work}/maps`,
        term: terms.find((term) => term.kind === "map") as Term,
        context: "the agent's mount namespace shows the map",
      });
    // The agent's scratch directories: its /tmp must be its run's own tmp, and /var/tmp and
    // /dev/shm hold nothing, since the machine's are every run's.
    const own = statSync(path.join(runDir, "tmp"));
    const tmp = statSync(`${root}/tmp`);
    if (tmp.dev !== own.dev || tmp.ino !== own.ino)
      hits.push({
        channel: "visible",
        where: "/tmp",
        term: terms.find((term) => term.kind === "case-file") as Term,
        context: "the agent's /tmp is not its run's own tmp: it is the machine's, every run's",
      });
    for (const shared of ["/var/tmp", "/dev/shm"]) {
      const entries = readdirSync(`${root}${shared}`);
      count(`visible ${shared} entries`, entries.length);
      for (const name of entries)
        search("visible", `${shared}/${name}`, `${shared}/${name}`, terms);
    }
    // Every label's checkout is empty in its view, so no agent reads another label's texts.
    for (const { checkout } of map.checkouts) {
      const entries = existsSync(`${root}${checkout}`) ? readdirSync(`${root}${checkout}`) : [];
      count("visible checkouts");
      if (entries.length > 0)
        hits.push({
          channel: "visible",
          where: checkout,
          term: terms.find((term) => term.kind === "checkout") as Term,
          context: `the agent's view shows ${entries.length} entries of a label's checkout`,
        });
    }
  }
  const dispatchFile = path.join(runDir, "dispatch.json");
  if (existsSync(dispatchFile)) {
    const { postgres, database } = JSON.parse(readFileSync(dispatchFile, "utf8")) as {
      postgres?: string;
      database?: string;
    };
    if (postgres && database) {
      try {
        const dump = execFileSync(
          "docker",
          ["exec", postgres, "pg_dump", "-U", "postgres", "--data-only", database],
          { encoding: "latin1", maxBuffer: 512 * 1024 * 1024 }
        );
        search("dispatch", `database ${database}`, dump, terms);
        count("dispatch bytes", dump.length);
      } catch (error) {
        unknowns.push(
          `dispatch: pg_dump of ${database} failed: ${error instanceof Error ? error.message.split("\n")[0] : error}`
        );
      }
    }
  }
  return { agents: pids.length, agent };
}

if (import.meta.main) {
  const args = Bun.argv.slice(2);
  const live = args.includes("--live");
  const runArg = args.find((arg) => !arg.startsWith("--"));
  if (runArg === undefined || !existsSync(runArg)) {
    console.error("usage: visible-world.ts <run dir> [--live]");
    process.exit(2);
  }
  const runDir = realpathSync(runArg);
  const work = path.dirname(path.dirname(runDir));
  const map = mapOf(work, runDir);
  if (map.own === undefined) {
    console.error(`visible-world.ts: no link under ${work}/maps/runs points at ${runDir}`);
    process.exit(2);
  }
  const terms = termsOf(map);
  console.log(
    `run ${map.own.name} (${runDir}): scenario ${map.own.scenario}, case ${map.own.case || "none"}, label ${map.own.label}; ${terms.length} terms from ${map.runs.length} runs and ${map.labels.length} labels`
  );
  const viewFile = path.join(work, "views", path.basename(runDir));
  const files = staticWorld(runDir, terms, existsSync(viewFile) ? mountsOf(viewFile) : undefined);
  const seen = live ? liveWorld(runDir, work, terms, map) : undefined;
  // The trees of its own that the run's view holds: its directory and its label's profile.
  const own = [runDir];
  try {
    own.push(realpathSync(path.join(work, "maps", "profiles", map.own.label)));
  } catch {
    // A label whose profile was removed: the view walk then names any profile it sees.
  }
  if (seen !== undefined) {
    if (seen.agent !== undefined)
      await viewWorld(runDir, terms, own, { mode: "root", where: `/proc/${seen.agent}/root` });
    else if (seen.agents > 0)
      unknowns.push(
        `view: none of the run's ${seen.agents} processes runs omp, so no walk of its view`
      );
  } else {
    if (existsSync(viewFile))
      await viewWorld(runDir, terms, own, { mode: "view", where: viewFile });
    else unknowns.push(`view: no view file ${viewFile}, so no walk of the agent's view`);
  }
  for (const hit of hits)
    console.log(
      `${hit.channel}\t${hit.where}\t${hit.term.kind}${hit.term.own ? " (own)" : ""}\t${hit.term.text}\t…${hit.context}…`
    );
  console.log(`read: ${[...counts].map(([channel, n]) => `${channel} ${n}`).join(", ")}`);
  const byChannel = new Map<string, number>();
  for (const hit of hits) byChannel.set(hit.channel, (byChannel.get(hit.channel) ?? 0) + 1);
  console.log(
    `hits: ${hits.length}${hits.length ? ` (${[...byChannel].map(([c, n]) => `${c} ${n}`).join(", ")})` : ""}`
  );
  if (files === 0) unknowns.push(`tree: read no file under ${runDir}`);
  if (seen?.agents === 0)
    unknowns.push(
      `process: no process carries SKILL_SCENARIO_RUN=${runDir}: run --live while its agent runs`
    );
  for (const why of unknowns) console.log(`UNKNOWN ${why}`);
  process.exit(hits.length > 0 ? 1 : unknowns.length > 0 ? 3 : 0);
}
