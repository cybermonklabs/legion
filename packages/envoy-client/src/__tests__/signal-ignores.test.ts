import { expect, test } from "bun:test";
import { writeFileSync } from "node:fs";
import * as path from "node:path";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";

const tempDir = temporaryDirectories("signal-ignores");

// A process started with SIGHUP and SIGQUIT ignored, as a shell's `trap ''` (or `nohup`, for
// SIGHUP) starts it, reads both as ignored at start, and SIGINT and SIGTERM as not, even after it
// has armed a listener for SIGHUP of its own: the module reads the mask when it is loaded, which
// comes before the script that imports it runs, and a listener clears the mask's bit. The control
// is the same script started with nothing ignored, which reads no signal as ignored.
for (const row of [
  { ignores: "HUP QUIT", want: { SIGHUP: true, SIGINT: false, SIGQUIT: true, SIGTERM: false } },
  { ignores: "", want: { SIGHUP: false, SIGINT: false, SIGQUIT: false, SIGTERM: false } },
]) {
  test(`a process started ignoring ${row.ignores || "nothing"} reads that as its ignores at start`, async () => {
    const script = path.join(tempDir(), "read.ts");
    writeFileSync(
      script,
      `import { ignoredAtStart } from ${JSON.stringify(path.join(import.meta.dir, "../signal-ignores"))};
process.on("SIGHUP", () => {});
const answer: Record<string, boolean> = {};
for (const signal of ["SIGHUP", "SIGINT", "SIGQUIT", "SIGTERM"] as const) answer[signal] = ignoredAtStart(signal);
console.log(JSON.stringify(answer));
process.exit(0);
`
    );
    const ignore = row.ignores === "" ? "" : `trap '' ${row.ignores}; `;
    const run = Bun.spawn(["/bin/sh", "-c", `${ignore}exec "$@"`, "sh", process.execPath, script], {
      stdout: "pipe",
      stderr: "pipe",
    });
    const [stdout, stderr, code] = await Promise.all([
      new Response(run.stdout).text(),
      new Response(run.stderr).text(),
      run.exited,
    ]);
    expect(code, stderr).toBe(0);
    expect(JSON.parse(stdout)).toEqual(row.want);
  });
}
