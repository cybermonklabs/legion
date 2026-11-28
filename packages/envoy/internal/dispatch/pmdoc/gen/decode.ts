// decode.ts <base64 Yjs update> <out> writes the browser editor's ProseMirror JSON to <out>.
// decode.ts --jsonl <in> <out> reads {id, update} lines and writes {id, pm} or {id, error} lines
// in the same order. Both forms write the result to a file, never to stdout.
import { readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { createHeadlessProof } from "@legion/proof-editor/headless";
import { yXmlFragmentToProsemirrorJSON } from "y-prosemirror";
import * as Y from "yjs";

const args = process.argv.slice(2);
const batch = args[0] === "--jsonl";
const [input, out] = batch ? args.slice(1) : args;
if (!input || !out || args.length !== (batch ? 3 : 2)) {
  console.error("usage: bun decode.ts <base64 Yjs update> <out> | --jsonl <in> <out>");
  process.exit(2);
}
const blockSchema = JSON.parse(
  readFileSync(join(import.meta.dir, "..", "schema", "blocks.json"), "utf8")
);
const { schema } = await createHeadlessProof({ blockSchema });

/** The schema supplies defaults for attributes the live document lacks. */
function decode(update: Uint8Array) {
  const ydoc = new Y.Doc();
  try {
    Y.applyUpdate(ydoc, update);
    const root = yXmlFragmentToProsemirrorJSON(ydoc.getXmlFragment("prosemirror"));
    return schema.nodeFromJSON(root).toJSON();
  } finally {
    ydoc.destroy();
  }
}

/** A failed update still occupies its line in the output. */
function decodeLine(line: string) {
  let id: number | null = null;
  try {
    const record: { id?: number; update?: unknown } | null = JSON.parse(line);
    if (typeof record?.id !== "number" || !Number.isSafeInteger(record.id)) {
      throw new Error("expected an integer id");
    }
    id = record.id;
    if (typeof record.update !== "string") {
      throw new Error("expected a base64 update");
    }
    const update = Buffer.from(record.update, "base64");
    // Buffer ignores invalid characters. Require the encoding the Go producer writes.
    if (update.toString("base64") !== record.update) {
      throw new Error("invalid base64 update");
    }
    return { id, pm: decode(update) };
  } catch (error) {
    return { id, error: error instanceof Error ? error.message : String(error) };
  }
}

if (batch) {
  const text = readFileSync(input, "utf8");
  const lines = text === "" ? [] : text.replace(/\n$/, "").split("\n");
  writeFileSync(out, lines.map((line) => `${JSON.stringify(decodeLine(line))}\n`).join(""));
} else {
  writeFileSync(out, JSON.stringify(decode(Buffer.from(input, "base64"))));
}
