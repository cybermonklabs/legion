import { DOCUMENT_CLIENT_ID_PARAMETER } from "@legion/contracts";
import {
  createDecoder,
  type Decoder,
  readVarString,
  readVarUint,
  readVarUint8Array,
} from "lib0/decoding";
import * as Y from "yjs";

import { readClientIdMessage } from "../web/src/features/doc/sync-frame";
import { createIssue, createProject, getArtifactText, newestArtifactVersion } from "./api";
import {
  connectedDot,
  cursorLabel,
  documentEditor,
  pendingEditCount,
  placeCaret,
  typeAtEnd,
} from "./editor";
import { expect, type Page, test, type WebSocketRoute } from "./test";
import { asUser } from "./users";

// The page writes and announces under the client id Dispatch gives it, which is 2^32 or above
// (docs client_ids.go), where Yjs in the browser has drawn 32-bit ids until now. This row holds
// that id through everything the editor does with it, with the pinned yjs, y-protocols and lib0:
// the socket's first frame gives it, the edits the page sends carry it, a version settles holding
// what was typed under it, a reconnect presents it and is given it back, a second browser is shown
// the cursor announced under it, and a reload replays the row saved under it.

/** Every frame each document socket of a page carried, in each direction, by the order they were
 *  made; `hold` keeps the sockets made from then on from reaching the server until `release`, and
 *  `sever` closes the ones made so far as a network blip does. */
async function observedTransport(page: Page) {
  const sockets: {
    url: string;
    received: Uint8Array[];
    sent: Uint8Array[];
    route: WebSocketRoute;
  }[] = [];
  const held: (() => void)[] = [];
  let holding = false;
  const bytes = (message: string | Buffer): Uint8Array =>
    typeof message === "string" ? new TextEncoder().encode(message) : new Uint8Array(message);
  await page.routeWebSocket(/\/ws\/doc\//u, (route) => {
    const socket = {
      received: [] as Uint8Array[],
      route,
      sent: [] as Uint8Array[],
      url: route.url(),
    };
    sockets.push(socket);
    const pending: (string | Buffer)[] = [];
    let server: WebSocketRoute | undefined;
    route.onMessage((message) => {
      socket.sent.push(bytes(message));
      if (server === undefined) {
        pending.push(message);
        return;
      }
      server.send(message);
    });
    const connect = () => {
      const connected = route.connectToServer();
      connected.onMessage((message) => {
        socket.received.push(bytes(message));
        route.send(message);
      });
      server = connected;
      for (const message of pending.splice(0)) {
        connected.send(message);
      }
    };
    if (holding) {
      held.push(connect);
    } else {
      connect();
    }
  });
  return {
    hold: () => {
      holding = true;
    },
    release: () => {
      holding = false;
      for (const connect of held.splice(0)) {
        connect();
      }
    },
    sever: async () => {
      for (const { route } of sockets) {
        await route.close({ code: 1012, reason: "transport blip" }).catch(() => undefined);
      }
    },
    sockets,
  };
}

/** The Hocuspocus frame's message type, the decoder left at the message's body. */
function messageOf(frame: Uint8Array): { decoder: Decoder; type: number } {
  const decoder = createDecoder(frame);
  readVarString(decoder); // the document name
  return { decoder, type: readVarUint(decoder) };
}

/** The client id a socket's first frame gave the page, as the page reads it. */
function givenId(received: Uint8Array[]): number | undefined {
  const first = received[0];
  return first === undefined ? undefined : readClientIdMessage(first);
}

/** The client of every struct in the Update frames a socket sent, decoded as the server decodes it. */
function updateClients(sent: Uint8Array[]): number[] {
  return sent.flatMap((frame) => {
    const { decoder, type } = messageOf(frame);
    if (type !== 0 || readVarUint(decoder) !== 2) {
      return [];
    }
    return Y.decodeUpdate(readVarUint8Array(decoder)).structs.map((struct) => struct.id.client);
  });
}

/** The client of every entry in the awareness frames a socket sent. */
function announcedClients(sent: Uint8Array[]): number[] {
  return sent.flatMap((frame) => {
    const { decoder, type } = messageOf(frame);
    if (type !== 1) {
      return [];
    }
    const update = createDecoder(readVarUint8Array(decoder));
    const clients: number[] = [];
    for (let count = readVarUint(update); count > 0; count -= 1) {
      clients.push(readVarUint(update));
      readVarUint(update); // the clock
      readVarString(update); // the state
    }
    return clients;
  });
}

