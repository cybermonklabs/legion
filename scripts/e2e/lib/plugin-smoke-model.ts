// The model the sessions of the Oh My Pi plugin's live scripts reach: lib/messages-standin.ts with
// the tool calls and the answers those scripts ask of a model, each recognised by the words of the
// script's own prompt, so no session reaches a real one.
//
//   bun scripts/e2e/lib/plugin-smoke-model.ts <port-file> <key-file> <requests-file>
//
// - packages/pi-envoy/scripts/smoke-delivery.sh: `Call the envoy_role_set tool with role="<role>".`
//   is answered with that call, and the Envoy card that later brings the session a message naming
//   `smoke-delivery-payload-<hex>` with a bash call that writes that text alone to received.txt.
// - packages/pi-envoy/scripts/smoke-btw.sh: `… before anything else: touch <file> && sleep <n>.` is
//   answered with that bash command; a BTW's side turn about `smoke-btw-payload-<hex>` with one line
//   that names it, which the plugin posts as the reply; and a steered message carrying that word
//   with a bash call of the `dispatch message` its reply_with names, its body naming it too.
// - packages/dispatch/e2e/acceptance/omp-roundtrip.sh: `Run this command in your shell, then wait:
//   dispatch ask …` is answered with a bash call of that command, and `Call envoy_role_set with role
//   <role>, then wait.` with that call.
// - packages/pi-envoy/scripts/smoke-registration-stall.sh asks for the single word ok, which is
//   what every turn no rule above matches is answered: an Envoy card, an answered ask, a title.
//
// A request that brings a tool's result back ends the turn, unless the session was sent a message
// beside that result. The stop-time self-check is answered PROCEEDING, and AskGate's two reviews of
// a Dispatch write with the verdicts that let it through.
import { isSelfCheck } from "../../../packages/pi-shared/test/omp-harness.ts";
import {
  afterToolResult,
  lastPrompt,
  quoted,
  runStandin,
  type Turn,
  text,
  toolCall,
} from "./messages-standin";

const DELIVERY_ROLE = /Call the envoy_role_set tool with role="([^"]+)"\./;
const DELIVERY_PAYLOAD = /smoke-delivery-payload-[0-9a-f]+/;
const BTW_SLEEP = /before anything else: (touch \S+ && sleep \d+)\./;
const BTW_PAYLOAD = /smoke-(?:btw|steer)-payload-[0-9a-f]+/;
// The reply_with of a steered message's card, `command: dispatch message --issue <key> --body ...
// --in-reply-to <message id>`, the command the session answers it with; its flags are read by name.
const STEER_REPLY = /dispatch message [^\n]*--in-reply-to [^\n]*/;
const ROUNDTRIP_ASK = /Run this command in your shell, then wait: (dispatch ask .+)$/m;
const ROUNDTRIP_ROLE = /Call envoy_role_set with role (\S+), then wait\./;

/** The text of the user messages since the assistant's last one, whatever tool results they carry
 *  beside it: Oh My Pi sends an Envoy card and the line that bids the session read it as messages
 *  of their own, and a message steered into a turn follows the tool result it waited for. */
function newest(turn: Turn): string {
  let start = turn.messages.length;
  while (start > 0 && turn.messages[start - 1]?.role === "user") start--;
  return turn.messages
    .slice(start)
    .map((message) => message.text)
    .join("\n\n");
}

runStandin((turn) => {
  // AskGate's two reviews of a Dispatch write, each under a system prompt of its own
  // (packages/pi-envoy/extensions/askgate): no rule matches, and the write is allowed.
  if (turn.system.includes("You are AskGate's rule check."))
    return text('{"matches": [], "reason": ""}');
  if (turn.system.includes("You are AskGate.")) return text('{"decision":"allow"}');
  if (isSelfCheck(turn.request)) {
    const side = JSON.stringify(turn.request.body.messages ?? null);
    if (side.includes("WAITING or PROCEEDING")) return text("PROCEEDING");
    const asked = BTW_PAYLOAD.exec(side)?.[0];
    return text(asked === undefined ? "ok" : `The side answer to ${asked}.`);
  }
  const latest = newest(turn);
  const payload = DELIVERY_PAYLOAD.exec(latest)?.[0];
  if (payload !== undefined) {
    const call = toolCall(turn, "bash", {
      command: `printf '%s' ${quoted(payload)} > received.txt`,
    });
    if (call !== undefined) return [call];
  }
  const steer = STEER_REPLY.exec(latest)?.[0];
  const steered = BTW_PAYLOAD.exec(latest)?.[0];
  const inReplyTo = /--in-reply-to (\S+)/.exec(steer ?? "")?.[1];
  if (inReplyTo !== undefined && steered !== undefined) {
    const issue = /--issue (\S+)/.exec(steer ?? "")?.[1];
    const call = toolCall(turn, "bash", {
      command: [
        "dispatch message",
        ...(issue === undefined ? [] : [`--issue ${issue}`]),
        `--in-reply-to ${inReplyTo}`,
        `--body ${quoted(`The steered answer to ${steered}.`)}`,
      ].join(" "),
    });
    if (call !== undefined) return [call];
  }
  if (afterToolResult(turn)) return text("Done.");
  const prompt = lastPrompt(turn);
  if (prompt !== undefined && prompt.calls.length === 0) {
    const role = DELIVERY_ROLE.exec(prompt.text)?.[1] ?? ROUNDTRIP_ROLE.exec(prompt.text)?.[1];
    const command = BTW_SLEEP.exec(prompt.text)?.[1] ?? ROUNDTRIP_ASK.exec(prompt.text)?.[1];
    const call =
      role !== undefined
        ? toolCall(turn, "envoy_role_set", { role })
        : command !== undefined
          ? toolCall(turn, "bash", { command })
          : undefined;
    if (call !== undefined) return [call];
  }
  return text("ok");
});
