import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import { closeSync, existsSync, mkdirSync, openSync, readFileSync, writeFileSync } from "node:fs";
import path from "node:path";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import {
  ARGUMENT_LIMIT,
  type BuiltWorld,
  CHECKS,
  type Check,
  DATA,
  defaultBounds,
  diskReadings,
  emit,
  judged,
  pinnedRoutes,
  pinSection,
  RIG,
  readData,
  readsHeld,
  report,
  rigShell,
  rigShellArgv,
  runRigShell,
  servedReads,
  Tree,
  Worlds,
  WRITE_FAILED,
  worldOfDir,
} from "./class-search";
import { pinnedReads } from "./production-shapes";
import { spawnChecked } from "./test-programs";

// The class search (class-search.ts, `rig.sh class-search`): every class of defect INFRA-764's
// review rounds found, each a named check whose rule judges each hit, the residuals declared
// apart (the architect's ruling of 2026-11-18T08:40Z). These rows hold that it finds no instance
// outside its residuals at this checkout, and that an instance planted of each class fails the
// check that searches for it, naming the instance.

const CHECKOUT = path.resolve(import.meta.dir, "../../../..");
const tree = new Tree(CHECKOUT);
/** The check of the search named `name`. */
function checkNamed(name: string): Check {
  const found = CHECKS.find((check) => check.name === name);
  if (found === undefined) throw new Error(`the class search has no check named ${name}`);
  return found;
}
/** The tree with `line` added after the last line of `file`, and where it then is. */
function planting(file: string, line: string): { tree: Tree; at: string } {
  const text = existing(file);
  const body = text === "" || text.endsWith("\n") ? text : `${text}\n`;
  return {
    tree: new Tree(CHECKOUT, new Map([[file, `${body}${line}\n`]])),
    at: `${file}:${body.split("\n").length}`,
  };
}
function existing(file: string): string {
  try {
    return readFileSync(path.join(CHECKOUT, file), "utf8");
  } catch {
    return "";
  }
}
/** Where each instance of `hits` is. */
const instances = (hits: readonly { at: string; verdict: string }[]) =>
  hits.filter((hit) => hit.verdict === "instance").map((hit) => hit.at);
/** The disk readings a row's report is printed with, where the row reads no disk. */
const DISK = { start: "s", end: "e" };

// Catches: a search that fails at the head it is run at, which is the zero the push waits for, or
// that prints no line for one of its checks or loses a residual it declares.
test("the class search finds no instance outside its residuals at this checkout, and each residual", () => {
  const done = spawnChecked(["bash", path.join(import.meta.dir, "rig.sh"), "class-search"], {
    env: process.env,
  });
  const stdout = done.stdout.toString();
  expect({ exitCode: done.exitCode, stderr: done.stderr.toString() }).toEqual({
    exitCode: 0,
    stderr: "",
  });
  expect(stdout).toContain(
    "result: 0 findings outside the residuals, 11 residuals found, 14 declared unreachable found, 0 stale\n"
  );
  for (const check of CHECKS) expect(stdout).toContain(`== ${check.name}\n`);
}, 900_000);

// The class search runs rig.sh's own functions, about 300 KB of them, which a runner's kernel
// refuses as one argument (Linux's MAX_ARG_STRLEN, 131,072 bytes): CI run 44545489442 failed every
// row that built a world with `E2BIG: argument list too long, posix_spawn 'bash'`, where a gVisor
// pod let the argument through. Catches: the rig's shell handed to bash as one argument again,
// which no row on a pod would see fail.
test("the class search hands bash the rig's own shell in a file, and no argument of it is past Linux's limit", () => {
  const lines = rigShell(tree);
  expect(Buffer.byteLength(lines.join("\n"))).toBeGreaterThan(ARGUMENT_LIMIT);
  const dir = temporaryDirectories("class-search-argv")("shell");
  const file = path.join(dir, "script.sh");
  const argv = rigShellArgv(file, [dir, "x"]);
  expect(argv.filter((arg) => Buffer.byteLength(arg) >= ARGUMENT_LIMIT)).toEqual([]);
  const ran = runRigShell(dir, [...lines, 'printf "%s %s\\n" "$1" "$2"'], [dir, "x"], {
    PATH: process.env.PATH ?? "",
  });
  expect([ran.exitCode, ran.stdout.toString(), ran.stderr.toString()]).toEqual([
    0,
    `${dir} x\n`,
    "",
  ]);
  expect(existsSync(file)).toBe(false);
});

