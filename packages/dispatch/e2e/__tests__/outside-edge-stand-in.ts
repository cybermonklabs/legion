/** The edge `outside-edge.test.ts` runs the outside driver (`e2e/acceptance/outside-edge.ts`)
 *  against: the secrets broker's edge and the edge listener on one origin, as the driver reaches
 *  them, beside the harness's own Dispatch (`e2e/run-server.sh`) and fake broker
 *  (`e2e/fake-broker.ts`), which that Dispatch relays its machine-login page to.
 *
 *  The listener half is envoy-client's relay socket stand-in
 *  (`@legion/envoy-client/test/relay-socket-stand-in`): its relay socket, its token exchange and
 *  its token checks, with the two listener routes a session's registration and send need served
 *  through its `routes` hook, and a peer that answers through its `deliver`.
 *  - `POST /v1/interests/subscribe` takes a registration and answers it as a listener does
 *    (`@legion/envoy-client/test/subscribe-answer`).
 *  - `POST /v1/messages/send` takes a send. To the peer, `outside-e2e ping <nonce> at <ms>` is
 *    answered `outside-e2e pong <nonce> at <ms>` and then `outside-e2e hello <nonce> at <ms>`, as
 *    the developer box's peer answers, each delivered to the sender's relay socket as the listener
 *    delivers a stored message; to the silent peer it is taken and never answered; to any other
 *    session it is 404, as the listener answers a send to a session it does not hold.
 *  - `DELETE /v1/sessions/<id>` ends a registration.
 *
 *  The broker half serves the device login's grants itself, ahead of the stand-in's exchange.
 *  - `POST /v1/oauth/device_authorization` reads the person from the signed request's
 *    `login_hint`, and puts a pending machine login for that person under a fresh code into the
 *    fake broker (`/__fixture/machine-logins`), which is where the Dispatch page finds and
 *    decides it. Its page is the outside origin's `/credentials/machine`.
 *  - `POST /v1/oauth/token`'s device-code grant reads that login back from the fake broker by its
 *    code (`/v1/machine-logins/lookup`): pending is `authorization_pending`, approved answers a
 *    DPoP-bound pair naming the credential the approval minted; a refresh mints a new pair. Every
 *    other grant goes on to the stand-in's exchange.
 *  The DPoP proofs of the device grant are not checked here: the relay stand-in checks the proofs
 *  of the trade and the socket, which are the ones the listener checks.
 */

import { randomBytes } from "node:crypto";
import { legacyAgentSubject } from "@legion/contracts";
import {
  type RelaySocketStandIn,
  startRelaySocketStandIn,
} from "@legion/envoy-client/test/relay-socket-stand-in";
import { consumerDelivery, TEST_STREAM_ID } from "@legion/envoy-client/test/subscribe-answer";
import type { RecordResponse } from "../../web/src/api/types";

/** One machine login the fake broker holds: the code its page selects it by, and its record. */
interface MachineLogin {
  readonly code: string;
  readonly record: RecordResponse;
}

/** A device authorization the stand-in answered, by its device code. */
interface Authorization {
  readonly code: string;
  readonly person: string;
}

export interface EdgeStandIn {
  /** The origin of both halves: `OUTSIDE_BROKER_URL` and `OUTSIDE_LISTENER_URL`. */
  readonly url: string;
  readonly relay: RelaySocketStandIn;
  /** Every session registered and not ended, and every send, in order. */
  readonly registered: Set<string>;
  readonly sends: { readonly from: string; readonly to: string; readonly message: string }[];
  /** Puts one more pending machine login for `person` under `code` into the fake broker: the
   *  tailnet leg's code, which the driver approves beside its own. */
  addMachineLogin(code: string, person: string): Promise<void>;
  /** The record the fake broker holds for `code` now. */
  lookup(code: string): Promise<RecordResponse>;
  /** Lists each login the page approved on the fake broker's machine-logins list
   *  (`/__fixture/launcher-credentials`), for its approver, as the broker lists a person's own
   *  live machine logins; the revoke the driver's last step makes takes a row off it. */
  seedApproved(): Promise<void>;
  /** The credential ids the fake broker lists for `person` now. */
  listed(person: string): Promise<string[]>;
  stop(): void;
}

/** A user code as the broker prints them: eight of its consonants, a hyphen in the middle. */
function userCode(): string {
  const letters = "BCDFGHJKLMNPQRSTVWXZ";
  const picked = [...randomBytes(8)].map((byte) => letters[byte % letters.length]).join("");
  return `${picked.slice(0, 4)}-${picked.slice(4)}`;
}

function oauthError(error: string): Response {
  return Response.json(
    { error, error_description: `the stand-in answered ${error}` },
    { status: 400 }
  );
}

