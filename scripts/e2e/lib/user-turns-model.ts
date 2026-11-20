// The model dispatch-user-turns.sh's session reaches: lib/messages-standin.ts as its Anthropic
// provider, with the answers and the tool calls that proof's checks ask of a model.
//
//   bun scripts/e2e/lib/user-turns-model.ts <port-file> <key-file> <requests-file>
//
// - The spec ask ("With `dispatch issue` in project <P>, create an issue titled "<T>" whose spec's
//   first section says `<S>` and whose second section repeats that time.") is answered with a call
//   of the session's bash tool that runs `dispatch issue` for that issue, with that spec.
// - A person's edit of that spec (an `artifact.human_edited` card) is answered with a bash call of
//   `dispatch doc-edit` that moves the time the edit left in the other section to the time the
//   person wrote.
// - A request that brings a tool's result back ends the turn.
// - A message that names code words (ALPHA<run>, SKILL<run>, TEMPLATE<run>…) is answered with them,
//   so a check that the session answered a Send, an Aside, a skill or a template finds its word.
// - The BTW side question is answered `4`, a compaction's summary request with a summary, and
//   AskGate's two reviews of a Dispatch write with their verdicts that let it through.
// - Anything else (an Envoy card, a title) is answered `ok`.
import { isSelfCheck, isSummarization } from "../../../packages/pi-shared/test/omp-harness.ts";
import {
  afterToolResult,
  lastPrompt,
  quoted,
  runStandin,
  text,
  toolCall,
} from "./messages-standin";

const SPEC_ASK =
  /With `dispatch issue` in project (\S+), create an issue titled "([^"]+)" whose spec's first section says `([^`]+)` and whose second section repeats that time\./;
const TIME = /\b\d{2}:\d{2}\b/;
/** The list of what people wrote to the session in the last 72 hours, which pi-envoy sends beside
 *  a person's message as a user message of its own (SESSION_ANSWERS_MARKER in
 *  packages/envoy-client/src/session-answers.ts, each message word for word, then
 *  SESSION_ANSWERS_REPLACES in packages/pi-envoy/src/session-answers-context.ts). It quotes every
 *  earlier message, the spec ask among them, so the rules below read the prompt without it: read
 *  with it, every later turn would be answered as the spec ask again. */
const ANSWERS_LIST =
  /\[Dispatch: what people wrote to this session in the last 72 hours[\s\S]*?read only the newest one as current\./g;

runStandin((turn) => {
  // AskGate's two reviews of a Dispatch write, each under a system prompt of its own
  // (packages/pi-envoy/extensions/askgate): no rule matches, and the write is allowed.
  if (turn.system.includes("You are AskGate's rule check."))
    return text('{"matches": [], "reason": ""}');
  if (turn.system.includes("You are AskGate.")) return text('{"decision":"allow"}');
  if (isSummarization(turn.request)) {
    return text("The person and the session exchanged code words and slash commands.");
  }
  if (isSelfCheck(turn.request)) {
    return text(
      JSON.stringify(turn.request.body.messages ?? null).includes("two plus two") ? "4" : "ok"
    );
  }
  if (afterToolResult(turn)) return text("Done.");
  const prompt = lastPrompt(turn);
  const words = (prompt?.text ?? "").replace(ANSWERS_LIST, "");
  const ask = SPEC_ASK.exec(words);
  if (ask !== null && prompt?.calls.length === 0) {
    const [, project, title, first] = ask as unknown as [string, string, string, string];
    const spec = [
      "## Schedule",
      "",
      first,
      "",
      "## Retries",
      "",
      `A retry of a failed export runs at ${TIME.exec(first)?.[0] ?? ""} UTC too.`,
    ].join("\n");
    const call = toolCall(turn, "bash", {
      command: `dispatch issue --force --project ${quoted(project)} --title ${quoted(title)} --spec ${quoted(spec)}`,
    });
    if (call !== undefined) return [call];
  }
  if (words.includes("artifact.human_edited") && prompt?.calls.length === 0) {
    const key =
      /owner: ([A-Z][A-Z0-9]*-[0-9]+)/.exec(words)?.[1] ??
      /\b([A-Z][A-Z0-9]*-[0-9]+)\b/.exec(words)?.[1];
    const before = /^-(?!--).*?(\d{2}:\d{2})/m.exec(words)?.[1];
    const after = /^\+(?!\+\+).*?(\d{2}:\d{2})/m.exec(words)?.[1];
    const call =
      key === undefined || before === undefined || after === undefined
        ? undefined
        : toolCall(turn, "bash", {
            command: `dispatch doc-edit --issue ${quoted(key)} --ops-json ${quoted(
              JSON.stringify([{ op: "replace", find: before, with: after }])
            )}`,
          });
    if (call !== undefined) return [call];
  }
  const codes = [...new Set(words.match(/\b[A-Z]{4,}[0-9a-f]{6}\b/g) ?? [])];
  return text(codes.length > 0 ? `The code word is ${codes.join(", ")}.` : "ok");
});
