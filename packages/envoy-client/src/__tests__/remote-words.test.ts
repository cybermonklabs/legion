// packages/envoy-client/src/__tests__/remote-words.test.ts
//
// The words another party wrote, as this package's errors print them: the Envoy listener's
// refusal (`EnvoyApiError`), a body something in its place wrote, Dispatch's refusal
// (`DispatchServiceError`: its code and its words) and a page in Dispatch's place
// (`DispatchGatewayError`). Each quotes a URL that carries a user, a password and an access token,
// or echoes the bearer its request carried, and none of it prints: in the error's message,
// `String()`, `JSON.stringify`, Node's and Bun's inspection, or the `dispatch` command's text.
//
// Each check is the designated assertion of cmd/redact-gate's mutation gate and its leak rows: it
// prints the output it read as `REDACT-CAPTURE <capture> <base64>` and fails with a message that
// begins `REDACT-ASSERT <capture>`. This file reads only what the clients printed, through the
// API they had before their remote fields were boxed, so the check's control 19 runs it on the
// tree where each leak is.

import { beforeEach, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { inspect } from "node:util";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import {
  REMOTE_PASSWORD,
  REMOTE_TOKEN,
  REMOTE_USER,
  REMOTE_WORDS,
} from "@legion/test-fixtures/print-test";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import { bearerCredential } from "../bearer";
import { runDispatchCli } from "../dispatch-cli";
import { resetAdviceMemory } from "../dispatch-execute";
import { DispatchClient } from "../dispatch-http";
import { createEnvoyClient } from "../transport";

const PROBE = [REMOTE_USER, REMOTE_PASSWORD, REMOTE_TOKEN];

/** Every way a caller prints an error: its message, and the printers that read its fields. */
function printed(error: unknown): Record<string, string> {
  return {
    message: error instanceof Error ? error.message : String(error),
    String: String(error),
    "JSON.stringify": JSON.stringify(error) ?? "",
    "util.inspect": inspect(error, { depth: 10 }),
    "Bun.inspect": Bun.inspect(error),
  };
}

/** The designated assertion: `text`, captured under `capture`, holds none of `secrets`. */
function requireNone(capture: string, text: string, secrets: readonly string[]): void {
  console.info(`REDACT-CAPTURE ${capture} ${Buffer.from(text, "utf8").toString("base64")}`);
  for (const secret of secrets) {
    expect(
      text.includes(secret),
      `REDACT-ASSERT ${capture}: the output prints the secret ${secret}: ${text}`
    ).toBe(false);
  }
}

/** Each printed form of `error`, as `<capture>/<form>`, holds none of `secrets`. */
function requireNoneIn(capture: string, error: unknown, secrets: readonly string[]): void {
  expect(error).toBeInstanceOf(Error);
  for (const [form, text] of Object.entries(printed(error))) {
    requireNone(`${capture}/${form.replace(/\W+/g, "-")}`, text, secrets);
  }
}

/** The error a read of the listener gets when every answer is `answer`, presenting `bearer`. */
async function envoyRefusal(answer: () => Response, bearer: string): Promise<unknown> {
  const client = createEnvoyClient({
    baseUrl: "http://listener",
    credential: bearerCredential(bearer),
    retry: false,
    fetch: (async () => answer()) as unknown as typeof fetch,
  });
  return client.getInterest("ses_sender").catch((caught: unknown) => caught);
}

test("an Envoy refusal the listener's schema reads prints none of the words it quotes", async () => {
  const error = await envoyRefusal(
    () =>
      Response.json(
        {
          error: REMOTE_WORDS,
          code: REMOTE_WORDS,
          reason: REMOTE_WORDS,
          expected: [REMOTE_WORDS],
          refused: [REMOTE_WORDS],
          holder: REMOTE_WORDS,
          holder_state: "unresponsive",
          last_seen: 1_790_000_000_000,
        },
        { status: 403 }
      ),
    fixtureSecret("listener")
  );
  requireNoneIn("envoy.refusal", error, PROBE);
  // What the agent acts on still prints: the holder's state, which the client says itself.
  expect((error as Error).message).toContain("is unresponsive, last seen");
});

test("an Envoy answer that is not the listener's error JSON prints none of the words it quotes", async () => {
  const page = `<html><body><h1>403 Forbidden</h1><p>${REMOTE_WORDS}</p></body></html>`;
  const error = await envoyRefusal(
    () => new Response(page, { status: 403, headers: { "Content-Type": "text/html" } }),
    fixtureSecret("listener")
  );
  requireNoneIn("envoy.page", error, PROBE);
  expect((error as Error).message).toStartWith(
    "GET http://listener/v1/interests/ses_sender failed with 403: a body that is not the " +
      "listener's error JSON"
  );
});

/** A bearer the clients present, holding the gate's `-RDX-` marker so an echo of it is evidence
 *  the gate reads in captured output. */
function echoedBearer(): string {
  return `${fixtureSecret("bearer")}-RDX-`;
}

test("an Envoy refusal that echoes the bearer its request carried prints none of it", async () => {
  const bearer = echoedBearer();
  const said = `the token ${bearer} is not admitted`;
  const error = await envoyRefusal(
    () =>
      Response.json(
        {
          error: said,
          code: said,
          reason: said,
          expected: [said],
          refused: [said],
          holder: said,
          holder_state: "gone",
        },
        { status: 403 }
      ),
    bearer
  );
  requireNoneIn("envoy.echo", error, [bearer]);
});

test("a Dispatch refusal that echoes the bearer its request carried prints none of it", async () => {
  const bearer = echoedBearer();
  const said = `the token ${bearer} is not admitted`;
  const client = new DispatchClient("http://dispatch.test", bearerCredential(bearer), (async () =>
    Response.json({ error: said, code: said }, { status: 403 })) as unknown as typeof fetch);
  const error = await client.listProjects().catch((caught: unknown) => caught);
  requireNoneIn("dispatch.echo", error, [bearer]);
});

const makeDirectory = temporaryDirectories("dispatch-remote-words");
/** The Dispatch bearer each command is handed. */
const TOKEN = fixtureSecret("dsp");

let stateDir = "";
beforeEach(() => {
  stateDir = makeDirectory();
  resetAdviceMemory();
});

/** What `dispatch <argv>` prints (`dispatch priorities` where it names none) when Dispatch
 *  answers every request with `answer`. */
async function cliText(
  answer: () => Response,
  argv: readonly string[] = ["priorities"]
): Promise<string> {
  let stdout = "";
  await runDispatchCli(
    argv,
    {
      HOME: stateDir,
      DISPATCH_HOST: "omp",
      DISPATCH_SESSION_ID: "ses-1",
      DISPATCH_URL: "http://dispatch.test",
      DISPATCH_TOKEN: TOKEN,
      DISPATCH_STATE_DIR: stateDir,
    },
    {
      stdout: (text) => {
        stdout += text;
      },
      stderr: () => {},
      readText: (path) => readFileSync(path, "utf-8"),
      cwd: stateDir,
      fetchImpl: (async () => answer()) as unknown as typeof fetch,
    }
  );
  return stdout;
}

test("a Dispatch refusal whose code quotes a URL reaches the dispatch command's text with no secret", async () => {
  const text = await cliText(() =>
    Response.json({ error: "the priorities are not readable", code: REMOTE_WORDS }, { status: 403 })
  );
  requireNone("dispatch.code/cli", text, PROBE);
  expect(text).toContain("the priorities are not readable");
});

test("a Dispatch refusal whose words quote a URL reaches the dispatch command's text with no secret", async () => {
  const text = await cliText(() =>
    Response.json({ error: REMOTE_WORDS, code: "FORBIDDEN" }, { status: 403 })
  );
  requireNone("dispatch.words/cli", text, PROBE);
  expect(text).toContain("FORBIDDEN");
});

test("a page in Dispatch's place that quotes a URL reaches the dispatch command's text with no secret", async () => {
  const text = await cliText(
    () =>
      new Response(`<html><body><h1>403 Forbidden</h1><p>${REMOTE_WORDS}</p></body></html>`, {
        status: 403,
        headers: { "Content-Type": "text/html" },
      })
  );
  requireNone("dispatch.page/cli", text, PROBE);
  expect(text).toContain("HTTP_403");
});

/** A refusal's data whose every word quotes the probe: a near copy's candidates (the shape the
 *  `dispatch issue` command lists), an edit's current tokens and its mismatches. */
const refusalData = {
  candidates: [
    {
      key: "DSP-9",
      title: REMOTE_WORDS,
      status: "in_progress",
      snippet: REMOTE_WORDS,
      shared_terms: 3,
      href: "/issues/DSP-9",
    },
  ],
  current: { document: REMOTE_WORDS, blocks: [{ id: "block-1", token: REMOTE_WORDS }] },
  mismatches: [
    { scope: "block", block_id: "block-1", expected: REMOTE_WORDS, current: REMOTE_WORDS },
  ],
};

test("a Dispatch refusal whose candidates, current tokens and mismatches quote a URL prints none of them", async () => {
  const client = new DispatchClient("http://dispatch.test", bearerCredential(TOKEN), (async () =>
    Response.json(
      { error: "the write was refused", code: "PRECONDITION_FAILED", ...refusalData },
      { status: 409 }
    )) as unknown as typeof fetch);
  const error = await client.listProjects().catch((caught: unknown) => caught);
  requireNoneIn("dispatch.data", error, PROBE);
});

test("a near copy whose candidates quote a URL reaches the dispatch command's text with no secret", async () => {
  const text = await cliText(
    () =>
      Response.json(
        { error: "possible duplicate of DSP-9", code: "POSSIBLE_DUPLICATE", ...refusalData },
        { status: 409 }
      ),
    ["issue", "--project", "DSP", "--title", "Nightly export retries for ever"]
  );
  requireNone("dispatch.duplicates/cli", text, PROBE);
  expect(text).toContain("DSP-9 [in_progress]");
});
