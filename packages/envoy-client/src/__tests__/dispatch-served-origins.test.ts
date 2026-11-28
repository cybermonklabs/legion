import { describe, expect, test } from "bun:test";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { bearerCredential } from "../bearer";
import { type OwnerContext, resolveOwnerArguments } from "../dispatch-commands/check";
import type { WhoamiOriginsFn } from "../dispatch-commands/context";
import type { ExecFn } from "../dispatch-cwd";
import { executeDispatchTool } from "../dispatch-execute";
import { ToolInputError } from "../tool-input-errors";

const CANONICAL = "https://a.example.test";
const SERVED = "https://b.example.test";
const FORMER = "https://old.example.test";
const OTHER = "https://other.example.test";

/** What the server names as its origins: its canonical address, the served one, the former one. */
const SERVER_ORIGINS = [CANONICAL, SERVED, FORMER] as const;

const unusedExec: ExecFn = async () => ({ stdout: "", stderr: "", exitCode: 1 });

/** What `resolveOwnerArguments` reads beside a call's arguments: a checkout with no repository,
 *  no environment, the command's configured addresses and the server's own list (`whoami`). */
function ownerContext(serverUrls: readonly string[], whoami: WhoamiOriginsFn): OwnerContext {
  return { cwd: "/workspace", env: {}, exec: unusedExec, serverUrls, whoami };
}

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

describe("served and former address links in resolveOwnerArguments (R10)", () => {
  for (const configured of [CANONICAL, SERVED]) {
    test(`reads canonical, served and former links as the same reference from a command configured with ${configured}`, async () => {
      for (const linkOrigin of [CANONICAL, SERVED, FORMER]) {
        let whoamiCalls = 0;
        const whoami: WhoamiOriginsFn = async () => {
          whoamiCalls += 1;
          return { origins: [...SERVER_ORIGINS] };
        };
        const problems: string[] = [];
        const resolved = await resolveOwnerArguments(
          "dispatch_read",
          { ref: `${linkOrigin}/issues/CORE-42/spec` },
          ownerContext([configured], whoami),
          problems
        );
        expect(problems).toEqual([]);
        expect(resolved.args.ref).toBe("dispatch://CORE-42/spec");
        expect(resolved.owner).toEqual({ kind: "issue", issue: "CORE-42" });
        // A link already on the configured origin needs no whoami call; any other origin asks once.
        expect(whoamiCalls).toBe(linkOrigin === configured ? 0 : 1);
      }
    });

    test(`refuses a link at another origin (${OTHER}) from a command configured with ${configured}`, async () => {
      let whoamiCalls = 0;
      const whoami: WhoamiOriginsFn = async () => {
        whoamiCalls += 1;
        return { origins: [...SERVER_ORIGINS] };
      };
      const problems: string[] = [];
      const resolved = await resolveOwnerArguments(
        "dispatch_read",
        { ref: `${OTHER}/issues/CORE-42` },
        ownerContext([configured], whoami),
        problems
      );
      expect(whoamiCalls).toBe(1);
      expect(problems.length).toBeGreaterThan(0);
      expect(resolved.owner).toBeNull();
      expect(resolved.ref).toBeNull();
    });
  }

  test("leaves served and former links refused when whoami throws", async () => {
    const refusingWhoami: WhoamiOriginsFn = async () => {
      throw new Error("whoami refused");
    };
    for (const linkOrigin of [SERVED, FORMER]) {
      const problems: string[] = [];
      const resolved = await resolveOwnerArguments(
        "dispatch_read",
        { ref: `${linkOrigin}/issues/CORE-42` },
        ownerContext([CANONICAL], refusingWhoami),
        problems
      );
      expect(problems.length).toBeGreaterThan(0);
      expect(resolved.owner).toBeNull();
    }
    // The command's own configured origin still resolves without calling whoami.
    const ownProblems: string[] = [];
    const own = await resolveOwnerArguments(
      "dispatch_read",
      { ref: `${CANONICAL}/issues/CORE-42` },
      ownerContext([CANONICAL], refusingWhoami),
      ownProblems
    );
    expect(ownProblems).toEqual([]);
    expect(own.args.ref).toBe("dispatch://CORE-42");
  });

  test("leaves served and former links refused when whoami answers no origins", async () => {
    const olderServerWhoami: WhoamiOriginsFn = async () => ({});
    for (const linkOrigin of [SERVED, FORMER]) {
      const problems: string[] = [];
      const resolved = await resolveOwnerArguments(
        "dispatch_read",
        { ref: `${linkOrigin}/issues/CORE-42` },
        ownerContext([CANONICAL], olderServerWhoami),
        problems
      );
      expect(problems.length).toBeGreaterThan(0);
      expect(resolved.owner).toBeNull();
    }
  });

  test("ignores stray DISPATCH_SERVED_ORIGINS and DISPATCH_FORMER_ORIGINS in the environment", async () => {
    const env = {
      DISPATCH_SERVED_ORIGINS: SERVED,
      DISPATCH_FORMER_ORIGINS: FORMER,
    };
    const noOriginsWhoami: WhoamiOriginsFn = async () => ({});
    for (const linkOrigin of [SERVED, FORMER]) {
      const problems: string[] = [];
      const resolved = await resolveOwnerArguments(
        "dispatch_read",
        { ref: `${linkOrigin}/issues/CORE-42` },
        { ...ownerContext([CANONICAL], noOriginsWhoami), env },
        problems
      );
      expect(problems.length).toBeGreaterThan(0);
      expect(resolved.owner).toBeNull();
    }
  });
});

