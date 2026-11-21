// The rules each plugin's skills-guard.test.ts runs over the skills it stages into dist/skills
// (scripts/pi-plugin-prepack.sh's partition for its package name). The skills are read by agents,
// and how much of each reaches the model is decided by Oh My Pi: a tool result over 51,200 bytes
// (`tools.artifactSpillThreshold`, 50 KiB) spills to an artifact that keeps only 20 KB at each end,
// so a longer skill arrives with its middle cut out. A `skill://` read is otherwise whole (no line
// limit, no line-length cap), so the file's own byte count is the measure. Reads by filesystem path
// stop at 300 lines, which is why every reference is linked as `skill://<name>/<path>`, and why each
// such link must resolve (the daemon's boot gate checks only the name before the first `/`).
import { existsSync, mkdtempSync, readdirSync, readFileSync, rmSync, statSync } from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
import { linkOmpNatives } from "./omp-natives";

export const SPILL_THRESHOLD_BYTES = 50 * 1024;
/** The repository root, where `skills/` and the prepack script live. */
export const REPO_ROOT = path.resolve(import.meta.dir, "../../..");
const PREPACK_SCRIPT = path.join(REPO_ROOT, "scripts/pi-plugin-prepack.sh");
// A link's path stops at whitespace, a closing bracket or quote, a code span, or Markdown emphasis
// (`**skill://…/pr-body.md**`); `#anchor` is kept so it can be checked against the target's headings.
const SKILL_LINK = /skill:\/\/([a-z0-9](?:[a-z0-9._-]*[a-z0-9])?)\/([^\s)`'"\]>*]+)/g;
// A relative Markdown link, `](../…)` or `](./…)`, up to its closing parenthesis.
const RELATIVE_LINK = /\]\((\.\.?\/[^\s)]+)\)/g;
// A task dispatch as the daemon's boot gate reads it (packages/daemon/internal/promptrefs/
// promptrefs.go): the name inside `agent="…"`, as `task(agent="<name>")` writes it.
const AGENT_DISPATCH = /agent="([a-z0-9][a-z0-9._-]*)"/g;

/**
 * Stages `packageName`'s skill partition into a fresh temporary directory through the prepack
 * script's `--stage-skills` mode, exactly as its `prepack` stages dist/skills, and returns it. The
 * caller removes the directory.
 */
export function stageSkills(packageName: string): string {
  const dest = path.join(mkdtempSync(path.join(os.tmpdir(), "staged-skills-")), "skills");
  const staged = Bun.spawnSync(["bash", PREPACK_SCRIPT, "--stage-skills", packageName, dest]);
  if (staged.exitCode !== 0) {
    rmSync(path.dirname(dest), { recursive: true, force: true });
    throw new Error(
      `${PREPACK_SCRIPT} --stage-skills ${packageName} exited ${staged.exitCode}:\n${staged.stderr.toString()}`
    );
  }
  return dest;
}

/** Every file under `directory`, recursively. */
export function files(directory: string): string[] {
  return readdirSync(directory, { recursive: true, encoding: "utf8" })
    .map((entry) => path.join(directory, entry))
    .filter((file) => statSync(file).isFile());
}

/** Every `<skillsRoot>/<name>/SKILL.md`. */
export function skillFiles(skillsRoot: string): string[] {
  return readdirSync(skillsRoot)
    .map((name) => path.join(skillsRoot, name, "SKILL.md"))
    .filter((file) => existsSync(file));
}

/**
 * The lines outside fenced code blocks, where a Markdown reader finds headings: a fence opens on
 * three or more backticks or tildes (up to three spaces in), closes on a line holding only a run of
 * the same character at least as long, and an unclosed fence runs to the end of the file.
 */
export function outsideFences(markdown: string): string[] {
  const kept: string[] = [];
  let fence: string | undefined;
  for (const line of markdown.split("\n")) {
    const marker = /^ {0,3}(`{3,}|~{3,})/.exec(line)?.[1];
    if (fence === undefined) {
      if (marker === undefined) kept.push(line);
      else fence = marker;
    } else if (
      marker?.[0] === fence[0] &&
      marker.length >= fence.length &&
      line.trim() === marker
    ) {
      fence = undefined;
    }
  }
  return kept;
}

/** `#`, which the parse below counts from the end of a line one code unit at a time. */
const HASH = "#".charCodeAt(0);
/** The characters `.` never matches, so no heading's text can hold one or run past one. */
const LINE_TERMINATOR = /[\n\r\u2028\u2029]/;

/**
 * The text a heading line renders, or undefined where the line is no heading: one to six `#`, then
 * at least one whitespace character, then at least one character of text, with the line's trailing
 * run of `#` and the whitespace before that run dropped.
 *
 * This reads `/^#{1,6}\s+(.+?)\s*#*$/` as it is written, line by line, rather than running it. That
 * expression is quadratic: `\s*#*$` rescans the line's trailing whitespace once for every expansion
 * of the lazy group, so one long heading line in a repository file holds every developer's run of
 * this guard for minutes (330 ms at 10,000 trailing spaces, 122 s at 200,000). The parse below
 * reads each line a bounded number of times and keeps that expression's meaning for every input,
 * oddities included: the group must match one character, so `## ###` is the heading `#`, and the
 * trailing hashes are dropped only where they end the line, so `## a  ##  ` is the heading `a  ##`.
 */
