// The model dispatch-session-settings.sh's sessions reach: lib/messages-standin.ts serving both of
// the run's providers, the Anthropic-shaped one and the OpenAI-shaped one, with the answers the
// driver's prompts ask for.
//
//   bun scripts/e2e/lib/session-settings-model.ts <port-file> <key-file> <requests-file>
//
// - "Reply with the one word: <word>" is answered <word>, on whichever model the session runs: the
//   driver reads which model a turn ran on from the session file, never from the answer.
// - "Use your bash tool to run exactly this command in the foreground: <command> . …" is answered
//   with a call of the session's bash tool running <command>, and the request that brings its result
//   back is answered `done`: the turn picker-during-turn switches models in the middle of.
// - Anything else (an Envoy card, a title) is answered `ok`.
import { afterToolResult, lastPrompt, runStandin, text, toolCall } from "./messages-standin";

runStandin((turn) => {
  if (afterToolResult(turn)) return text("done");
  const prompt = lastPrompt(turn)?.text ?? "";
  const command =
    /Use your bash tool to run exactly this command in the foreground: (.+?) \. When it has finished/.exec(
      prompt
    )?.[1];
  const call = command === undefined ? undefined : toolCall(turn, "bash", { command });
  if (call !== undefined) return [call];
  return text(/Reply with the one word: (\S+)/.exec(prompt)?.[1] ?? "ok");
});