describe("dispatch read over HTTP with served and former address links (R10)", () => {
  for (const configured of [CANONICAL, SERVED]) {
    test(`reads an issue from served and former links and refuses another origin when configured with ${configured}`, async () => {
      const requested: string[] = [];
      const fetchImpl = async (url: RequestInfo | URL): Promise<Response> => {
        const target = new URL(String(url));
        requested.push(`${target.origin}${target.pathname}`);
        if (target.pathname === "/api/v1/whoami") {
          return jsonResponse({
            kind: "agent",
            owner: null,
            project: null,
            scopes: [],
            service: null,
            session: null,
            origins: SERVER_ORIGINS,
          });
        }
        if (target.pathname === "/api/v1/issues/CORE-42") {
          return jsonResponse({
            key: "CORE-42",
            title: "Served origins issue",
            status: "open",
            route: null,
            open_asks: [],
            children: [],
            last_seq: 0,
          });
        }
        if (target.pathname === "/api/v1/issues/CORE-42/events") return jsonResponse([]);
        throw new Error(`unexpected request: ${target.origin}${target.pathname}`);
      };
      const config = {
        url: configured,
        credential: bearerCredential(fixtureSecret("dsp")),
      };

      for (const linkOrigin of [CANONICAL, SERVED, FORMER]) {
        requested.length = 0;
        const result = await executeDispatchTool({
          tool: "dispatch_read",
          args: { ref: `${linkOrigin}/issues/CORE-42/log` },
          cwd: "/workspace",
          host: "omp",
          config,
          env: {},
          exec: unusedExec,
          fetchImpl: fetchImpl as typeof fetch,
        });
        expect(result.text).toBe("Key: CORE-42\nEvents:\n- none");
        const expected =
          linkOrigin === configured
            ? [`${configured}/api/v1/issues/CORE-42`, `${configured}/api/v1/issues/CORE-42/events`]
            : [
                `${configured}/api/v1/whoami`,
                `${configured}/api/v1/issues/CORE-42`,
                `${configured}/api/v1/issues/CORE-42/events`,
              ];
        expect(requested).toEqual(expected);
      }

      requested.length = 0;
      await expect(
        executeDispatchTool({
          tool: "dispatch_read",
          args: { ref: `${OTHER}/issues/CORE-42/log` },
          cwd: "/workspace",
          host: "omp",
          config,
          env: {},
          exec: unusedExec,
          fetchImpl: fetchImpl as typeof fetch,
        })
      ).rejects.toBeInstanceOf(ToolInputError);
      expect(requested).toEqual([`${configured}/api/v1/whoami`]);
    });
  }
});
