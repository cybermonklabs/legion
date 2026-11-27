import { expect, test } from "bun:test";
import { writeFileSync } from "node:fs";
import path from "node:path";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { runKeyCommand } from "./model-call";

const dir = temporaryDirectory("model-call");

// Catches: a key command whose own child hangs (a wrapper's curl to a metadata server) and holds
// the read of the key past the limit, because the limit stopped the wrapper alone and the child
// kept the output pipe open. The wrapper's child here sleeps for ten minutes with that pipe open,
// so the read ends inside the row's 20 s only where the limit stopped the child too.
test("the key command's limit ends the read and stops every process the command started", async () => {
  const wrapper = path.join(dir, "key-command");
  writeFileSync(wrapper, "#!/bin/sh\nsleep 600 &\nwait\necho key\n", { mode: 0o700 });
  await expect(runKeyCommand(wrapper, 200)).rejects.toThrow(
    `the key command ${wrapper} ran past its limit of 200 ms, so every process it started was stopped`
  );
}, 20_000);
