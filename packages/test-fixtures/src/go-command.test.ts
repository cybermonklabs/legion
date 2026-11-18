import { expect, test } from "bun:test";
import { rmSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { goOutput, goOutputSync } from "./go-command";
import { temporaryDirectories } from "./temporary-directories";

const makeDirectory = temporaryDirectories("test-fixtures-go-command");

const DOWNLOADING = "go: downloading example.com/stalled v1.0.0";

/**
 * A stand-in go, written into `dir`, that writes the line a module download starts with and then
 * waits, as a stalled download does, until the file `hold` is gone (a minute at most).
 */
function stalledGo(dir: string, hold: string): void {
  writeFileSync(hold, "");
  writeFileSync(
    join(dir, "go"),
    `#!/bin/sh\necho '${DOWNLOADING}' >&2\n` +
      `for _ in $(seq 1 600); do\n  [ -e '${hold}' ] || exit 1\n  sleep 0.1\ndone\nexit 1\n`,
    { mode: 0o755 }
  );
}

/**
 * What a process that calls the helper with the stalled go first on PATH writes on its standard
 * error while go waits: read until the downloading line arrives, or for 20 seconds at most. go
 * cannot return before the hold file goes, which is after the read, so anything read reached the
 * stream as go wrote it and not in a message the helper builds once go exits. The caller is then
 * killed, as a test's timeout or a job's would end it.
 */
async function stderrWhileGoWaits(call: string): Promise<string> {
  const dir = makeDirectory("stalled");
  const hold = join(dir, "hold");
  stalledGo(dir, hold);
  const caller = join(dir, "caller.ts");
  writeFileSync(
    caller,
    `import { goOutput, goOutputSync } from ${JSON.stringify(join(import.meta.dir, "go-command.ts"))};\n${call}\n`
  );
  const child = Bun.spawn([process.execPath, caller], {
    cwd: dir,
    // TMPDIR is the test's own directory: goOutputSync makes its scratch directory there, and the
    // SIGKILL below ends the caller before its cleanup runs, so the directory goes with `dir`
    // rather than staying in the caller's temporary directory.
    env: { ...process.env, PATH: pathWith(dir), TMPDIR: dir },
    stdout: "ignore",
    stderr: "pipe",
  });
  const reader = child.stderr.getReader();
  const decoder = new TextDecoder();
  // The bound on a read that never sees the line, as a helper that hands go's standard error on
  // only once go exits would leave it, so its failure is this test's assertion and not the
  // runner's timeout; the line's arrival ends the wait well before it.
  const deadline = AbortSignal.timeout(20_000);
  const { promise: expired, resolve: expire } = Promise.withResolvers<undefined>();
  deadline.addEventListener("abort", () => expire(undefined));
  let seen = "";
  try {
    while (!seen.includes(DOWNLOADING)) {
      const next = await Promise.race([reader.read(), expired]);
      if (next === undefined || next.done) break;
      seen += decoder.decode(next.value, { stream: true });
    }
    return seen;
  } finally {
    child.kill("SIGKILL");
    await child.exited;
    // The stand-in go outlives the caller until the hold file goes. goOutput's case starts it
    // directly; goOutputSync's starts bash, which runs it piped into tee (go-command.ts's TEE),
    // and bash and tee end when go does.
    rmSync(hold);
    await reader.cancel();
  }
}

test("goOutput hands a stalled go's standard error on before go returns", async () => {
  expect(
    await stderrWhileGoWaits(
      `void goOutput(["build", "."], { cwd: process.cwd() }); void goOutputSync;`
    )
  ).toContain(DOWNLOADING);
}, 30_000);

test("goOutputSync hands a stalled go's standard error on before go returns", async () => {
  expect(
    await stderrWhileGoWaits(`goOutputSync(["build", "."], { cwd: process.cwd() }); void goOutput;`)
  ).toContain(DOWNLOADING);
}, 30_000);

/** `dir` ahead of this process's PATH. */
function pathWith(dir: string): string {
  const { PATH = "" } = process.env;
  return `${dir}:${PATH}`;
}

/** A stand-in go in `dir` that writes to both streams and exits 3. */
function failingGo(dir: string): string {
  writeFileSync(join(dir, "go"), "#!/bin/sh\necho to-stdout\necho to-stderr >&2\nexit 3\n", {
    mode: 0o755,
  });
  return pathWith(dir);
}

test("goOutput returns standard output and fails naming the command, with both streams", async () => {
  const dir = makeDirectory("failing");
  const env = { ...process.env, PATH: failingGo(dir) };
  const failed = goOutput(["version"], { cwd: dir, env });
  await expect(failed).rejects.toThrow(`go version in ${dir} exited 3:\nto-stdout\nto-stderr\n`);
  writeFileSync(join(dir, "go"), "#!/bin/sh\necho to-stdout\n", { mode: 0o755 });
  expect(await goOutput(["version"], { cwd: dir, env })).toBe("to-stdout\n");
});

test("goOutputSync returns standard output and fails naming the command, with both streams", () => {
  const dir = makeDirectory("failing-sync");
  const env = { ...process.env, PATH: failingGo(dir) };
  expect(() => goOutputSync(["version"], { cwd: dir, env })).toThrow(
    `go version in ${dir} exited 3:\nto-stdout\nto-stderr\n`
  );
  writeFileSync(join(dir, "go"), "#!/bin/sh\ncat\n", { mode: 0o755 });
  expect(
    goOutputSync(["version"], { cwd: dir, env, stdin: new TextEncoder().encode("from-stdin") })
  ).toBe("from-stdin");
});
