import { expect, test } from "bun:test";
import { bearerCredential } from "@legion/envoy-client/bearer";
import { DispatchClient } from "@legion/envoy-client/dispatch-http";
import { liveContext, transcriptPictures, withDeliveredPictures } from "./delivery-pictures";

const session = "01a1058e-f14f-7684-87eb-3dc885955551";
const address = `dispatch://agent/${session}/artifact/shot-png@v1`;
/** A picture Dispatch no longer serves. */
const gone = `dispatch://agent/${session}/artifact/gone-png@v1`;
/** A picture whose read never answers: the delivery's timeout, or Dispatch unreachable. */
const unreachable = `dispatch://agent/${session}/artifact/slow-png@v1`;
const png = new Uint8Array([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 1, 2]);

async function client(): Promise<DispatchClient> {
  const fetchImpl = async (url: string | URL | Request): Promise<Response> => {
    const { pathname } = new URL(String(url));
    if (pathname === `/api/v1/agents/${session}/artifacts/shot-png`) {
      return Response.json({
        id: "pic-1",
        name: "shot.png",
        kind: "image",
        versions: [{ number: 1, mime: "image/png", size: png.length }],
      });
    }
    if (pathname === "/api/v1/artifacts/pic-1/versions/1") {
      return new Response(png, { headers: { "Content-Type": "image/png" } });
    }
    if (pathname === `/api/v1/agents/${session}/artifacts/gone-png`) {
      return Response.json({ code: "NOT_FOUND", error: "artifact not found" }, { status: 404 });
    }
    if (pathname === `/api/v1/agents/${session}/artifacts/slow-png`) {
      throw new DOMException("The operation timed out.", "TimeoutError");
    }
    throw new Error(`unexpected request: ${pathname}`);
  };
  return new DispatchClient(
    "http://dispatch.test",
    bearerCredential("secret"),
    fetchImpl as typeof fetch
  );
}

const image = {
  type: "image" as const,
  data: Buffer.from(png).toString("base64"),
  mimeType: "image/png",
};

test("a delivered card carries its pictures as image blocks after the text that names them", async () => {
  expect(
    await withDeliveredPictures("envoy: card", [address, address], client, true, new Set())
  ).toEqual({
    content: [
      {
        type: "text",
        text: `envoy: card\n\nPictures:\n- image 1: ${address} (shot.png, image/png, 10 bytes)`,
      },
      image,
    ],
    shown: [address],
  });
});

test("a person's own turn keeps their text exactly, with their pictures beside it", async () => {
  const body = `Look:\n\n![shot.png](${address})`;
  expect(await withDeliveredPictures(body, [address], client, false, new Set())).toEqual({
    content: [{ type: "text", text: body }, image],
    shown: [address],
  });
});

test("a picture Dispatch cannot serve is named and costs the delivery neither its text nor its other pictures", async () => {
  const { content, shown } = await withDeliveredPictures(
    "envoy: card",
    [address, gone, unreachable],
    client,
    true,
    new Set()
  );

  const [card, ...pictures] = Array.isArray(content) ? content : [];
  expect(pictures).toEqual([image]);
  expect(shown).toEqual([address]);
  const lines = card?.type === "text" ? card.text.split("\n") : [];
  expect(lines.slice(0, 4)).toEqual([
    "envoy: card",
    "",
    "Pictures:",
    `- image 1: ${address} (shot.png, image/png, 10 bytes)`,
  ]);
  expect(lines.slice(4).map((line) => line.split(" (unavailable: ")[0])).toEqual([
    `- not shown: ${gone}`,
    `- not shown: ${unreachable}`,
  ]);
  // Every picture unavailable: the delivery goes out as its text alone.
  expect(
    await withDeliveredPictures("plain", [gone, unreachable], client, true, new Set())
  ).toEqual({ content: "plain", shown: [] });
});

test("a Dispatch configuration that cannot be read leaves the delivery its text", async () => {
  const broken = async (): Promise<DispatchClient | undefined> => {
    throw new Error("dispatch config: DISPATCH_TOKEN_FILE cannot be read");
  };
  expect(await withDeliveredPictures("envoy: card", [address], broken, true, new Set())).toEqual({
    content: "envoy: card",
    shown: [],
  });
});

test("a delivery with no pictures, or no Dispatch to read them from, stays text", async () => {
  const plain = { content: "plain", shown: [] };
  expect(await withDeliveredPictures("plain", [], client, true, new Set())).toEqual(plain);
  expect(
    await withDeliveredPictures("plain", [address], async () => undefined, true, new Set())
  ).toEqual(plain);
});

