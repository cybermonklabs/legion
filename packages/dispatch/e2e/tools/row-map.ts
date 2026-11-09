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
 *  - **A row that moved** is a row of a `*.stub.e2e.ts` file. Its assertions are the ones it
 *    had: a move renames the file and lifts the shared seeding into the fixture, and changes no
 *    line of what the row asserts, which is what makes "before and after" a mechanical claim
 *    rather than a prose one. The reason it may be stubbed is its file's own header comment.
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
import { fileURLToPath } from "node:url";
import { listing } from "../__tests__/playwright-listing";
import {
  pullRequestLaneProjects,
  rowsOfListing,
  stubProjectNames,
  tagged,
} from "../configurations";
import { realStackReasons } from "../stub/real-stack-reasons";

// A listing names a file relative to the config's own directory, which is `e2e/`.
const suiteDirectory = fileURLToPath(new URL("..", import.meta.url));
const mapPath = fileURLToPath(new URL("../row-map.md", import.meta.url));
const recordings = fileURLToPath(new URL("../__recordings__/", import.meta.url));

const prTag = tagged("pr");

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

/** The `// @pr: ` reason written above the row `title` names in `file`: why a pull request needs
 *  to see that row's failure first. The comment is read from the source, which the listing
 *  carries none of, and the row is found by its own title - the last segment of the listing's
 *  title path, since a `describe` block's title is in the path and not in the `test(` call.
 *
 *  The nearest `// @pr: ` line above the call is the row's, so one line can cover a run of rows
 *  below it where one reason serves the file. A row the source holds no line for answers the
 *  empty string rather than failing: the generation is a map, and a missing reason reads as a
 *  blank cell a reviewer can see. */
function tagReason(file: string, title: string): string {
  const own = title.split(" › ").at(-1) ?? title;
  let reason = "";
  for (const line of readFileSync(`${suiteDirectory}${file}`, "utf8").split("\n")) {
    const written = /^\s*\/\/ @pr:\s*(.*)$/.exec(line);
    if (written !== null) {
      reason = written[1].trim();
      continue;
    }
    if (line.includes(own)) return reason;
  }
  return "";
}

function table(header: readonly string[], rows: readonly (readonly string[])[]): string {
  return [
    `| ${header.join(" | ")} |`,
    `| ${header.map(() => "---").join(" | ")} |`,
    ...rows.map((row) => `| ${row.map((cell) => cell.replaceAll("|", "\\|")).join(" | ")} |`),
  ].join("\n");
}

// The one `playwright test --list` subprocess of this package, which `e2e/__tests__/
// playwright-listing.ts` holds: its argv, its working directory, the environment floor it
// runs under and what a non-zero exit says are written once there.
const rows = rowsOfListing(await listing("e2e/playwright.config.ts", {}));
const moved = rows.filter((row) => stubProjectNames.includes(row.project));
const real = rows.filter((row) => !stubProjectNames.includes(row.project));

// One entry per moved row, not one per project it runs in: a row runs in `stub` or in
// `stub-phone`, never both, so the two are already disjoint.
const movedLines = moved
  .map((row) => [
    `\`e2e/${row.file}\``,
    row.title,
    "unchanged - the row kept its title and every assertion it had",
    stubReason(row.file),
  ])
  .sort((a, b) => a[0].localeCompare(b[0]) || a[1].localeCompare(b[1]));

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
const realLines = realFiles.map((file) => {
  const held = real.filter((row) => row.file === file);
  const onLane = held.filter((row) => prTag.test(row.title));
  return [
    `\`e2e/${file}\``,
    String(new Set(held.map((row) => row.title)).size),
    String(new Set(onLane.map((row) => row.title)).size),
    realStackReasons[file],
  ];
});

// The third class: a row the pull-request lane no longer schedules. It runs on the full lane,
// which ci.yaml runs for every merge group and the nightly runs daily.
const displaced = [...new Set(real.filter((row) => !prTag.test(row.title)).map((row) => row.file))]
  .sort()
  .map((file) => {
    const held = real.filter((row) => row.file === file && !prTag.test(row.title));
    return [
      `\`e2e/${file}\``,
      String(new Set(held.map((row) => row.title)).size),
      realStackReasons[file],
    ];
  });

// The real-stack rows the pull-request lane keeps, each with the `// @pr:` line above it: the
// half of the lane that is not the stubbed layer, and the one a reviewer judges for breadth.
const kept = [
  ...new Map(
    real
      .filter((row) => prTag.test(row.title))
      .map((row) => [`${row.file}\u0000${row.title}`, row] as const)
  ).values(),
]
  .sort((a, b) => a.file.localeCompare(b.file) || a.title.localeCompare(b.title))
  .map((row) => [`\`e2e/${row.file}\``, row.title, tagReason(row.file, row.title)]);

// The pull-request lane is four projects and the tag, which is exactly what the shard job
// runs: a tagged row in `webkit` or `firefox` is on the full lane like any other.
const laneRows = rows.filter(
  (row) => pullRequestLaneProjects.includes(row.project) && prTag.test(row.title)
);
const document = `# The row map of dispatch://INFRA-1407

Generated by \`bun e2e/tools/row-map.ts --write\`; \`e2e/__tests__/row-map.test.ts\` fails when
this file and a fresh generation differ. Every count below is read from
\`playwright test --list\`, not written by hand.

The suite holds ${rows.length} rows over every project. ${moved.length} of them run on the
stubbed layer, ${real.length} against the real stack. The pull-request lane schedules
${laneRows.length} of them — the rows tagged \`@pr\` in \`chromium\`, \`iphone\`, \`stub\` and
\`stub-phone\` — and every other row runs on the full lane, which the merge queue and the nightly
run, so \`main\` is gated as it was.

## The rows that moved

Each kept its title and every assertion it had; what changed is the file it lives in, the
fixture it takes and the server behind it. The reason is its file's own header.

${table(["file", "row", "assertion before and after", "why a recording can answer it"], movedLines)}

## The rows that stayed, and the server behaviour they depend on

One line per file, which is the granularity the reasons are written at
(\`e2e/stub/real-stack-reasons.ts\`). "on the lane" is how many of that file's rows carry
\`@pr\` and so run on a pull request.

${table(["file", "rows", "on the lane", "the server behaviour its rows depend on"], realLines)}

## The rows that left the pull-request lane

These run on the full lane alone: at the merge queue, which gates \`main\`, and on the nightly.
What they lose is feedback on the pull request itself.

${table(["file", "rows off the lane", "why it was not kept"], displaced)}

## The real-stack rows the pull-request lane keeps

These pay a Go server, a Postgres, three fakes, a sign-in and a database reset on every pull
request, so each is here because a pull request needs to see its failure first. The reason is
the \`// @pr: \` line above the row.

${table(["file", "row", "why the lane keeps it"], kept)}
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
  console.error(
    "Literals a stubbed row asserts that its own recording also holds. Most are the row's own " +
      "seeding; read each for a value the server chose rather than the row.\n"
  );
  for (const file of files) {
    const source = readFileSync(`${suiteDirectory}${file}`, "utf8");
    const family = file.replace(/\.stub\.e2e\.ts$/, "");
    const recorded = new Map<string, string>();
    try {
      for (const name of new Bun.Glob("*.har").scanSync(`${recordings}${family}/`)) {
        recorded.set(name, readFileSync(`${recordings}${family}/${name}`, "utf8"));
      }
    } catch {
      // A family with no recording directory holds no row of this listing any more.
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
