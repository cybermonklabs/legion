import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import { readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { IDEMPOTENCY_KEY_HEADER } from "@legion/contracts";
import { dispatchCliLive, useDispatchCliDirectories } from "../../test/dispatch-cli-live";
import {
  type DispatchStack,
  dispatchStackEnabled,
  startDispatchStack,
} from "../../test/dispatch-stack";
import { runDispatchCli } from "../dispatch-cli";
import { resetAdviceMemory } from "../dispatch-execute";
import { forgetKeyedLayerMemory } from "../dispatch-http";

// Dispatch's keyed layer names a write's Idempotency-Key on every answer to it (`keyedWrites`,
// packages/envoy/internal/dispatch/api/request_keys.go), and the `dispatch` command takes a write's
// 2xx without its key for something in Dispatch's place (`DispatchClient.#answer`). This holds
// Dispatch to that over the real CLI against a real Dispatch: one call of each write command, each
// answered 2xx, and every keyed answer recorded. Each must name its key, and every call exit 0.
// `review` and `send-to-agent` need the person scope, so they run under a personal agent token
// alice mints; `architecture-sync` runs with no `--repo`, the sync of every source, which for a
// project with none is Dispatch's 200 `[]`.

const PERSON = "alice";
const SESSION = "session-write-echoes-key-live";
const PROJECT = "ECHO";
const PNG = Buffer.from("89504e470d0a1a0a0000000d49484452", "hex");

// Reserved here, at the file's module scope: the file's own `afterAll` removes each once the
// `describe` below has stopped the server writing into it.
const directories = useDispatchCliDirectories();

/** One keyed write the CLI sent, and the answer's status and whether it named the key back. */
interface Answered {
  readonly route: string;
  readonly status: number;
  readonly echoed: boolean;
}

describe.skipIf(!dispatchStackEnabled)(
  "every write the dispatch command makes is answered under its key",
  () => {
    let stack: DispatchStack | undefined;
    const answered: Answered[] = [];
    const { asPerson, createIssue, upload } = dispatchCliLive({
      stack: () => stack,
      session: SESSION,
      person: PERSON,
      spec: "# Design\n\nThe old wording stays.\n",
      directories,
    });

    /** Global fetch, with every keyed write's answer recorded. */
    const recording = (async (input: string | URL | Request, init?: RequestInit) => {
      const response = await fetch(input, init);
      const key = new Headers(init?.headers).get(IDEMPOTENCY_KEY_HEADER);
      const method = init?.method ?? "GET";
      if (key !== null && (method === "POST" || method === "PATCH")) {
        const path = new URL(String(input)).pathname.replace(
          /[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}/g,
          "<id>"
        );
        answered.push({
          route: `${method} ${path}`,
          status: response.status,
          echoed: response.headers.get(IDEMPOTENCY_KEY_HEADER) === key,
        });
      }
      return response;
    }) as typeof fetch;

    /** The real CLI, in this process, as the agent's session, under `token` (the stack's shared
     *  agent token unless named); what it printed and its exit. */
    async function dispatch(
      argv: readonly string[],
      token = stack?.token
    ): Promise<{ code: number; stdout: string }> {
      resetAdviceMemory();
      forgetKeyedLayerMemory();
      let stdout = "";
      const code = await runDispatchCli(
        argv,
        {
          HOME: directories.state,
          DISPATCH_STATE_DIR: directories.state,
          DISPATCH_HOST: "omp",
          DISPATCH_SESSION_ID: SESSION,
          DISPATCH_URL: stack?.url,
          DISPATCH_TOKEN: token,
        },
        {
          stdout: (text) => {
            stdout += text;
          },
          stderr: () => undefined,
          readText: (path) => readFileSync(path, "utf-8"),
          cwd: directories.state,
          fetchImpl: recording,
        }
      );
      return { code, stdout };
    }

    beforeAll(async () => {
      stack = await startDispatchStack({ work: directories.stack });
      await asPerson("POST", "/api/v1/projects", { key: PROJECT, name: `${PROJECT} project` });
    }, 600_000);
    afterAll(async () => {
      await stack?.stop();
    }, 60_000);

    test("each 2xx a write command gets names its Idempotency-Key", async () => {
      const issue = await createIssue(PROJECT, "Answer every write under its key");
      const key = issue.key;
      await upload(key, "plan.md", "# Plan\n\nOne step.\n");
      await upload(
        key,
        "verification.md",
        ':::check{#k1 surface="cli" run="bun test"}\nThe write is answered under its key.\n:::\n'
      );
      writeFileSync(join(directories.state, "shot.png"), PNG);
      const id = (pattern: RegExp, stdout: string): string => {
        const found = pattern.exec(stdout)?.[1];
        if (found === undefined) throw new Error(`no id in ${stdout}`);
        return found;
      };
      const uuid = "([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})";

      // A personal agent token alice mints holds the scopes her own credential does, the person
      // scope among them, which `review` and `send-to-agent` need and the shared token lacks.
      const personal = await asPerson<{ readonly token: string }>(
        "POST",
        "/api/v1/me/agent-tokens",
        { name: "write-echoes-key" }
      );

      const ran: {
        readonly argv: readonly string[];
        readonly code: number;
        readonly said: string | undefined;
      }[] = [];
      const run = async (argv: readonly string[], token?: string): Promise<string> => {
        const { code, stdout } = await dispatch(argv, token);
        ran.push({ argv, code, said: stdout.split("\n")[0] });
        return stdout;
      };
      await run(["issue", "--project", PROJECT, "--title", "Another echo", "--force"]);
      await run(["message", "--issue", key, "--body", "Started."]);
      const comment = id(
        new RegExp(`comment ${uuid}`),
        await run(["comment", "--issue", key, "--body", "Looks right."])
      );
      await run(["comment", "--issue", key, "--body", "With a picture.", "--image", "shot.png"]);
      const ask = id(
        new RegExp(`Asked ${uuid}`),
        await run([
          "ask",
          "--issue",
          key,
          "--question",
          "Which way?",
          "--option",
          "Left: the short way",
          "--option",
          "Right: the long way",
        ])
      );
      await run(["edit-ask", "--ask", ask, "--question", "Which way now?"]);
      await run(["resolve-ask", "--ask", ask, "--kind", "retracted", "--reason", "Superseded."]);
      await run(["resolve-comment", "--comment", comment]);
      await run([
        "suggest",
        "--issue",
        key,
        "--artifact",
        "spec",
        "--quote",
        "old wording",
        "--replace-with",
        "new wording",
      ]);
      await run(["artifact", "--issue", key, "--name", "notes.md", "--content", "# Notes\n"]);
      await run([
        "doc-edit",
        "--issue",
        key,
        "--artifact",
        "spec",
        "--ops-json",
        JSON.stringify([{ op: "insert", markdown: "One more line.", after: "end" }]),
      ]);
      await run(["request-approval", "--issue", key, "--summary", "Ready for a look."]);
      // The doc-edit above made version 2 of the spec, which the request names.
      await run(
        ["review", "--issue", key, "--version", "2", "--verdict", "approved"],
        personal.token
      );
      await run(["plan-approve", "--issue", key, "--version", "1"]);
      await run(["claim", "--issue", key]);
      await run([
        "handoff-write",
        "--issue",
        key,
        "--phase",
        "implement",
        "--body",
        JSON.stringify({ schemaVersion: 1, phase: "implement", issue: key }),
      ]);
      await run([
        "result",
        "--issue",
        key,
        "--check",
        "k1",
        "--role",
        "implementer",
        "--verdict",
        "pass",
        "--commit",
        "1".repeat(40),
      ]);
      await run(
        ["send-to-agent", "--session", SESSION, "--body", "A note.", "--delivery", "aside"],
        personal.token
      );
      await run(["issue-update", "--issue", key, "--status", "done", "--reason", "Shipped."]);
      await run(["project", "--key", "ECHB", "--name", "Echo B", "--default-assignee", PERSON]);
      // A sync answers 2xx only for a project with a source, which takes the GitHub App reading
      // the repository, and the stack runs none: Dispatch answers this project 404
      // SOURCE_NOT_FOUND. The keyed layer sets the key on the answer before the key is claimed
      // and the handler runs (request_keys.go, `keyedWrites`), so a 404 from the sync's handler
      // that names the key shows the route answers every outcome under it, a 2xx included.
      await run(["architecture-sync", "--project", PROJECT]);
      const sync = "POST /api/v1/projects/ECHO/architecture-sources/sync";
      expect(answered.filter((one) => one.route === sync)).toEqual([
        { route: sync, status: 404, echoed: true },
      ]);

      const unnamed = answered.filter((one) => one.status < 300 && !one.echoed);
      const refused = ran.filter((one) => one.code !== 0);
      expect({ unnamed, refused }).toEqual({
        unnamed: [],
        refused: [
          {
            argv: ["architecture-sync", "--project", PROJECT],
            code: 1,
            said: `SOURCE_NOT_FOUND: no architecture source configured for ${PROJECT}`,
          },
        ],
      });
      // Every other command's write reached the keyed layer and was answered 2xx under its key.
      expect(answered.filter((one) => one.route !== sync && one.status >= 300)).toEqual([]);
    }, 300_000);
  }
);
