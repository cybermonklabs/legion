// The `dispatch` command each host plugin bundles into its dist/dispatch.js and puts on its
// agents' shell PATH through a `bin/dispatch` shim. `runDispatchCli` holds the behaviour.
import { readFileSync } from "node:fs";
import { runDispatchCli } from "../src/dispatch-cli";
import { exitProcess } from "../src/process-exit";

const readText = (path: string): string => readFileSync(path === "-" ? 0 : path, "utf-8");

// Every omp session reads this command through a pipe, and Bun's `process.exit` drops what a pipe
// has not yet taken, a write whose callback has run included, so the process ends once stdout and
// stderr have taken every byte written to them (`exitProcess`). A reader that closed its end fails
// its stream, and nothing more is written there: the reader no longer wants the rest.
const code = await runDispatchCli(Bun.argv.slice(2), process.env, {
  stdout: (text) => {
    process.stdout.write(text);
  },
  stderr: (text) => {
    process.stderr.write(text);
  },
  readText,
  cwd: process.cwd(),
});
await exitProcess(code);
