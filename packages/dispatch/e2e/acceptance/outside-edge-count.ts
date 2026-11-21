/** The request counter of the outside driver (`outside-edge.ts`), whose step 4 judges the run's
 *  peak rate against the edge's rules (`outside-edge-limits.ts`). It must be the driver's first
 *  import: importing it wraps this process's `fetch` and `WebSocket`, and the relay client takes
 *  its WebSocket constructor when its module loads (`@legion/envoy-client/listener-relay`), so a
 *  module loaded before this one would open sockets this counter never sees. Every request the
 *  process makes through either is counted when it is made, whatever answers it. The browser's
 *  own requests are not made through these: the driver counts them from the browser's events. */

import type { Counted } from "./outside-edge-limits";

/** Every request counted so far, in the order they were made. */
export const counted: Counted[] = [];

/** Counts one request to `url`, made now. */
export function count(url: string): void {
  counted.push({ at: Date.now(), url });
}

function urlOf(input: unknown): string {
  if (typeof input === "string") return input;
  if (input instanceof URL) return input.href;
  if (input instanceof Request) return input.url;
  return String(input);
}

globalThis.fetch = new Proxy(globalThis.fetch, {
  apply(target, self, args: Parameters<typeof fetch>) {
    count(urlOf(args[0]));
    return Reflect.apply(target, self, args);
  },
});

globalThis.WebSocket = new Proxy(globalThis.WebSocket, {
  construct(target, args: unknown[], newTarget) {
    count(urlOf(args[0]));
    return Reflect.construct(target, args, newTarget);
  },
});
