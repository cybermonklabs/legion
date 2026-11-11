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

/** GitHub's heading anchor: lower-cased, punctuation dropped, each space a hyphen. */
export function anchors(markdown: string): Set<string> {
  return new Set(
    outsideFences(markdown).flatMap((line) => {
      const heading = /^#{1,6}\s+(.+?)\s*#*$/.exec(line)?.[1];
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
 * `markdown` with every run of whitespace collapsed to one space, beside the offset in `markdown`
 * each character of that collapsed text came from. A rule written over a sentence reads the
 * collapsed text, because these files are wrapped and a sentence of one crosses lines; the offsets
 * give a hit back the line of the file a reader opens.
 */
function collapsedWithOffsets(markdown: string): { text: string; offsets: number[] } {
  const parts: string[] = [];
  const offsets: number[] = [];
  const whitespace = /\s+/g;
  let read = 0;
  for (let run = whitespace.exec(markdown); run !== null; run = whitespace.exec(markdown)) {
    for (let at = read; at < run.index; at += 1) offsets.push(at);
    parts.push(markdown.slice(read, run.index));
    offsets.push(run.index);
    parts.push(" ");
    read = run.index + run[0].length;
  }
  for (let at = read; at < markdown.length; at += 1) offsets.push(at);
  parts.push(markdown.slice(read));
  return { text: parts.join(""), offsets };
}

/**
 * The prohibition INFRA-1303 retired from the tester's evidence rule
 * (skills/legion-worker/references/pr-body.md): "no CI lane is rerun as evidence", and the same
 * sentence in the active voice, "never re-run a CI lane as evidence", which the tester's prompt
 * part carried beside it. A red at the head that the change did not make is now the phase
 * holder's to re-run once (`skill://legion-worker/references/red-at-the-head.md`), so a file that
 * still forbids the re-run of a lane costs the tree a whole lap for a failure its code never
 * caused.
 *
 * Two shapes, each with its subject enumerated rather than left open. The passive: a universal
 * negative (`no`, `neither`) over a lane noun — `lane`, `check`, `suite`, `job` — that is re-run.
 * The active: `never re-runs a …` over the same nouns behind a determiner. `run` is deliberately
 * no such noun, which is what lets the sentences the change keeps pass: "A merge-group run is
 * never re-run" (merge-gate.md, pr-body.md and the implementer's prompt overlay) and "Where no
 * run at the head holds the failing check, the role re-runs nothing" (red-at-the-head.md). "No
 * role re-runs a group build." passes because a role is no lane, and "it gets no re-run", said of
 * a check that judges the pull request itself, because a re-run is no lane either.
 */
const NO_LANE_IS_RERUN =
  /\b(?:no|neither)\s+(?:CI\s+|required\s+|test\s+)?(?:lanes?|checks?|suites?|jobs?)\b[^.]{0,40}?\bre-?run\b|\bnever\s+re-?runs?\s+(?:a|an|any|the)\s+(?:CI\s+|required\s+|test\s+)?(?:lane|check|suite|job)\b/i;

/**
 * The prohibition INFRA-1303 retired from the tester's prompt part
 * (packages/daemon/internal/prompts/roles/core/tester.md): "rerun none of them yourself", said of
 * the CI lanes the tester has just been told to read on GitHub. The tester holds `testing`, so
 * the head's one re-run is its to press (`skill://legion-worker/references/red-at-the-head.md`),
 * and a tester told to re-run none of them reads that rule as written for somebody else.
 *
 * The object is enumerated — `them`, `these`, `those`, or the lane nouns themselves — rather than
 * left open, and the verb must be a re-run, never a bare `run`. That is what lets the sentence
 * the prompt keeps in its place pass: "do not run in your own pod what CI runs for you", which
 * forbids the local run and says nothing of the re-run.
 */
const RERUN_NONE_OF_THEM_YOURSELF =
  /\bre-?runs?\s+(?:none|neither)\s+of\s+(?:them|these|those|the\s+(?:lanes|checks|suites|jobs))\b|\b(?:never|do not|don't)\s+re-?run\s+(?:any\s+of\s+)?(?:them|these|those)\b[^.]{0,40}?\byourself\b/i;

/**
 * The prohibition INFRA-1303 retired from the reviewer's thread rule
 * (skills/legion-worker/references/review-threads.md): "you re-run nothing", said to the reviewer
 * of a review bot that is red while a thread it opened still stands. The reviewer holds
 * `reviewing`, so such a red is one it may re-run once
 * (`skill://legion-worker/references/red-at-the-head.md`); what that file keeps is the reason a
 * re-run does not clear this particular red, never a ban on pressing one.
 *
 * The subject is enumerated — the reviewer, by name or as `you` — rather than left open, so the
 * sentences that deny the re-run to a role holding no phase still pass: "The merger, the
 * architect in `accepting` and an implementer that runs retro re-run nothing." and "A role that
 * is told of a red whose phase it does not hold re-runs nothing." (both red-at-the-head.md), and
 * "a re-run does not clear that red" (review-threads.md), which names no re-runner at all. The
 * gap is bounded by `[^.]{0,40}`, so a `the reviewer` of the sentence before a full stop cannot
 * reach across it to the `re-run nothing` of the next.
 */
const REVIEWER_RERUNS_NOTHING =
  /\b(?:you|the reviewer|your review)\b[^.]{0,40}?\bre-?runs?\s+(?:nothing|none)\b/i;

/** The three prohibitions the re-run rule retired, in the order they are written above. */
const RETIRED_RERUN_PROHIBITIONS = [
  NO_LANE_IS_RERUN,
  RERUN_NONE_OF_THEM_YOURSELF,
  REVIEWER_RERUNS_NOTHING,
];

/**
 * Every place in the Markdown files under `roots` that still forbids the re-run INFRA-1303 turned
 * on, each as one `<file>:<line>`. The three prohibitions were deleted from six skill files and
 * two daemon prompt parts at once, and a forward merge keeps both sides of a deletion in silence,
 * so the rule is written over whole partitions rather than over the eight files it cleared: a
 * ninth file that acquires one fails here too.
 *
 * Each prohibition is matched against the whitespace-collapsed text, because each crosses a line
 * in the file it came from, and the hit's offset is mapped back through `collapsedWithOffsets` to
 * the line a reader opens. One hit per prohibition per file: the patterns carry no `g` flag, so a
 * file that says it twice is named once and reads the same after the first is fixed.
 */
export function retiredRerunProhibitions(roots: readonly string[]): string[] {
  return roots.flatMap((root) =>
    files(root)
      .filter((file) => file.endsWith(".md"))
      .flatMap((file) => {
        const markdown = readFileSync(file, "utf8");
        const { text, offsets } = collapsedWithOffsets(markdown);
        // A hit under the repository reads from its root (`skills/…`, `packages/daemon/…`); a hit
        // in a staged partition reads as that partition has it
        // (`legion-worker/references/pr-body.md`), since its root is a temporary directory.
        const name = file.startsWith(`${REPO_ROOT}${path.sep}`)
          ? path.relative(REPO_ROOT, file)
          : path.relative(root, file);
        return RETIRED_RERUN_PROHIBITIONS.flatMap((prohibition) => {
          const found = text.match(prohibition);
          if (found?.index === undefined) return [];
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

/** The merge gate, which carries the merger's merge-queue rule and one pointer to that page. */
export const MERGE_GATE = path.join(REPO_ROOT, "skills/legion-worker/references/merge-gate.md");

/** The pull-request body reference, which points at that page twice. */
export const PR_BODY = path.join(REPO_ROOT, "skills/legion-worker/references/pr-body.md");

/**
 * What one skill file gets wrong about a rule pinned over the repository's own `skills/`: it
 * still carries a claim the rule retired (`retired.claim`, a pattern rather than its one wording,
 * so a reword of the same shape is caught too), or it has lost one of the sentences the rule
 * turns on (each of `required`, pinned whole and literally). A rule that retired no claim, whose
 * pins are all that hold it, passes `null` for `retired`. Each fault is one sentence naming
 * `file` and saying what a reader of the wrong text would believe. The text is
 * whitespace-normalized first, because these files are wrapped and a sentence of one crosses
 * lines.
 */
function prosePinFaults(
  file: string,
  markdown: string,
  retired: { claim: RegExp; because: string } | null,
  required: readonly { sentence: string; because: string }[]
): string[] {
  const text = markdown.replace(/\s+/g, " ");
  const faults: string[] = [];
  if (retired !== null) {
    const found = text.match(retired.claim);
    if (found !== null) faults.push(`${file} says "${found[0]}": ${retired.because}`);
  }
  for (const { sentence, because } of required) {
    if (!text.includes(sentence)) {
      faults.push(`${file} does not say "${sentence}": ${because}`);
    }
  }
  return faults;
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
  return prosePinFaults(
    "skills/legion-worker/SKILL.md",
    markdown,
    {
      claim: ROLE_STAYS_LIVE,
      because:
        "a released pod ends every role process of it, so no role stays live for as long as its issue is in the workflow",
    },
    [
      {
        sentence: RELEASE_IS_NO_STOP,
        because: "a worker told nothing about release reads its pod's deletion as a crash",
      },
    ]
  );
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
  return prosePinFaults(
    "skills/legion-worker/references/tools.md",
    markdown,
    {
      claim: LOGIN_OWNS_DATABASES,
      because:
        "the skill cannot know what a deployment's session login may do, so it claims no power for that login",
    },
    [
      {
        sentence: NO_READ_THROUGH_SESSION_URL,
        because:
          "a role told nothing about reads reaches the shared store through the write-capable session URL",
      },
    ]
  );
}

/**
 * The sentence of `## Who may re-run` that carries INFRA-1303's rule, pinned whole and literally.
 * A reader who lost it would believe the re-run belongs to whoever notices the red: the merger,
 * the architect in `accepting`, an implementer in retro, or a role merely told of a red in a
 * phase it does not hold — each of which the three sentences beside it refuse. The grant is what
 * the page exists for; the sentences that deny the re-run read as a list of exceptions to
 * nothing once it is gone.
 */
const PHASE_HOLDER_PRESSES_THE_RERUN =
  "The holder of the issue's phase presses the re-run: the tester in `testing`, the reviewer in `reviewing`, the implementer in `implementing`.";

/**
 * The merger's merge-queue rule, its two sentences pinned whole and literally. The first is the
 * one re-queue: a role that lost it sends the work back to `implementing` on the first
 * `failed_checks` ejection of a head GitHub itself may queue again, which is the lap this change
 * exists to save. The second is the bound on it, and a role that lost that one reads the first as
 * leave to re-run the group build — which puts nothing back in the queue, since GitHub deletes
 * the entry's branch (measured on cybermonklabs/legion#2811).
 */
const EJECTION_IS_QUEUED_ONCE_MORE =
  "When GitHub takes the pull request out of the merge queue because the group build failed a check (`failed_checks`) and the head has not changed, the merger queues that same head once more.";
const NO_ROLE_RERUNS_A_GROUP_BUILD = "No role re-runs a group build.";

/**
 * The four pointers to the page, each pinned whole and literally: the worker skill's own
 * References row, which is how a role reaches the page at all; the merge gate's; and the two in
 * the pull-request body reference — the one in `## Never waiting on CI`, beside the
 * implementer's own re-run, and the one in the tester's evidence bullet, where the prohibition
 * this change retired used to stand. A reference is reached only through a link from somewhere a
 * role reads, so a lost pointer leaves the rule shipped and unread: the role goes on from the
 * text around the gap, which tells it to finish its round on a red the change never made.
 */
const SKILL_LINKS_THE_PAGE =
  "| A check is red at the head of the pull request, in the phase the role holds | `skill://legion-worker/references/red-at-the-head.md` |";
const MERGE_GATE_POINTS_AT_THE_PAGE =
  "A required check that is red at the head is a different case, which `skill://legion-worker/references/red-at-the-head.md` decides.";
const NEVER_WAITING_POINTS_AT_THE_PAGE =
  "`skill://legion-worker/references/red-at-the-head.md` holds the rest of that rule: the test of whether the head's one re-run is already spent, and the record the role writes before it re-runs.";
const TESTER_EVIDENCE_POINTS_AT_THE_PAGE =
  "A red at the head that the change did not make is the tester's to re-run once, and `skill://legion-worker/references/red-at-the-head.md` holds the rule.";

/**
 * What the four files INFRA-1303 wrote or changed get wrong about a red at the head: the worker
 * skill (`WORKER_SKILL`), the page that holds the rule (`RED_AT_THE_HEAD`), the merge gate
 * (`MERGE_GATE`) and the pull-request body reference (`PR_BODY`), each read by its caller. These
 * pins alone hold the change's positive half; the prohibitions it retired are refused across
 * whole partitions by `retiredRerunProhibitions`, so nothing here carries a retired claim of its
 * own.
 */
export function redAtTheHeadFaults(pages: {
  workerSkill: string;
  redAtTheHead: string;
  mergeGate: string;
  prBody: string;
}): string[] {
  return [
    ...prosePinFaults("skills/legion-worker/SKILL.md", pages.workerSkill, null, [
      {
        sentence: SKILL_LINKS_THE_PAGE,
        because:
          "a reference the skill does not link is a page no role opens, and a read by path stops at 300 lines",
      },
    ]),
    ...prosePinFaults(
      "skills/legion-worker/references/red-at-the-head.md",
      pages.redAtTheHead,
      null,
      [
        {
          sentence: PHASE_HOLDER_PRESSES_THE_RERUN,
          because:
            "a page that grants the re-run to nobody leaves every role that reads a red free to press it",
        },
      ]
    ),
    ...prosePinFaults("skills/legion-worker/references/merge-gate.md", pages.mergeGate, null, [
      {
        sentence: EJECTION_IS_QUEUED_ONCE_MORE,
        because:
          "a merger told nothing of the re-queue sends the work back on the first failed_checks ejection of a head GitHub would queue again",
      },
      {
        sentence: NO_ROLE_RERUNS_A_GROUP_BUILD,
        because:
          "a role that re-runs a group build puts nothing back in the queue, since GitHub deletes the entry's branch",
      },
      {
        sentence: MERGE_GATE_POINTS_AT_THE_PAGE,
        because:
          "a reviewer reading the merge gate reaches the re-run rule only through this pointer",
      },
    ]),
    ...prosePinFaults("skills/legion-worker/references/pr-body.md", pages.prBody, null, [
      {
        sentence: NEVER_WAITING_POINTS_AT_THE_PAGE,
        because:
          "an implementer reading `## Never waiting on CI` reaches the spent-re-run test only through this pointer",
      },
      {
        sentence: TESTER_EVIDENCE_POINTS_AT_THE_PAGE,
        because:
          "a tester reading its evidence rule reaches the re-run rule only through this pointer",
      },
    ]),
  ];
}
