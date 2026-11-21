import { afterEach, expect, test } from "bun:test";
import * as path from "node:path";
import {
  gapNoticeText,
  inboundRestoredNoticeText,
  inboundUnavailableNoticeText,
} from "@legion/envoy-client/consumer-delivery";
import { failureNoticeText } from "@legion/envoy-client/delivery-ledger";
import {
  type Block,
  type Cleanup,
  call,
  deliveryFor,
  messageStream,
  ompRoot,
  type Request,
  serveStandin,
  spawnRpc,
  toolResultsIn,
  writeStandinProfile,
} from "@legion/pi-shared/test/omp-harness";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { sessionIdChangedText, unreadMessagesText } from "../src/envoy-notices";

// dispatch://INFRA-3686 R2 ("a step that a text gives today goes into the reader's prompt where the
// prompt does not give it"), and the architect's ruling that each step of Envoy's own notices is in
// the description of the tool it names, and that each notice names that tool on its kind line. The
// steps are in envoy_inbox's and envoy_whoami's descriptions (`ENVOY_NOTICE_STEPS`,
// packages/envoy-client/src/tool-contract.ts), each keyed by its notice's kind. The Oh My Pi that
// .omp-pin names mounts an extension's tool that names no loadMode as an xd:// device
// (`tools.xdev`, on by default), and its system prompt gives each device the first line of its
// description cut to 200 bytes (`tools.xdevDocs`, `catalog` by default), which holds no step; Legion
// sets neither (internal/podsafety writes no settings overlay; deploy/kubernetes/operator-route/
// overlay.yml sets no tools key). A model reads a device's full description when it opens
// xd://<tool>. So each notice's kind line names the tool by its name (`notice: session id changed
// (envoy_whoami)`), a name that is the tool's in Claude Code too, where the message-not-shown
// notice also reaches. This case renders each of the six notices, reads the tool its kind line
// names, and has the session's first turn open each such device: the catalog the turn carries
// names each device, and what the model reads when it opens one holds the step of each notice that
// names it.
// LEGION_TEST_OMP names the binary and LEGION_TEST_NATS_URL the NATS server; without both the
// case skips.
const omp = process.env.LEGION_TEST_OMP;
const nats = process.env.LEGION_TEST_NATS_URL;

const cleanup: Cleanup = [];
afterEach(async () => {
  for (const step of cleanup.splice(0).reverse()) await step();
});

/** Each of Envoy's own notices, rendered as its builder writes it. */
const NOTICES: readonly string[] = [
  gapNoticeText({
    kind: "retention_gap",
    stream: "stream-7f3a",
    retentionHours: 72,
    message: "messages older than 72 hours may be missing",
  }),
  inboundUnavailableNoticeText("the listener answered 503: consumer limit reached"),
  inboundRestoredNoticeText(),
  failureNoticeText({
    eventId: "evt-0a1b",
    sender: "ses_alice (Release review)",
    summary: "DSP-1 needs review",
    publishedAt: "2026-10-26T11:59:00Z",
    error: "the transcript write failed",
    readWith: "envoy_inbox",
    envelope: { event_id: "evt-0a1b", source: "agent" },
  }),
  sessionIdChangedText(
    "01a0aaaa-0000-7000-8000-000000000001",
    "01a0bbbb-0000-7000-8000-000000000002"
  ),
  unreadMessagesText(3),
];

/** The kind a notice's kind line states (`notice: <kind>`), and the tool the kind names by its
 *  name at its end, `(<tool>)`: undefined where it names none, or names a path. */
function kindOf(notice: string): { readonly kind: string; readonly tool: string | undefined } {
  const kind = /^ {2}notice: (.+)$/m.exec(notice)?.[1] ?? "";
  return { kind, tool: /\(([a-z][a-z0-9_]*)\)$/.exec(kind)?.[1] };
}

/** The sentence of `description` that gives the step of the notice of `kind`, keyed by it. */
function stepOf(description: string, kind: string): string | undefined {
  const key = kind.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
  return new RegExp(`On notice: ${key}, [^.]+\\.`).exec(description)?.[0];
}

/** Whether a Messages request is a turn of the session: one that offers the agent's tools, as the
 *  binary's side calls (a title, a summary) do not. */
function isTurn(body: Record<string, unknown>): boolean {
  return Array.isArray(body.tools) && body.tools.length > 0;
}

/** The id of the reply whose tool calls open the devices; a call's id is `toolu_<id>_<index>`. */
const OPENS = "msg_open";