/** The person a signed device-authorization request names, read without its signature. */
function loginHint(request: string): string {
  const claims: unknown = JSON.parse(
    Buffer.from(request.split(".")[1] ?? "", "base64url").toString("utf8")
  );
  const hint =
    typeof claims === "object" && claims !== null ? Reflect.get(claims, "login_hint") : undefined;
  return typeof hint === "string" ? hint : "";
}

/** The envelope a listener delivers for a send from `from` to `to`. */
function envelope(from: string, to: string, text: string) {
  const id = randomBytes(16).toString("hex");
  return {
    event_id: id,
    source: "agent",
    source_event_id: id,
    source_session: from,
    topic: legacyAgentSubject(to),
    dedupe_key: `agent.${from}.${id}`,
    issued_at: Date.now(),
    payload_summary: text,
    payload: text,
    trace_id: id,
    sender: { session_id: from },
  };
}

/**
 * Starts the edge for one stack: `dispatch` is its canonical origin, where the fake broker's
 * fixtures are not, `fakeBroker` the fake broker's origin, `outsideOrigin` the origin the device
 * login's page is on, and `peer` and `silentPeer` the two sessions a send may reach.
 */
export function startEdgeStandIn(options: {
  readonly fakeBroker: string;
  readonly outsideOrigin: string;
  readonly peer: string;
  readonly silentPeer: string;
}): EdgeStandIn {
  const logins: MachineLogin[] = [];
  const authorizations = new Map<string, Authorization>();
  const registered = new Set<string>();
  const sends: EdgeStandIn["sends"] = [];
  let sequence = 0;

  const putLogins = async (): Promise<void> => {
    const put = await fetch(`${options.fakeBroker}/__fixture/machine-logins`, {
      method: "PUT",
      body: JSON.stringify(logins),
    });
    if (!put.ok) throw new Error(`the fake broker took no machine logins: ${put.status}`);
  };

  const addMachineLogin = async (code: string, person: string): Promise<void> => {
    // The fixture replaces the fake broker's whole list, so each login already in it is read
    // back first: a decision the page made since it was put there stays made.
    for (const [index, login] of logins.entries()) {
      logins[index] = { code: login.code, record: await lookup(login.code) };
    }
    const now = Date.now();
    logins.push({
      code,
      record: {
        approver: person,
        decided: null,
        enrollment: null,
        expires_at: new Date(now + 10 * 60_000).toISOString(),
        identifiers: ["outside-edge-e2e"],
        kind: "launcher_credential",
        lifetime_seconds: 900,
        reason: "",
        record_id: `record-${logins.length + 1}-${code}`,
        requested_at: new Date(now).toISOString(),
        rules_version: "v3",
        session: { enrollment: null, request: null },
        service: null,
        state: "pending",
      },
    });
    await putLogins();
  };

  const lookup = async (code: string): Promise<RecordResponse> => {
    const answer = await fetch(`${options.fakeBroker}/v1/machine-logins/lookup`, {
      method: "POST",
      body: JSON.stringify({ code }),
    });
    if (!answer.ok) throw new Error(`the fake broker holds no login of ${code}: ${answer.status}`);
    return (await answer.json()) as RecordResponse;
  };

  const seedApproved = async (): Promise<void> => {
    const rows = [];
    for (const login of logins) {
      const record = await lookup(login.code);
      const credential = record.decided?.credential_id;
      if (record.state !== "approved" || credential == null) continue;
      rows.push({
        approved_by: record.approver,
        credential_id: credential,
        expired: false,
        expires_at: new Date(Date.now() + 30 * 86_400_000).toISOString(),
        host: record.identifiers?.[0] ?? "",
        issued_at: record.decided?.at ?? new Date().toISOString(),
        person: null,
        service: null,
        subject: null,
        listedFor: record.approver,
      });
    }
    const put = await fetch(`${options.fakeBroker}/__fixture/launcher-credentials`, {
      method: "PUT",
      body: JSON.stringify(rows),
    });
    if (!put.ok) throw new Error(`the fake broker took no machine-logins list: ${put.status}`);
  };

  const listed = async (person: string): Promise<string[]> => {
    const answer = await fetch(
      `${options.fakeBroker}/v1/launcher-credentials?${new URLSearchParams({ approver: person })}`
    );
    const body: unknown = await answer.json();
    const rows =
      typeof body === "object" && body !== null ? Reflect.get(body, "credentials") : undefined;
    return Array.isArray(rows) ? rows.map((row) => String(Reflect.get(row, "credential_id"))) : [];
  };

  const loginAnswer = (credential: string): Response =>
    Response.json({
      access_token: `edge-login-${randomBytes(16).toString("hex")}`,
      token_type: "DPoP",
      expires_in: 900,
      refresh_token: `edge-refresh-${randomBytes(16).toString("hex")}`,
      scope: "machine",
      credential_id: credential,
      credential_expires_at: new Date(Date.now() + 30 * 86_400_000)
        .toISOString()
        .replace(/\.\d{3}Z$/, "Z"),
    });

  const token = async (request: Request): Promise<Response | undefined> => {
    const form = new URLSearchParams(await request.text());
    switch (form.get("grant_type")) {
      case "urn:ietf:params:oauth:grant-type:device_code": {
        const authorization = authorizations.get(form.get("device_code") ?? "");
        if (authorization === undefined) return oauthError("invalid_grant");
        const record = await lookup(authorization.code);
        if (record.state === "pending") return oauthError("authorization_pending");
        if (record.state === "denied") return oauthError("access_denied");
        if (record.state !== "approved" || record.decided?.credential_id == null) {
          return oauthError("expired_token");
        }
        return loginAnswer(record.decided.credential_id);
      }
      case "refresh_token":
        return loginAnswer("refreshed");
      default:
        // The exchange: the relay stand-in's own trade mints the session's token.
        return undefined;
    }
  };

  const send = async (request: Request, relay: RelaySocketStandIn): Promise<Response> => {
    const body: unknown = await request.json();
    const field = (name: string) => {
      const value = typeof body === "object" && body !== null ? Reflect.get(body, name) : undefined;
      return typeof value === "string" ? value : "";
    };
    const from = field("source_session");
    const to = field("target_session");
    const message = field("message");
    sends.push({ from, to, message });
    if (to !== options.peer && to !== options.silentPeer) {
      return Response.json(
        { error: `no session ${to}`, code: "session_not_found" },
        { status: 404 }
      );
    }
    const ping = /^outside-e2e ping ([A-Za-z0-9-]{1,64})( at [0-9]+)?$/.exec(message);
    if (to === options.peer && ping !== null) {
      // The answers go as the peer's own sends would reach the sender: frames on the sender's
      // relay socket, a moment after this send is answered, the pong and then the hello.
      const answer = (word: string, afterMs: number) =>
        setTimeout(() => {
          sequence += 1;
          relay.deliver(from, {
            headers: {
              "Envoy-Subject": [legacyAgentSubject(from)],
              "Envoy-Stream-Seq": [String(sequence)],
              "Envoy-Stream-Id": [TEST_STREAM_ID],
              "Envoy-Delivered": ["1"],
              "Envoy-Stored-At": [new Date().toISOString()],
            },
            data: JSON.stringify(
              envelope(to, from, `outside-e2e ${word} ${ping[1]} at ${Date.now()}`)
            ),
          });
        }, afterMs);
      answer("pong", 100);
      answer("hello", 200);
    }
    return Response.json({
      ...envelope(from, to, message),
      recipient: to,
      recipient_state: "live",
    });
  };

  const relay = startRelaySocketStandIn({
    routes: async (request, standIn) => {
      const { pathname } = new URL(request.url);
      if (request.method === "POST" && pathname === "/v1/oauth/device_authorization") {
        const form = new URLSearchParams(await request.text());
        const code = userCode();
        const deviceCode = `edge-device-${randomBytes(16).toString("hex")}`;
        const person = loginHint(form.get("request") ?? "");
        authorizations.set(deviceCode, { code, person });
        await addMachineLogin(code, person);
        return Response.json({
          device_code: deviceCode,
          user_code: code,
          verification_uri: `${options.outsideOrigin}/credentials/machine`,
          expires_in: 600,
          interval: 1,
        });
      }
      if (request.method === "POST" && pathname === "/v1/oauth/token") return token(request);
      if (request.method === "POST" && pathname === "/v1/interests/subscribe") {
        const body: unknown = await request.json();
        const session =
          typeof body === "object" && body !== null ? String(Reflect.get(body, "session_id")) : "";
        registered.add(session);
        return Response.json({
          session_id: session,
          machine_id: "outside-edge-stand-in",
          dir: "/outside-edge-e2e",
          topics: [legacyAgentSubject(session)],
          delivery: consumerDelivery(session),
        });
      }
      if (request.method === "POST" && pathname === "/v1/messages/send")
        return send(request, standIn);
      const ended = /^\/v1\/sessions\/([^/]+)$/.exec(pathname);
      if (request.method === "DELETE" && ended !== null) {
        registered.delete(decodeURIComponent(ended[1] ?? ""));
        return Response.json({ deleted: true });
      }
      return undefined;
    },
  });

  return {
    get url() {
      return relay.url;
    },
    relay,
    registered,
    sends,
    addMachineLogin,
    lookup,
    seedApproved,
    listed,
    stop: () => relay.stop(),
  };
}
