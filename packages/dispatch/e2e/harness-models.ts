/** Whether this run's Dispatch server answers and decides through models, from one switch,
 * `DISPATCH_E2E_MODELS`, which `e2e/run-server.sh` reads the same way:
 *
 * - unset: no model at all, which is what every spec but the models lane's is written against -
 *   the bar finds hits, answers nothing and says what it cannot do;
 * - `fake`: the models lane (`bun run e2e:models`). The harness starts `e2e/fake-chat-model.ts`
 *   on `FAKE_CHAT_MODEL_PORT` and `e2e/fake-decision-model.ts` on `FAKE_DECISION_MODEL_PORT`,
 *   and the server calls them as a deployment calls its writing model (`DISPATCH_ANSWERER=endpoint`)
 *   and its decision model (`DISPATCH_DECISION_MODEL`, the model the committed record chose).
 *
 * Both fakes serve https on loopback, since the server takes only an https endpoint for either
 * and the calls it makes are to be the calls a deployment makes. `e2e/harness-start.ts` makes
 * one directory per stack before its first row starts (`makeModelsMaterial`): a self-signed
 * certificate for 127.0.0.1 and its key, which both fakes serve, and one bearer token per fake,
 * which each fake requires of every model call. Its path is `DISPATCH_E2E_MODELS_DIR` in every
 * row's environment and in the stack's own variables, so the server is handed the certificate as
 * `SSL_CERT_FILE` and the token files as its own settings (`e2e/run-server.sh`), and a test
 * trusts the certificate when it scripts the fakes (`e2e/fake-models-helpers.ts`).
 *
 * A deployed run (`PLAYWRIGHT_BASE_URL`) starts no server, so it starts no fake model either and
 * a row that reads one is skipped. This module reads the environment and the file system only
 * when a function of it is called, so any process may import it.
 */

import { execFileSync } from "node:child_process";
import { randomBytes } from "node:crypto";
import { writeFileSync } from "node:fs";
import { join } from "node:path";

export const usesFakeModels =
  process.env.DISPATCH_E2E_MODELS === "fake" && !process.env.PLAYWRIGHT_BASE_URL;

/** The variable naming one stack's models directory. */
export const modelsDirVariable = "DISPATCH_E2E_MODELS_DIR";

/** The files of a models directory: the certificate both fakes serve and the server and the tests
 *  trust, its key, and the bearer token each fake requires. */
export interface ModelsFiles {
  readonly certificate: string;
  readonly key: string;
  readonly chatToken: string;
  readonly decisionToken: string;
}

export function modelsFiles(dir: string): ModelsFiles {
  return {
    certificate: join(dir, "cert.pem"),
    chatToken: join(dir, "chat-token"),
    decisionToken: join(dir, "decision-token"),
    key: join(dir, "key.pem"),
  };
}

/** The files of the models directory `dir` names, as a stack's variable gives it to `who`, which
 *  is refused naming the variable when it is unset: only the harness makes the directory. */
export function modelsFilesIn(dir: string | undefined, who: string): ModelsFiles {
  if (dir === undefined || dir === "") {
    throw new Error(
      `${who} needs ${modelsDirVariable}, the models directory e2e/harness-start.ts makes for ` +
        "each stack of the models lane (DISPATCH_E2E_MODELS=fake)"
    );
  }
  return modelsFiles(dir);
}

/** Writes a models directory's files into `dir`, which exists and is empty: a certificate for
 *  127.0.0.1, as `packages/envoy/scripts/going-smoke.sh` makes its fake model's, since a subject
 *  alternative name of that address is what Go checks a served certificate against, valid for a
 *  day, and a fresh token per fake. */
export function makeModelsMaterial(dir: string): ModelsFiles {
  const files = modelsFiles(dir);
  try {
    execFileSync(
      "openssl",
      [
        "req",
        "-x509",
        "-newkey",
        "rsa:2048",
        "-sha256",
        "-days",
        "1",
        "-nodes",
        "-keyout",
        files.key,
        "-out",
        files.certificate,
        "-subj",
        "/CN=127.0.0.1",
        "-addext",
        "subjectAltName=IP:127.0.0.1",
      ],
      { stdio: ["ignore", "ignore", "pipe"] }
    );
  } catch (error) {
    // execFileSync's error carries the child's stderr, which says why openssl refused.
    const stderr =
      error instanceof Error && "stderr" in error ? String(error.stderr ?? "").trim() : "";
    throw new Error(
      `The Dispatch e2e harness cannot start the models lane: openssl could not make the fake ` +
        `models' certificate${stderr === "" ? "" : `: ${stderr}`}`
    );
  }
  for (const token of [files.chatToken, files.decisionToken]) {
    writeFileSync(token, randomBytes(32).toString("base64url"), { mode: 0o600 });
  }
  return files;
}
