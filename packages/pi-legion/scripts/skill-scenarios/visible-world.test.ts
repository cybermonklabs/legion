import { expect, setDefaultTimeout, test } from "bun:test";
import { lstatSync, mkdirSync, readlinkSync, symlinkSync, writeFileSync } from "node:fs";
import path from "node:path";
import { skipWithout } from "@legion/test-fixtures/host-tools";
import { spawnWithin } from "@legion/test-fixtures/row-clock";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { fileRowLimit, SCRIPT_RUN } from "./test-limit";

// Each row runs visible-world.ts as a process of its own, and the view rows walk a real bubblewrap
// view: each test and hook gets the limit test-limit.ts computes from the bounds of what they wait
// for, and the fixture's removal of the reservations below runs within it.
setDefaultTimeout(fileRowLimit({ ...SCRIPT_RUN, part: "visible-world.ts" }));

// Each row's work directory, reserved here (the fixture's hook makes it before the rows run).
const works = {
  planted: temporaryDirectory("visible-world-planted", { real: true }),
  others: temporaryDirectory("visible-world-others", { real: true }),
  clean: temporaryDirectory("visible-world-clean", { real: true }),
  stray: temporaryDirectory("visible-world-stray", { real: true }),
  view: temporaryDirectory("visible-world-view", { real: true }),
  links: temporaryDirectory("visible-world-links", { real: true }),
  role: temporaryDirectory("visible-world-role", { real: true }),
};

// The view rows walk a real bubblewrap view, which needs bwrap and a user namespace it may make
// (AppArmor lets a user make one on some hosts and not on others). Read once, so one warning
// names what is missing for the whole file.
const bwrapMissing = skipWithout("bwrap");
const withoutBwrap =
  bwrapMissing || Bun.spawnSync(["bwrap", "--ro-bind", "/", "/", "true"]).exitCode !== 0;
if (withoutBwrap && !bwrapMissing)
  console.warn(
    "Skipped the view rows of visible-world.test.ts: bwrap cannot make its namespaces on this host."
  );

// A work directory laid out as rig.sh lays it out: runs and profiles of random names, and the map's
// links that name them (rig.sh's header, "the map"), with `files` in the run's directory. `runs`
// names the map's runs, each with its directory's id; the run itself is 0123456789abcdef.
const RUNS: readonly (readonly [string, string])[] = [
  ["reviewer-own-head-sibling-fails-ablate-head-1", "0123456789abcdef"],
  ["reviewer-own-head-ci-only-head-2", "fedcba9876543210"],
];
function runIn(work: string, files: Record<string, string>, runs = RUNS) {
  const run = path.join(work, "runs", "0123456789abcdef");
  mkdirSync(path.join(run, "cwd"), { recursive: true });
  mkdirSync(path.join(work, "maps", "profiles"), { recursive: true });
  mkdirSync(path.join(work, "maps", "runs"), { recursive: true });
  for (const [label, id] of [
    ["head", "1111111111111111"],
    ["ablate-head", "2222222222222222"],
  ] as const) {
    mkdirSync(path.join(work, "profiles", id), { recursive: true });
    writeFileSync(path.join(work, "profiles", id, "checkout"), `/checkouts/${id}\n`);
    symlinkSync(`../../profiles/${id}`, path.join(work, "maps", "profiles", label));
  }
  for (const [name, id] of runs)
    symlinkSync(`../../runs/${id}`, path.join(work, "maps", "runs", name));
  writeFileSync(path.join(run, "pane.env"), `HOME=${run}/home\nOMP_PROFILE=skills\n`);
  writeFileSync(
    path.join(run, "prompt.txt"),
    "Continue a usage page. Issue: LWEVAL-1. Pull request: #702.\n"
  );
  for (const [name, text] of Object.entries(files)) {
    mkdirSync(path.dirname(path.join(run, name)), { recursive: true });
    writeFileSync(path.join(run, name), text);
  }
  return run;
}
const search = (run: string) => {
  const result = spawnWithin({ ...SCRIPT_RUN, part: "visible-world.ts" }, [
    "bun",
    path.join(import.meta.dir, "visible-world.ts"),
    run,
  ]);
  return { code: result.exitCode, out: result.stdout.toString() };
};