test.skipIf(omp === undefined || nats === undefined)(
  "each of Envoy's own notices names a tool whose full description gives its step",
  async () => {
    const named = NOTICES.map(kindOf);
    expect(named.filter(({ tool }) => tool === undefined).map(({ kind }) => kind)).toEqual([]);
    const tools = [...new Set(named.map(({ tool }) => tool ?? ""))];
    const binary = omp ?? "";
    const { home, workspace, sessions } = await ompRoot(binary, "notice-steps-omp-", cleanup);
    let turns = 0;
    const { requests, base } = serveStandin(cleanup, async (url, body) => {
      if (url.pathname === "/anthropic/v1/messages") {
        // The first turn opens each device a notice names, as a model told the notice does; the
        // turn after it ends.
        const opens = isTurn(body) && turns++ === 0;
        const blocks: readonly Block[] = opens
          ? tools.map((tool) => call("read", { path: `xd://${tool}` }))
          : [{ type: "text", text: "Read." }];
        return new Response(messageStream(blocks, opens ? OPENS : `msg_${turns}`), {
          headers: { "content-type": "text/event-stream" },
        });
      }
      if (url.pathname.startsWith("/anthropic/")) return Response.json({ data: [] });
      if (url.pathname === "/api/v1/asks/open") return new Response("unavailable", { status: 503 });
      const answers = /^\/api\/v1\/agents\/([^/]+)\/answers$/.exec(url.pathname);
      if (answers !== null) {
        return Response.json({
          session_id: decodeURIComponent(answers[1] ?? ""),
          since: new Date(Date.now() - 72 * 3_600_000).toISOString(),
          total: 0,
          items: [],
        });
      }
      const id = typeof body.session_id === "string" ? body.session_id : "";
      const interest = { session_id: id, machine_id: "notice-steps-machine", dir: workspace };
      return Response.json({ ...interest, topics: [], ...deliveryFor(url.pathname, body, id) });
    });
    await writeStandinProfile(home, base);
    const rpc = spawnRpc(
      binary,
      {
        extensions: [path.join(import.meta.dir, "envoy.ts")],
        home,
        workspace,
        sessions,
        env: {
          ENVOY_URL: base,
          ENVOY_NATS_URL: nats ?? "",
          DISPATCH_URL: base,
          DISPATCH_TOKEN: fixtureSecret("dsp"),
        },
        onFrame: () => undefined,
      },
      cleanup
    );
    // A real binary's startup and its requests are awaited by polling what the stand-in recorded,
    // as card-delivery-omp.test.ts's `until` does: neither gives a promise to await.
    const until = async <T>(what: string, found: () => T | undefined): Promise<T> => {
      for (const deadline = Date.now() + 120_000; Date.now() < deadline; ) {
        const value = found();
        if (value !== undefined) return value;
        await Bun.sleep(50);
      }
      throw new Error(`the session never sent ${what}`);
    };
    const turnsSent = () =>
      requests.filter(
        (request) => request.path === "/anthropic/v1/messages" && isTurn(request.body)
      );
    await Promise.race([
      until("its registration to the listener", () =>
        requests.find((request) => request.path === "/v1/interests/subscribe")
      ),
      rpc.closed,
    ]);
    rpc.send({ type: "prompt", message: "Go on." });
    const first = await Promise.race([until("the model a turn", () => turnsSent()[0]), rpc.closed]);
    const opened: Request = await Promise.race([
      until("the model what the devices it opened read", () =>
        turnsSent().find((request) =>
          toolResultsIn(request).some(({ tool_use_id }) =>
            tool_use_id.startsWith(`toolu_${OPENS}_`)
          )
        )
      ),
      rpc.closed,
    ]);

    // The catalog the first turn carries names each device a notice names, and says for every
    // device that opening it gives its full description.
    const catalog = JSON.stringify(first.body.system);
    expect(tools.filter((tool) => !catalog.includes(`xd://${tool}`))).toEqual([]);
    expect(catalog).toContain("Read xd://<tool> for full docs");
    // What the model read when it opened each one: the device's full description.
    const results = toolResultsIn(opened);
    const read = new Map(
      tools.map((tool, index) => [
        tool,
        results.find(({ tool_use_id }) => tool_use_id === `toolu_${OPENS}_${index}`),
      ])
    );
    expect(
      tools.filter((tool) => read.get(tool) === undefined || read.get(tool)?.is_error === true)
    ).toEqual([]);
    expect(
      named
        .filter(({ kind, tool }) => stepOf(read.get(tool ?? "")?.text ?? "", kind) === undefined)
        .map(({ kind }) => kind),
      JSON.stringify(Object.fromEntries(read))
    ).toEqual([]);
  },
  240_000
);
