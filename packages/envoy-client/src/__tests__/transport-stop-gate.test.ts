import { describe, expect, test } from "bun:test";
import { delivered } from "../../test/delivered";
import { consumerDelivery } from "../../test/subscribe-answer";
import {
  createEnvoyClient,
  EnvoyApiError,
  type EnvoyClient,
  EnvoyStoppingError,
  type SubscribeInput,
  stopHandBack,
} from "../transport";

// The client's stop gate (R24 of dispatch://INFRA-3589): once the host's stop reading answers
// true, a call of the session's registration or role state is refused before anything is sent,
// and the stop's own calls and the session's own content still go.

const SESSION = "ses_stopping";

/** A message the listener stored, as a send's and a publish's answer carry it. */
const ENVELOPE = {
  event_id: "event-stop",
  source: "agent",
  source_session: SESSION,
  source_event_id: "agent.event-stop",
  topic: "notifications.agent.ses_peer",
  dedupe_key: "agent.ses_peer.event-stop",
  issued_at: 1,
  payload_summary: "hello",
  trace_id: "trace-stop",
};

const REGISTRATION: SubscribeInput = {
  sessionID: SESSION,
  directory: "/work",
  topics: ["notifications.dispatch.issue.DSP-1.>"],
  port: 0,
  title: "a stopping session",
  driving: false,
  selfSubscribed: true,
  plugin: { name: "@sjawhar/pi-envoy", version: "11.0.0" },
  delivery: { consumer: true, epoch: "epoch-1" },
};

/** A listener stand-in that answers each request by its route and records it, method first. */
function listener() {
  const fetched: string[] = [];
  const fetch = async (input: string | URL | Request, init?: RequestInit): Promise<Response> => {
    const path = new URL(input.toString()).pathname;
    fetched.push(`${init?.method ?? "GET"} ${path}`);
    if (path === "/v1/interests/subscribe") {
      return Response.json({
        session_id: SESSION,
        machine_id: "host-a",
        dir: "/work",
        topics: REGISTRATION.topics,
        delivery: consumerDelivery(SESSION),
      });
    }
    if (path === "/v1/roles/set" || path === `/v1/interests/${SESSION}`) {
      return Response.json({
        session_id: SESSION,
        machine_id: "host-a",
        dir: "/work",
        topics: ["notifications.role.reviewer"],
      });
    }
    if (path === "/v1/roles/reviewer") {
      return Response.json({ role: "reviewer", holder: SESSION, last_seen: 1 });
    }
    if (path === "/v1/messages/send") return Response.json({ ...ENVELOPE, recipient: "ses_peer" });
    if (path === "/v1/messages/publish") return Response.json(ENVELOPE);
    return new Response("ok");
  };
  return { fetched, fetch };
}

/** A client whose host's stop has begun. */
function stoppedClient(): { readonly client: EnvoyClient; readonly fetched: string[] } {
  const { fetched, fetch } = listener();
  return {
    client: createEnvoyClient({ baseUrl: "http://listener", fetch, stopping: () => true }),
    fetched,
  };
}

/** Answers what `call` threw, failing the row when it did not throw. */
async function refusal(call: Promise<unknown>): Promise<unknown> {
  return call.then(
    () => {
      throw new Error("the call was sent and answered once the stop had begun");
    },
    (error: unknown) => error
  );
}

