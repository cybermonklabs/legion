// The views demo's `/ws/live` (packages/dispatch/e2e/views-demo/README.md): a WebSocket that never
// leaves the page and speaks Dispatch's live protocol (packages/contracts dispatch-api-live.ts)
// from the stub. The `legion` view is answered with the board route's body, again whenever the
// stub's board moves; the event feed starts at the stub's head and carries the stub's own events.
// Any other view, and any other socket, is refused and recorded as unanswered.
import type { LegionBoardResponse, LiveClientFrame, LiveServerFrame } from "@legion/contracts";
import type { DemoServer } from "./server";

const HEARTBEAT_MS = 15_000;

type SocketListener = (event: Event) => void;

/** The WebSocket the stub puts in place of the browser's own: it opens once the stub is ready. */
export function demoWebSocket(ready: Promise<DemoServer>): typeof WebSocket {
  class DemoSocket extends EventTarget {
    static readonly CONNECTING = 0;
    static readonly OPEN = 1;
    static readonly CLOSING = 2;
    static readonly CLOSED = 3;
    readonly CONNECTING = 0;
    readonly OPEN = 1;
    readonly CLOSING = 2;
    readonly CLOSED = 3;
    readonly url: string;
    readonly protocol = "";
    readonly extensions = "";
    readonly bufferedAmount = 0;
    binaryType: BinaryType = "blob";
    readyState = 0;
    onopen: SocketListener | null = null;
    onmessage: SocketListener | null = null;
    onclose: SocketListener | null = null;
    onerror: SocketListener | null = null;
    private server: DemoServer | undefined;
    private stopBeat: (() => void) | undefined;
    /** What each subscription listens to, by its id, to let go on unsubscribe or close. */
    private readonly subs = new Map<string, () => void>();

    constructor(url: string | URL, _protocols?: string | string[]) {
      super();
      this.url = String(url);
      // `ws:///ws/live` from a page at a `file:` address, which has no host: read the path as text.
      const live = /\/ws\/live(?:[?#]|$)/.test(this.url);
      void ready.then((server) => {
        if (this.readyState !== 0) return;
        this.server = server;
        if (!live) {
          server.missed("WEBSOCKET", new URL(this.url, "ws://demo.invalid"));
          this.readyState = 3;
          this.fire(new Event("error"));
          this.fire(new CloseEvent("close", { code: 1006 }));
          return;
        }
        this.readyState = 1;
        this.fire(new Event("open"));
        const beat = setInterval(() => this.frame({ type: "heartbeat" }), HEARTBEAT_MS);
        this.stopBeat = () => clearInterval(beat);
      });
    }

    send(data: string): void {
      const server = this.server;
      if (this.readyState !== 1 || server === undefined) return;
      const frame = JSON.parse(data) as LiveClientFrame;
      this.subs.get(frame.sub)?.();
      this.subs.delete(frame.sub);
      if (frame.type === "unsubscribe") return;
      const view = frame.view;
      if (view.kind === "legion") {
        const snapshot = () => {
          if (!server.hasLegion(view.project)) {
            this.frame({
              type: "error",
              sub: frame.sub,
              code: "LEGION_NOT_CONFIGURED",
              message: "this project has no Legion daemon",
            });
            return;
          }
          // The contract's card still names `checks_verdict`, which the demo's board leaves
          // out (#2731's newest head removes it); the frame carries the body as it is.
          const body = server.legionBoard() as unknown as LegionBoardResponse;
          this.frame({
            type: "snapshot",
            sub: frame.sub,
            seq: server.eventHead(),
            changed_at: null,
            body,
            revs: { items: {}, maps: {}, counts: {}, fields: {} },
          });
        };
        snapshot();
        this.subs.set(frame.sub, server.onBoard(snapshot));
        return;
      }
      if (view.kind === "events") {
        this.frame({
          type: "events_start",
          sub: frame.sub,
          since: server.eventHead(),
          replay_complete: true,
        });
        this.subs.set(
          frame.sub,
          server.onEvent((id, event) => this.frame({ type: "event", sub: frame.sub, id, event }))
        );
        return;
      }
      server.missed("LIVE", new URL(`/ws/live?kind=${view.kind}`, "ws://demo.invalid"));
      this.frame({
        type: "error",
        sub: frame.sub,
        code: "DEMO_UNANSWERED",
        message: "the views demo answers no such view",
      });
    }

    close(code = 1000): void {
      if (this.readyState >= 2) return;
      this.readyState = 3;
      this.stopBeat?.();
      for (const stop of this.subs.values()) stop();
      this.subs.clear();
      setTimeout(() => this.fire(new CloseEvent("close", { code, wasClean: true })), 0);
    }

    private frame(frame: LiveServerFrame): void {
      if (this.readyState !== 1) return;
      const data = JSON.stringify(frame);
      setTimeout(() => this.fire(new MessageEvent("message", { data })), 0);
    }

    private fire(event: Event): void {
      this.dispatchEvent(event);
      const handler = {
        open: this.onopen,
        message: this.onmessage,
        close: this.onclose,
        error: this.onerror,
      }[event.type];
      handler?.call(this, event);
    }
  }
  return DemoSocket as unknown as typeof WebSocket;
}
