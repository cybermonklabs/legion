import { beforeEach, describe, expect, test } from "bun:test";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { bearerCredential } from "../bearer";
import type { ExecFn } from "../dispatch-cwd";
import { executeDispatchTool } from "../dispatch-execute";
import { forgetKeyedLayerMemory } from "../dispatch-http";

// A write the server's reviewer holds (INFRA-3231's R5) answers with its reserved id and a `held`
// object: no person sees the text until the review ends and the writer edits it or posts it. The
// command's text is what the agent reads of the write, so it must not say the text posted, and it
// must say that Dispatch holds it. Tester's red test: at fb63391f the command prints
// "Posted comment <id>" and "Posted. You've sent …" for a held comment and a held ask.

beforeEach(forgetKeyedLayerMemory);

const config = {
  enabled: true,
  url: "http://dispatch.test",
  credential: bearerCredential(fixtureSecret("dsp")),
  error: null,
};

function repoExec(repo: string): ExecFn {
  return async (file, args) => {
    if (file === "jj" && args.join(" ") === "git remote list") {
      return { stdout: `origin https://github.com/${repo}.git\n` };
    }
    throw new Error(`unexpected command: ${file} ${args.join(" ")}`);
  };
}

const held = { state: "reviewing", deadline: null };
const advice = {
  issue_status: "in_progress",
  session_writes_since_human: 3,
  your_open_asks: [],
  legion_managed_status: false,
};

function run(tool: string, args: Record<string, unknown>, answer: unknown) {
  const fetchImpl = async (): Promise<Response> =>
    new Response(JSON.stringify(answer), {
      status: 201,
      headers: { "Content-Type": "application/json" },
    });
  return executeDispatchTool({
    tool,
    args,
    cwd: "/workspace",
    host: "omp",
    config,
    env: {},
    exec: repoExec("owner/repo"),
    fetchImpl: fetchImpl as unknown as typeof fetch,
  });
}

describe("a write the reviewer holds", () => {
  test("a held comment is not reported as posted, and its text says Dispatch holds it", async () => {
    const result = await run(
      "dispatch_comment",
      { issue: "DSP-42", body: "The run is done. This names the problem." },
      {
        id: "c8433e14-0047-4cca-a875-a7b1a6880500",
        issue_key: "DSP-42",
        ask_id: null,
        reply_to: null,
        body: "The run is done. This names the problem.",
        advice,
        held,
      }
    );
    expect(result.text).not.toContain("Posted");
    expect(result.text).toContain("c8433e14-0047-4cca-a875-a7b1a6880500");
    expect(result.text.toLowerCase()).toContain("held");
  });

  test("a held ask is not reported as posted, and its text says Dispatch holds it", async () => {
    const result = await run(
      "dispatch_ask",
      { issue: "DSP-42", question: "Ship the audit?\nThe run is done. This names the problem." },
      {
        id: "d236c23c-240b-4854-b6fe-cbae3b6868a7",
        issue_key: "DSP-42",
        author: { kind: "session", id: "session-1" },
        question: "Ship the audit?\nThe run is done. This names the problem.",
        options: [],
        multiple: false,
        urgency: "med",
        anchor: null,
        state: "open",
        answer: null,
        created_at: "2026-11-19T16:36:00Z",
        advice,
        held,
      }
    );
    expect(result.text).not.toContain("Posted");
    expect(result.text).toContain("d236c23c-240b-4854-b6fe-cbae3b6868a7");
    expect(result.text.toLowerCase()).toContain("held");
  });
});
