// The requests the Dispatch e2e drivers (dispatch-claimant-delivery.ts, verification-results.ts)
// make of a real envoy-dispatch over HTTP, and how each reads the answer: one place, so the two
// drivers read an answer alike and their tests (dispatch-driver-calls.test.ts) reach every path
// without a run.
//
// Every request carries a credential, the run's agent token or the person's session cookie, and a
// gateway or a proxy in Dispatch's place can answer with a page of its own that echoes the request
// back, that credential with it. So an answer the drivers print is printed whole only where it is
// Dispatch's own - its error JSON on a refusal, JSON on a write it took - and any other body is
// named through `bodyForText`: one line with the credential the request carried redacted, never
// the body whole.
import { bodyForText } from "../../../packages/envoy-client/src/excerpt";

export type Json = Record<string, unknown>;

/** The secret `headers` present, which an excerpt of an answer that is not Dispatch's own
 *  redacts: the token after the `Authorization` scheme, or the value of the session cookie a
 *  person's request sends (`dsession=<value>`). */
function presented(headers: Record<string, string>): string {
  const authorization = headers.Authorization;
  if (authorization !== undefined) return authorization.slice(authorization.indexOf(" ") + 1);
  const cookie = headers.Cookie ?? "";
  return cookie.slice(cookie.indexOf("=") + 1);
}

/** `text` as Dispatch's error JSON, an object whose `error` is a string, or undefined for any
 *  other body. */
function dispatchErrorOf(text: string): (Json & { readonly error: string }) | undefined {
  try {
    const parsed: unknown = JSON.parse(text);
    if (typeof parsed === "object" && parsed !== null && "error" in parsed) {
      if (typeof parsed.error === "string") return parsed as Json & { readonly error: string };
    }
  } catch {
    // Not JSON, so no error JSON of Dispatch's.
  }
  return undefined;
}

/** A refused answer's body as an error names it: Dispatch's error JSON as Dispatch wrote it, and
 *  any other body through `bodyForText`, `secret` redacted. */
function refusedBody(text: string, secret: string): string {
  return dispatchErrorOf(text) === undefined
    ? bodyForText(text, secret, "Dispatch's error JSON")
    : text;
}

/** `method path` against `dispatchUrl` with `headers` and, on a write, `body` as JSON: the
 *  answer's JSON, or undefined for an empty one. A status other than 2xx throws, naming the body
 *  as `refusedBody` does. */
export async function dispatchCall(
  dispatchUrl: string,
  method: string,
  path: string,
  headers: Record<string, string>,
  body?: Json
): Promise<unknown> {
  const response = await fetch(new URL(path, dispatchUrl), {
    body: body === undefined ? undefined : JSON.stringify(body),
    headers: body === undefined ? headers : { ...headers, "Content-Type": "application/json" },
    method,
  });
  const text = await response.text();
  if (!response.ok) {
    throw new Error(
      `${method} ${path}: ${response.status} ${refusedBody(text, presented(headers))}`
    );
  }
  return text === "" ? undefined : JSON.parse(text);
}

/** A write the server refuses, as the case that asserts the refusal reads it: `answer` is its
 *  status and its code, and `error` the words it carries, which a case may read for the commit
 *  and the repository they name. */
export interface Refusal {
  readonly answer: string;
  readonly error: string;
}

/**
 * The write `method path` with `headers` and `body` as JSON, as a case that expects it refused
 * reads its answer. Dispatch's error JSON gives the code and the words, as Dispatch wrote them.
 * Any other body was written by something in Dispatch's place, so it is no Dispatch code a case
 * can compare with, and both fields name it through `bodyForText`: so does `error` for a 2xx
 * whose body is not the JSON Dispatch answers a write it took with.
 */
export async function dispatchRefusal(
  dispatchUrl: string,
  method: string,
  path: string,
  headers: Record<string, string>,
  body: Json
): Promise<Refusal> {
  const response = await fetch(new URL(path, dispatchUrl), {
    body: JSON.stringify(body),
    headers: { ...headers, "Content-Type": "application/json" },
    method,
  });
  const text = await response.text();
  const secret = presented(headers);
  if (response.ok) {
    let error = text;
    try {
      JSON.parse(text);
    } catch {
      error = bodyForText(text, secret, "Dispatch's JSON");
    }
    return { answer: `${response.status} (the write was taken)`, error };
  }
  const refused = dispatchErrorOf(text);
  if (refused !== undefined) {
    return { answer: `${response.status} ${String(refused.code ?? "")}`, error: refused.error };
  }
  const named = bodyForText(text, secret, "Dispatch's error JSON");
  return { answer: `${response.status} ${named}`, error: named };
}

/** The bytes `GET path` answers with `headers`, whatever its content type. A status other than
 *  2xx throws, naming the body as `refusedBody` does. */
export async function dispatchBytes(
  dispatchUrl: string,
  path: string,
  headers: Record<string, string>
): Promise<Uint8Array> {
  const response = await fetch(new URL(path, dispatchUrl), { headers });
  if (!response.ok) {
    const text = await response.text();
    throw new Error(`GET ${path}: ${response.status} ${refusedBody(text, presented(headers))}`);
  }
  return new Uint8Array(await response.arrayBuffer());
}
