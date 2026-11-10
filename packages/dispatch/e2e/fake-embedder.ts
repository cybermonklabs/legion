import { harnessPorts } from "./harness-ports";

// The model service Dispatch embeds at, as the AWS SDK reaches Bedrock: `POST
// /model/{modelId}/invoke` with Cohere Embed v4's body (`internal/dispatch/embed/bedrock.go`).
// The server addresses it through AWS_ENDPOINT_URL_BEDROCK_RUNTIME, which `e2e/run-server.sh`
// sets to this listener under DISPATCH_E2E_EMBEDDER=fake (`e2e/harness-embedder.ts`); it checks
// no signature, since the SDK signs every call with the static, fake credentials that script
// hands it and nothing before Bedrock itself would verify them.
//
// It is the browser harness's counterpart of `api/search_meaning_test.go`'s `fakeEmbedder`:
// deterministic, text-keyed and counting. Every call is recorded by its input type, so a row
// reads back how many embeddings a stretch of typing cost, which is the whole of what the bar's
// two tiers are about - the keystroke searches the words alone and embeds nothing, and only a
// pause pays for one embedding.
//
// What it answers is a unit vector of `dimension` floats drawn from the text's own hash, so two
// texts that are not the same are near-orthogonal: a cosine of about 0 ± 0.03 at this width,
// well under the floor search keeps a meaning hit above (searchMeaningFloor, 0.25). A row that
// wants a hit by meaning seeds a group of words through `/__fixture/markers`: every text
// holding one of a group's words is embedded as that group, so a query and the item a row means
// it to find take one vector - a cosine of exactly 1 - while nothing else comes near either.
// The words are matched without regard to case, as the dev embedder reads a text's words.

/** Cohere Embed v4's width, which the server asks for and the embeddings column is sized for
 *  (`embed.Dimension`). An answer of any other width is refused by the client. */
const dimension = 1536;

/** What the server sends: Cohere Embed v4's own body (`bedrockEmbedRequest`). */
interface EmbedRequest {
  readonly input_type: string;
  readonly texts: readonly string[];
}

/** One call this listener answered: its input type as the server named it (`search_query` for a
 *  search's own query, `search_document` for the text of an indexed item) and the texts it
 *  carried. */
export interface FakeEmbedCall {
  readonly input_type: string;
  readonly texts: readonly string[];
}

/** What `GET /__fixture/calls` answers: every call since the last reset, oldest first. */
export interface FakeEmbedCalls {
  readonly calls: readonly FakeEmbedCall[];
}

/** Every piece of fixture state this listener holds; `PUT /__fixture/reset` replaces it whole. */
interface FakeEmbedderState {
  /** The marker groups a row seeded, lower-cased: a text holding a word of one is embedded as
   *  that group. */
  markers: readonly (readonly string[])[];
  calls: FakeEmbedCall[];
}

function emptyState(): FakeEmbedderState {
  return { calls: [], markers: [] };
}

let state = emptyState();

/** FNV-1a over the bytes of `text`, the hash `embed.DevClient` uses for the same purpose: a word
 *  differs from the word beside it in every bit of the result. */
function hash(text: string): number {
  let value = 0x811c9dc5;
  for (const byte of new TextEncoder().encode(text)) {
    value = Math.imul(value ^ byte, 0x01000193) >>> 0;
  }
  return value;
}

/** The vector of a text: `dimension` floats from a generator seeded by the text's hash, scaled
 *  to length 1. Two different texts take different seeds and so point in directions that are
 *  near-orthogonal at this width; one text always takes the same direction, so a query embedded
 *  twice answers the same vector and a row's expected order never moves. */
function vectorOf(text: string): number[] {
  // xorshift32, seeded by the hash (never 0, which the generator cannot leave).
  let seed = hash(text) || 1;
  const vector = new Array<number>(dimension);
  let length = 0;
  for (let index = 0; index < dimension; index++) {
    seed ^= seed << 13;
    seed >>>= 0;
    seed ^= seed >>> 17;
    seed ^= seed << 5;
    seed >>>= 0;
    // Centred on zero, so the directions are spread over the whole sphere rather than one
    // corner of it, where every pair of vectors would be close.
    const component = seed / 0xffffffff - 0.5;
    vector[index] = component;
    length += component * component;
  }
  const scale = 1 / Math.sqrt(length);
  return vector.map((component) => component * scale);
}

/** The text a vector is drawn from: the group whose words the text holds, so a query and the
 *  item a row means it to find are one text to this fake, and the text itself where it holds
 *  none of them. */
function keyOf(text: string): string {
  const lowered = text.toLowerCase();
  const group = state.markers.findIndex((words) => words.some((word) => lowered.includes(word)));
  return group === -1 ? text : `marker group ${group}`;
}

// FAKE_EMBEDDER_PORT, or with it unset port 0, so the OS chooses; the line at the end reports
// the port bound, which e2e/harness-start.ts reads.
const server = Bun.serve({
  hostname: "127.0.0.1",
  port: harnessPorts().fakeEmbedder.port,
  async fetch(request) {
    const url = new URL(request.url);
    if (
      request.method === "POST" &&
      url.pathname.startsWith("/model/") &&
      url.pathname.endsWith("/invoke")
    ) {
      const body = (await request.json()) as EmbedRequest;
      const texts = body.texts ?? [];
      state.calls.push({ input_type: body.input_type, texts: [...texts] });
      // Bedrock's own answer shape, confirmed against the live service: the vectors nest under
      // `embeddings.float` whatever embedding types were asked for.
      return Response.json({ embeddings: { float: texts.map((text) => vectorOf(keyOf(text))) } });
    }
    if (request.method === "GET" && url.pathname === "/__fixture/calls") {
      const body: FakeEmbedCalls = { calls: state.calls };
      return Response.json(body);
    }
    if (request.method === "PUT" && url.pathname === "/__fixture/markers") {
      const markers = (await request.json()) as string[][];
      state = {
        ...state,
        markers: markers.map((words) => words.map((word) => word.toLowerCase())),
      };
      return Response.json({ ok: true });
    }
    if (request.method === "PUT" && url.pathname === "/__fixture/calls") {
      // The calls alone, so a row that made the server embed while it was setting its fixture
      // up can start counting from zero without seeding its markers again.
      state = { ...state, calls: [] };
      return Response.json({ ok: true });
    }
    if (request.method === "PUT" && url.pathname === "/__fixture/reset") {
      state = emptyState();
      return Response.json({ ok: true });
    }
    if (url.pathname === "/healthz") return Response.json({ ok: true });
    return Response.json({ message: "not found" }, { status: 404 });
  },
});

console.log(`fake embedder listener on 127.0.0.1:${server.port}`);