// A pod's `git` is Legion's leak-check wrapper, whose guard refuses every push for which it finds
// no shared check: through PUBLIC_LEAK_CHECK, then public-leak-check.sh on PATH, then the pointer
// file under the configuration home (packages/daemon/internal/leakguard's Resolve). The class
// search builds its worlds with rig.sh's worker_fixture under a HOME of its own, which holds no
// pointer file, and gave that build PATH and HOME alone: on the tester's pod, whose check is on no
// PATH entry, the first world's push was refused with `LEAK CHECK SETUP ERROR` and the search
// exited 2 (tester, 2026-11-19). The first row below brings that pod to every host: it runs the
// second in a child `bun test` whose environment, set before the child loads this file, reaches
// the check through PUBLIC_LEAK_CHECK alone, behind a `git` that looks for it as the guard does.
describe("the worlds the class search builds, under a leak-check git", () => {
  const BUILD = "every world is built, each push finding the shared check";
  /** Set for the child `bun test` the first row starts, and by nothing else. */
  const CHILD = "SKILL_SCENARIOS_CLASS_SEARCH_GUARD_ROW";
  const freshDir = temporaryDirectories("class-search-guard");

  // Catches: a world build whose environment drops the caller's PUBLIC_LEAK_CHECK, on any host
  // (leak-check-env.ts). The stand-in git then finds no check for the world's push and refuses it
  // with `LEAK CHECK SETUP ERROR`, exit 2, as the pod's guard refused the tester's; the child's row
  // fails on the build's error, and this row fails quoting the child's report.
  test("they carry the caller's PUBLIC_LEAK_CHECK to every push, as a pane does", () => {
    const dir = freshDir();
    const bin = path.join(dir, "bin");
    mkdirSync(bin);
    const check = path.join(dir, "check", "public-leak-check.sh");
    mkdirSync(path.dirname(check));
    writeFileSync(check, "#!/bin/sh\nexit 0\n", { mode: 0o755 });
    const pushes = path.join(dir, "pushes");
    // Legion's leak-check git as far as this row needs one (/opt/legion/bin/git and the guard its
    // pre-push hook runs): a push runs only where the shared check is found as Resolve finds it,
    // and is recorded by its refspec with the check found; every other command is the real git's.
    writeFileSync(
      path.join(bin, "git"),
      [
        "#!/bin/sh",
        'case " $* " in',
        '*" push "*)',
        "  for last; do :; done",
        `  pointer=\${XDG_CONFIG_HOME:-\${HOME:-}/.config}/public-leak-check/path`,
        `  if [ -n "\${PUBLIC_LEAK_CHECK:-}" ]; then`,
        "    check=$PUBLIC_LEAK_CHECK",
        "  elif check=$(command -v public-leak-check.sh); then",
        "    :",
        '  elif [ -r "$pointer" ]; then',
        '    check=$(cat "$pointer")',
        "  else",
        '    echo "LEAK CHECK SETUP ERROR: the push of $last: the shared public leak check could not be found: PUBLIC_LEAK_CHECK names none, public-leak-check.sh is on no PATH entry, and the pointer file $pointer could not be read" >&2',
        "    exit 2",
        "  fi",
        '  if [ ! -f "$check" ]; then',
        '    echo "LEAK CHECK SETUP ERROR: the push of $last: the shared public leak check could not be found: $check is not there" >&2',
        "    exit 2",
        "  fi",
        `  printf '%s %s\\n' "$last" "$check" >>'${pushes}'`,
        "  ;;",
        "esac",
        'exec /usr/bin/git "$@"',
        "",
      ].join("\n"),
      { mode: 0o755 }
    );
    // The caller's PATH with no directory that holds the check, as on the tester's pod.
    const callerPath = (process.env.PATH ?? "")
      .split(":")
      .filter((entry) => entry !== "" && !existsSync(path.join(entry, "public-leak-check.sh")));
    const child = spawnChecked(
      [
        process.execPath,
        "test",
        import.meta.path,
        "-t",
        `^the worlds the class search builds, under a leak-check git ${BUILD}$`,
      ],
      {
        cwd: path.resolve(import.meta.dir, "../.."),
        env: {
          ...process.env,
          PATH: [bin, ...callerPath].join(":"),
          PUBLIC_LEAK_CHECK: check,
          [CHILD]: "1",
        },
      }
    );
    const report = `${child.stdout.toString()}${child.stderr.toString()}`;
    expect({ code: child.exitCode, report }).toEqual({
      code: 0,
      report: expect.stringMatching(/^ 1 pass$[\s\S]*^ 0 fail$/m),
    });
    // Each world pushes its base and its pull request's branch, and every push found the check
    // the caller named, the one place it could find it.
    const recorded = readFileSync(pushes, "utf8").split("\n").filter(Boolean);
    expect(recorded.length).toBeGreaterThanOrEqual(2);
    expect(recorded.filter((line) => !line.endsWith(` ${check}`))).toEqual([]);
  }, 300_000);

  // Registered in that child alone: anywhere else the `git` on PATH and the check are the host's,
  // and the row would read the host rather than the class search.
  if (process.env[CHILD] !== undefined)
    test(BUILD, () => {
      const worlds = new Worlds(tree);
      try {
        expect(worlds.all().length).toBeGreaterThan(0);
      } finally {
        worlds.done();
      }
    }, 240_000);
});

