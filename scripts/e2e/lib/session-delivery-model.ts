// The model envoy-session-delivery.sh's two sessions reach, the Oh My Pi one and the Claude Code
// one: lib/messages-standin.ts with the answers that proof asks of a model.
//
//   bun scripts/e2e/lib/session-delivery-model.ts <port-file> <key-file> <requests-file>
//
// The setup prompt, "Call <subscribe> with topics <JSON list>. Then call <role-set> with role
// "<role>". Then reply with the single word ok.", is answered with those two tool calls, one a
// request, in that order, by the names the prompt gives (Oh My Pi's `envoy_*` tools, Claude Code's
// `mcp__legion__envoy_*`), and then `ok`. Claude Code offers its MCP tools deferred, behind its
// ToolSearch tool, so where the request offers the named tool only through ToolSearch, the
// stand-in first loads both with `select:<name>,<name>`, as a model there does. Every other turn,
// an Envoy event's among them, is answered `ok` with no tool call, so what a session's transcript
// holds of an event is the event alone.
import { lastPrompt, runStandin, text, toolCall } from "./messages-standin";

const SETUP =
  /Call (\S+) with topics (\[[^\]]*\])\. Then call (\S+) with role "([^"]+)"\. Then reply with the single word ok\./;

runStandin((turn) => {
  const prompt = lastPrompt(turn);
  const setup = SETUP.exec(prompt?.text ?? "");
  if (setup !== null && prompt !== undefined) {
    const [, subscribe, topics, roleSet, role] = setup as unknown as [
      string,
      string,
      string,
      string,
      string,
    ];
    const step = prompt.calls.filter((name) => name !== "ToolSearch").length;
    if (step < 2) {
      const call =
        (step === 0
          ? toolCall(turn, subscribe, { topics: JSON.parse(topics) })
          : toolCall(turn, roleSet, { role })) ??
        toolCall(turn, "ToolSearch", { query: `select:${subscribe},${roleSet}` });
      if (call !== undefined) return [call];
    }
  }
  return text("ok");
});
