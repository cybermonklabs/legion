// The views demo's stub (packages/dispatch/e2e/views-demo/README.md), installed before the app's
// own entry by vite.demo.config.ts, so no app file imports it. It replaces `fetch` and
// `WebSocket` for the page: every `/api/` and `/auth/` request and the `/ws/live` socket are
// answered in the browser by DemoServer, from the fixtures; anything else goes to the browser's
// own fetch. It works the same from the dev server and from the single file opened from the disk,
// where no service worker can run. The demo's clock goes in first, before any other module reads
// the time.
import "./clock";
import { showBanner } from "./banner";
import { loadFixtures } from "./fixtures";
import { demoWebSocket } from "./live";
import { DemoServer } from "./server";

declare global {
  interface Window {
    /** The requests no stub route answered, for the screenshot run to read. */
    __dispatchDemoUnanswered?: string[];
  }
}

/** How long each answer takes, so the page meets the order of events a network gives it. */
const ANSWER_DELAY_MS = 15;

const nativeFetch = window.fetch.bind(window);
const ready = loadFixtures(nativeFetch).then((fixtures) => {
  const server = new DemoServer(fixtures);
  window.__dispatchDemoUnanswered = server.unanswered;
  return server;
});
ready.catch((error: unknown) => console.error("[views demo] the fixtures did not load", error));

function stubbed(url: URL): boolean {
  return url.pathname.startsWith("/api/") || url.pathname.startsWith("/auth/");
}

async function demoFetch(input: RequestInfo | URL, init?: RequestInit): Promise<Response> {
  const request = input instanceof Request ? input : undefined;
  const url = new URL(request?.url ?? String(input), window.location.href);
  if (!stubbed(url)) return nativeFetch(input, init);
  const method = (init?.method ?? request?.method ?? "GET").toUpperCase();
  const raw = init?.body ?? (request === undefined ? undefined : await request.clone().text());
  const body = typeof raw === "string" && raw !== "" ? JSON.parse(raw) : undefined;
  const server = await ready;
  const answered = Promise.withResolvers<void>();
  setTimeout(answered.resolve, ANSWER_DELAY_MS);
  await answered.promise;
  if (init?.signal?.aborted === true) throw new DOMException("aborted", "AbortError");
  const answer = server.handle(method, url, body);
  if (answer === undefined) {
    server.missed(method, url);
    return Response.json(
      { code: "DEMO_UNANSWERED", error: `the views demo answers no ${method} ${url.pathname}` },
      { status: 404 }
    );
  }
  if (answer.status === 204) return new Response(null, { status: 204 });
  return Response.json(answer.body, { status: answer.status });
}

// The page calls fetch with a request and its options alone; nothing reads the other members
// `typeof fetch` names (Bun's `preconnect`), so the stub's function stands for it as it is.
window.fetch = demoFetch as typeof fetch;

window.WebSocket = demoWebSocket(ready);

// An upload goes by XMLHttpRequest, which no view of the demo sends; one that does is recorded.
const nativeOpen = XMLHttpRequest.prototype.open;
XMLHttpRequest.prototype.open = function open(
  this: XMLHttpRequest,
  method: string,
  url: string | URL,
  ...rest: [boolean?, string?, string?]
) {
  const target = new URL(String(url), window.location.href);
  if (stubbed(target)) void ready.then((server) => server.missed(`XHR ${method}`, target));
  return nativeOpen.call(this, method, url, rest[0] ?? true, rest[1], rest[2]);
};

void ready.then((server) => showBanner(server.fixtures.manifest));
