import { fakeModelServing, modelsDirVariable } from "./harness-models";
import { harnessPorts, stackValue } from "./harness-ports";

// The decision model the command bar asks before it answers, as a deployment's
// DISPATCH_DECISION_MODEL_ENDPOINT reaches one: `POST {endpoint}/v1/systemone`, System One's own
// body (`packages/envoy/agentloop/decide.go`), checking the token the file
// DISPATCH_DECISION_MODEL_TOKEN_FILE names. The server reaches it under the models lane
// (`e2e/harness-models.ts`), over https with the certificate the harness made for the stack, so
// the call is the one a deployment makes.
//
// The server asks three choice questions (`packages/envoy/going/decide.go`): `action`, among the
// catalogue actions the caller may run and `none`; `context`, among `page`, `named` and `none`;
// and `display`, between `chart` and `prose`. A row scripts the answers (`PUT /__fixture/script`):
// a list of replies taken one per call in order, then the default reply, and with neither each
// question answered at confidence 0.99 with `none`, `none` and `prose`. A reply names, per
// question, the choice, its confidence and optionally the probabilities of every choice; with
// none given, the choice gets the confidence and the other choices the call offered share the
// rest evenly, so a row that reads a runner-up names the probabilities itself. A
// choice the call did not offer is answered 422 naming the offered ones, since a model can only
// choose among those, and a row that scripts another reads that the server never offered it.
// `status` answers that status instead and `delayMs` waits first, past the server's one-second
// bound where a row means to read a decision that never came.
//
// It counts the calls it answered and the ones refused for their bearer, and keeps the last
// bodies it received (`GET /__fixture/calls`).

/** One question's scripted answer. */
export interface DecisionAnswer {
  readonly choice: string;
  readonly confidence: number;
  readonly probabilities?: Readonly<Record<string, number>>;
}

/** What the model answers one call with, by question name. */
export interface DecisionReply {
  readonly action?: DecisionAnswer;
  readonly context?: DecisionAnswer;
  readonly display?: DecisionAnswer;
  readonly status?: number;
  readonly delayMs?: number;
}

/** What `PUT /__fixture/script` takes: the replies, one per call in order, and the reply every
 *  call after them gets. */
export interface DecisionScript {
  readonly replies?: readonly DecisionReply[];
  readonly default?: DecisionReply;
}

/** A call's body as the server sent it (`systemOneRequest`). */
export interface DecisionRequestBody {
  readonly model: string;
  readonly state: unknown;
  readonly questions: Readonly<
    Record<
      string,
      {
        readonly type: string;
        readonly instructions?: string;
        readonly criteria?: unknown;
      }
    >
  >;
}

/** What `GET /__fixture/calls` answers, since the last reset. */
export interface FakeDecisionCalls {
  /** Every call to the decision route that presented the bearer, refused ones included. */
  readonly calls: number;
  /** Calls refused 401 for their bearer. */
  readonly unauthorized: number;
  /** The last bodies received, oldest first. */
  readonly requests: readonly DecisionRequestBody[];
}

/** How many bodies `requests` keeps. */
const keptRequests = 20;

/** The answer to a question no reply scripts. */
const unscripted: Readonly<Record<string, string>> = {
  action: "none",
  context: "none",
  display: "prose",
};
const unscriptedConfidence = 0.99;

interface FakeDecisionState {
  calls: number;
  unauthorized: number;
  requests: DecisionRequestBody[];
  queue: DecisionReply[];
  fallback: DecisionReply | undefined;
}

function emptyState(): FakeDecisionState {
  return { calls: 0, fallback: undefined, queue: [], requests: [], unauthorized: 0 };
}

let state = emptyState();

const serving = fakeModelServing(
  stackValue(process.env, modelsDirVariable),
  "decisionToken",
  "the fake decision model"
);

/** The choices a choice question offered: its criteria's names, in the order the call sent them. */
function offered(criteria: unknown): string[] {
  return criteria !== null && typeof criteria === "object" && !Array.isArray(criteria)
    ? Object.keys(criteria)
    : [];
}

/** The probabilities of an answer: the scripted ones, or the choice's confidence and the rest
 *  shared evenly by the other offered choices. */
