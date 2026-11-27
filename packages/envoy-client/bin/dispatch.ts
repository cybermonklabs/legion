// The `dispatch` command each host plugin bundles into its dist/dispatch.js and puts on its
// agents' shell PATH through a `bin/dispatch` shim. `runDispatchCli` holds the behaviour.
import { runDispatchCli } from "../src/dispatch-cli";
import { readFileText } from "../src/dispatch-file-text";
import { exitProcess } from "../src/process-exit";

// Every omp session reads this command through a pipe, and Bun's `process.exit` drops what a pipe
// has not yet taken, a write whose callback has run included, so the process ends once stdout and
// stderr have taken every byte written to them (`exitProcess`). A reader that closed its end fails
// its stream, and nothing more is written there: the reader no longer wants the rest. Stored
// content or a body that is not JSON goes to the stream as its bytes, unchanged.
const code = await runDispatchCli(Bun.argv.slice(2), process.env, {
  stdout: (text) => {
    process.stdout.write(text);
  },
  stdoutBytes: (bytes) => {
    process.stdout.write(bytes);
  },
  stderr: (text) => {
    process.stderr.write(text);
  },
  readText: readFileText,
  cwd: process.cwd(),
});
await exitProcess(code);