export function headingText(line: string): string | undefined {
  // The hashes `#{1,6}` matches are the line's whole leading run: after a shorter run stands a `#`,
  // which is no whitespace, so a line opening with seven leaves `\s+` nothing to match.
  const opener = /^(#{1,6})(\s+)/.exec(line);
  if (opener === null) return undefined;
  const spaces = opener[2];
  const rest = line.slice(opener[0].length);
  if (rest === "") {
    // Whitespace is all that follows the hashes, so `\s+` hands its last character back to the
    // group, which must match one, and the capture is that one character. The engine hands back
    // another only where the character is a line terminator, which `.` never matches, and it keeps
    // the first character for `\s+` itself.
    for (let at = spaces.length - 1; at >= 1; at--) {
      const character = spaces.charAt(at);
      if (!LINE_TERMINATOR.test(character)) return character;
    }
    return undefined;
  }
  // The group is lazy, so it stops at the first point where `\s*#*$` can read the whole remainder:
  // the end of the line, the start of its trailing run of `#`, or the start of the whitespace
  // before that run. Nowhere earlier can, since the character before that whitespace is neither.
  // `trimEnd` drops exactly the characters `\s` matches (both are WhiteSpace and LineTerminator),
  // in one pass of its own rather than a regular expression call for every character.
  let end = rest.length;
  while (end > 0 && rest.charCodeAt(end - 1) === HASH) end--;
  end = rest.slice(0, end).trimEnd().length;
  const heading = rest.slice(0, Math.max(end, 1));
  // Past a line terminator the group cannot reach, so the expression finds no heading at all: the
  // shorter captures it could take instead all leave a remainder `\s*#*$` will not read.
  return LINE_TERMINATOR.test(heading) ? undefined : heading;
}

/** GitHub's heading anchor: lower-cased, punctuation dropped, each space a hyphen. */
export function anchors(markdown: string): Set<string> {
  return new Set(
    outsideFences(markdown).flatMap((line) => {
      const heading = headingText(line);
      if (heading === undefined) return [];
      return [
        heading
          .toLowerCase()
          .replace(/[^\p{L}\p{N}\s_-]/gu, "")
          .replace(/\s/g, "-"),
      ];
    })
  );
}

export interface SkillLink {
  /** The Markdown file holding the link. */
  readonly source: string;
  readonly name: string;
  readonly target: string;
  readonly anchor: string | undefined;
}

/** Every `skill://<name>/<path>[#anchor]` link in the Markdown files under `linkingRoots`. */
export function skillLinks(linkingRoots: readonly string[]): SkillLink[] {
  return linkingRoots
    .flatMap(files)
    .filter((file) => file.endsWith(".md"))
    .flatMap((file) =>
      [...readFileSync(file, "utf8").matchAll(SKILL_LINK)].map(([, name = "", raw]) => {
        // A link that ends a sentence carries its full stop.
        const [target = "", anchor] = (raw ?? "").replace(/[.,;:]+$/, "").split("#");
        return { source: file, name, target, anchor };
      })
    );
}

/** The files under `root` at or over Oh My Pi's spill threshold, so a skill:// read of one is cut. */
export function oversizedFiles(root: string): { file: string; bytes: number }[] {
  return files(root)
    .map((file) => ({ file: path.relative(root, file), bytes: statSync(file).size }))
    .filter(({ bytes }) => bytes >= SPILL_THRESHOLD_BYTES);
}

/** The skill bodies under `skillsRoot` of 500 lines or more, whose detail belongs in references. */
export function longSkillBodies(skillsRoot: string): { file: string; lines: number }[] {
  return skillFiles(skillsRoot)
    .map((file) => ({
      file: path.relative(skillsRoot, file),
      lines: readFileSync(file, "utf8").split("\n").length,
    }))
    .filter(({ lines }) => lines >= 500);
}

/**
 * The skills under `skillsRoot` whose frontmatter name is not their directory's name. Oh My Pi
 * resolves `skill://<name>` by the frontmatter name, and `brokenSkillLinks` resolves it by the
 * directory, so the two must agree.
 */
export function misnamedSkills(
  skillsRoot: string
): { directory: string; name: string | undefined }[] {
  return skillFiles(skillsRoot)
    .map((file) => {
      const frontmatter = /^---\n([\s\S]*?)\n---\n/.exec(readFileSync(file, "utf8"))?.[1] ?? "";
      return {
        directory: path.basename(path.dirname(file)),
        name: /^name:\s*(.*?)\s*$/m.exec(frontmatter)?.[1],
      };
    })
    .filter(({ directory, name }) => name !== directory);
}

/**
 * The links of `links` that name no file under `<skillsRoot>/<name>/`, or an anchor no heading of
 * that file renders, each as one sentence naming the source.
 */
export function brokenSkillLinks(links: readonly SkillLink[], skillsRoot: string): string[] {
  return links.flatMap(({ source, name, target, anchor }) => {
    const link = `skill://${name}/${target}`;
    const file = path.join(skillsRoot, name, target);
    if (!existsSync(file)) return [`${source}: ${link} names no file`];
    if (anchor !== undefined && !anchors(readFileSync(file, "utf8")).has(anchor)) {
      return [`${source}: ${link}#${anchor} names no heading`];
    }
    return [];
  });
}

/**
 * The relative Markdown links (`](../…)`, `](./…)`) in the files under `partitionRoot` whose target
 * is not a file inside it. A staged partition is installed on its own, so a link out of it, into a
 * skill the other plugin ships, dangles there; such a link is written `skill://<name>/<path>`,
 * which resolves by name once both plugins are installed. Links inside code fences are examples,
 * not links a reader follows, so they are not held to this.
 */
export function brokenRelativeLinks(partitionRoot: string): string[] {
  const inside = `${path.resolve(partitionRoot)}${path.sep}`;
  return files(partitionRoot)
    .filter((file) => file.endsWith(".md"))
    .flatMap((file) =>
      [...outsideFences(readFileSync(file, "utf8")).join("\n").matchAll(RELATIVE_LINK)].flatMap(
        ([, raw = ""]) => {
          const target = raw.split("#")[0] ?? "";
          const resolved = path.resolve(path.dirname(file), target);
          if (!resolved.startsWith(inside)) {
            return [`${path.relative(partitionRoot, file)}: ${raw} leaves the staged partition`];
          }
          if (!existsSync(resolved) || !statSync(resolved).isFile()) {
            return [`${path.relative(partitionRoot, file)}: ${raw} names no file`];
          }
          return [];
        }
      )
    );
}

export interface AgentDispatch {
  /** The Markdown file holding the dispatch. */
  readonly source: string;
  readonly name: string;
}

/**
 * Every `task(agent="<name>")` dispatch in the Markdown files under `roots`. Code fences are not
 * excluded: the daemon's gate reads a prompt whole, and a dispatch shown in a fence is still what a
 * model copies.
 */
export function agentDispatches(roots: readonly string[]): AgentDispatch[] {
  return roots
    .flatMap(files)
    .filter((file) => file.endsWith(".md"))
    .flatMap((file) =>
      [...readFileSync(file, "utf8").matchAll(AGENT_DISPATCH)].map(([, name = ""]) => ({
        source: file,
        name,
      }))
    );
}

/**
 * The task agents the Oh My Pi binary `omp` resolves with no plugin installed: the names of the
 * files `omp agents unpack` writes under a fresh HOME. That HOME is a temporary directory holding
 * only the binary's natives (hardlinks to the shared copy, omp-natives.ts), and the binary's
 * environment names no profile or config directory, so it writes where a person's does. The
 * directory is removed before returning.
 */
export async function bundledAgents(omp: string): Promise<string[]> {
  const home = mkdtempSync(path.join(os.tmpdir(), "omp-bundled-agents-"));
  try {
    await linkOmpNatives(omp, home);
    const unpacked = Bun.spawnSync([omp, "agents", "unpack"], {
      env: { HOME: home, PATH: "/usr/local/bin:/usr/bin:/bin" },
    });
    if (unpacked.exitCode !== 0) {
      throw new Error(
        `${omp} agents unpack exited ${unpacked.exitCode}:\n${unpacked.stderr.toString()}`
      );
    }
    return files(path.join(home, ".omp"))
      .filter((file) => file.endsWith(".md"))
      .map((file) => path.basename(file, ".md"));
  } finally {
    rmSync(home, { recursive: true, force: true });
  }
}

/** The task agents the plugin at `packageRoot` ships: the `*.md` under its `agents/`, if any. */
export function shippedAgents(packageRoot: string): string[] {
  const agents = path.join(packageRoot, "agents");
  if (!existsSync(agents)) return [];
  return readdirSync(agents)
    .filter((file) => file.endsWith(".md"))
    .map((file) => path.basename(file, ".md"));
}

/**
 * The dispatches in the Markdown files under `roots` naming an agent not in `available`, each as
 * one sentence naming the source.
 */
export function unresolvedAgentDispatches(
  roots: readonly string[],
  available: ReadonlySet<string>
): string[] {
  return agentDispatches(roots)
    .filter(({ name }) => !available.has(name))
    .map(
      ({ source, name }) =>
        `${source} dispatches ${name}, which neither this plugin nor Oh My Pi ships`
    );
}

/**
 * The offset in `markdown` that each character of its whitespace-collapsed text came from. A rule
 * written over a sentence reads the collapsed text, because these files are wrapped and a
 * sentence of one crosses lines; these offsets give a hit back the line of the file a reader
 * opens.
 */
function collapsedOffsets(markdown: string): number[] {
  const offsets: number[] = [];
  const whitespace = /\s+/g;
  let read = 0;
  for (let run = whitespace.exec(markdown); run !== null; run = whitespace.exec(markdown)) {
    for (let at = read; at < run.index; at += 1) offsets.push(at);
    offsets.push(run.index);
    read = run.index + run[0].length;
  }
  for (let at = read; at < markdown.length; at += 1) offsets.push(at);
  return offsets;
}

/**
 * The lane nouns the patterns below enumerate as a re-run's subject or object: `lane`, `check`,
 * `suite` and `job`. Every pattern builds its group from this one source, so a noun added here
 * reaches each of them: `${LANE}` alone is the singular, `${LANE}s?` either number, and `${LANE}s`
 * the plural. `run` is deliberately no such noun (`NO_LANE_IS_RERUN` says why).
 */
const LANE = "(?:lane|check|suite|job)";
/** The word a lane noun may carry before it and still be one: `CI lane`, `required check`, `test suite`. */
const LANE_QUALIFIER = String.raw`(?:CI\s+|required\s+|test\s+)?`;

/**
 * The prohibition INFRA-1303 retired from the tester's evidence rule
 * (skills/legion-worker/references/pr-body.md): "no CI lane is rerun as evidence", and the same
 * sentence in the active voice, "never re-run a CI lane as evidence", which the tester's prompt
 * part carried beside it. Both stood beside a read of CI the tester no longer makes
 * (`RETIRED_TESTER_CI_READS`, below), and a red at the head that the change did not make is now
 * the reviewer's in `reviewing` and the implementer's while it holds `implementing` to re-run once
 * (`skill://legion-worker/references/red-at-the-head.md`), so a file that still forbids the
 * re-run of a lane, read by either of them, costs the tree a whole lap for a failure its code
 * never caused.
 *
 * Three shapes, each with its subject enumerated rather than left open. The passive: a lane noun
 * under a universal negative (`no`, `neither`) as the subject of the re-run, with nothing between
 * the noun and `re-run` but auxiliaries (`is`, `gets`, `may be`, `is ever`), so the prohibition is
 * on that lane. A negative that governs another verb passes, though a re-run follows it: "If no
 * checks appear after a minute, re-run the workflow from the Actions tab." and "When no job starts,
 * rerun `apply.sh`". The active: `never re-runs a …` over the same nouns behind a determiner. The
 * imperative: `re-run no …` over the same nouns, the shape cybermonklabs/legion#4447 brought back
 * into pr-body.md as "rerun no lane as evidence" in a forward merge that took main's side. `run` is
 * deliberately no such noun, which is what lets the sentences the change keeps pass: "A
 * merge-group run is never re-run" (merge-gate.md, pr-body.md and the implementer's prompt overlay)
 * and "Where no run at the head holds the failing check, the role re-runs nothing"
 * (red-at-the-head.md). "No role re-runs a group build." passes because a role is no lane, and "it
 * gets no re-run", said of a check that judges the pull request itself, because a re-run is no lane
 * either. The active form lets one limit through, a `for` naming the rows a ban covers:
 * merge-gate.md's "never re-run a job for a `cancelled` row that a newer run of the same name has
 * superseded" keeps the re-run of the red the page names, and passes. The guard reads every other
 * word after the noun as the ban itself: "Never re-run a check that failed." and "Never re-run a
 * lane that failed on itself." forbid the very re-run the page gives, and are refused. "Never
 * re-run a check that the change made red" keeps that re-run, yet the guard refuses it too, on
 * purpose, for it to be reworded: by the sentence's shape alone, a `that` clause which limits the
 * ban cannot be told apart from one which restates the retired ban. That is the cost of reading
 * shape: the guard refuses some correct text too.
 */
const NO_LANE_IS_RERUN = new RegExp(
  [
    String.raw`\b(?:no|neither)\s+${LANE_QUALIFIER}${LANE}s?\s+(?:(?:is|are|was|were|gets?|got|be|been|ever|may|can|will|should|must)\s+){0,3}re-?run\b`,
    String.raw`\bnever\s+re-?runs?\s+(?:a|an|any|the)\s+${LANE_QUALIFIER}${LANE}\b(?!\s+for\b)`,
    String.raw`\bre-?runs?\s+no\s+${LANE_QUALIFIER}${LANE}s?\b`,
  ].join("|"),
  "i"
);

/**
 * The prohibition INFRA-1303 retired from the tester's prompt part
 * (packages/daemon/internal/prompts/roles/core/tester.md): "rerun none of them yourself", said of
 * the CI lanes the tester had just been told to read on GitHub. The tester reads no CI now
 * (`RETIRED_TESTER_CI_READS`), so the sentence has no lanes left to be said of, and the head's one
 * re-run is the reviewer's in `reviewing` and the implementer's while it holds `implementing`
 * (`skill://legion-worker/references/red-at-the-head.md`): either of them, told to re-run none of
 * them, reads that rule as written for somebody else.
 *
 * The object is enumerated — `them`, `these`, `those`, or the lane nouns themselves — rather than
 * left open, and the verb must be a re-run, never a bare `run`. That is what lets the sentence
 * the prompt keeps in its place pass: "you do not run in your own pod what CI runs for you", which
 * forbids the local run and says nothing of the re-run.
 */
const RERUN_NONE_OF_THEM_YOURSELF = new RegExp(
  String.raw`\bre-?runs?\s+(?:none|neither)\s+of\s+(?:them|these|those|the\s+${LANE}s)\b|\b(?:never|do not|don't)\s+re-?run\s+(?:any\s+of\s+)?(?:them|these|those)\b[^.]{0,40}?\byourself\b`,
  "i"
);

/**
 * The prohibition INFRA-1303 retired from the reviewer's thread rule
 * (skills/legion-worker/references/review-threads.md): "you re-run nothing", said to the reviewer
 * of a review bot that is red while a thread it opened still stands. The reviewer holds
 * `reviewing`, so such a red is one it may re-run once
 * (`skill://legion-worker/references/red-at-the-head.md`); what that file keeps is the reason a
 * re-run does not clear this particular red, never a ban on pressing one.
 *
 * The subject is the reviewer: by name (`the reviewer`, `your review`), or as `you` in a sentence
 * that names a review bot before it, which is the retired claim's own shape. A bare `you` can be
 * any role, so it passes: the natural way to tell the merger it re-runs nothing is "You re-run
 * nothing: …". So do the sentences that deny the re-run to a role holding no phase: "The merger,
 * the architect in `accepting` and an implementer that runs retro re-run nothing." and "A role
 * that is told of a red whose phase it does not hold re-runs nothing." (both red-at-the-head.md),
 * and "a re-run does not clear that red" (review-threads.md), which names no re-runner at all. The
 * cost is a ban written to the reviewer as a bare `you` with no review bot in its sentence, which
 * passes too. The gap is bounded by `[^.]{0,40}`, so a `the reviewer` of the sentence before a
 * full stop cannot reach across it to the `re-run nothing` of the next, and the review bot is
 * read in the same sentence as its `you`. The `you` is found through a lookbehind, which Bun's
 * JavaScriptCore runs, so a hit names the line of the `you`.
 */
const REVIEWER_RERUNS_NOTHING =
  /\b(?:the reviewer|your review|you(?<=\breview\s+bot\b[^.]*\byou))\b[^.]{0,40}?\bre-?runs?\s+(?:nothing|none)\b/i;

/**
 * The prohibition INFRA-1303 retired from the Envoy skill (skills/envoy/SKILL.md): "A Legion
 * implementer waits this way only on a re-run of a red check its task names.", which gave the
 * wait to one role and to one red. The reviewer in `reviewing` waits that way now as the
 * implementer does while it holds `implementing`, on the red the counted set carries
 * (`skill://legion-worker/references/red-at-the-head.md`), so the sentence names too few roles
 * and the wrong red. The `@sjawhar/pi-envoy` partition stages that file and none of the three
 * files above, so without this pattern a forward merge could restore the sentence there and every
 * guard would stay green.
 *
 * The subject is the implementer and the limiting word is `only`, so a sentence that names both
 * holders passes: "The reviewer in `reviewing` or the implementer while it holds `implementing`
 * waits this way on the re-run …".
 */
const ONLY_THE_IMPLEMENTER_WAITS = /\ba Legion implementer waits this way only\b/i;

/**
 * The send-back INFRA-1303 retired from the merger's watch
 * (skills/legion-worker/references/merge-gate.md): "a head the queue took out over its own
 * `merge_group` checks is work to fix, so the issue goes back", which sent the work back on the
 * first `failed_checks` removal. The merger's prompt part
 * (packages/daemon/internal/prompts/roles/merger.md) carried the same rule as "That is work to
 * fix. Send the issue back", and for a while in this change as "is work to fix, so send the issue
 * back". A `failed_checks` removal at a head that has not changed now gets that same head queued
 * once more, and the issue goes back only on the second such removal of that head (the merger's
 * prompt part, where cybermonklabs/legion#4447 moved every step of the merger's): the
 * first-removal rule sent cybermonklabs/legion#3972 round a lap on a flaky group build
 * (INFRA-3204's retro).
 *
 * The subject is the `merge_group` checks, and the gap after them is `.{0,30}` rather than
 * `[^.]`, so it reaches across the one full stop of the prompt part's "checks failed. That is
 * work to fix." Between `is work to fix` and the send-back any punctuation stands, or none: the
 * comma of "is work to fix, so send the issue back", the full stop of "That is work to fix. Send
 * the issue back", and the colon of "That is work to fix: send the issue back", which no file
 * carried and which a `[,.]` let through. The clause that replaced it keeps every word up to
 * `goes back` and then says when: "… is work to fix, so the issue goes back on the second such
 * removal of that head", so a send-back followed by `on the second` or `only on the second`
 * passes, and so does the prompt part's "the issue goes back only on the second such removal of
 * that head", which calls nothing work to fix.
 */
const FIRST_EJECTION_GOES_BACK =
  /\bmerge_group`?\s+checks\b.{0,30}?\bis\s+work\s+to\s+fix\b\s*[^\w\s]*\s+(?:so\s+)?(?:the\s+issue\s+goes\s+back|send\s+the\s+issue\s+back)\b(?!\s+(?:only\s+)?on\s+the\s+second\b)/i;

/**
 * The ban on a wait INFRA-1303 retired from the tester's prompt part
 * (packages/daemon/internal/prompts/roles/core/tester.md): "A lane still running is read as
 * running; never wait for it.", which the tester's evidence rule
 * (skills/legion-worker/references/pr-body.md) carried as "A lane GitHub shows `queued` or `in
 * progress` is read as such: the tester never waits for it". Both read the running lane, a read
 * of CI the tester no longer makes (`RETIRED_TESTER_CI_READS`), and the re-run of a red at the
 * head is the reviewer's in `reviewing` and the implementer's while it holds `implementing`, each
 * its own to press and its own to wait on (`skill://legion-worker/references/red-at-the-head.md`),
 * so either of them, told never to wait for a running lane, reads the one wait that page gives it
 * as forbidden.
 *
 * The subject is a lane noun — `lane`, `check`, `suite`, `job` — in a running state — `running`,
 * `queued`, `in progress` — and the ban is `never wait(s) for it` or `them` in the same sentence.
 * The sentences kept beside it keep a wait and so pass: the reviewer's "You wait for none of them
 * but the three `skill://legion-worker/references/red-at-the-head.md` gives you" and "One
 * `queued` or `in_progress`: wait." (go/reviewer.md). So do the implementer's "never wait for CI"
 * and the tester's "wait on no CI", which name no running lane and no `it`.
 */
const NO_WAIT_FOR_A_RUNNING_LANE = new RegExp(
  String.raw`\b${LANE}s?\b[^.]{0,60}?\b(?:running|queued|in[\s_]progress)\b[^.]{0,60}?\bnever\s+waits?\s+for\s+(?:it|them)\b`,
  "i"
);

/**
 * The grant INFRA-1303 retired from every text that offers the re-run as one choice among others to
 * whoever was told of a red: the architect's prompt part's "re-run it, wait for it, or have the
 * implementer fix the code", the worker prompt part's "whether it is re-run, waited for or fixed is
 * a decision for the roles it reached" (packages/daemon/internal/prompts/go/worker-common.md), the
 * troubleshooting page's "Re-run a check that failed on itself, have the implementer fix code that
 * is at fault, or leave it to GitHub", and the concepts page's "re-run a check that failed on
 * itself, fix code that is at fault, or wait". Each reached a role that holds no phase, the merger
 * among them, and a re-run that role presses spends the head's one re-run
 * (`skill://legion-worker/references/red-at-the-head.md`). Two rounds of a case-sensitive grep for
 * the wording missed two of them, so the shape is pinned, not the words.
 *
 * The shape is a list of alternatives that starts with the re-run: the bare word `re-run`, a comma
 * within 40 characters, a second choice (`have`, `fix`, `wait`, `leave`), then `or` and a third.
 * `\bre-?run\b` does not match `re-runs`, which is what lets the sentence that replaced them pass:
 * "either re-runs a check that failed on itself once at that head, the tester, the merger and an
 * implementer that runs retro re-run nothing, the implementer fixes code that is at fault, and a
 * check still running is waited for" (concepts.md), whose bare `re-run nothing,` has no choice
 * after its comma.
 */
const RERUN_AMONG_THE_CHOICES =
  /\bre-?run\b[^,.;]{0,40},\s+(?:have|fix|wait|leave)\w*\b[^.;]{0,60}?,?\s+or\s+(?:have|fix|wait|leave)\w*\b/i;

/**
 * The same grant written as a decision handed to every role the daemon told: worker-common.md's "a
 * decision for the roles it reached", merge-gate.md's "leaves the decision to the roles it told",
 * and the concepts page's "A red is for those roles to decide on". The daemon tells the architect,
 * the phase's worker and the implementer, and only one of them holds the phase.
 *
 * The plural `roles` and the told or reached roles are what it reads, so the sentence that
 * replaced the concepts page's passes: "A red is for the reviewer in `reviewing` or the
 * implementer while it holds `implementing` to act on", and so does "the daemon names the checks
 * GitHub reported and judges none of them".
 */
const DECISION_FOR_THE_ROLES_TOLD =
  /\b(?:decision|decide)\b[^.]{0,20}?\b(?:for|to)\s+the\s+roles\s+(?:it|they)\s+(?:told|reached)\b|\bfor\s+(?:the|those)\s+roles\b[^.]{0,30}?\bto\s+decide\b/i;

/**
 * The grant of the reviewer's re-run of a check the base does not require, for the approval. The
 * reviewer's Go overlay (packages/daemon/internal/prompts/go/reviewer.md) and the page gave the
 * reviewer the re-run of any red the change did not make, while main's merge gate
 * (skills/legion-worker/references/merge-gate.md, cybermonklabs/legion#4211) says "A check the
 * base does not require holds no approval. You neither wait for it nor re-run it for the
 * approval". The architect kept main's rule for such a check and the reviewer's one re-run for a
 * check the base requires, so a text that has the reviewer re-run the first kind before it
 * approves holds the approval on a check that holds nothing.
 *
 * One sentence, read up to its full stop, holds all three in any order: a check the base does not
 * require (`check(s) (that) the base (branch) does not require`); a re-run (`re-run`, `re-runs`,
 * `rerun`) that no `nor`, `not`, `never`, `no` or `neither` just before it negates and that is no
 * `re-run(s) nothing` or `none`; and the approval (`approv…`). So it refuses "Before you approve,
 * re-run a check the base does not require once." and "A red on a check the base does not require
 * is yours to re-run once before you approve." It lets through main's two sentences, which put the
 * check and the re-run in different sentences; the page's "The reviewer neither waits for a check
 * the base does not require nor re-runs it for the approval" and the overlay's "A check the base
 * does not require you neither wait for nor re-run for the approval", whose re-run a `nor` negates;
 * the overlay's "a red the change did not cause, on a check the base requires, is yours to re-run
 * once", whose check is a required one; and "The implementer re-runs a red on a check the base
 * does not require once.", which holds no approval, since the implementer keeps that re-run.
 * The first lookahead puts the hit where one of the three words starts, so a hit names the line
 * of the sentence's first such word. The negation is a lookbehind, which Bun's JavaScriptCore runs.
 */
const UNREQUIRED_CHECK_RERUN_FOR_THE_APPROVAL =
  /\b(?=checks?\b|approv|re-?runs?\b)(?=[^.]*?\bchecks?\s+(?:that\s+)?the\s+base(?:\s+branch)?\s+does\s+not\s+require\b)(?=[^.]*?\bapprov)(?=[^.]*?(?<!\b(?:nor|not|never|no|neither)\s+)\bre-?runs?\b(?!\s+(?:nothing|none)\b))/i;

/**
 * Main's rule turned on a required check: "You neither wait for a required check nor re-run it for
 * the approval." Main's sentence bans the wait and the re-run for a check the base does not
 * require, whose red holds no approval. A required check's red holds the merge, so the reviewer
 * waits for one still running and re-runs once a red of one the change did not make
 * (`skill://legion-worker/references/red-at-the-head.md`), and a reviewer told to do neither
 * costs the tree a whole lap for a failure its code never caused.
 *
 * The shape is main's own: `neither wait(s) for`, a determiner (`a`, `an`, `any`, `the`),
 * `required` and a lane noun (`check`, `lane`, `job`, `suite`), then `nor re-run` before the
 * sentence's full stop. Main's sentence passes, since its object is `it`, and so does the page's
 * "The reviewer neither waits for a check the base does not require nor re-runs it for the
 * approval", whose check is no `required` one.
 */
const REQUIRED_CHECK_NEITHER_WAITED_FOR_NOR_RERUN = new RegExp(
  String.raw`\bneither\s+waits?\s+for\s+(?:a|an|any|the)\s+required\s+${LANE}s?\b[^.]*?\bnor\s+re-?runs?\b`,
  "i"
);

/** The ten rules INFRA-1303 retired, in the order they are written above. */
const RETIRED_RERUN_PROHIBITIONS = [
  NO_LANE_IS_RERUN,
  RERUN_NONE_OF_THEM_YOURSELF,
  REVIEWER_RERUNS_NOTHING,
  ONLY_THE_IMPLEMENTER_WAITS,
  FIRST_EJECTION_GOES_BACK,
  NO_WAIT_FOR_A_RUNNING_LANE,
  RERUN_AMONG_THE_CHOICES,
  DECISION_FOR_THE_ROLES_TOLD,
  UNREQUIRED_CHECK_RERUN_FOR_THE_APPROVAL,
  REQUIRED_CHECK_NEITHER_WAITED_FOR_NOR_RERUN,
];

/**
 * Every place in the Markdown files under `roots` that still carries a rule INFRA-1303 retired,
 * each as one `<file>:<line>`: a sentence that forbids the re-run it turned on or the wait on that
 * re-run, gives the wait to the wrong role, sends the work back on the first `failed_checks`
 * ejection the merger now queues once more, offers the re-run to every role told of a red, gives
 * the reviewer a re-run of a check the base does not require before it approves, or turns main's
 * ban on that wait and that re-run onto a required check. They were deleted from skill files and
 * daemon prompt parts at once, and a forward merge keeps both sides of a deletion in silence, so
 * the rule is written over whole partitions rather than over the files it cleared: another file
 * that acquires one fails here too.
 */
export function retiredRerunProhibitions(roots: readonly string[]): string[] {
  return retiredRuleHits(roots, RETIRED_RERUN_PROHIBITIONS);
}

/*
 * The tester's read of CI, which INFRA-1303 retired on Sami's decision
 * (dispatch://INFRA-1303/ask/dd2229bf-075c-4524-b045-41f39978dbb3: "let CI do its job, your job is
 * to boot and run the app's real workflows like a user would"). The tester reads no check run,
 * re-runs no CI and waits on no CI, and a red at the head ends no round of its; the re-run is the
 * reviewer's in `reviewing` and the implementer's while it holds `implementing`
 * (`skill://legion-worker/references/red-at-the-head.md`). Each pattern below is one shape of the
 * tester's retired sentences. A tester that reads one again fails its round on a red the change
 * never made, which is the lap INFRA-1303 is about.
 */

/**
 * The read of the static lanes: main's tester prompt part's "read CI for everything static — lint,
 * types, the unit and integration suites and every other lane are CI's work, read on GitHub at the
 * head you tested", the concepts page's "reads CI for everything static", and the pull-request
 * body reference's "Lint, types and the unit and integration suites are CI's, read on GitHub at the
 * head" (main's "are CI's: read them on GitHub at the head"), and its earlier "are CI's, read there
 * and recorded for no later step". Two shapes: `read(s) CI for everything static`, and the lanes
 * called CI's (`CI's`, `CI's work`) with a comma or a colon and then a read of the lanes
 * themselves: `read them`, or a `read` whose place is where the tester made it, `read on GitHub`,
 * `read there` or `read at the head`. The object is what the pattern reads, so a sentence that
 * sends its reader to one job's log passes: "Formatting is CI's: read the `fmt` job's log to see
 * what it changed." and "Lint is CI's work, read the job log on GitHub when it fails." A reword
 * that drops the place is still refused: "… are CI's: read them at the head you tested." The
 * sentences that keep a `ci_check` check CI's pass, since no such read follows: "A check carrying
 * `ci_check` is CI's: that read shows its one derived result" (the tester's role part) and "a
 * check carrying `ci_check` is CI's, and its result …" (its core). So do the sentences that
 * replaced the read: "every other lane are CI's work, and you do not run in your own pod what CI
 * runs for you" (the core) and "are CI's: the tester lets CI do its job" (pr-body.md).
 */
const TESTER_READS_THE_STATIC_LANES =
  /\breads?\s+CI\s+for\s+everything\s+static\b|\bCI's(?:\s+work)?[,:]\s+read\s+(?:them\b|(?:on\s+GitHub|there|at\s+the\s+head)\b)/i;

/**
 * The read of the head's checks before a verdict: the tester's prompt part's "Read the head's
 * checks on GitHub before any verdict, whether or not a check of the issue carries `ci_check`",
 * and the pull-request body reference's "The tester reads the head's checks on GitHub before any
 * verdict". The subject is the tester: by name (`the tester reads`), as `you read`, or as the
 * imperative `Read` that opens a sentence or a list item, which is the prompt part's own shape.
 * The opening is read through a lookbehind, so a hit names the line of the `Read`. The object is
 * the head's checks, a lane noun or its runs, with the word a lane noun may carry before it, so
 * "The tester reads the head's CI checks before any verdict." is refused too. A verdict is the
 * tester's word, and another role's read passes: "The reviewer reads the head's checks on GitHub
 * itself before it approves" (concepts.md) and "The reviewer reads the head's checks on GitHub
 * before any verdict it gives."
 */
const HEADS_CHECKS_BEFORE_ANY_VERDICT = new RegExp(
  String.raw`(?:\bthe\s+tester\s+reads|\byou\s+read|(?<=(?:^|[.!?:;]\s|[-*]\s))read)\s+the\s+head's\s+${LANE_QUALIFIER}(?:${LANE}s|runs)\b[^.]{0,30}?\bbefore\s+any\s+verdict\b`,
  "i"
);

/**
 * The phase's holder, named by the phase alone: `the role that holds the issue's phase`, `the role
 * holding the issue's phase` or `the holder of the issue's phase`, which the tester read as itself
 * while it held `testing`.
 */
const PHASE_HOLDER = String.raw`(?:role\s+(?:that\s+holds|holding)|holder\s+of)\s+the\s+issue's\s+phase\b`;

/**
 * What a holder re-runs or presses when it is the retired re-run: a red, a lane noun, a run, a
 * workflow, CI or the re-run itself, behind an optional determiner and an optional word of its
 * state (`the failed jobs`, `a red check`, `its cancelled run`), or `gh run rerun` as the command.
 */
const RERUN_OBJECT = String.raw`(?:\`?gh\s+run\s+rerun\b|(?:(?:a|an|the|each|every|any|that|this|one|its)\s+)?(?:(?:red|failed|failing|cancelled|required)\s+)?${LANE_QUALIFIER}(?:red|${LANE}s?|runs?|workflows?|re-?runs?|CI)\b)`;

/**
 * The tester as a holder of the re-run. Four shapes: the three-holder list that opened with it,
 * "the tester in `testing`, the reviewer in `reviewing` …" (the red-at-the-head page, the worker
 * prompt part, the concepts and troubleshooting pages); the pull-request body reference's "is the
 * tester's to re-run once"; the page's "The tester and the implementer still re-run such a red
 * once"; and the phase's holder (`PHASE_HOLDER`) as the one who re-runs. That holder is either the
 * subject of `re-run(s)` or `presses` with a re-run's object (`RERUN_OBJECT`, or `it` or `them`
 * after `re-runs`) — the tester's prompt part's "The role that holds the issue's phase re-runs a
 * red at the head", the page's "The holder of the issue's phase presses the re-run" and the
 * architect's prompt part's "the role that holds the issue's phase re-runs a red the change did
 * not make once" — or the agent of a passive re-run (`is re-run`, `gets re-run`, `be re-run`)
 * earlier in its sentence, pr-body.md's "is re-run once with `gh run rerun <run-id> --failed`, by
 * the role that holds the issue's phase". The agent is the first `by` after that re-run, with no
 * other auxiliary between, and it is read through a lookbehind, so a hit names the line of the
 * holder. So a holder that presses or re-runs something else passes: "The role that holds the
 * issue's phase presses Enter in its pane to accept the prompt." and "The role that holds the
 * issue's phase reruns its last command after a restart." So does a holder that is the object or
 * the agent of another verb, though a re-run stands in its sentence: "After a restart the daemon
 * starts the role that holds the issue's phase again, and that role reruns its last command.", "A
 * pod restarted by the daemon re-runs its setup, and is then taken by the role that holds the
 * issue's phase.", "The daemon reruns the reminder, which is read by the role that holds the
 * issue's phase." and "A job is re-run by the daemon, and its log is read by the role that holds
 * the issue's phase." The cost is a passive re-run whose agent stands past a second auxiliary,
 * which passes. The sentences that replaced them name the two holders and pass: "The holders of
 * the re-run are the reviewer in `reviewing` and the implementer while it holds `implementing`."
 * and "The tester presses nothing" (the page, the worker prompt part). So does the worker prompt
 * part's "One role holds the issue's phase at a time", which names no holder of a re-run.
 */
const THE_TESTER_HOLDS_THE_RERUN = new RegExp(
  [
    String.raw`\btester\s+in\s+\`?testing\`?\s*,\s+the\s+reviewer\b`,
    String.raw`\bthe\s+tester's\s+to\s+re-?run\b`,
    String.raw`\bthe\s+tester\s+and\s+the\s+implementer\s+still\s+re-?run\b`,
    String.raw`\b${PHASE_HOLDER}\s+(?:re-?runs?\s+(?:it\b|them\b|${RERUN_OBJECT})|presses\s+${RERUN_OBJECT})`,
    String.raw`(?<=\b(?:is|are|was|were|be|been|being|gets?|got)\s+re-?run\b(?:(?!\b(?:by|is|are|was|were|be|been|being|gets?|got)\b)[^.])*\bby\s+the\s+)${PHASE_HOLDER}`,
  ].join("|"),
  "i"
);

/**
 * The tester's waits on CI: the tester's prompt part's "you wait for none of them but three: the
 * run that holds a red, until it ends; that re-run; and a red the counted set does not yet report
 * `completed`, however you learned of it", and its earlier "you wait for none of them but that
 * re-run"; the pull-request body reference's "The tester waits on CI for nothing but what that
 * page gives it", its earlier "the tester waits for none of them but the re-run", and its "however
 * the tester learned of it". What marks the tester's wait as one on CI is `on CI`, or `none of
 * them`, the lanes the sentence names: a wait `on CI for nothing`, `for nothing on CI` or `for none
 * of them` is read with the `but` of its exceptions, with no comma, colon or semicolon between,
 * and any other wait `on CI` is refused whatever it waits for ("The tester waits on CI only for the
 * re-run that page gives it."), save one for nothing (`for nothing`, `for none`, `for no …`). So a
 * sentence that has the tester wait for something else passes: "The tester waits for the app to
 * boot, but reads no CI.", "The tester waits until the deploy finishes, but never on CI.", "The
 * tester waits for nothing but the app's own boot." and "The tester waits on CI for nothing, but it
 * waits for the app to boot." The reviewer's wait passes, since its list is "the three" the page
 * gives it: "You wait for none of them but the three
 * `skill://legion-worker/references/red-at-the-head.md` gives you" (go/reviewer.md). So does the
 * tester's "wait on no CI".
 */
const THE_TESTER_WAITS_ON_CI =
  /\bthe\s+tester\s+waits?\s+(?:on\s+CI\s+for\s+(?:nothing|none)|for\s+(?:nothing|none)\s+on\s+CI|for\s+none\s+of\s+them)\b[^.,:;]{0,20}?\bbut\b|\bthe\s+tester\s+waits?\s+on\s+CI\b(?!\s+for\s+(?:nothing|none|no)\b)|\byou\s+wait\s+for\s+none\s+of\s+them\s+but\s+(?:three|that\s+re-?run)\b|\bhowever\s+the\s+tester\s+learned\b/i;

/**
 * The tester's verdict on a check CI proves: the worker skill's `ci_check` reference's "A
 * `ci_check` check goes in `failures` only when its derived result is a fail, or its run concluded
 * `skipped`, `neutral` or `cancelled`", a line of the tester's. A derived result is CI's, and the
 * reviewer reads it before it approves (the architect, 2026-11-20), so a tester that puts one in
 * its `failures` fails its round on CI. The subject is a `ci_check` check and the verb `goes in
 * failures`, so the sentence that replaced it passes: "It neither reads nor records a `ci_check`
 * check's derived result, which is CI's".
 */
const CI_CHECK_IN_THE_TESTERS_FAILURES = /\bci_check`?\s+check\s+goes\s+in\s+`?failures\b/i;

/** The tester's own record: a `test` handoff or record, `--phase test` or `phase: "test"`. */
const TEST_RECORD = String.raw`(?:\btest\`?\s+(?:handoff|record)\b|--phase\s+test\b|\bphase:\s*"test")`;

/** The field `redAtTheHead`, unless a `no`, `never` or `not` just before it says a record lacks it. */
const RED_AT_THE_HEAD_FIELD = String.raw`(?<!\b(?:no|never|not)\s+\`?)\bredAtTheHead\b`;

/**
 * The tester's record of a red at the head. Three shapes: `dispatch handoff-write`'s phase list
 * holding `test` in the sentence of the re-run or its record, the page's "written with `dispatch
 * handoff-write --issue <issue> --phase <implement|test|review> --body-file <file>` before the `gh
 * run rerun` call" until 0ac2891a; the tester's record (`TEST_RECORD`) in one sentence with
 * `redAtTheHead` (`RED_AT_THE_HEAD_FIELD`), in either order; and the tester as the subject that
 * records the red, "the tester records the red" or "the tester's `redAtTheHead`". The red it
 * records is the noun or a red lane: `red` followed by a stop, a lane noun or a word that follows
 * a noun (`at`, `in`, `that`, `the` …), so a red that names something else passes: "The tester
 * notes the red banner the app shows when the login fails." The tester reads no CI, so no red at
 * the head is in its record, and a text that puts one there brings back the read the record is
 * made from. The records that stay pass: the page's `--phase <implement|review>` before the `gh
 * run rerun` call, the reviewer's "names the red in its `redAtTheHead` record and its review
 * body", the handoff reads' `--phase <plan|implement|test|review>` (the worker skill) and
 * `--phase test --version <n>` (the implementer's role part), the tester's own `test` handoff and
 * its `failures`, which name no `redAtTheHead`, a sentence that says the tester's record carries no
 * `redAtTheHead`, and "The tester writes the red test for a way no test would catch".
 */
const THE_TESTER_RECORDS_A_RED = new RegExp(
  [
    String.raw`\bhandoff-write\b[^.]{0,60}?--phase\s+<[a-z|]*\btest\b[a-z|]*>(?=[^.]*?\b(?:re-?run|redAtTheHead)\b)`,
    `${TEST_RECORD}(?=[^.]*?${RED_AT_THE_HEAD_FIELD})`,
    `${RED_AT_THE_HEAD_FIELD}(?=[^.]*?${TEST_RECORD})`,
    String.raw`\bthe\s+tester\s+(?:records?|writes?(?:\s+down)?|notes?|names?)\s+(?:the|a|each|that|this)\s+red\b(?=\s*[^\w\s]|\s*$|\s+(?:(?:CI\s+)?${LANE}s?|at|in|on|of|from|to|for|with|as|and|or|before|after|when|while|that|which|whose|it|its|the|this)\b)`,
    String.raw`\bthe\s+tester's\s+\`?redAtTheHead\b`,
  ].join("|"),
  "i"
);

/** The tester's six retired shapes, in the order they are written above. */
const RETIRED_TESTER_CI_READS = [
  TESTER_READS_THE_STATIC_LANES,
  HEADS_CHECKS_BEFORE_ANY_VERDICT,
  THE_TESTER_HOLDS_THE_RERUN,
  THE_TESTER_WAITS_ON_CI,
  CI_CHECK_IN_THE_TESTERS_FAILURES,
  THE_TESTER_RECORDS_A_RED,
];

/**
 * Every place in the Markdown files under `roots` that still tells the tester to read CI, gives it
 * the re-run of a red at the head, has it wait on CI, has it judge a check CI proves, or has it
 * record a red at the head, each as one `<file>:<line>`. The sentences were deleted from skill
 * files, daemon prompt parts and docs pages at once, and a forward merge keeps both sides of a
 * deletion in silence, so this rule too is written over whole partitions.
 */
export function retiredTesterCiReads(roots: readonly string[]): string[] {
  return retiredRuleHits(roots, RETIRED_TESTER_CI_READS);
}

/**
 * Every place in the Markdown files under `roots` that one of `rules` matches, each as one
 * `<file>:<line>`.
 *
 * Each rule is matched against the whitespace-collapsed text (`collapsed`), the collapse the prose
 * pins read too, because these files are wrapped and a retired sentence can cross a line wherever
 * it stands. The hit's offset is mapped back through `collapsedOffsets`, whose per-character map
 * is built once for a file that holds a hit and not at all for any other, to the line a reader
 * opens: the line where the sentence's first matched word stands. One hit per rule per file: the
 * patterns carry no `g` flag, so a file that says it twice is named once and reads the same after
 * the first is fixed.
 */
function retiredRuleHits(roots: readonly string[], rules: readonly RegExp[]): string[] {
  return roots.flatMap((root) =>
    files(root)
      .filter((file) => file.endsWith(".md"))
      .flatMap((file) => {
        const markdown = readFileSync(file, "utf8");
        const text = collapsed(markdown);
        // A hit under the repository reads from its root (`skills/…`, `packages/daemon/…`); a hit
        // in a staged partition reads as that partition has it
        // (`legion-worker/references/pr-body.md`), since its root is a temporary directory.
        const name = file.startsWith(`${REPO_ROOT}${path.sep}`)
          ? path.relative(REPO_ROOT, file)
          : path.relative(root, file);
        let offsets: number[] | undefined;
        return rules.flatMap((rule) => {
          const found = text.match(rule);
          if (found?.index === undefined) return [];
          offsets ??= collapsedOffsets(markdown);
          return [`${name}:${markdown.slice(0, offsets[found.index]).split("\n").length}`];
        });
      })
  );
}

/** The worker skill, the one file the release rule below is written over. */
export const WORKER_SKILL = path.join(REPO_ROOT, "skills/legion-worker/SKILL.md");

/** The worker skill's tool reference, the one file the shared-database rules are written over. */
export const WORKER_TOOLS = path.join(REPO_ROOT, "skills/legion-worker/references/tools.md");

/** The page that holds the re-run rule, the one file that rule is pinned in. */
export const RED_AT_THE_HEAD = path.join(
  REPO_ROOT,
  "skills/legion-worker/references/red-at-the-head.md"
);

/**
 * The merge gate, which carries the reviewer's one pointer to that page. The merger's merge-queue
 * rule and the reviewer's round order are in their role prompts since cybermonklabs/legion#4447
 * ("The merger's steps are all in its role prompt"), and
 * `packages/daemon/internal/prompts/roles_test.go` pins them there.
 */
export const MERGE_GATE = path.join(REPO_ROOT, "skills/legion-worker/references/merge-gate.md");

/** The pull-request body reference, which points at that page. */
export const PR_BODY = path.join(REPO_ROOT, "skills/legion-worker/references/pr-body.md");

/** The worker skill's reference for a check CI proves, which says what the tester does with one. */
export const CI_CHECKS = path.join(REPO_ROOT, "skills/legion-worker/references/ci-checks.md");

/** The controller's reference for a finding, which holds its census of a test a role re-ran green. */
export const FINDING_ASKS = path.join(
  REPO_ROOT,
  "skills/legion-controller/references/finding-asks.md"
);

/**
 * `markdown` with each run of whitespace made one space. The rules below read a skill file this
 * way, because these files are wrapped and a sentence of one crosses lines.
 */
function collapsed(markdown: string): string {
  return markdown.replace(/\s+/g, " ");
}

/**
 * What one skill file gets wrong by still carrying a claim a rule over the repository's own
 * `skills/` retired: `retired.claim` is a pattern rather than its one wording, so a reword of the
 * same shape is caught too. The fault is one sentence naming `file`, quoting what matched and
 * saying what a reader of the wrong text would believe.
 */
function retiredClaimFaults(
  file: string,
  markdown: string,
  retired: { claim: RegExp; because: string }
): string[] {
  const found = collapsed(markdown).match(retired.claim);
  return found === null ? [] : [`${file} says "${found[0]}": ${retired.because}`];
}

/**
 * What one skill file gets wrong by having lost a sentence a rule over the repository's own
 * `skills/` turns on: each of `required` is pinned whole and literally. Each fault is one sentence
 * naming `file` and saying what a reader of the text without it would believe.
 */
function missingSentenceFaults(
  file: string,
  markdown: string,
  required: readonly { sentence: string; because: string }[]
): string[] {
  const text = collapsed(markdown);
  return required
    .filter(({ sentence }) => !text.includes(sentence))
    .map(({ sentence, because }) => `${file} does not say "${sentence}": ${because}`);
}

/**
 * The claim the idle release retired (INFRA-945): that a role, or the pane it runs in, stays live
 * for as long as its issue is in the workflow. Under `runtime: kubernetes` the daemon now releases
 * an issue pod that has had no turn in progress and no turn due for the idle period, which ends
 * every role process of it and deletes the pod, its disk and its Sandbox. What outlives that is
 * the claim's session, its phase and the task it holds, which a wake resumes — so a sentence
 * saying the *session* is kept is the rule as it now is, and one saying the *role* stays live is
 * what this forbids. The subject is listed rather than inferred so the two read apart: "your
 * session is kept, until your issue leaves the workflow, to answer questions" passes, and main's
 * "you stay live only to answer questions" does not.
 */
const ROLE_STAYS_LIVE =
  /\b(?:you|your (?:role|pane|process|pod|agent)|the (?:role|worker|pane|process))\s+(?:stays?|remains?|keeps?)\s+(?:live|alive|running|up)\b/i;

/**
 * The sentence the skill must carry instead, which is the heading of the bullet that tells a
 * worker what a release does to it. It is pinned whole and literally: a worker that reads nothing
 * about release reads its pod's deletion as a crash and its restored workspace as lost work.
 */
const RELEASE_IS_NO_STOP = "A released pod is not a stop.";

/** What `markdown` — skills/legion-worker/SKILL.md — gets wrong about a released pod. */
export function retiredWorkerLifecycleClaims(markdown: string): string[] {
  const file = "skills/legion-worker/SKILL.md";
  return [
    ...retiredClaimFaults(file, markdown, {
      claim: ROLE_STAYS_LIVE,
      because:
        "a released pod ends every role process of it, so no role stays live for as long as its issue is in the workflow",
    }),
    ...missingSentenceFaults(file, markdown, [
      {
        sentence: RELEASE_IS_NO_STOP,
        because: "a worker told nothing about release reads its pod's deletion as a crash",
      },
    ]),
  ];
}

/**
 * The claim INFRA-1315 retired: that a login, or the URL that carries one, can create and drop
 * databases on the shared server. The skill ships to every deployment and cannot know what a
 * deployment's session login may do, so it claims no power for it: a deployment that splits that
 * login gives its roles one that owns nothing, and a role that read the old claim there would
 * believe a power it does not have. The claim is a pattern rather than its one wording, and the
 * subject is named rather than left open, so the file's own "Each makes and drops a database of
 * its own per test", said of the pod's server, stays outside it; the rule the file does carry,
 * that nothing which creates or drops a database gets the session URL, names no login that can
 * and so passes too.
 */
const LOGIN_OWNS_DATABASES =
  /\b(?:login|account|credentials?|URL|DSN)\b[^.]{0,40}?\b(?:can|could|may|is able to|is allowed to|has (?:the )?(?:rights?|permissions?|privileges?) to)\s+(?:create|make|drop|delete)\b[^.]{0,40}?\bdatabases?\b/i;

/**
 * The sentence of `## Reading a shared database` a role must not lose, pinned whole and literally:
 * a role that reads nothing about it reads the shared store through the write-capable session URL,
 * which on 2026-11-05 left a session-level read-only setting on a pooled backend and failed 1,432
 * session writes.
 */
const NO_READ_THROUGH_SESSION_URL =
  "Never read it through the session URL the file `OMP_SESSION_SQL_DSN_FILE` names.";

/** What `markdown` — skills/legion-worker/references/tools.md — gets wrong about a shared database. */
export function brokenSharedDatabaseRules(markdown: string): string[] {
  const file = "skills/legion-worker/references/tools.md";
  return [
    ...retiredClaimFaults(file, markdown, {
      claim: LOGIN_OWNS_DATABASES,
      because:
        "the skill cannot know what a deployment's session login may do, so it claims no power for that login",
    }),
    ...missingSentenceFaults(file, markdown, [
      {
        sentence: NO_READ_THROUGH_SESSION_URL,
        because:
          "a role told nothing about reads reaches the shared store through the write-capable session URL",
      },
    ]),
  ];
}

/**
 * The sentence of `## Who may re-run` that carries INFRA-1303's rule, pinned whole and literally:
 * the reviewer in `reviewing` and the implementer while it holds `implementing`, since the tester
 * reads no CI (Sami's decision, dispatch://INFRA-1303/ask/dd2229bf-075c-4524-b045-41f39978dbb3).
 * A reader who lost it would believe the re-run belongs to whoever notices the red: the tester,
 * the merger, the architect in `accepting`, an implementer in retro, or a role merely told of a
 * red in a phase it does not hold — each of which the sentences beside it refuse. The grant is
 * what the page exists for; the sentences that deny the re-run read as a list of exceptions to
 * nothing once it is gone.
 */
const TWO_HOLDERS_PRESS_THE_RERUN =
  "The holders of the re-run are the reviewer in `reviewing` and the implementer while it holds `implementing`.";

/**
 * The sentence of `## Who may re-run` that keeps the tester out of it, pinned whole and literally.
 * The daemon names every check of a settlement to the worker of the issue's current phase, the
 * tester in `testing` among them, so a tester that lost this sentence and read the page would
 * take a red it was told of for one it must press or fail its round on.
 */
const TESTER_PRESSES_NOTHING =
  "The tester presses nothing: it reads no CI, and a red at the head does not end its round.";

/**
 * The sentence of `## Who may re-run` that limits the reviewer's re-run, pinned whole and
 * literally. The grant above gives the reviewer in `reviewing` the re-run of a red, and the merge
 * gate's "A check the base does not require holds no approval" (cybermonklabs/legion#4211), a rule
 * the architect kept, takes a check the base does not require out of it. A reviewer that lost
 * this sentence reads the grant as a re-run of every red the counted set carries, and holds its
 * approval on a check that holds none.
 */
const REVIEWER_LEAVES_AN_UNREQUIRED_CHECK =
  "The reviewer neither waits for a check the base does not require nor re-runs it for the approval, as `skill://legion-worker/references/merge-gate.md` says.";

/**
 * The sentence of `## One re-run belongs to the head` that tells an unreported check from an
 * absent one, pinned whole and literally. The reviewer's existing re-run, main's, covers a
 * required check that is cancelled or absent, absent meaning that a run at the head holds its job
 * while the check is missing from the list; where no run at the head holds the job, the reviewer
 * tells the architect and does not approve. A check whose failed job a run at the head holds while
 * the counted set does not list it yet is unreported, and is waited for. A reader that lost the
 * sentence takes a failed job it read off the run for an absent check, and re-runs it before
 * GitHub counts it (the architect's rulings of 2026-11-20 on INFRA-1303's review round 3).
 */
const UNREPORTED_IS_NOT_ABSENT =
  "A check the counted set does not list while a run at the head holds its failed job is unreported, not absent: it gets no re-run and is waited for, as *The act and the wait* says.";

/**
 * The sentence of `## One re-run belongs to the head` that keeps `--required` off the counted set,
 * pinned whole and literally (the architect's gap review of 2026-11-20, item 7). `gh pr checks
 * --required` prints the checks the base requires and no other (the docs page
 * checks-and-re-runs.md, measured at `ed157dc9`), so a role that lost this sentence and read the
 * counted set with it sees no lane under `ci-ok` and no check the base does not require, and on a
 * base that requires nothing takes its exit 1 for a failed read. The scorer reads such a read as no
 * read of the counted set (score.ts, COUNTED_READ).
 */
const COUNTED_SET_IS_NEVER_REQUIRED =
  "Never with `--required`, which prints the checks the base requires and no other, so a lane under an aggregate such as `ci-ok` and a check the base does not require are never in it, and on a base that requires nothing it prints that no required checks are reported and exits 1.";

/**
 * The exit statuses of the ancestry read, pinned literally. The sentence read "1 is not carried
 * and anything else is cannot compare" until INFRA-1303's review round 3, so exit 0, carried, fell
 * under "cannot compare" and a role following it never concluded the base carries the fix.
 */
const ANCESTRY_EXIT_STATUSES = "where 0 is carried, 1 is not, and anything else is cannot compare";

/**
 * Three sentences of the page made true while the spec was redrafted to match the head
 * (INFRA-1303's architect, 2026-11-20), each pinned literally:
 *
 * - `REVIEW_ENDS_ITS_ROUND`: why the reviewer repeats its record in its review. It read "since a
 *   review cannot be written before the re-run", which is false: GitHub takes a review at any time.
 * - `SPENT_IS_SAID`: a role whose head's re-run is spent says so when it ends the round, the words
 *   world `e`'s scorer reads (score.ts, RED_SPENT).
 * - `NULL_ROLLUP_WAITS_FIRST`: a `null` rollup read first in a turn is an absence to wait on, as
 *   every other first read is, and only the read on the wake after it is GitHub counting nothing.
 */
const REVIEW_ENDS_ITS_ROUND = "since the reviewer's one review ends its round";
const SPENT_IS_SAID = "names the check, and says the head's one re-run is spent";
const NULL_ROLLUP_WAITS_FIRST =
  "read first in a turn, is waited on like any other absence, as *The act and the wait* says";
/**
 * When the daemon's backstop comes. The page said "about an hour after it went idle", but the
 * daemon releases an idle pod after 10 minutes and the backstop wakes an open phase's worker an
 * hour after the release (packages/daemon/internal/config/kubernetes.go; go/worker-common.md,
 * "60 minutes after the release").
 */
const BACKSTOP_AFTER_THE_RELEASE =
  "the bound is the daemon's backstop about an hour after the release";
/**
 * A green re-run of a red in no test owes the record and no census step, the architect's decision
 * of 2026-11-20: the census is one issue per test, and a download's or a runner's fault belongs to
 * the tree that owns that step. World `c`, whose red is a setup step and in no test, already owes
 * no step there (`disposal: false` in red_world_table).
 */
const NO_TEST_OWES_NO_CENSUS =
  "A green re-run of a red in no test, such as a download that answered 5xx or a runner that ended, owes the record before the press and none of these three steps";
/**
 * A green re-run whose log read failed: the role knows the job and not the test's name, so it
 * makes no search and posts no comment, and its message to the controller names the job and says
 * the log could not be read (the architect, 2026-11-20). World `h`'s red is such a test: a test
 * whose attempt-1 log is past retention (`log_gone`), and it owes no search or comment there
 * (`disposal: false`).
 */
const UNREAD_LOG_NAMES_THE_JOB =
  "After a green re-run whose log read failed, the role knows the job but not the test's name: it makes no search, posts no comment, and sends step 3's message to the controller, naming the job and saying the log could not be read.";

/**
 * The sentences INFRA-1303's batch of record at 6a643564 showed a role misreading, each pinned
 * whole and literally in the words the architect ruled on 2026-11-19. A reader of the page that
 * lost one acts as a run of that batch did:
 *
 * - `RECORD_BEFORE_THE_PRESS`: the record is the phase's handoff record, before the press. Two
 *   runs read "It writes the failure down, then re-runs the failed jobs once." as a note and
 *   pressed first, one as `echo "failure recorded: …" && gh run rerun 7001002 --failed`.
 * - `RUN_STATUS_IS_NO_LEAVE` and `A_RED_HOWEVER_LEARNED_WAITS`: a red the counted set does not
 *   report is waited for whatever showed it. Three runs read "presses the re-run only on
 *   `completed`" off the run's own status, saw the check absent from the counted set, and pressed.
 * - `THE_BOUND_IS_THE_REMINDER` and `NO_OTHER_WAKE_READS`: only the settlement's wake reads the
 *   counted set. Three runs read the checks when the thermonuclear pair's results woke them, before
 *   any settlement; one wrote "Now a new turn (thermo wake) — I may read the counted set once".
 * - `NO_SETTLEMENT_IS_BOUNDED` (the architect's ruling of 2026-11-19 19:33Z): the re-run's wait has
 *   the same bound, and a re-run still running at the bound is waited on again. A role that lost
 *   it waits forever for a settlement that never comes, or, with only the bound, judges a lane
 *   that runs past the 15-minute reminder before it ends.
 * - `GREEN_RUNS_THE_DISPOSAL`: a green re-run owes the three steps of *What becomes of the
 *   record* before the phase completes. Two runs approved after green and posted nothing.
 * - `A_FAILING_TEST_IS_NO_EXTERNAL_CAUSE`: a failing test of a package the change does not touch
 *   takes the one re-run like any other red the change did not make. Two runs read such a test's
 *   first red as another tree's defect and requested changes with no re-run. Its second red read
 *   "it is the round's failure" until the architect's gap review of 2026-11-20 (item 3), a
 *   tester's act that no holder of the re-run has; it is handled as *Red again* says.
 */
const RECORD_BEFORE_THE_PRESS =
  "It writes the failure down in a handoff record of its own phase (`dispatch handoff-write`) before the `gh run rerun` call that re-runs the failed jobs once.";
const RUN_STATUS_IS_NO_LEAVE =
  "The run's own `completed` is no leave to press while the counted set does not report that check `completed`.";
const A_RED_HOWEVER_LEARNED_WAITS =
  "**A red the counted set does not yet report `completed` is not yet a red to act on, however the role learned of it.** A task that names a failing check, a notice, another role's word or the role's own read of the runs, the jobs or the check-runs endpoint is not the counted set.";
const THE_BOUND_IS_THE_REMINDER =
  "No wake comes at all: the bound is the phase reminder or the daemon's backstop.";
const NO_OTHER_WAKE_READS =
  "Any other wake, a subagent's result or a message, is no leave to read: the role finishes that work and ends the turn WAITING again.";
const NO_SETTLEMENT_IS_BOUNDED =
  "Where no settlement comes, the bound is the same as before a press: on the phase reminder or the daemon's backstop the role reads the counted set once. A re-run still queued or in progress then is waited on again, with the turn ended WAITING, and a completed one is read as the settlement would be.";
const GREEN_RUNS_THE_DISPOSAL =
  "Green: the round goes on, and the role records that it re-ran, with the run and the attempt. Before it completes its phase, it runs the three steps of *What becomes of the record* below.";
const A_FAILING_TEST_IS_NO_EXTERNAL_CAUSE =
  "A failing test is never the second case, whatever package holds it: one that is not the change's takes the one re-run on its first red, and on its second red it is handled as *Red again* above says, never as an external cause.";

/**
 * The page's retired "fails the round" (the architect's gap review of 2026-11-20, item 3): "It
 * fails the round, or it requests changes" on a spent re-run, "Red again: the round fails, or the
 * reviewer requests changes", "the round fails on it with no re-run" on a red the change made, and
 * "it is the round's failure" on a second red. Failing the round was the tester's verdict, and the
 * tester holds no re-run: on such a red the reviewer requests changes, and the implementer records
 * the red, tells the architect, and completes its phase, and the reviewer judges the red in its
 * round. The class is the verb and the noun both ways, `fails the round`, `the round fails`, `a
 * failed round` and `the round's failure`, whatever the tense or the determiner. The sentences
 * that replaced them name each holder's act, and the page's "the round goes on", "ends no round on
 * it" and "does not end its round" hold no failure, so each passes.
 */
const THE_ROUND_FAILS_ON_A_RED =
  /\b(?:fails?|failed|failing)\s+(?:the|its|a|this)\s+round\b|\b(?:the|its|a|this)\s+round\s+(?:fails|failed)\b|\bfailed\s+round\b|\bround's\s+failure\b/i;

/**
 * The three pointers to the page, each pinned whole and literally: the worker skill's own
 * References line, which is how a role reaches the page at all, and which sends only the two
 * holders there; the merge gate's; and the pull-request body reference's in `## Never waiting on
 * CI`, beside the holders' one re-run. A reference is reached only through a link from somewhere a
 * role reads, so a lost pointer leaves the rule shipped and unread: the role goes on from the text
 * around the gap, which tells it to finish its round on a red the change never made.
 */
const SKILL_LINKS_THE_PAGE =
  "A check red at the head of the pull request, while you hold `reviewing` or `implementing`: `skill://legion-worker/references/red-at-the-head.md`.";
const MERGE_GATE_POINTS_AT_THE_PAGE =
  "A required check that is red at the head is a different case, which `skill://legion-worker/references/red-at-the-head.md` decides.";
const NEVER_WAITING_POINTS_AT_THE_PAGE =
  "`skill://legion-worker/references/red-at-the-head.md` holds the rest of that rule: which red is the change's, which a pass in the role's own pod does not move; that the counted set must report the check before any re-run, however the role learned of the red; the test of whether the head's one re-run is already spent; and the record the role writes before it re-runs.";

/**
 * The same rulings where a role reads them outside the page, each pinned whole and literally: the
 * re-run in `## Never waiting on CI`, whose holders are the page's two and whose wait has the
 * page's bound where no settlement comes; and the tester's evidence bullet, which has the tester
 * let CI do its job in Sami's words (dispatch://INFRA-1303/ask/dd2229bf-075c-4524-b045-41f39978dbb3).
 * The tester's own pins that bullet carried until then — that it reads the head's checks before
 * any verdict, re-runs a red the change did not make, waits on that re-run, and keeps a local pass
 * from moving the page's reading — went with the rule they pinned, and `retiredTesterCiReads`
 * refuses their sentences.
 */
const NEVER_WAITING_NAMES_THE_HOLDERS =
  "by the reviewer in `reviewing` or the implementer while it holds `implementing`, after it records the failure";
const RERUN_WAIT_IS_BOUNDED =
  "(where no settlement comes, the bound is the phase reminder or the daemon's backstop: it reads the counted set once then, and waits again on a re-run still queued or in progress)";
const TESTER_LETS_CI_DO_ITS_JOB =
  "the tester lets CI do its job, boots and runs the app's real workflows like a user would, and reads no check run, re-runs no CI and waits on no CI";

/**
 * What the four files INFRA-1303 wrote or changed get wrong about a red at the head: the worker
 * skill (`WORKER_SKILL`), the page that holds the rule (`RED_AT_THE_HEAD`), the merge gate
 * (`MERGE_GATE`) and the pull-request body reference (`PR_BODY`), each read by its caller. These
 * pins hold the change's positive half; the prohibitions it retired are refused across whole
 * partitions by `retiredRerunProhibitions` and `retiredTesterCiReads`, so the one retired claim
 * here is the page's own, `THE_ROUND_FAILS_ON_A_RED`, a wording only the page carried.
 */
export function redAtTheHeadFaults(pages: {
  workerSkill: string;
  redAtTheHead: string;
  mergeGate: string;
  prBody: string;
}): string[] {
  return [
    ...missingSentenceFaults("skills/legion-worker/SKILL.md", pages.workerSkill, [
      {
        sentence: SKILL_LINKS_THE_PAGE,
        because:
          "a reference the skill does not link is a page no role opens, and a read by path stops at 300 lines; one that sends every phase there sends the tester to a red that is not its own",
      },
    ]),
    ...missingSentenceFaults(
      "skills/legion-worker/references/red-at-the-head.md",
      pages.redAtTheHead,
      [
        {
          sentence: TWO_HOLDERS_PRESS_THE_RERUN,
          because:
            "a page that grants the re-run to nobody leaves every role that reads a red free to press it",
        },
        {
          sentence: TESTER_PRESSES_NOTHING,
          because:
            "a tester told of a red at the head presses it, or fails its round on it, for a red the change never made",
        },
        {
          sentence: REVIEWER_LEAVES_AN_UNREQUIRED_CHECK,
          because:
            "a reviewer reaches the limit on a check the base does not require only through this sentence",
        },
        {
          sentence: REVIEW_ENDS_ITS_ROUND,
          because:
            "a reviewer told a review cannot be written before the re-run reads a rule GitHub does not hold",
        },
        {
          sentence: SPENT_IS_SAID,
          because:
            "a role that ends the round on a spent re-run without saying so leaves the reader no reason",
        },
        {
          sentence: NULL_ROLLUP_WAITS_FIRST,
          because:
            "a role that takes a null rollup on its first read for an empty set re-runs nothing and ends a wait that has not begun",
        },
        {
          sentence: UNREAD_LOG_NAMES_THE_JOB,
          because:
            "a role whose log read failed has no test name to search for, and without this sentence it searches for nothing or invents one",
        },
        {
          sentence: NO_TEST_OWES_NO_CENSUS,
          because:
            "a role that files a census issue for a download's or a runner's fault puts a flaky-test record where no test is",
        },
        {
          sentence: BACKSTOP_AFTER_THE_RELEASE,
          because:
            "a role that counts the backstop from the pod going idle expects its wake fifty minutes early",
        },
        {
          sentence: ANCESTRY_EXIT_STATUSES,
          because:
            "a page that leaves exit 0 under cannot compare has a role fetch and read again forever, never concluding the base carries the fix",
        },
        {
          sentence: UNREPORTED_IS_NOT_ABSENT,
          because:
            "a reviewer that takes an unreported check for an absent one re-runs a failed job the counted set does not report",
        },
        {
          sentence: COUNTED_SET_IS_NEVER_REQUIRED,
          because:
            "a role that reads the counted set with `--required` sees no lane under `ci-ok` and no check the base does not require, and takes a base that requires nothing for a failed read",
        },
        {
          sentence: RECORD_BEFORE_THE_PRESS,
          because:
            "a role that reads the record as a note presses the re-run before Dispatch holds the failure",
        },
        {
          sentence: RUN_STATUS_IS_NO_LEAVE,
          because:
            "a role that reads the run's own `completed` as leave presses while the counted set does not report the check",
        },
        {
          sentence: A_RED_HOWEVER_LEARNED_WAITS,
          because:
            "a role that learned of the red from its own read of the runs takes the wait for one it was only told of",
        },
        {
          sentence: THE_BOUND_IS_THE_REMINDER,
          because:
            "a role that reads its own next turn as the bound reads the counted set on any wake",
        },
        {
          sentence: NO_OTHER_WAKE_READS,
          because:
            "a role woken by a subagent's result or a message reads the checks before the settlement comes",
        },
        {
          sentence: NO_SETTLEMENT_IS_BOUNDED,
          because:
            "a role whose settlement never comes waits forever, or judges a lane still running at the reminder",
        },
        {
          sentence: GREEN_RUNS_THE_DISPOSAL,
          because:
            "a role that reads only the Green bullet completes its phase with the occurrence unposted",
        },
        {
          sentence: A_FAILING_TEST_IS_NO_EXTERNAL_CAUSE,
          because:
            "a role that finds a failing test in another tree's package skips the one re-run",
        },
      ]
    ),
    ...retiredClaimFaults(
      "skills/legion-worker/references/red-at-the-head.md",
      pages.redAtTheHead,
      {
        claim: THE_ROUND_FAILS_ON_A_RED,
        because:
          "failing the round was the tester's verdict, and no holder of the re-run has it: the reviewer requests changes, and the implementer records the red, tells the architect, and completes its phase, and the reviewer judges the red in its round",
      }
    ),
    ...missingSentenceFaults("skills/legion-worker/references/merge-gate.md", pages.mergeGate, [
      {
        sentence: MERGE_GATE_POINTS_AT_THE_PAGE,
        because:
          "a reviewer reading the merge gate reaches the re-run rule only through this pointer",
      },
    ]),
    ...missingSentenceFaults("skills/legion-worker/references/pr-body.md", pages.prBody, [
      {
        sentence: NEVER_WAITING_POINTS_AT_THE_PAGE,
        because:
          "a role reading `## Never waiting on CI` reaches the spent-re-run test and the reading of whose red it is only through this pointer",
      },
      {
        sentence: NEVER_WAITING_NAMES_THE_HOLDERS,
        because:
          "a role reading `## Never waiting on CI` takes the re-run for the phase's, and the tester in `testing` presses it",
      },
      {
        sentence: RERUN_WAIT_IS_BOUNDED,
        because:
          "a role whose settlement never comes waits forever, or judges a lane still running at the reminder",
      },
      {
        sentence: TESTER_LETS_CI_DO_ITS_JOB,
        because:
          "a tester reading its evidence rule without it reads CI again, and fails its round on a red the change never made",
      },
    ]),
  ];
}

/**
 * The sentences of the controller's census of a test a role re-ran green, pinned whole and
 * literally. The role has already commented the occurrence on an open issue that tracks the test
 * (`skill://legion-worker/references/red-at-the-head.md`, *What becomes of the record*) and
 * messages the controller after every green re-run, so the controller acts only where the role
 * found no issue or one that reads `done`. A forward merge of main's guide cut lost that
 * condition, and the text then asked the controller for a second comment on the occurrence
 * (INFRA-1303's review round 3).
 */
const CONTROLLER_ACTS_ON_AN_UNTRACKED_TEST =
  "You act on it when the role found no issue for the test, or found one that reads `done`. For a test an open issue tracks, the role has already commented the occurrence there, and you do nothing more.";

/**
 * What `markdown` — skills/legion-controller/references/finding-asks.md — gets wrong about a test
 * a role re-ran green.
 */
export function censusFaults(markdown: string): string[] {
  const file = "skills/legion-controller/references/finding-asks.md";
  return missingSentenceFaults(file, markdown, [
    {
      sentence: CONTROLLER_ACTS_ON_AN_UNTRACKED_TEST,
      because:
        "a controller that acts on every green re-run comments the occurrence a second time on the issue the role already commented",
    },
  ]);
}

/**
 * The tester's line of `## What each role does`, pinned whole and literally. A `ci_check` check's
 * result is the one Dispatch derives from its CI run, and Dispatch refuses any role's result for
 * it (`409 CHECK_PROVED_BY_CI`); the reviewer reads it before it approves. The line told the
 * tester to put such a check in its `failures` on a failed derived result until the tester came to
 * read no CI (the architect, 2026-11-20, on Sami's decision
 * dispatch://INFRA-1303/ask/dd2229bf-075c-4524-b045-41f39978dbb3), so a tester that lost this
 * line fails its round on CI again.
 */
const TESTER_LEAVES_A_CI_CHECK_TO_CI =
  "The tester runs and records only the checks with no `ci_check`. It neither reads nor records a `ci_check` check's derived result, which is CI's: the reviewer reads it before it approves.";

/** What `markdown` — skills/legion-worker/references/ci-checks.md — gets wrong about the tester. */
export function testerCiCheckFaults(markdown: string): string[] {
  const file = "skills/legion-worker/references/ci-checks.md";
  return missingSentenceFaults(file, markdown, [
    {
      sentence: TESTER_LEAVES_A_CI_CHECK_TO_CI,
      because:
        "a tester that reads a `ci_check` check's derived result puts a red of CI's in its failures and fails its round on it",
    },
  ]);
}