test("a delivery leaves the session's pictures as they were: the caller adds what it showed once the host took it", async () => {
  const shown = new Set<string>();
  const first = await withDeliveredPictures("envoy: first", [address], client, true, shown);
  // The host refused the first send: nothing was added, so the retry shows the picture again.
  const retried = await withDeliveredPictures("envoy: first", [address], client, true, shown);
  expect(shown.size).toBe(0);
  expect(retried).toEqual(first);

  // The host took the retry, so its picture joins the session's.
  for (const picture of retried.shown) shown.add(picture);
  const second = await withDeliveredPictures("envoy: second", [address], client, true, shown);
  const body = `Again:\n\n![shot.png](${address})`;
  const turn = await withDeliveredPictures(body, [address], client, false, shown);

  expect(first).toEqual({
    content: [
      {
        type: "text",
        text: `envoy: first\n\nPictures:\n- image 1: ${address} (shot.png, image/png, 10 bytes)`,
      },
      image,
    ],
    shown: [address],
  });
  expect(second).toEqual({
    content: `envoy: second\n\nPictures:\n- not shown: ${address} (shown earlier this session; dispatch doc-read shows it again)`,
    shown: [],
  });
  // A person's own turn keeps their text exactly as they sent it.
  expect(turn).toEqual({ content: body, shown: [] });
});

test("a transcript's pictures are those its results and cards name as shown and a person's turn fully carries", () => {
  const picture = (slug: string) => `dispatch://LEGION-1/artifact/${slug}-png@v1`;
  const message = (role: string, content: unknown) => ({
    type: "message",
    message: { role, content },
  });
  const entries = [
    message("toolResult", [
      {
        type: "text",
        text: [
          "Pictures:",
          `- image 1: ${picture("read")} (read.png, image/png, 10 bytes)`,
          `- not shown: ${picture("withheld")} (shown earlier this session; dispatch doc-read shows it again)`,
        ].join("\n"),
      },
      image,
    ]),
    // dispatch doc-read names the one picture it shows the same way.
    message("toolResult", [
      {
        type: "text",
        text: `Picture opened.png: image/png, version 1, 10 bytes.\n- image 1: ${picture("opened")} (opened.png, image/png, 10 bytes)`,
      },
      image,
    ]),
    {
      type: "custom_message",
      customType: "envoy-message",
      content: [
        {
          type: "text",
          text: `envoy: card\n\nPictures:\n- image 1: ${picture("card")} (card.png, image/png, 10 bytes)`,
        },
        image,
      ],
      display: true,
    },
    // A person's turn whose every picture was shown beside it.
    message("user", [
      { type: "text", text: `Look:\n\n![a.png](${picture("turn")})\n![a.png](${picture("turn")})` },
      image,
    ]),
    // One picture shown of two: which one is not written down, so neither counts.
    message("user", [
      { type: "text", text: `![b.png](${picture("one")})\n![c.png](${picture("other")})` },
      image,
    ]),
    // Text that quotes a picture line without starting a line with it, and a turn with no image.
    message("assistant", [
      { type: "text", text: `See "- image 1: ${picture("quoted")} (q.png)".` },
    ]),
    message("user", `![d.png](${picture("typed")})`),
    { type: "custom", customType: "envoy-dispatch-handled-attempt", data: {} },
  ];

  expect(transcriptPictures(entries)).toEqual([
    picture("read"),
    picture("opened"),
    picture("card"),
    picture("turn"),
  ]);
});

/** What `liveContext` may read of an entry before the cut. */
const CUT_FIELDS: ReadonlySet<PropertyKey> = new Set(["type", "id", "firstKeptEntryId"]);

/**
 * `entry` as it stands before the live context's cut: its `type`, `id` and `firstKeptEntryId`
 * read as they are, and every other member, its message or content first of all, throws when it
 * is read or asked for, as a long session's start must touch no body the cut left behind.
 */
function beforeCut(entry: object): object {
  const refuse = (key: PropertyKey): never => {
    throw new Error(`${String(key)} of an entry before the cut was read`);
  };
  return new Proxy(entry, {
    get: (target, key, receiver) =>
      CUT_FIELDS.has(key) ? Reflect.get(target, key, receiver) : refuse(key),
    has: (target, key) => (CUT_FIELDS.has(key) ? Reflect.has(target, key) : refuse(key)),
  });
}