// Catches: a data file whose hand search drops an entry of the 46, maps one to no check and no
// row, or names a check the search has not got; a residual with no ruling; and an unreachable
// entry with no reason or naming a check the search has not got.
test("the data file maps each of the hand search's 46 entries, and gives each residual its ruling", () => {
  const data = readData(readFileSync(DATA, "utf8"));
  expect(data.searches.map(({ entry }) => entry)).toEqual(
    Array.from({ length: 46 }, (_, index) => index + 1)
  );
  expect(data.residuals.map(({ ruled, words }) => ruled !== "" && words !== "")).toEqual(
    data.residuals.map(() => true)
  );
  expect(data.unreachable.map(({ reason }) => reason !== "")).toEqual(
    data.unreachable.map(() => true)
  );
  expect(() =>
    readData(
      JSON.stringify({ residuals: [], unreachable: [], searches: [{ entry: 1, finding: "x" }] })
    )
  ).toThrow("hand search entry 1 names neither a check or a row that holds it");
  expect(() =>
    readData(
      JSON.stringify({
        residuals: [],
        unreachable: [],
        searches: [{ entry: 1, finding: "x", check: ["none"] }],
      })
    )
  ).toThrow("hand search entry 1 names no check: none");
  expect(() =>
    readData(
      JSON.stringify({
        residuals: [],
        unreachable: [{ name: "x", check: "none", at: "x", reason: "x" }],
        searches: [],
      })
    )
  ).toThrow("unreachable entry x names no check: none");
});

/** The check of the stand-ins' declarations of what they do not model. */
const NOT_MODELLED = "stand-ins: a behaviour a stand-in declares not modelled";

