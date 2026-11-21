import { expect, test } from "bun:test";
import { writeFileSync } from "node:fs";
import { join } from "node:path";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";

const scratch = temporaryDirectories("within-row-test");
const helper = join(import.meta.dir, "within-row.ts");

// A file of two rows, run by a `bun test` of its own. The first starts a child that ends only when
// it is killed, which the first row's 500 ms guard does, and asserts on its exit status after its
// `await`, as the rows of github-apps.test.ts do; WAIT is that wait as the row writes it. The second
// row waits for the same exit and then for one turn of the event loop, so the first row's body,
// queued on that exit before it, has run (or stayed at its `await`) by the time the file ends.
function twoRows(wait: string): string {
  return `import { expect, test } from "bun:test";
import { withinRow } from ${JSON.stringify(helper)};
let exited: Promise<unknown> = Promise.resolve();
test("a row its guard ends", async () => {
  const child = Bun.spawn(["sh", "-c", "exec sleep 600"], { stdout: "pipe", stderr: "pipe" });
  const all = Promise.all([new Response(child.stdout).text(), child.exited]);
  exited = all;
  const [, code] = await ${wait};
  expect(code).toBe(0);
}, 500);
test("the row after it", async () => {
  await exited;
  await new Promise((done) => setImmediate(done));
});
`;
}

function run(wait: string) {
  const dir = scratch();
  writeFileSync(join(dir, "rows.test.ts"), twoRows(wait));
  const ran = Bun.spawnSync([process.execPath, "test", "./rows.test.ts"], {
    cwd: dir,
    env: { PATH: process.env.PATH ?? "", HOME: dir },
    stdout: "pipe",
    stderr: "pipe",
  });
  return `${ran.stdout.toString()}${ran.stderr.toString()}`;
}

test("a row its guard ended runs nothing after its wait, so no error lands outside it", () => {
  const out = run("withinRow(all)");
  expect(out).toContain("this test timed out after 500ms");
  expect(out).toContain("(pass) the row after it");
  expect(out).not.toContain("Unhandled error");
  expect(out).toMatch(/^ 1 pass$/m);
  expect(out).toMatch(/^ 1 fail$/m);
  expect(out).not.toMatch(/^ \d+ errors?$/m);
});

// The control: the same rows awaiting the child directly. The killed child's exit resumes the
// first row after its guard ended it, and its assertion throws where no row runs.
test("without it, the ended row's assertion on the killed child throws outside the row", () => {
  const out = run("all");
  expect(out).toContain("this test timed out after 500ms");
  expect(out).toContain("Unhandled error");
  expect(out).toContain("Received: 143");
});
