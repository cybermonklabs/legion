import { mkdirSync, writeFileSync } from "node:fs";
import { dirname } from "node:path";
const marker = process.argv[2];
if (marker) {
  mkdirSync(dirname(marker), { recursive: true });
  writeFileSync(marker, "");
}
const answer = (id: unknown, result: unknown) => process.stdout.write(`${JSON.stringify({ jsonrpc: "2.0", id, result })}\n`);
const refuse = (id: unknown, code: number, message: string) => process.stdout.write(`${JSON.stringify({ jsonrpc: "2.0", id, error: { code, message } })}\n`);
let pending = "";
process.stdin.setEncoding("utf8");
process.stdin.on("data", (chunk: string) => {
  pending += chunk;
  let at = pending.indexOf("\n");
  while (at >= 0) {
    const line = pending.slice(0, at).trim();
    pending = pending.slice(at + 1);
    at = pending.indexOf("\n");
    if (line === "") continue;
    let message: { id?: unknown; method?: string; params?: { protocolVersion?: string; arguments?: { text?: unknown } } };
    try { message = JSON.parse(line); } catch { continue; }
    // A notification (notifications/initialized, notifications/cancelled) carries no id and gets no answer.
    if (message.id === undefined || message.id === null) continue;
    switch (message.method) {
      case "initialize":
        answer(message.id, { protocolVersion: message.params?.protocolVersion ?? "2025-06-18", capabilities: { tools: {} }, serverInfo: { name: "legion-stage4b-fixture", version: "0.0.1" } });
        break;
      case "ping":
        answer(message.id, {});
        break;
      case "tools/list":
        answer(message.id, { tools: [{ name: "fixture_echo", description: "Echoes text (Stage 4b fixture).", inputSchema: { type: "object", properties: { text: { type: "string" } } } }] });
        break;
      case "tools/call":
        answer(message.id, { content: [{ type: "text", text: String(message.params?.arguments?.text ?? "") }] });
        break;
      default:
        refuse(message.id, -32601, `method not found: ${String(message.method)}`);
    }
  }
});
process.stdin.on("end", () => process.exit(0));