// Catches: a search that misses a stand-in's declaration of a behaviour it does not model, which
// it answers otherwise than production with no ruling that it is a residual and no showing that
// no scenario reaches it (the architect's ruling of 2026-11-19T04:50Z). At this checkout every
// declaration is covered, by the residual it was ruled or by the entry that says why no world
// reaches it, the `Unreachable:` sentence beside it included; a new declaration is a finding
// naming its file and line; and an entry no declaration matches is stale, which fails the search.
test("every stand-in's declaration of a behaviour not modelled is a residual or declared unreachable", async () => {
  const data = readData(readFileSync(DATA, "utf8"));
  const check = checkNamed(NOT_MODELLED);
  const worlds = new Worlds(tree, []);
  try {
    const here = judged(
      [{ check, hits: await check.run(tree, worlds) }],
      data.residuals,
      data.unreachable
    );
    expect(here.findings.map(({ hit }) => hit.at)).toEqual([]);
    expect([here.stale, here.staleUnreachable]).toEqual([
      data.residuals.filter((residual) => residual.check !== NOT_MODELLED),
      [],
    ]);
    expect(here.unreachable.map(({ entry }) => entry.name)).toEqual(
      data.unreachable.map(({ name }) => name)
    );
    const { tree: planted, at } = planting(
      `${RIG}/gh-standin.ts`,
      "// A planted behaviour of GitHub's is not\n// modelled here."
    );
    const found = judged(
      [{ check, hits: await check.run(planted, worlds) }],
      data.residuals,
      data.unreachable
    );
    expect(found.findings.map(({ hit }) => [hit.at, hit.text])).toEqual([
      [at, "A planted behaviour of GitHub's is not modelled here."],
    ]);
    expect(report(found, data, "head", DISK)).toContain(
      `== findings outside the residuals: 1\n  ${NOT_MODELLED}: ${at}  A planted behaviour of GitHub's is not modelled here.\n`
    );
    const stale = {
      name: "a planted entry",
      check: NOT_MODELLED,
      at: "planted\\.ts:",
      reason: "x",
    };
    const gone = judged([{ check, hits: await check.run(tree, worlds) }], data.residuals, [
      ...data.unreachable,
      stale,
    ]);
    expect(gone.staleUnreachable).toEqual([stale]);
    expect(report(gone, data, "head", DISK)).toContain(
      `== stale unreachable entries (declared, and no declaration found): 1\n  a planted entry (${NOT_MODELLED})\n`
    );
  } finally {
    worlds.done();
  }
});

// Catches: a residual whose instance is gone left declared, which would read as a ruling on a
// finding the search no longer makes; and an instance a residual does not cover counted as one.
test("a residual no instance is of is stale, and an instance no residual covers is a finding", () => {
  const data = readData(readFileSync(DATA, "utf8"));
  const [first] = data.residuals;
  if (first === undefined) throw new Error("the data file declares no residual");
  const check = checkNamed(first.check);
  const none = judged([{ check, hits: [] }], data.residuals);
  expect(none.stale.map(({ name }) => name)).toEqual(data.residuals.map(({ name }) => name));
  expect(report(none, data, "head", DISK)).toContain(
    `== stale residuals (declared, and no instance found): ${data.residuals.length}\n`
  );
  const other = judged(
    [
      {
        check,
        hits: [{ at: "planted.ts:1", text: "planted", verdict: "instance", why: "planted" }],
      },
    ],
    data.residuals
  );
  expect(other.findings.map(({ hit }) => hit.at)).toEqual(["planted.ts:1"]);
});

// A run of `rig.sh class-search` at 7812df81 exited 2 after 549 s with both streams empty. Catches:
// a report without the readings of the disks at the search's start and at its end, which say
// whether the disk the search writes to was full; and a reading that throws where the directory
// cannot be read, which would lose the report it opens.
test("the report opens with the free space of the checkout's disk and of $TMPDIR's, at the search's start and end", () => {
  const data = readData(readFileSync(DATA, "utf8"));
  const disks = [
    ["the checkout", CHECKOUT],
    ["$TMPDIR", process.env.TMPDIR ?? "/tmp"],
  ] as const;
  const reading = diskReadings(disks);
  const measured = (dir: string) => `${dir}: [0-9]+ MiB free of [1-9][0-9]* MiB`;
  expect(reading).toMatch(
    new RegExp(`^the checkout ${measured(CHECKOUT)}; \\$TMPDIR ${measured(disks[1][1])}$`)
  );
  const printed = report(judged([], []), data, "head", { start: reading, end: "the end's" });
  expect(printed).toStartWith(
    `class search of head\ndisk at the start: ${reading}\ndisk at the end: the end's\n\n`
  );
  const gone = path.join(CHECKOUT, "no-such-directory");
  expect(diskReadings([["gone", gone]])).toStartWith(`gone ${gone}: unread (ENOENT`);
});