describe("the stop gate", () => {
  // The control row at the end of this block makes the same five calls with no stop reading.
  test("once the stop has begun, a registration, an unsubscribe, a role set, a role get and an interest read each refuse before any fetch, naming the stop", async () => {
    const { client, fetched } = stoppedClient();

    const refused = [
      await refusal(client.subscribe(REGISTRATION)),
      await refusal(client.unsubscribe({ sessionID: SESSION, topics: ["notifications.slack.x"] })),
      await refusal(client.setRole({ sessionID: SESSION, role: "reviewer", soft: true })),
      await refusal(client.getRole("reviewer")),
      await refusal(client.getInterest(SESSION)),
    ];

    expect(fetched).toEqual([]);
    expect(refused.map((error) => (error as EnvoyStoppingError).call)).toEqual([
      "subscribe",
      "unsubscribe",
      "setRole",
      "getRole",
      "getInterest",
    ]);
    for (const error of refused) {
      expect(error).toBeInstanceOf(EnvoyStoppingError);
      expect((error as Error).message).toStartWith(
        "this session is stopping, so it sends the listener nothing:"
      );
    }
    expect((refused[0] as Error).message).toBe(
      "this session is stopping, so it sends the listener nothing: POST http://listener/v1/interests/subscribe was not sent"
    );
  });

  test("once the stop has begun, the stop's DELETE and its hand-back are sent, and so are a send and a publish", async () => {
    const { client, fetched } = stoppedClient();

    await client.unregisterSession({ sessionID: SESSION, clientInstanceID: "0f8c2b1a4d6e7f90" });
    await stopHandBack(client)({
      sessionID: SESSION,
      topics: ["notifications.role.reviewer"],
      clientInstanceID: "0f8c2b1a4d6e7f90",
    });
    const sent = await client.send({
      sourceSessionID: SESSION,
      targetSessionID: "ses_peer",
      message: "hello",
    });
    const published = await client.publish({
      sourceSessionID: SESSION,
      topic: "notifications.agent.ses_peer",
      message: "hello",
    });

    expect(fetched).toEqual([
      `DELETE /v1/sessions/${SESSION}`,
      "POST /v1/interests/unsubscribe",
      "POST /v1/messages/send",
      "POST /v1/messages/publish",
    ]);
    expect(delivered(sent).envelope.event_id).toBe(ENVELOPE.event_id);
    expect(delivered(published).envelope.event_id).toBe(ENVELOPE.event_id);
  });

  test("a registration whose fetch started before the stop is answered, and a retry after the stop is not sent and ends the call with the sent attempt's answer", async () => {
    let stopping = false;
    const fetched: string[] = [];
    const answers: (() => void)[] = [];
    const bothOnTheWire = Promise.withResolvers<void>();
    const { fetch: answer } = listener();
    // The first registration is answered 200 and the second 503, each only once the row lets it
    // go, after the stop began.
    let registrations = 0;
    const fetch = async (input: string | URL | Request, init?: RequestInit): Promise<Response> => {
      fetched.push(`${init?.method ?? "GET"} ${new URL(input.toString()).pathname}`);
      registrations += 1;
      const failing = registrations > 1;
      const { promise, resolve } = Promise.withResolvers<void>();
      answers.push(resolve);
      if (answers.length === 2) bothOnTheWire.resolve();
      await promise;
      return failing
        ? Response.json({ error: "the listener failed" }, { status: 503 })
        : answer(input, init);
    };
    const client = createEnvoyClient({
      baseUrl: "http://listener",
      fetch,
      stopping: () => stopping,
    });

    const accepted = client.subscribe(REGISTRATION);
    const failed = refusal(client.subscribe(REGISTRATION));
    await bothOnTheWire.promise;
    stopping = true;
    for (const go of answers.splice(0)) go();

    expect((await accepted).topics).toEqual([...REGISTRATION.topics]);
    // The 503 asks for a retry, which the gate refuses: no third request is made, and the call
    // ends with the 503 its sent attempt got, as a call whose retries ran out does.
    const error = await failed;
    expect(error).not.toBeInstanceOf(EnvoyStoppingError);
    expect(error).toBeInstanceOf(EnvoyApiError);
    expect((error as EnvoyApiError).details.status).toBe(503);
    expect(fetched).toEqual(["POST /v1/interests/subscribe", "POST /v1/interests/subscribe"]);
  });

  test("a call whose first attempt got a 5xx or no answer, and whose retry the gate refuses, rejects with that attempt's own error; one whose first attempt the gate refuses rejects with EnvoyStoppingError", async () => {
    // Each fetch begins the stop as it answers: the retry that follows meets the gate.
    let stopping = false;
    const fetched: string[] = [];
    const answering = (answer: () => Response) =>
      createEnvoyClient({
        baseUrl: "http://listener",
        fetch: async (input: string | URL | Request, init?: RequestInit): Promise<Response> => {
          fetched.push(`${init?.method ?? "GET"} ${new URL(input.toString()).pathname}`);
          stopping = true;
          return answer();
        },
        stopping: () => stopping,
      });

    const busy = answering(() => Response.json({ error: "the listener failed" }, { status: 503 }));
    const afterBusy = await refusal(busy.setRole({ sessionID: SESSION, role: "reviewer" }));
    expect(afterBusy).toBeInstanceOf(EnvoyApiError);
    expect((afterBusy as EnvoyApiError).details.status).toBe(503);
    expect(fetched).toEqual(["POST /v1/roles/set"]);

    stopping = false;
    fetched.splice(0);
    const silent = answering(() => {
      throw new Error("the connection reset before an answer");
    });
    const afterSilence = await refusal(silent.getRole("reviewer"));
    expect(afterSilence).not.toBeInstanceOf(EnvoyStoppingError);
    expect((afterSilence as Error).message).toStartWith(
      "GET http://listener/v1/roles/reviewer got no answer"
    );
    expect(fetched).toEqual(["GET /v1/roles/reviewer"]);

    // Already stopping as the call starts: nothing is sent, and the call says so.
    fetched.splice(0);
    const first = await refusal(busy.setRole({ sessionID: SESSION, role: "reviewer" }));
    expect(first).toBeInstanceOf(EnvoyStoppingError);
    expect(fetched).toEqual([]);
  });

  test("control: with no stop reading, every call is sent", async () => {
    const { fetched, fetch } = listener();
    const client = createEnvoyClient({ baseUrl: "http://listener", fetch });

    await client.subscribe(REGISTRATION);
    await client.unsubscribe({ sessionID: SESSION, topics: ["notifications.slack.x"] });
    await client.setRole({ sessionID: SESSION, role: "reviewer", soft: true });
    await client.getRole("reviewer");
    await client.getInterest(SESSION);

    expect(fetched).toEqual([
      "POST /v1/interests/subscribe",
      "POST /v1/interests/unsubscribe",
      "POST /v1/roles/set",
      "GET /v1/roles/reviewer",
      `GET /v1/interests/${SESSION}`,
    ]);
  });
});
