import { readFileSync } from "node:fs";
import { readErrorCode } from "@legion/broker-client/error-text";
import { holdSecret } from "@legion/contracts/held-secrets";

/** Whether `value` can go in a header as one bearer: no whitespace and no control character. A
 * multi-line value would otherwise reach a `fetch` error, which quotes the header it refuses. */
export function sendable(value: string): boolean {
  return !/[\s\p{Cc}]/u.test(value);
}

/** Reads the secret a `<VARIABLE>_FILE` pointer names: the file's contents, trimmed. The Legion
 * daemon hands every pane secret over this way (a 0600 file under its state directory) instead of
 * as a `-e KEY=VALUE` tmux argv, which is world-readable via `/proc`. A set pointer is a claim the
 * daemon made, so a missing, unreadable, or blank file throws naming the variable — callers never
 * fall back to the plain variable or a config file. So does a secret that is not `sendable`. No
 * error names the path, and none carries the secret: a secret pasted into the pointer in place of
 * its path (an nkey seed into `NATS_NKEY_SEED_FILE`) is the path, which the session's notices and
 * the model would otherwise read; the operator reads the path from the variable. */
export function readSecretFile(variable: string, filePath: string): string {
  let contents: string;
  try {
    contents = readFileSync(filePath, "utf8");
  } catch (error) {
    throw new Error(`${variable} names a file that could not be read (${readErrorCode(error)})`);
  }
  const value = contents.trim();
  if (value.length === 0) throw new Error(`${variable} names a file that is empty`);
  if (!sendable(value)) {
    throw new Error(`${variable} names a file that holds whitespace or a control character`);
  }
  // Held, so every line this process prints is scrubbed of it (`@legion/contracts/held-secrets`).
  return holdSecret(value);
}