// The search wrote its report once at its end with `process.stdout.write` and its error with
// `console.error`, neither checked, and a run at 7812df81 exited 2 with both streams empty.
// /dev/full fails every write with ENOSPC, as a full disk does. Catches: a write that fails left
// unsaid, on either stream; a failed write that keeps the search's own status, which reads as
// that verdict with no report behind it; and writes that go through answered otherwise than the
// search's own status, or cut.
test("a report or an error line that cannot be written exits 3, naming the write on the other stream", () => {
  const dir = temporaryDirectories("class-search-writes")("writes");
  const full = openSync("/dev/full", "w");
  const opened: number[] = [full];
  const file = (name: string) => {
    const fd = openSync(path.join(dir, name), "w");
    opened.push(fd);
    return fd;
  };
  const text = (name: string) => readFileSync(path.join(dir, name), "utf8");
  try {
    expect(WRITE_FAILED).toBe(3);
    const err = file("report-err");
    expect(emit({ report: "the report\n" }, 0, { out: full, err })).toBe(WRITE_FAILED);
    expect(text("report-err")).toBe(
      `class-search: the report's write to stdout (fd ${full}) failed: ENOSPC: no space left on device, write, after 0 of 11 bytes; the search's own status was 0\n`
    );
    const out = file("error-out");
    expect(emit({ error: "class-search: boom\n" }, 2, { out, err: full })).toBe(WRITE_FAILED);
    expect(text("error-out")).toBe(
      `class-search: the error's write to stderr (fd ${full}) failed: ENOSPC: no space left on device, write, after 0 of 19 bytes; the search's own status was 2\n`
    );
    const whole = "x".repeat(200_000);
    expect(emit({ report: whole }, 1, { out: file("ok-out"), err: file("ok-err") })).toBe(1);
    expect([text("ok-out") === whole, text("ok-err")]).toEqual([true, ""]);
  } finally {
    for (const fd of opened) closeSync(fd);
  }
});

// Catches: the search's own process writing its usage line unchecked, so a stderr that takes
// nothing leaves exit 2 and no word; the line naming the failed write goes to stdout instead.
test("the search's own process names a failed write of its usage line on stdout, exit 3", () => {
  const full = openSync("/dev/full", "w");
  try {
    const done = Bun.spawnSync(
      [process.execPath, path.join(import.meta.dir, "class-search.ts"), "x"],
      {
        stdio: ["ignore", "pipe", full],
      }
    );
    expect([done.exitCode, done.stdout.toString()]).toEqual([
      3,
      "class-search: the error's write to stderr (fd 2) failed: ENOSPC: no space left on device, write, after 0 of 40 bytes; the search's own status was 2\n",
    ]);
  } finally {
    closeSync(full);
  }
}, 60_000);