function probabilities(answer: DecisionAnswer, choices: readonly string[]): Record<string, number> {
  if (answer.probabilities !== undefined) return { ...answer.probabilities };
  const others = choices.filter((choice) => choice !== answer.choice);
  const share = others.length === 0 ? 0 : (1 - answer.confidence) / others.length;
  return Object.fromEntries([
    [answer.choice, answer.confidence],
    ...others.map((choice) => [choice, share]),
  ]);
}

/** The answers to the call's questions, or the first scripted choice the call did not offer. */
function answers(
  body: DecisionRequestBody,
  reply: DecisionReply | undefined
): { answers: Record<string, unknown> } | { refused: string } {
  const out: Record<string, unknown> = {};
  const scripted: Readonly<Record<string, DecisionAnswer | undefined>> = {
    action: reply?.action,
    context: reply?.context,
    display: reply?.display,
  };
  for (const [name, question] of Object.entries(body.questions ?? {})) {
    const choices = offered(question.criteria);
    const answer: DecisionAnswer = scripted[name] ?? {
      choice: unscripted[name] ?? choices[0] ?? "",
      confidence: unscriptedConfidence,
    };
    if (!choices.includes(answer.choice)) {
      return {
        refused:
          `the ${name} question offered ${choices.join(", ") || "no choice"}, ` +
          `not ${JSON.stringify(answer.choice)}`,
      };
    }
    out[name] = {
      choice: answer.choice,
      confidence: answer.confidence,
      probabilities: probabilities(answer, choices),
      type: "choice",
    };
  }
  return { answers: out };
}

async function decide(request: Request): Promise<Response> {
  if (request.headers.get("authorization") !== serving.authorization) {
    state.unauthorized++;
    return Response.json(
      { error: "a call whose bearer is not the one DISPATCH_DECISION_MODEL_TOKEN_FILE holds" },
      { status: 401 }
    );
  }
  const body = (await request.json()) as DecisionRequestBody;
  state.calls++;
  state.requests = [...state.requests, body].slice(-keptRequests);
  const reply = state.queue.shift() ?? state.fallback;
  if (reply?.delayMs !== undefined && reply.delayMs > 0) await Bun.sleep(reply.delayMs);
  if (reply?.status !== undefined) {
    return Response.json(
      { error: "the decision service refused this call" },
      { status: reply.status }
    );
  }
  const answered = answers(body, reply);
  if ("refused" in answered) {
    return Response.json({ error: answered.refused }, { status: 422 });
  }
  const input = JSON.stringify(body).length;
  return Response.json({
    answers: answered.answers,
    model: body.model,
    usage: { input_tokens: Math.ceil(input / 4), output_tokens: 12 },
  });
}

// FAKE_DECISION_MODEL_PORT, or with it unset port 0, so the OS chooses; the line at the end
// reports the port bound, which e2e/harness-start.ts reads.
const server = Bun.serve({
  hostname: "127.0.0.1",
  port: harnessPorts().fakeDecisionModel.port,
  tls: serving.tls,
  async fetch(request) {
    const url = new URL(request.url);
    if (request.method === "POST" && url.pathname.endsWith("/v1/systemone")) {
      return decide(request);
    }
    if (request.method === "GET" && url.pathname === "/__fixture/calls") {
      const calls: FakeDecisionCalls = {
        calls: state.calls,
        requests: state.requests,
        unauthorized: state.unauthorized,
      };
      return Response.json(calls);
    }
    if (request.method === "PUT" && url.pathname === "/__fixture/script") {
      const script = (await request.json()) as DecisionScript;
      state = { ...state, fallback: script.default, queue: [...(script.replies ?? [])] };
      return Response.json({ ok: true });
    }
    if (request.method === "PUT" && url.pathname === "/__fixture/reset") {
      state = emptyState();
      return Response.json({ ok: true });
    }
    if (url.pathname === "/healthz") return Response.json({ ok: true });
    return Response.json({ message: "not found" }, { status: 404 });
  },
});

console.log(`fake decision model listener on 127.0.0.1:${server.port}`);
