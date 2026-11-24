// The body of a request a stand-in server took, as the stand-in reads it before it answers.
//
// A stand-in is a test's own server, and a request can reach it from a client the test did not
// start: a client of a test that has ended keeps posting to the address its own server had, and
// the kernel can give that port to the stand-in (`loopbackHost` keeps a stand-in off the address
// such clients share). Such a request carries its own route and its own body, a form body as an
// OAuth client posts one, say. A stand-in that parses every body as JSON throws out of its
// server's `fetch` on that request, and Bun fails whatever test is running with it. So a stand-in
// reads each body here, and answers a body it cannot take with a refusal that names the request,
// which fails nothing.

/** What `readStandinBody` read of one request: the JSON object its client sent, or the refusal a
 *  stand-in answers a body that is not one with. */
export type StandinBody =
  | { readonly body: Record<string, unknown>; readonly refusal?: undefined }
  | { readonly body?: undefined; readonly refusal: Response };

/**
 * Reads the body of `request`, a request a stand-in server took: a POST's body parsed as JSON, and
 * `{}` for a POST with an empty body and for any other method, whose body is not read. A POST body
 * that cannot be read, is not JSON, or is JSON but not an object is refused: the refusal is a 400
 * whose `error` names the request's method and path and what was wrong with its body, and the same
 * words go to stderr, so a run's log shows the request even where the client that sent it reads
 * nothing. The caller answers the refusal and neither records the request nor hands it on.
 */
export async function readStandinBody(request: Request): Promise<StandinBody> {
  if (request.method !== "POST") return { body: {} };
  let text: string;
  try {
    text = await request.text();
  } catch (error) {
    return refuse(request, `its body could not be read (${String(error)})`);
  }
  if (text === "") return { body: {} };
  let parsed: unknown;
  try {
    parsed = JSON.parse(text);
  } catch {
    const type = request.headers.get("content-type") ?? "no content-type";
    return refuse(request, `its body is not JSON (${type}, ${text.length} characters)`);
  }
  if (!isJsonObject(parsed)) return refuse(request, "its body is JSON but not an object");
  return { body: parsed };
}

/** Whether `value`, parsed JSON, is an object: not an array, `null` or a scalar. */
function isJsonObject(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

/** The refusal of `request`, saying `why`, and its line on stderr. */
function refuse(
  request: Request,
  why: string
): Extract<StandinBody, { readonly refusal: Response }> {
  const url = new URL(request.url);
  const error = `the stand-in at ${url.origin} refused ${request.method} ${url.pathname}: ${why}`;
  console.error(error);
  return { refusal: Response.json({ error }, { status: 400 }) };
}
