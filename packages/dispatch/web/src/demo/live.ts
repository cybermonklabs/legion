// The ask card demo's `/ws/live` (packages/dispatch/e2e/ask-card-demo/README.md): a WebSocket that
// never leaves the page and speaks Dispatch's live protocol (packages/contracts
// dispatch-api-live.ts) from the stub. The event feed starts at the stub's head and carries the
// stub's own events; any other view is refused. A document's socket is the document service's
// (`document-room.ts`); any other socket is refused and recorded.
import type { LiveClientFrame, LiveServerFrame } from "@legion/contracts";
import { type DocumentSocket, openDocumentSocket } from "./document-room";
import type { DemoServer } from "./server";

const HEARTBEAT_MS = 15_000;

type SocketListener = (event: Event) => void;

/** The WebSocket the stub puts in place of the browser's own. */
export function demoWebSocket(server: DemoServer): typeof WebSocket {
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
    /** The document service's side, for a document's socket. */
    private document: DocumentSocket | undefined;

    constructor(url: string | URL, _protocols?: string | string[]) {
      super();
      this.url = String(url);
      // `ws:///ws/live` from a page at a `file:` address, which has no host: read the path as text.
      const live = /\/ws\/live(?:[?#]|$)/.test(this.url);
      setTimeout(() => {
        if (this.readyState !== 0) return;
        this.server = server;
        this.document = openDocumentSocket(this.url, (bytes) => this.binary(bytes));
        if (this.document !== undefined) {
          this.readyState = 1;
          this.fire(new Event("open"));
          return;
        }
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
      }, 0);
    }

    send(data: string | ArrayBufferView): void {
      const server = this.server;
      if (this.readyState !== 1 || server === undefined) return;
      if (typeof data !== "string") {
        this.document?.receive(new Uint8Array(data.buffer, data.byteOffset, data.byteLength));
        return;
      }
      const frame = JSON.parse(data) as LiveClientFrame;
      this.subs.get(frame.sub)?.();
      this.subs.delete(frame.sub);
      if (frame.type === "unsubscribe") return;
      const view = frame.view;
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
      // A page's own live views (a board, a list) fall back to their HTTP reads.
      this.frame({
        type: "error",
        sub: frame.sub,
        code: "DEMO_UNANSWERED",
        message: "the ask card demo answers no such view",
      });
    }

    close(code = 1000): void {
      if (this.readyState >= 2) return;
      this.readyState = 3;
      this.stopBeat?.();
      this.document?.close();
      for (const stop of this.subs.values()) stop();
      this.subs.clear();
      setTimeout(() => this.fire(new CloseEvent("close", { code, wasClean: true })), 0);
    }

    private frame(frame: LiveServerFrame): void {
      if (this.readyState !== 1) return;
      const data = JSON.stringify(frame);
      setTimeout(() => this.fire(new MessageEvent("message", { data })), 0);
    }

    /** One binary frame to the page, as an `arraybuffer` socket receives it. */
    private binary(bytes: Uint8Array): void {
      if (this.readyState !== 1) return;
      const data = bytes.slice().buffer;
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