describe("the writes and the reads of the world", () => {
  let worlds: Worlds;
  let approved: BuiltWorld;
  beforeAll(() => {
    worlds = new Worlds(tree);
    const found = worlds.all().find((world) => world.name === "threads-closed-correct");
    if (found === undefined) throw new Error("no world of threads-closed's correct was built");
    approved = found;
  }, 300_000);
  afterAll(() => worlds?.done());
  /** The approved world with `routes` ahead of its own, under a directory of its own. */
  function planted(routes: readonly unknown[]): Worlds {
    const dir = worlds.copy(approved);
    const file = path.join(dir, "fixtures.json");
    const fixtures = JSON.parse(readFileSync(file, "utf8"));
    fixtures.routes = [...routes, ...fixtures.routes];
    writeFileSync(file, JSON.stringify(fixtures));
    return new Worlds(tree, [worldOfDir("planted", dir)]);
  }

  // Catches: the route every write once fell to, `{id:6001}` and exit 0 for any POST, PATCH or
  // PUT no route above it took, which changed nothing (INFRA-764 round 25's PUT of a review).
  test.each([
    "writes: every write route of every world is modelled or refused",
    "writes: no route of any world is a catch-all",
  ])("a catch-all write route fails %s, naming it", async (name) => {
    const on = planted([{ match: "^gh api", stdout: { id: 6001 } }]);
    try {
      expect(instances(await checkNamed(name).run(tree, on))).toContain(
        "planted: fixtures.json routes[0]"
      );
    } finally {
      on.done();
    }
  }, 600_000);

  // Catches: a gh command gh's source reads as a write answered by a route with a success the
  // world does not keep, as `gh pr (edit|comment|review|ready)` once answered any of them.
  test("a gh write command a route answers with a success that changes nothing fails, naming it", async () => {
    const on = planted([{ match: "^gh pr merge( |$)", stdout: "merged" }]);
    try {
      const hits = await checkNamed(
        "writes: every gh write command the world serves is modelled"
      ).run(tree, on);
      expect(instances(hits)).toEqual(["gh-writes.json: pr merge"]);
    } finally {
      on.done();
    }
  }, 600_000);

  // GitHub refuses the review App, which holds no `workflows` permission (appauth's
  // writePermissions), a push that changes a workflow, and takes the implement App's. Catches: a
  // world whose remote takes the review App's workflow push, here one whose remote hands no
  // command to its proc-receive hook (no receive.procReceiveRefs), which the rig's remote was until
  // INFRA-764 found the review App's fix of a workflow comment taken where production refuses it.
  // At this checkout the check finds both Apps answered as GitHub answers them.
  test("a world whose remote takes the review App's workflow push fails the push check, naming it", async () => {
    const name = "writes: each App's workflow push is answered as GitHub answers that App";
    const unplanted = new Worlds(tree, [approved]);
    const dir = worlds.copy(approved);
    const unset = spawnChecked(
      ["git", "-C", path.join(dir, "remote.git"), "config", "--unset", "receive.procReceiveRefs"],
      { env: process.env }
    );
    expect(unset.exitCode).toBe(0);
    const on = new Worlds(tree, [worldOfDir("planted", dir)]);
    try {
      const own = await checkNamed(name).run(tree, unplanted);
      expect(own.map((hit) => [hit.at, hit.verdict])).toEqual([
        ["threads-closed-correct: the Implement App's push of .github/workflows/greet.yml", "true"],
        ["threads-closed-correct: the Review App's push of .github/workflows/greet.yml", "true"],
      ]);
      expect(instances(await checkNamed(name).run(tree, on))).toEqual([
        "planted: the Review App's push of .github/workflows/greet.yml",
      ]);
    } finally {
      unplanted.done();
      on.done();
    }
  }, 600_000);

  // Catches: a read production refuses (the 403 a role App gets for a branch's protection) that
  // the world answers, which tells an agent what production would not.
  test("a read production refuses that the world answers fails, naming it", async () => {
    const on = planted([
      { match: "^gh api .*/branches/main/protection", methods: ["GET"], stdout: {} },
    ]);
    try {
      const hits = await checkNamed(
        "reads: production's pinned answers the world does not give"
      ).run(tree, on);
      expect(instances(hits)).toContain("production-shapes.json: branches-main-protection");
    } finally {
      on.done();
    }
  }, 600_000);

  // Catches: a check that drops a spelling of the token's print, or reads one that prints none.
  test("each gh auth call production prints the token for is found, and no other", async () => {
    const hits = await checkNamed(
      "reads: a credential production prints that the world holds none of"
    ).run(tree, worlds);
    expect(hits.map((hit) => [hit.at, hit.verdict])).toEqual([
      ["gh auth status -t", "instance"],
      ["gh auth status --show-token", "instance"],
      ["gh auth token", "instance"],
    ]);
  }, 300_000);
});

// Catches: a GET the world serves with no pin and no word of the README's pin section, which the
// world answers as it likes and no row holds to production.
test("an unpinned GET the README does not name fails the reads check, naming it", () => {
  const route = "GET repos/{owner}/{repo}/hooks";
  const hits = readsHeld(
    [...servedReads(tree), { route, by: "planted" }],
    pinnedRoutes(pinnedReads()),
    pinSection(readFileSync(path.join(CHECKOUT, "packages/pi-envoy/scripts/README.md"), "utf8"))
  );
  expect(instances(hits)).toEqual([route]);
});

