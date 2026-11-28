import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import { chmod, mkdir, mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";

// The header of run.ts says 2 when the command cannot run, and 1 only when a review failed. Each
// case below stops before any agent runs, so a 1 would tell the reader that a reviewer failed.
describe("the exit status of a command that cannot run", () => {
  const RUN = join(import.meta.dir, "..", "run.ts");
  let scratch = "";
  let omp = "";

  beforeAll(async () => {
    scratch = await mkdtemp(join(tmpdir(), "spec-review-eval-exit-"));
    omp = join(scratch, "omp");
    await writeFile(omp, "#!/bin/sh\nexit 0\n", "utf8");
    await chmod(omp, 0o755);
  });
  afterAll(async () => {
    await rm(scratch, { recursive: true, force: true });
  });

  async function command(out: string): Promise<{ code: number; stderr: string }> {
    const env: Record<string, string> = {};
    for (const [name, value] of Object.entries(process.env)) {
      if (value !== undefined && name !== "DISPATCH_TEST_DATABASE_URL") env[name] = value;
    }
    const child = Bun.spawn({
      cmd: [process.execPath, RUN, "--omp", omp, "--out", out],
      env,
      stdin: "ignore",
      stdout: "ignore",
      stderr: "pipe",
    });
    const [stderr, code] = await Promise.all([new Response(child.stderr).text(), child.exited]);
    return { code, stderr };
  }

  test("is 2 with no DISPATCH_TEST_DATABASE_URL", async () => {
    const out = join(scratch, "no-database");
    await mkdir(out);
    const { code, stderr } = await command(out);
    expect(stderr).toContain("DISPATCH_TEST_DATABASE_URL");
    expect(code).toBe(2);
  }, 30_000);

  test("is 2 with an --out that an earlier run used, and names what it holds", async () => {
    const out = join(scratch, "used");
    await mkdir(join(out, "run-1", "stack"), { recursive: true });
    const { code, stderr } = await command(out);
    expect(stderr).toContain(`--out ${out} is not empty: it holds run-1;`);
    expect(code).toBe(2);
  }, 30_000);

  test("is 2 with an --out that is a file, in the command's own words", async () => {
    const out = join(scratch, "a-file");
    await writeFile(out, "not a directory\n", "utf8");
    const { code, stderr } = await command(out);
    expect(stderr.trim()).toBe(`--out ${out} is not a directory`);
    expect(code).toBe(2);
  }, 30_000);
});
