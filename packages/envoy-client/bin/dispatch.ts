// The `dispatch` command each host plugin bundles into its dist/dispatch.js and puts on its
// agents' shell PATH through a `bin/dispatch` shim. `runDispatchCli` holds the behaviour.
import { runDispatchCli } from "../src/dispatch-cli";
import { readFileText } from "../src/dispatch-file-text";

// Bun's `process.exit` drops what a pipe has not yet taken (every omp session reads this command
// through one), so each write's callback is awaited before the process exits. A write that fails
// (the reader closed its end) is not retried: the reader no longer wants the rest. Stored content
// or a body that is not JSON goes to the stream as its bytes, unchanged.
const pending: Promise<void>[] = [];
const writeTo =
  (stream: NodeJS.WriteStream) =>
  (chunk: string | Uint8Array): void => {
    const { promise, resolve } = Promise.withResolvers<void>();
    stream.write(chunk, () => resolve());
    pending.push(promise);
  };

const code = await runDispatchCli(Bun.argv.slice(2), process.env, {
  stdout: writeTo(process.stdout),
  stdoutBytes: writeTo(process.stdout),
  stderr: writeTo(process.stderr),
  readText: readFileText,
  cwd: process.cwd(),
});
await Promise.all(pending);
process.exit(code);