/** The check of the bound a test file that runs processes sets its rows. */
const DEFAULT_BOUND = "hand search 11: a test that runs processes under bun's default bound";

/** Each check that greps the tree, and a line of its class planted in a file it reads. */
const PLANTED: readonly [check: string, file: string, line: string][] = [
  [
    "reads: a remote helper key on the world's clone",
    `${RIG}/rig.sh`,
    "  git config remote.upstream.vcs other",
  ],
  [
    "an agent's world: a record of the rig's that names the rig",
    `${RIG}/rig.sh`,
    `  cp "$(label_provenance "\${P##*/}")" "$R/provenance"`,
  ],
  // The bind world_paths' `empty` kind made until 2026-11-19 (private_world's `empty)` case).
  [
    "an agent's world: a path the namespace fills with the rig's own content",
    `${RIG}/rig.sh`,
    '    empty) : >/dev/shm/empty && mount --bind /dev/shm/empty "$p" && mount -o remount,bind,ro "$p" || exit 1 ;;',
  ],
  [
    "readers: a reader reads the run's archive and its recorded provenance alone",
    `${RIG}/health.ts`,
    'export const PLANTED_SCRATCH = process.env.TMPDIR ?? "";',
  ],
  [
    "readers: no reader decides a world fact from an agent's text",
    `${RIG}/score.ts`,
    "export const plantedResolved = (call: Call) => /PRRT_[\\w-]+/.test(callText(call));",
  ],
  [
    "hand search 1: a branch on a label's name",
    `${RIG}/score.ts`,
    'export const plantedBase = (label: string) => label === "base";',
  ],
  [
    "hand search 3: a gate on a tell main's engine makes unreachable",
    "skills/planted.md",
    "Move on once the issue is `awaiting_merge`.",
  ],
  [
    "hand search 4 and 6: one fact defined in two places",
    `${RIG}/worker-pane.ts`,
    "export function reviewOf(): void {}",
  ],
  [
    "hand search 6: a reader of world.json of its own",
    `${RIG}/health.ts`,
    'const plantedWorld = readFileSync(path.join(".", "world.json"), "utf8");',
  ],
  [
    "hand search 5: a citation of a test by its line",
    "docs/planted.md",
    "The row at rig.test.ts:120 holds it.",
  ],
  [
    "hand search 7: a verb read by a reader of its own",
    `${RIG}/score.ts`,
    'const plantedVerb = (argv: string[]) => argv.includes("-X");',
  ],
  [
    "hand search 8: a prompt restating a skill's rule",
    "packages/daemon/internal/prompts/roles/planted.md",
    "Resolve each thread as review-threads says.",
  ],
  [
    "hand search 9: a reference to a deleted symbol",
    "packages/pi-legion/src/planted.ts",
    'export const planted = "READY_NOT_APPROVED";',
  ],
  [
    "hand search 11: a bound a slow host fails",
    `${RIG}/planted.test.ts`,
    "expect(Date.now() - started).toBeLessThan(5_000);",
  ],
  [DEFAULT_BOUND, `${RIG}/planted.test.ts`, 'const planted = spawnChecked(["true"]);'],
  [
    "hand search 15 and 31: a reader of another program's source as text",
    `${RIG}/daemon-standin.ts`,
    'const plantedHolds = ENGINE_SOURCE.includes("func planted(");',
  ],
  [
    "hand search 30: a reader of the boot record of its own",
    `${RIG}/score.ts`,
    'const plantedBoots = readFileSync(path.join(dir, BOOTED), "utf8");',
  ],
  [
    "hand search 20 and 35: a hand-kept list of the rig's programs",
    `${RIG}/rig.sh`,
    "for program in gh-standin.ts daemon-standin.ts; do :; done",
  ],
  [
    "hand search 37: a reader of the system prompt's argument as its text",
    `${RIG}/score.ts`,
    'const plantedPrompt = readFileSync(path.join(dir, "system-args"), "utf8");',
  ],
  [
    "hand search 40: an identity a commit sets that the environment beats",
    `${RIG}/rig.sh`,
    "  git -c user.name=planted commit -qm planted",
  ],
  [
    "hand search 41: a database a shared server keeps",
    `${RIG}/planted.test.ts`,
    'await sql("CREATE DATABASE planted");',
  ],
  [NOT_MODELLED, `${RIG}/daemon-pane.go`, "// The planted rule is not modelled."],
];