const live = (slug: string) => `dispatch://LEGION-1/artifact/live-${slug}-png@v1`;
/** A person's turn whose image block shows the picture `slug`, as Oh My Pi keeps it. */
const turnShowing = (id: string, slug: string) => ({
  type: "message",
  id,
  message: {
    role: "user",
    content: [{ type: "text", text: `Look:\n\n![${slug}.png](${live(slug)})` }, image],
  },
});
/** A delivered card that shows the picture `slug`. */
const cardShowing = (id: string, slug: string) => ({
  type: "custom_message",
  id,
  customType: "envoy-message",
  content: [
    {
      type: "text",
      text: `envoy: card\n\nPictures:\n- image 1: ${live(slug)} (${slug}.png, image/png, 10 bytes)`,
    },
    image,
  ],
  display: true,
});
const reply = (id: string) => ({
  type: "message",
  id,
  message: { role: "assistant", content: [{ type: "text", text: "Seen." }] },
});
const compaction = (id: string, firstKeptEntryId: string) => ({
  type: "compaction",
  id,
  summary: "The session so far.",
  firstKeptEntryId,
  tokensBefore: 150_000,
});
const resetBoundary = (id: string) => ({ type: "reset_boundary", id });
/** Where each entry of `context` stands on `branch`. */
const positions = (branch: readonly unknown[], context: readonly unknown[]) =>
  context.map((entry) => branch.indexOf(entry));

test("a branch's live context starts at the latest compaction's first kept entry and reads nothing else of an entry before it", () => {
  const branch = [
    beforeCut(turnShowing("e0", "early")),
    // An older compaction: the latest one decides.
    beforeCut(compaction("e1", "e0")),
    beforeCut(reply("e2")),
    reply("e3"),
    cardShowing("e4", "kept"),
    compaction("e5", "e3"),
    turnShowing("e6", "late"),
  ];

  const context = liveContext(branch);

  // The compaction stays in it, as the host opens the context with its summary.
  expect(positions(branch, context)).toEqual([3, 4, 5, 6]);
  expect(transcriptPictures(context)).toEqual([live("kept"), live("late")]);
});

test("a /clear after the latest compaction starts the live context after its boundary", () => {
  const branch = [
    beforeCut(turnShowing("e0", "early")),
    beforeCut(cardShowing("e1", "kept")),
    beforeCut(compaction("e2", "e1")),
    beforeCut(turnShowing("e3", "compacted")),
    beforeCut(resetBoundary("e4")),
    turnShowing("e5", "late"),
  ];

  const context = liveContext(branch);

  expect(positions(branch, context)).toEqual([5]);
  expect(transcriptPictures(context)).toEqual([live("late")]);
});

test("a /clear before the latest compaction leaves the live context to the compaction", () => {
  const branch = [
    beforeCut(turnShowing("e0", "early")),
    beforeCut(resetBoundary("e1")),
    beforeCut(turnShowing("e2", "cleared")),
    cardShowing("e3", "kept"),
    compaction("e4", "e3"),
    turnShowing("e5", "late"),
  ];

  const context = liveContext(branch);

  expect(positions(branch, context)).toEqual([3, 4, 5]);
  expect(transcriptPictures(context)).toEqual([live("kept"), live("late")]);
});

test("a /clear on a branch with no compaction starts the live context after its boundary", () => {
  const branch = [
    beforeCut(turnShowing("e0", "early")),
    beforeCut(cardShowing("e1", "carded")),
    beforeCut(resetBoundary("e2")),
    turnShowing("e3", "late"),
  ];

  const context = liveContext(branch);

  expect(positions(branch, context)).toEqual([3]);
  expect(transcriptPictures(context)).toEqual([live("late")]);
});

// The kept entry stands on another branch of the session's tree, the compaction keeps none (an
// Anthropic-native compaction writes "" and replays a snapshot instead), or it names an entry
// after the compaction: the host starts the context after the compaction, its summary first.
test("a compaction whose first kept entry does not stand before it on the branch starts the live context at the compaction", () => {
  for (const firstKept of ["e9-on-another-branch", "", "e3"]) {
    const branch = [
      beforeCut(turnShowing("e0", "early")),
      beforeCut(reply("e1")),
      compaction("e2", firstKept),
      turnShowing("e3", "late"),
    ];

    const context = liveContext(branch);

    expect(positions(branch, context)).toEqual([2, 3]);
    expect(transcriptPictures(context)).toEqual([live("late")]);
  }
});

test("with neither a compaction nor a /clear the live context is the whole branch", () => {
  const branch = [turnShowing("e0", "early"), reply("e1"), cardShowing("e2", "late")];

  const context = liveContext(branch);

  expect(context).toBe(branch);
  expect(transcriptPictures(context)).toEqual([live("early"), live("late")]);
});
