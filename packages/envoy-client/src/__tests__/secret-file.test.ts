import { describe, expect, test } from "bun:test";
import { writeFileSync } from "node:fs";
import * as path from "node:path";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import { nkeys } from "nats";
import { readSecretFile } from "../secret-file";

/** The message `run` rejects with. */
async function refusal(run: () => Promise<unknown>): Promise<string> {
  try {
    await run();
  } catch (error) {
    return error instanceof Error ? error.message : String(error);
  }
  throw new Error("it did not throw");
}

describe("readSecretFile", () => {
  const makeDirectory = temporaryDirectories("secret-file");

  test("returns the file contents trimmed", async () => {
    const dir = makeDirectory();
    const file = path.join(dir, "token");
    writeFileSync(file, "  the-token\n");
    expect(await readSecretFile("DISPATCH_TOKEN_FILE", file)).toBe("the-token");
  });

  test("throws naming the variable, never the path, for an unreadable or empty file", async () => {
    const dir = makeDirectory();
    const missing = path.join(dir, "missing");
    const empty = path.join(dir, "empty");
    writeFileSync(empty, "\n");
    const unread = await refusal(() => readSecretFile("DISPATCH_TOKEN_FILE", missing));
    expect(unread).toBe("DISPATCH_TOKEN_FILE names a file that could not be read (ENOENT)");
    expect(unread).not.toContain(dir);
    expect(await refusal(() => readSecretFile("DISPATCH_TOKEN_FILE", empty))).toBe(
      "DISPATCH_TOKEN_FILE names a file that is empty"
    );
  });

  // A seed pasted into the pointer in place of its path is the path, which the old text, and the
  // runtime's ENOENT text after it, both quoted; the session's notices and the model read it.
  test("a secret pasted into the pointer in place of its path is named in no error", async () => {
    const seed = new TextDecoder().decode(nkeys.createUser().getSeed());
    const unread = await refusal(() => readSecretFile("NATS_NKEY_SEED_FILE", seed));
    expect(unread).toBe("NATS_NKEY_SEED_FILE names a file that could not be read (ENOENT)");
    expect(unread).not.toContain(seed);
  });

  // A multi-line secret cannot be one bearer, and fetch's refusal of the header would quote it.
  test("throws naming the variable, never the path or the secret, for one with whitespace inside", async () => {
    const dir = makeDirectory();
    for (const contents of ["S3CRET\nnotes\n", "S3CRET TOKEN", "S3CRET\u0000"]) {
      const file = path.join(dir, "token");
      writeFileSync(file, contents);
      const message = await refusal(() => readSecretFile("ENVOY_TOKEN_FILE", file));
      expect(message).toBe(
        "ENVOY_TOKEN_FILE names a file that holds whitespace or a control character"
      );
      expect(message).not.toContain("S3CRET");
    }
  });
});
