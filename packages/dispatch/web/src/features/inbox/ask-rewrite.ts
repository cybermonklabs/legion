// Rewrite and Define (prototype, `e2e/ask-card-demo/README.md`): what an ask read carries of a rewrite,
// and the two routes the card calls. No Dispatch route holds either yet, so the routes are
// called with the page's own fetch, outside the typed client, and the ask read's `rewrite` is
// read off the row rather than out of the generated contract.
import type { Actor, Ask, AskOption } from "../../api/types";

/** A rewrite of an ask: running, applied, or refused. `original` is the wording the agent
 *  wrote, which the card shows in place of the rewrite on `w`. `by` says who asked for it: a
 *  person who pressed Rewrite, or Dispatch's check when the agent opened the ask. */
export interface AskRewrite {
  state: "running" | "done" | "failed";
  by: "person" | "check";
  at: string;
  original: { question: string; options: Pick<AskOption, "label" | "description">[] };
  /** Why a failed rewrite was not applied, for the badge's tooltip. */
  reason?: string;
}

/** The rewrite an ask read carries, if any. The field is the prototype's, outside the generated
 *  contract, so it is read with a guard rather than through the `Ask` type. */
export function askRewrite(ask: Ask): AskRewrite | undefined {
  if (!("rewrite" in ask)) return undefined;
  const rewrite: unknown = ask.rewrite;
  if (typeof rewrite !== "object" || rewrite === null || !("state" in rewrite)) return undefined;
  // The stub writes the whole object, so a read with a state is the prototype's own shape.
  const held = rewrite as AskRewrite;
  return held;
}

async function call<T>(method: string, path: string, body?: unknown): Promise<T> {
  const response = await fetch(path, {
    body: body === undefined ? undefined : JSON.stringify(body),
    credentials: "same-origin",
    headers: body === undefined ? undefined : { "content-type": "application/json" },
    method,
  });
  const answer: unknown = await response.json().catch(() => undefined);
  if (!response.ok) {
    const message =
      typeof answer === "object" && answer !== null && "error" in answer
        ? String(answer.error)
        : `${method} ${path}: ${response.status}`;
    throw new Error(message);
  }
  return answer as T;
}

/** `POST /api/v1/asks/{id}/rewrite`: rewrite the ask so it stands alone. The answer is the ask
 *  as the rewrite left it: rewritten, or with a failed rewrite and the wording unchanged. */
export function rewriteAsk(id: string): Promise<Ask> {
  return call("POST", `/api/v1/asks/${encodeURIComponent(id)}/rewrite`);
}

/** Where the selected words stand: in an ask, or in a document. */
export type DefinitionPlace =
  | { ask_id: string }
  | { issue: string; document: string; version?: number };

/** A definition: what the words mean where they stand, from Dispatch's record, and, for words
 *  in an ask, the asking agent's own answer once it comes. */
export interface Definition {
  id: string;
  words: string;
  meaning: string;
  /** The place in the documents that defines the words, where one does. */
  link?: { label: string; href: string };
  agent: {
    state: "asking" | "answered" | "none";
    actor?: Actor;
    answer?: string;
  };
}

/** `POST /api/v1/definitions`: define `words` where they stand. */
export function defineWords(words: string, place: DefinitionPlace): Promise<Definition> {
  return call("POST", "/api/v1/definitions", { words, ...place });
}

/** `GET /api/v1/definitions/{id}`: the definition again, with the agent's answer once it came. */
export function readDefinition(id: string): Promise<Definition> {
  return call("GET", `/api/v1/definitions/${encodeURIComponent(id)}`);
}
