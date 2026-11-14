// The shapes the ask card demo's stub answers with (packages/dispatch/e2e/ask-card-demo/README.md):
// one route, one answer, and the helpers every route module of the stub builds them with.

/** The answer of one request: its status and its JSON body. */
export interface DemoAnswer {
  status: number;
  body: unknown;
}

/** One route of the stub: its method, its path, and its answer from the path's captures, the
 *  query and the body. A promise where the route takes time, as a rewrite does. */
export interface DemoRoute {
  method: string;
  pattern: RegExp;
  answer: (
    captures: string[],
    q: URLSearchParams,
    body: unknown
  ) => DemoAnswer | Promise<DemoAnswer>;
}

export function ok(body: unknown): DemoAnswer {
  return { status: 200, body };
}

export function notFound(code: string): DemoAnswer {
  return { status: 404, body: { code, error: `${code.toLowerCase()} (ask card demo)` } };
}

/** A time as the server writes it (RFC 3339 with trailing fractional zeros dropped). */
export function goTime(date: Date): string {
  return date.toISOString().replace(/\.?0+Z$/, "Z");
}