// Catches, for each check that greps: a pattern that misses its class, and a rule that calls an
// instance of it true. The planted line is the one instance the check finds that it does not
// find at this checkout. A check that walks the whole tree (the citations, the deleted symbols)
// reads every text file of packages/ twice.
test.each(PLANTED)(
  "a planted instance fails %s, naming it",
  async (name, file, line) => {
    const { tree: plantedTree, at } = planting(file, line);
    const check = checkNamed(name);
    const worlds = new Worlds(tree, []);
    try {
      const before = new Set(instances(await check.run(tree, worlds)));
      const found = instances(await check.run(plantedTree, worlds)).filter(
        (one) => !before.has(one)
      );
      expect(found).toEqual([at]);
    } finally {
      worlds.done();
    }
  },
  60_000
);

// Catches: hand search 3 reading a line about the reviewer's thermonuclear pair, whose two halves
// are its two review agents with a `Thermo` row each (main's gate on an approval in
// skills/legion-worker/references/merge-gate.md), as a gate on the engine's retired tell; and an
// exemption so wide that a line holding "both halves" and naming no `Thermo` row is read true.
test("hand search 3 reads a line of the thermonuclear pair's Thermo rows as true, and any other both halves as an instance", async () => {
  const file = "skills/planted.md";
  const text = [
    "Your `APPROVE` needs both halves run at the reviewed head, or both `Thermo` rows reading `not run`.",
    "Approve only once both halves have handed off.",
  ].join("\n");
  const worlds = new Worlds(tree, []);
  try {
    const hits = await checkNamed(
      "hand search 3: a gate on a tell main's engine makes unreachable"
    ).run(new Tree(CHECKOUT, new Map([[file, `${text}\n`]])), worlds);
    expect(
      hits.filter(({ at }) => at.startsWith(`${file}:`)).map(({ at, verdict }) => [at, verdict])
    ).toEqual([
      [`${file}:1`, "true"],
      [`${file}:2`, "instance"],
    ]);
  } finally {
    worlds.done();
  }
});

// Catches: a check that takes a setDefaultTimeout of any value for the bound, so a file whose rows
// run processes under one second passes; a bound set inside a block read as the file's; a `_`
// separator read as no number; and a call in a string or a comment read as a process run.
test("a test file that runs processes is held only by a top-level bound of 30 s or more", () => {
  const file = `${RIG}/planted.test.ts`;
  const spawn = 'const planted = spawnChecked(["true"]);';
  const planted: [string, string][] = [
    ["none", spawn],
    ["one second", `setDefaultTimeout(1_000);\n${spawn}`],
    ["just under", `setDefaultTimeout(29_999);\n${spawn}`],
    ["in a block", `describe("d", () => {\n  setDefaultTimeout(60_000);\n});\n${spawn}`],
    ["thirty seconds", `setDefaultTimeout(30000);\n${spawn}`],
    ["a minute", `setDefaultTimeout(60_000);\nconst run = () => Bun.spawnSync(["true"]);`],
    ["no process", `const code = 'execSync("true");';\n// ${spawn}`],
  ];
  expect(
    planted.map(([name, text]) => {
      const hit = defaultBounds(new Tree(CHECKOUT, new Map([[file, `${text}\n`]]))).find(
        ({ at }) => at === file || at.startsWith(`${file}:`)
      );
      return [name, hit?.at, hit?.verdict];
    })
  ).toEqual([
    ["none", `${file}:1`, "instance"],
    ["one second", `${file}:2`, "instance"],
    ["just under", `${file}:2`, "instance"],
    ["in a block", `${file}:4`, "instance"],
    ["thirty seconds", `${file}:2`, "true"],
    ["a minute", `${file}:2`, "true"],
    ["no process", file, "true"],
  ]);
});