// Catches: a search that reports clean on a run whose world names its label, because it reads
// neither a file's bytes nor its path.
test("a file in the run's tree that names the run's label is a hit, by its bytes and by its path", () => {
  const run = runIn(works.planted, {
    "cwd/notes.txt": "this run is ablate-head\n",
    "cwd/ablate-head.md": "x\n",
  });
  const { code, out } = search(run);
  expect(code).toBe(1);
  expect(out).toContain(`tree\t${run}/cwd/notes.txt\tlabel (own)\tablate-head\t`);
  expect(out).toContain(`tree\t${run}/cwd/ablate-head.md (its path)\tlabel (own)\tablate-head\t`);
});

// Catches: terms taken from the run's own name alone, so another case is no hit; and a case.txt
// beside the agent's working directory read as harmless.
test("another case's name, the word ablate and a case.txt are hits too", () => {
  const run = runIn(works.others, {
    "fixture.json": '{"case":"ci-only"}\n',
    "case.txt": "x\n",
    "tmp/a": "Ablate\n",
  });
  const { code, out } = search(run);
  expect(code).toBe(1);
  expect(out).toContain(`tree\t${run}/fixture.json\tcase\tci-only\t`);
  expect(out).toContain(`tree\t${run}/case.txt\tcase-file\tcase.txt\t`);
  expect(out).toContain(`tree\t${run}/tmp/a\tword\tablate\t`);
});

// Catches: a search that flags ordinary prose for a label that is a plain word (`head`). The run
// has no view file, so the view channel says UNKNOWN and only the file channels are read here.
test("a world that names nothing has no hit in its own files, and the word head in prose is no hit", () => {
  const run = runIn(works.clean, { "cwd/README.md": "Review the head of #702.\n" });
  const { out } = search(run);
  expect(out).toMatch(/^read: env 2, command 1, tree \d+/m);
  expect(
    out.split("\n").filter((line) => /^(env|command|tree|transcript|plugins|git)\t/.test(line))
  ).toEqual([]);
});

// Catches: a walk that names a run's case from some scenarios' patterns and not from every one the
// scorer names, so a ci-pass-and-fail run is walked as the scenario ci-pass-and-fail-<role> with no
// case, and neither its bare scenario nor its role is searched for. A plain-word case is searched in
// a rig name's shapes, as `-tester-`.
test("a ci-pass-and-fail run's scenario and its role are hits, as scenario (own) and case (own)", () => {
  const run = runIn(
    works.role,
    { "cwd/notes.txt": "this run is ci-pass-and-fail\n", "cwd/role.txt": "run-tester-1\n" },
    [["ci-pass-and-fail-tester-head-1", "0123456789abcdef"]]
  );
  const { code, out } = search(run);
  expect(out).toContain(`tree\t${run}/cwd/notes.txt\tscenario (own)\tci-pass-and-fail\t`);
  expect(out).toContain(`tree\t${run}/cwd/role.txt\tcase (own)\t-tester-\t`);
  expect(out).not.toContain("ci-pass-and-fail-tester\t");
  expect(code).toBe(1);
});

// Catches: a clean answer from a search that read nothing.
test("a run the map does not name is refused, not reported clean", () => {
  runIn(works.stray, {});
  const stray = path.join(works.stray, "runs", "9999999999999999");
  mkdirSync(stray);
  expect(search(stray).code).toBe(2);
});

/** A view file of the run, as launch writes it (<work>/views/<id>, NUL-separated bwrap arguments):
 *  the system's /usr and the links at / into it, so find runs, the run's directory, each of
 *  `binds`, and a /proc and /dev of its own. */
