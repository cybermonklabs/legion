import { afterAll, beforeAll, expect, test } from "bun:test";
import { readFileSync, rmSync } from "node:fs";
import * as path from "node:path";
import {
  brokenRelativeLinks,
  brokenSkillLinks,
  files,
  longSkillBodies,
  misnamedSkills,
  oversizedFiles,
  REPO_ROOT,
  skillLinks,
  stageSkills,
} from "@legion/pi-shared/test/skills-guard";

// The rules (`@legion/pi-shared/test/skills-guard`) over the skills this package stages into
// dist/skills: the partition scripts/pi-plugin-prepack.sh gives @sjawhar/pi-legion, staged here as
// its prepack stages it. Everything a Legion agent reads that can link into a skill is a linking
// root: the staged skills themselves and the daemon's prompts (its role prompts in roles/ and its
// overlays in go/). A `skill://` link resolves by name through Oh My Pi's discovery, so it is held to
// the repository's skills/, where both plugins' skills live: `skill://dispatch/…` from this
// partition is valid because the Envoy plugin, installed beside this one, ships it.
const repoSkillsRoot = path.join(REPO_ROOT, "skills");
const promptsRoot = path.join(REPO_ROOT, "packages/daemon/internal/prompts");
let staged: string;

beforeAll(() => {
  staged = stageSkills("@sjawhar/pi-legion");
});

afterAll(() => {
  rmSync(path.dirname(staged), { recursive: true, force: true });
});

test("every staged file is under Oh My Pi's spill threshold, so a skill:// read arrives whole", () => {
  expect(oversizedFiles(staged)).toEqual([]);
});

test("every skill body stays under 500 lines, its detail in references", () => {
  expect(longSkillBodies(staged)).toEqual([]);
});

test("every skill's frontmatter name is its directory's name", () => {
  expect(misnamedSkills(staged)).toEqual([]);
});

test("every skill://<name>/<path> link names a file that exists, and its #anchor a heading in it", () => {
  expect(brokenSkillLinks(skillLinks([staged, promptsRoot]), repoSkillsRoot)).toEqual([]);
});

test("every relative link resolves to a file inside the staged partition", () => {
  expect(brokenRelativeLinks(staged)).toEqual([]);
});

// Reads by filesystem path stop at 300 lines, so a worker reaches a reference only through a
// `skill://legion-worker/<path>` link from somewhere it reads.
test("every legion-worker reference is linked from a skill or a prompt", () => {
  const workerRoot = path.join(staged, "legion-worker");
  const linked = new Set(
    skillLinks([staged, promptsRoot])
      .filter(({ name }) => name === "legion-worker")
      .map(({ target }) => target)
  );
  const unlinked = files(path.join(workerRoot, "references"))
    .map((file) => path.relative(workerRoot, file))
    .filter((reference) => !linked.has(reference));
  expect(unlinked).toEqual([]);
});

// Whitespace is collapsed before a match, as shipped-agents.test.ts collapses it, so a rule read
// out of a guide survives a rewrap of the file it is in.
const collapsed = (file: string) => readFileSync(file, "utf8").replace(/\s+/g, " ");

// Only a review that requests changes counts a review round (the daemon's recordRound): a tester's
// fail and a move back, the architect's from `accepting` among them, count none, as the architect's
// prompt parts say (`go/architect-common.md`). The two guides that say what the cap counts say so
// too, and each sentence that counted every move back is gone. A move made at a conflicting head
// raises no pair, whoever made it (recordPass), so the concepts page says that of any such move.
// The guides are read where they live in the repository.
test("the guides say only a review that requests changes counts a round", () => {
  const guide = (relative: string, duties: RegExp[]) => {
    const text = collapsed(path.join(REPO_ROOT, "docs/site/src/content/docs/legion", relative));
    return duties.filter((duty) => !duty.test(text)).map((duty) => `${relative} lacks ${duty}`);
  };
  expect([
    ...guide("concepts.md", [
      /Only a review that requests changes counts a round: a tester's fail and a move back by a worker or the architect count none\./,
      /A request for changes made while the record holds the head conflicting with its base is a conflict round/,
      /a move made while the head conflicts with its base raises no pair/,
      /^(?![\s\S]*Every move back to an earlier phase counts a round)/,
    ]),
    ...guide("troubleshooting.md", [
      /only a review that requests changes raises the count/,
      /^(?![\s\S]*every move back to an earlier phase raises the count)/,
    ]),
  ]).toEqual([]);
});

// skills/AGENTS.md bounds a skill's SKILL.md and each of its references at 150 lines and 8 KB,
// counted here as 8,000 bytes. Every file of the Legion role skills is held to it, so a file that
// grows past it moves its detail one level down before it ships.
test("every file of the Legion role skills fits the skill budget", () => {
  const budgeted = files(staged).filter((file) =>
    /^legion-[a-z-]+\/(SKILL|references\/[^/]+)\.md$/.test(path.relative(staged, file))
  );
  expect(budgeted.length).toBeGreaterThan(20);
  const over = budgeted.flatMap((file) => {
    const text = readFileSync(file, "utf8");
    const bytes = Buffer.byteLength(text);
    const lines = text.split("\n").length - (text.endsWith("\n") ? 1 : 0);
    return bytes > 8_000 || lines > 150
      ? [`${path.relative(staged, file)}: ${lines} lines, ${bytes} bytes`]
      : [];
  });
  expect(over).toEqual([]);
});
