import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import { chmod, mkdir, mkdtemp, readdir, rm, writeFile } from "node:fs/promises";
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

  // Each case sets ANTHROPIC_API_KEY, which the command refuses to run without, and
  // ANTHROPIC_BASE_URL, unless `change` takes one out.
  async function command(
    out: string,
    change: Record<string, string | undefined> = {}
  ): Promise<{ code: number; stderr: string }> {
    const env: Record<string, string> = {};
    const given: Record<string, string | undefined> = {
      ...process.env,
      DISPATCH_TEST_DATABASE_URL: undefined,
      ANTHROPIC_BASE_URL: "https://gateway.example",
      ANTHROPIC_API_KEY: "a-key",
      ...change,
    };
    for (const [name, value] of Object.entries(given)) {
      if (value !== undefined) env[name] = value;
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

  test("is 2 with no ANTHROPIC_API_KEY, names it, and starts no stack and no agent", async () => {
    const out = join(scratch, "no-key");
    const temporary = join(scratch, "no-key-tmp");
    await mkdir(out);
    await mkdir(temporary);
    // With a database named, only the refusal keeps the command from starting a stack in --out.
    const { code, stderr } = await command(out, {
      ANTHROPIC_API_KEY: undefined,
      DISPATCH_TEST_DATABASE_URL: "postgres://127.0.0.1:1/none",
      TMPDIR: temporary,
    });
    expect(stderr).toContain("ANTHROPIC_API_KEY is not set");
    expect(code).toBe(2);
    expect(await readdir(out)).toEqual([]);
    expect(await readdir(temporary)).toEqual([]);
  }, 30_000);

  // omp lists and calls Anthropic's models with the key alone, so a missing gateway is no refusal:
  // the command goes on to the stack, which this case gives no database.
  test("goes past the model's variables with no ANTHROPIC_BASE_URL", async () => {
    const out = join(scratch, "no-gateway");
    await mkdir(out);
    const { code, stderr } = await command(out, { ANTHROPIC_BASE_URL: undefined });
    expect(stderr).not.toContain("ANTHROPIC_BASE_URL");
    expect(stderr).toContain("DISPATCH_TEST_DATABASE_URL must name the PostgreSQL server");
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
