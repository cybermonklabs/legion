import type { WebSocket as BrowserWebSocket, Page, WebSocketRoute } from "@playwright/test";

import type { LiveClientFrame, LiveServerFrame } from "../web/src/api/types";

/** Whether a socket the page opened is its one live socket, the one every live view rides. */
export function isLiveSocket(socket: BrowserWebSocket): boolean {
  return new URL(socket.url()).pathname === "/ws/live";
}

/** Lets a routed live socket use Dispatch's own messages and preserves its close code, so a row
 *  that routes the live socket to hold one of its opens still talks to the real server. */
export function connect(route: WebSocketRoute): void {
  const server = route.connectToServer();
  server.onClose((code, reason) =>
    route.close({
      ...(code === undefined ? {} : { code }),
      ...(reason === undefined ? {} : { reason }),
    })
  );
}

/** What a page's live sockets did: those open now, every one it opened, every frame it sent, and
 *  the type of every frame it received, in order. */
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