function viewFile(work: string, run: string, binds: readonly string[]) {
  const view = ["--ro-bind", "/usr", "/usr"];
  for (const dir of ["/bin", "/sbin", "/lib", "/lib32", "/lib64", "/libx32"]) {
    try {
      if (lstatSync(dir).isSymbolicLink()) view.push("--symlink", readlinkSync(dir), dir);
      else view.push("--ro-bind", dir, dir);
    } catch {
      // Not on this system.
    }
  }
  view.push("--bind", run, run);
  for (const bind of binds) view.push("--ro-bind", bind, bind);
  view.push("--proc", "/proc", "--dev", "/dev", "--unshare-pid", "--die-with-parent");
  mkdirSync(path.join(work, "views"), { recursive: true });
  writeFileSync(path.join(work, "views", path.basename(run)), `${view.join("\0")}\0`);
}

// Catches: a search that reads the run's own files and the names above it, and so reports clean
// while the agent's view holds, deep in a directory outside the run, a name that tells its case
// and another label; and a walk that cannot set the view up reported clean. The plant is four
// levels below the directory the view binds, where no listing of a directory's own names reaches.
test.skipIf(withoutBwrap)(
  "the view walk names a deep path of the agent's view that holds a case and a label, and a view it cannot walk is UNKNOWN",
  () => {
    const run = runIn(works.view, { "cwd/notes.txt": "Review the head of #702.\n" });
    const outside = path.join(works.view, "outside");
    const plant = path.join(outside, "a", "b", "c", "d", "sibling-fails-ablate-head-2");
    mkdirSync(plant, { recursive: true });
    viewFile(works.view, run, [outside]);
    const planted = search(run);
    expect(planted.out).toMatch(/^view: walked all \d+ names of the agent's view/m);
    expect(planted.out).toContain(`view\t${plant}\tcase (own)\tsibling-fails\t`);
    expect(planted.out).toContain(`view\t${plant}\tlabel (own)\tablate-head\t`);
    expect(planted.code).toBe(1);
    // The same view without the plant's directory: the walk still walks, and names nothing.
    viewFile(works.view, run, []);
    const clean = search(run);
    expect(clean.out).toMatch(/^view: walked all \d+ names of the agent's view/m);
    expect(clean.out.split("\n").filter((line) => line.startsWith("view\t"))).toEqual([]);
    // A view whose directory is gone: bwrap cannot set it up, and the walk is no clean answer.
    viewFile(works.view, run, [path.join(works.view, "gone")]);
    const broken = search(run);
    expect(broken.out).toMatch(/^UNKNOWN view \(view .*\): the walk said: bwrap: /m);
    expect(broken.code).toBe(3);
  }
);

// Catches: a search that reads every place a link of the run points at, the places the agent's
// view does not hold among them, so that a link mise leaves in a run's state to the operator's home
// reads all of the home (one such search held 36 GB before it was stopped), and one that names a
// file the agent cannot read as a place it could learn its label from.
test("the plugins channel reads a place a link points at only where the run's view holds it", () => {
  const run = runIn(works.links, {});
  const held = path.join(works.links, "held");
  const unheld = path.join(works.links, "unheld");
  for (const dir of [held, unheld]) {
    mkdirSync(dir);
    writeFileSync(path.join(dir, "notes.md"), "this label is ablate-head\n");
  }
  symlinkSync(held, path.join(run, "cwd", "plugins"));
  symlinkSync(unheld, path.join(run, "cwd", "home"));
  viewFile(works.links, run, [held]);
  const { out } = search(run);
  expect(out).toContain(`plugins\t${held}/notes.md\tlabel (own)\tablate-head\t`);
  expect(out).not.toContain(`${unheld}/notes.md`);
  expect(out).toMatch(/links to nothing the view holds 1\b/);
});
