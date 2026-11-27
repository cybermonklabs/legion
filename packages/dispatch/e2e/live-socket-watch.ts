import type { WebSocket as BrowserWebSocket, Page, WebSocketRoute } from "@playwright/test";

import type { LiveClientFrame, LiveServerFrame } from "../web/src/api/types";

/** Whether a socket the page opened is its one live socket, the one every live view rides. */
export function isLiveSocket(socket: BrowserWebSocket): boolean {
  return new URL(socket.url()).pathname === "/ws/live";
}

/** Lets a routed live socket use Dispatch's own messages and preserves its close code, so a row
 *  that routes the live socket to hold one of its opens still talks to the real server. Returns
 *  the server side, whose messages a row may take over. */
export function connect(route: WebSocketRoute): WebSocketRoute {
  const server = route.connectToServer();
  server.onClose((code, reason) =>
    route.close({
      ...(code === undefined ? {} : { code }),
      ...(reason === undefined ? {} : { reason }),
    })
  );
  return server;
}

/** `connect` for a routed live socket whose page is handed no `event` frame. Every other frame
 *  of Dispatch's (`events_start`, endings, errors, heartbeats) reaches the page as it comes, and
 *  the socket's close stays Dispatch's own. An event is dropped, never delivered later: the page's
 *  feed resumes after the last event it applied, so a socket that ends with events dropped leaves
 *  that point where the socket began, and the next connection's replay carries them. A row that
 *  rotates a page "before it has seen any event" gets that state whatever else commits on the
 *  shared feed meanwhile. `watchLiveSockets` still records a dropped event in `received`, which
 *  is what Dispatch sent and not what the route handed on. */
export function connectWithoutEvents(route: WebSocketRoute): void {
  connect(route).onMessage((message) => {
    if ((JSON.parse(String(message)) as LiveServerFrame).type !== "event") route.send(message);
  });
}

/** What a page's live sockets did: those open now, every one it opened, every frame it sent, and
 *  the type of every frame Dispatch sent them, in order. */
export interface LiveSockets {
  readonly open: Set<BrowserWebSocket>;
  readonly opened: BrowserWebSocket[];
  readonly sent: LiveClientFrame[];
  readonly received: LiveServerFrame["type"][];
}

/** Records what the page's live sockets do from here on: call it before the page opens one. */
export function watchLiveSockets(page: Page): LiveSockets {
  const sockets: LiveSockets = { open: new Set(), opened: [], received: [], sent: [] };
  page.on("websocket", (socket) => {
    if (!isLiveSocket(socket)) return;
    sockets.open.add(socket);
    sockets.opened.push(socket);
    socket.on("close", () => sockets.open.delete(socket));
    // The page sends only the contract's client frames and Dispatch only its server frames, each
    // one JSON object.
    socket.on("framesent", ({ payload }) => {
      sockets.sent.push(JSON.parse(String(payload)) as LiveClientFrame);
    });
    socket.on("framereceived", ({ payload }) => {
      sockets.received.push((JSON.parse(String(payload)) as LiveServerFrame).type);
    });
  });
  return sockets;
}