test("a page given a client id above 2^32 types, settles a version, reconnects under it, shows its cursor and replays a saved row under it", async ({
  browser,
}) => {
  await createProject({ key: "CLID", name: "Client ids" });
  const issue = await createIssue({
    project: "CLID",
    spec: "# Client ids\n\nAlice's paragraph.\n",
    title: "Client ids above 2^32",
  });
  const artifactId = issue.primary_artifact_id;
  const alice = await asUser(browser, "alice");
  const bob = await asUser(browser, "bob");
  try {
    const alicePage = await alice.newPage();
    const transport = await observedTransport(alicePage);
    const bobPage = await bob.newPage();
    await Promise.all([
      alicePage.goto(`/issues/${issue.key}/spec`),
      bobPage.goto(`/issues/${issue.key}/spec`),
    ]);
    const bobEditor = documentEditor(bobPage);
    await expect(connectedDot(alicePage)).toHaveText("connected");
    await expect(connectedDot(bobPage)).toHaveText("connected");
    const first = transport.sockets[0];
    if (first === undefined) {
      throw new Error("alice's page opened no document socket");
    }
    expect(new URL(first.url).searchParams.get(DOCUMENT_CLIENT_ID_PARAMETER)).toBe("");
    const given = givenId(first.received);
    expect(given).toBeGreaterThanOrEqual(2 ** 32);

    // Typed under the given id: every struct the page sends carries it whole.
    await typeAtEnd(alicePage, " minted");
    await expect(bobEditor).toContainText("minted");
    expect(new Set(updateClients(first.sent))).toEqual(new Set([given]));

    // A version settles holding what was typed under it, attributed to alice.
    await expect
      .poll(async () => {
        const newest = await newestArtifactVersion(artifactId);
        if (newest === undefined) {
          return undefined;
        }
        return {
          alice: newest.version.authors?.some(({ id }) => id === "alice") ?? false,
          holds: newest.markdown.includes("minted"),
        };
      })
      .toEqual({ alice: true, holds: true });

    // A reconnect presents the id and is given it back, and the page types on under it.
    await transport.sever();
    await expect.poll(() => transport.sockets.length).toBe(2);
    const second = transport.sockets[1];
    if (second === undefined) {
      throw new Error("alice's page did not reconnect");
    }
    expect(new URL(second.url).searchParams.get(DOCUMENT_CLIENT_ID_PARAMETER)).toBe(String(given));
    await expect.poll(() => givenId(second.received)).toBe(given);
    await expect(connectedDot(alicePage)).toHaveText("connected");
    await typeAtEnd(alicePage, " again");
    await expect(bobEditor).toContainText("again");
    expect(new Set(updateClients(second.sent))).toEqual(new Set([given]));

    // Bob is shown the cursor alice's page announces under it. The provider also sends back the
    // states it was sent of the room's other readers - bob's, under the id bob's page was given -
    // which the server's filter drops (awareness_limit.go); every id it sends is whole.
    await placeCaret(alicePage, "after", "Alice's");
    await expect(cursorLabel(bobPage, "alice")).toBeVisible();
    const announced = transport.sockets.flatMap((socket) => announcedClients(socket.sent));
    expect(announced).toContain(given);
    expect(announced.every((client) => client >= 2 ** 32)).toBe(true);

    // A row saved under it while the socket is down is replayed by the reloaded page under it.
    transport.hold();
    await transport.sever();
    await typeAtEnd(alicePage, " saved row");
    await expect.poll(() => pendingEditCount(alicePage, artifactId)).toBeGreaterThanOrEqual(1);
    const beforeReload = transport.sockets.length;
    await alicePage.reload();
    transport.release();
    await expect
      .poll(async () => (await getArtifactText(artifactId)).markdown)
      .toContain("saved row");
    const reloaded = transport.sockets.at(-1);
    if (reloaded === undefined || transport.sockets.length <= beforeReload) {
      throw new Error("the reloaded page opened no document socket");
    }
    const fresh = givenId(reloaded.received);
    expect(fresh).toBeGreaterThanOrEqual(2 ** 32);
    expect(fresh).not.toBe(given);
    expect(updateClients(reloaded.sent)).toContain(given);
    await expect(bobEditor).toContainText("saved row");
  } finally {
    await bob.close();
    await alice.close();
  }
});
