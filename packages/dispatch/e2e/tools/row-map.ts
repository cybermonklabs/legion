/** `bun e2e/tools/row-map.ts [--write]`: the map of what moved to the stubbed layer, what stayed
 *  on the real stack and what left the pull-request lane, which `e2e/row-map.md` holds and the
 *  pull request links.
 *
 *  dispatch://INFRA-1407's k2 asks, for every row the change moved, what it asserted before and
 *  what it asserts after, and for every row it did not move, the server behaviour its assertion
 *  depends on. At 994 declared rows the second half is hundreds of lines, and GitHub refuses a
 *  pull-request body over 65,536 characters, so the map is a committed file the body links and
 *  `e2e/__tests__/row-map.test.ts` fails when it and a fresh generation differ.
 *
 *  Where each half comes from:
 *
 *  - **A row that moved** is a row of a `*.stub.e2e.ts` file. A move renames the file, lifts
 *    the shared seeding into the fixture and adds the lane tag to the title, and usually
 *    changes no line of what the row asserts, which is what makes "before and after" a
 *    mechanical claim rather than a prose one. A row where something did change says so, from
 *    `e2e/stub/moved-row-notes.ts`. The reason it may be stubbed is its file's own header
 *    comment.
 *  - **A row that stayed** carries its file's reason from `e2e/stub/real-stack-reasons.ts`, and
 *    a file the listing names that has none there fails the generation.
 *  - **A row that left the pull-request lane** is a row with no `@pr` tag. It runs on the full
 *    lane, which the merge queue and the nightly run, so it still gates `main`.
 *
 *  `--search` also reads each moved row's recording for the literals its assertions name and
 *  prints the hits, which is k2's negative control made mechanical: a row whose asserted text is
 *  in its own recording is reading back something the server wrote rather than something the
 *  page computed, and a reviewer reads that short list rather than hundreds of lines of prose.
 */
import { readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { listing, projectArguments } from "../__tests__/playwright-listing";
import { clockProjectNames, rowsOfListing, stubProjectNames } from "../configurations";
import { movedRowNotes, unchangedByTheMove } from "../stub/moved-row-notes";
import { realStackReasons } from "../stub/real-stack-reasons";
import {
  committedRecordings,
  familyOf,
  recordingFamily,
  recordingFor,
  recordingsUnder,
} from "../stub/recordings";

// A listing names a file relative to the config's own directory, which is `e2e/`.
const suiteDirectory = fileURLToPath(new URL("..", import.meta.url));
const mapPath = fileURLToPath(new URL("../row-map.md", import.meta.url));

/** The first paragraph of a stub spec file's header comment, which says why its rows may be
 *  stubbed. A file with no header comment fails the generation: the reason is the whole point. */
function stubReason(file: string): string {
  const source = readFileSync(`${suiteDirectory}${file}`, "utf8");
  const header = /^\/\*\*([\s\S]*?)\*\//.exec(source);
  if (header === null) {
    throw new Error(
      `${file} has no header comment, so the map has no reason for the rows it holds. Write one ` +
        "saying what its rows read and why a recording can answer it."
    );
  }
  return header[1]
    .split("\n")
    .map((line) => line.replace(/^\s*\*? ?/, "").trimEnd())
    .join(" ")
    .replace(/\s+/g, " ")
    .trim();
}

function table(header: readonly string[], rows: readonly (readonly string[])[]): string {
  return [
    `| ${header.join(" | ")} |`,
    `| ${header.map(() => "---").join(" | ")} |`,
    ...rows.map((row) => `| ${row.map((cell) => cell.replaceAll("|", "\\|")).join(" | ")} |`),
  ].join("\n");
}

// The two `playwright test --list` subprocesses this map is read from, started together so the
// second collects while the first does. Each runs through `e2e/__tests__/playwright-listing.ts`,
// which holds the argv, the working directory, the environment floor and what a non-zero exit
// says. Measured at this head on this two-core pod: the default listing alone takes about three
// and a half seconds, the wall-clock one about one, and the two together about four.
//
// The second names the three wall-clock projects (`clockProjectNames`, e2e/configurations.ts).
// They carry `default: false` since dispatch://INFRA-1280, so a listing that names no project
// holds no row of `agent-workspace-timing.e2e.ts`, `heavy-bodies.e2e.ts` or
// `nested-directive.e2e.ts` - 51 rows of those three files in this head's wall-clock listing -
// and with the default listing alone this generation refused outright: "the listing names no
// real-stack row in them: heavy-bodies.e2e.ts, nested-directive.e2e.ts", measured by running
// the tool before the second listing was added, when those two were the whole of the set.
// Neither listing names a process count, so both run one Dispatch process, which is
// the only mode whose project list declares the wall-clock projects at all.
const [defaultListing, clockListing] = await Promise.all([
  listing("e2e/playwright.config.ts", {}),
  listing("e2e/playwright.config.ts", {}, projectArguments(clockProjectNames)),
]);
const rows = [...rowsOfListing(defaultListing), ...rowsOfListing(clockListing)];
const moved = rows.filter((row) => stubProjectNames.includes(row.project));
const real = rows.filter((row) => !stubProjectNames.includes(row.project));

// One entry per moved row and not one per project it runs in: `stub` runs every stubbed row
// and `stub-phone` runs the `@phone` ones again at a phone's width, so each of those rows is
// listed twice by the listing and once here. What the map says of a row - the file it lives
// in, what the move did to what it asserts, and why a recording can answer it - is true of the
// row rather than of the viewport it ran at.
//
// The usual sentence is that the move touched no line of what the row asserts, which is what
// k2 is read from. A row where that is false carries its own note instead
// (`e2e/stub/moved-row-notes.ts`), because a reviewer reads this map rather than the diff and
// a row the usual sentence covers is a row nobody looks at.
const noteKey = (row: { file: string; title: string }): string => `${row.file} › ${row.title}`;
const movedLines = [
  ...new Map(
    moved.map((row) => [
      noteKey(row),
      [
        `\`e2e/${row.file}\``,
        row.title,
        movedRowNotes[noteKey(row)] ?? unchangedByTheMove,
        stubReason(row.file),
      ],
    ])
  ).values(),
].sort((a, b) => a[0].localeCompare(b[0]) || a[1].localeCompare(b[1]));

// A note for a row the listing does not name reads as a current account of a row that is gone
// or renamed, which is the same fault the map itself exists to prevent. The other direction
// cannot be checked here: nothing in this checkout holds a row's assertions before the move,
// so whether a row needs a note is a person's reading.
const named = new Set(moved.map(noteKey));
const strayNotes = Object.keys(movedRowNotes).filter((key) => !named.has(key));
if (strayNotes.length > 0) {
  throw new Error(
    "e2e/stub/moved-row-notes.ts holds a note for these rows, and the listing names no moved " +
      `row of that file and title: delete each entry, or correct its key.\n  ${strayNotes.join("\n  ")}`
  );
}

// Every committed recording belongs to a row the listing still names and expects to run.
// Nothing else on a pull request checks this: record mode writes the path of the row it is
// running and deletes nothing, so a row that is renamed or removed leaves its `.har` and
// `.scene.json` behind, the contract test goes on reading them because they are still valid
// bodies, and the pull request merges green. The first nightly on `main` then re-records,
// finds a committed file no row wrote, and fails - with the full lane in that same run red
// beside it. This is the one-way half of that comparison, and it needs no harness: the
// listing names the rows, and `recordingFor` is the same rule the fixture writes by.
//
// A row the listing marks `skipped` owns nothing, because it will not run: that is a row
// skipped where it is declared - `test.fixme`, which is how a flaky row is parked,
// `test.skip(title, fn)`, a `describe.skip`, or a describe-level `test.skip(true)` or
// `test.fixme()`. Each is decided when the file is read, so the row runs no fixture at all,
// and the replay's own guard cannot see it; this is where it is caught instead.
//
// What is left to the replay is the row the listing cannot read: one that skips itself inside
// its body, and one that has stopped taking `scene`. Both are listed and expected to run, so
// they own their file here, and only the run knows otherwise (`deadRecordingGuard`,
// `e2e/stub/stub-test.ts`). A describe-level modifier whose condition is a function of a
// worker fixture is seen by neither: the listing marks its rows `passed`, and Playwright runs
// it before any test-scoped fixture. `packages/dispatch/AGENTS.md`, "The stubbed layer", says
// so, and every stubbed row skips inside its own body.
const owned = new Set(
  moved.filter((row) => row.expectedStatus !== "skipped").map((row) => recordingFor(row, ".har"))
);
const unowned = recordingsUnder(committedRecordings).filter((path) => !owned.has(path));
if (unowned.length > 0) {
  throw new Error(
    "These committed recordings belong to no row that the listing names and expects to run, " +
      "so the next re-record run would report each of them as written by no row any more. " +
      "Delete each, with the `.scene.json` beside it, or restore the row that wrote it - a " +
      "row the listing marks skipped, `test.fixme` among them, writes nothing while it stays " +
      `so:\n  ${unowned.join("\n  ")}`
  );
}

// One entry per real-stack file, which is the granularity the reasons are written at, with the
// rows it still holds. A file the listing names and the reasons do not is a gap, not a blank.
const realFiles = [...new Set(real.map((row) => row.file))].sort();
const missing = realFiles.filter((file) => realStackReasons[file] === undefined);
if (missing.length > 0) {
  throw new Error(
    `These files hold real-stack rows and no reason in e2e/stub/real-stack-reasons.ts, so the ` +
      `map would not say why their rows did not move:\n  ${missing.join("\n  ")}`
  );
}
// And the other direction, so the list cannot keep a reason for a file that is gone or whose
// every row moved: such an entry reads as a file the suite still has and tells a reader
// something that is no longer true.
const unused = Object.keys(realStackReasons).filter((file) => !realFiles.includes(file));
if (unused.length > 0) {
  throw new Error(
    `e2e/stub/real-stack-reasons.ts holds a reason for these files, and the listing names no ` +
      `real-stack row in them: delete each entry.\n  ${unused.join("\n  ")}`
  );
}
const realLines = realFiles.map((file) => [`\`e2e/${file}\``, realStackReasons[file]]);

const document = `# The row map of dispatch://INFRA-1407

Generated by \`bun e2e/tools/row-map.ts --write\` from \`playwright test --list\`;
\`e2e/__tests__/row-map.test.ts\` fails when this file and a fresh generation differ.

It carries no counts, deliberately. Every tree that works on this suite adds and removes rows,
and a map that counted them would be red on each of their pull requests and on every merge of
one into this branch, for a change that has nothing to do with what this map says. What it
names instead is stable under that: the rows this change moved, and the files whose rows
stayed with the server behaviour those rows depend on, which is what k2 is read from. The
real-stack rows the pull-request lane keeps are not listed either, for the same reason - every
tree that tags or retitles one would move this file - and each of them carries its own
\`// @pr: \` reason in the source, beside the tag, which is where a reviewer reads it. The
counts a reader wants are measured rather than listed: they are in k1's recorded result and in
the pull request's body.

A row added to a file this map already names changes nothing here. A **new spec file** does:
its rows need a line in \`e2e/stub/real-stack-reasons.ts\` saying what the server decides for
them, and the generation refuses to write a map without one.

## The rows that moved

Each row's third column says what the move did to what it asserts. What a move changes is the
file the row lives in, the fixture it takes, the server behind it, and the \` @pr\` the title
gained for the lane axis; usually it touches no line of what the row asserts, which is what
makes "before and after" a mechanical claim rather than a prose one, and a row where something
did change says so in that column. The reason a recording can answer it is its file's own
header.

${table(["file", "row", "assertion before and after", "why a recording can answer it"], movedLines)}

## The rows that stayed, and the server behaviour they depend on

One line per file, which is the granularity the reasons are written at
(\`e2e/stub/real-stack-reasons.ts\`). Every row of these files that the last table does not name
runs on the full lane alone: at the merge queue, which gates \`main\`, and on the nightly. What
such a row loses is feedback on the pull request itself.

${table(["file", "the server behaviour its rows depend on"], realLines)}
`;

if (process.argv.includes("--write")) {
  writeFileSync(mapPath, document);
  console.log(`wrote ${mapPath}`);
} else {
  process.stdout.write(document);
}

if (process.argv.includes("--search")) {
  // k2's negative control, made mechanical. For each stub spec file, the literals its rows
  // assert that its own recordings also hold.
  //
  // What a hit means, and what it does not. Most hits are the file's own seeding read back: a
  // row that seeds "Review this tablet issue." and asserts the page shows it will find that
  // text in its recording, and that is the layer working. A hit is worth a look when the
  // literal is something **the server wrote rather than the row** - a key it minted, a count it
  // computed, a message it chose - because then the recording answers the claim and the
  // server's own rule is no longer exercised. The tool cannot tell the two apart, so it prints
  // the list and a reviewer reads it; that is the division k2 asks for.
  const literal = /(?:toHaveText|toContainText|getByText|toHaveValue)\(\s*"([^"]{4,})"/g;
  const files = [...new Set(moved.map((row) => row.file))].sort();
  const committed = recordingsUnder(committedRecordings);
  if (committed.length === 0) {
    throw new Error(
      `There are no recordings under ${committedRecordings}, so this search would read nothing and ` +
        "report nothing. Record them with `bun run e2e:stub:record` in packages/dispatch."
    );
  }
  console.error(
    "Literals a stubbed row asserts that its own recording also holds. Most are the row's own " +
      "seeding; read each for a value the server chose rather than the row.\n"
  );
  for (const file of files) {
    const source = readFileSync(`${suiteDirectory}${file}`, "utf8");
    const family = familyOf(file);
    // A row that runs in both stubbed projects has a recording under each, so a family's
    // recordings are every committed path the layout reads that family out of.
    const recorded = new Map<string, string>();
    for (const name of committed.filter((path) => recordingFamily(path) === family)) {
      recorded.set(name, readFileSync(join(committedRecordings, name), "utf8"));
    }
    const hits = new Set<string>();
    for (const match of source.matchAll(literal)) {
      for (const [name, text] of recorded) {
        if (text.includes(match[1])) {
          hits.add(`${JSON.stringify(match[1])} in ${name}`);
          break;
        }
      }
    }
    if (hits.size > 0) {
      console.error(`e2e/${file}`);
      for (const hit of [...hits].sort()) console.error(`  ${hit}`);
    }
  }
}
