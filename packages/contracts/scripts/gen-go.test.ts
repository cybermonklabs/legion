import { describe, expect, test } from "bun:test";
import { mkdirSync, mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, relative, resolve } from "node:path";
import { skipWithout } from "@legion/test-fixtures/host-tools";

const output = resolve(import.meta.dir, "../../envoy/internal/contracts/generated.go");
const envelopeOutput = resolve(import.meta.dir, "../../envoy/listenerwire/envelope_generated.go");
const dispatchToolsOutput = resolve(
  import.meta.dir,
  "../../envoy/internal/contracts/dispatch_tools_generated.go"
);
const blockSchemaOutput = resolve(
  import.meta.dir,
  "../../envoy/internal/dispatch/pmdoc/schema_generated.go"
);
const blockSchemaTypes = resolve(import.meta.dir, "../src/block-schema.generated.ts");
const dispatchFrameOutputs = [
  resolve(import.meta.dir, "../../envoy/internal/contracts/dispatch_frame_generated.go"),
  resolve(import.meta.dir, "../schemas/dispatch-frame.schema.json"),
  resolve(import.meta.dir, "../fixtures/dispatch-frame-verdicts.json"),
];
// Every file the generator writes, so a copy of the whole set is checked and only the hand-edit
// below is stale.
const generated = [
  output,
  envelopeOutput,
  dispatchToolsOutput,
  blockSchemaOutput,
  blockSchemaTypes,
  ...dispatchFrameOutputs,
];
const questionSchema = resolve(import.meta.dir, "../schemas/dispatch-question.schema.json");
const generator = resolve(import.meta.dir, "gen-go.ts");
const repoRoot = resolve(import.meta.dir, "../../..");

// Every case here runs `gen-go --check`, which renders each Go file through gofmt and refuses to
// compare without it, so a host with no gofmt is one where these cases say nothing.
const skip = skipWithout("gofmt");

describe.skipIf(skip)("gen-go", () => {
  // The generator shells out to gofmt, so Bun's 5 s default cannot cover a cold CI runner.
  // `--check` compares a fresh render with the committed file and never writes it, so a constant
  // changed in TypeScript without regenerating fails here instead of being rewritten in place.
  test("the committed Go contract is what the generator emits, without a question schema", async () => {
    const process = Bun.spawn(["bun", generator, "--check"], {
      cwd: repoRoot,
      stderr: "pipe",
      stdout: "pipe",
    });
    const stderr = await new Response(process.stderr).text();
    const exitCode = await process.exited;

    expect(stderr).toBe("");
    expect(exitCode).toBe(0);

    // The envelope's Go mirror is the listener's wire package's, where its clients read it
    // without a server package, and internal/contracts names the same types.
    const envelope = await Bun.file(envelopeOutput).text();
    expect(envelope).toContain(`package listenerwire`);
    expect(envelope).toContain(`type EnvelopeSender struct {
\tSessionID string   \`json:"session_id"\`
\tMachine   string   \`json:"machine,omitempty"\`
\tCwd       string   \`json:"cwd,omitempty"\`
\tTitle     string   \`json:"title,omitempty"\`
\tRoles     []string \`json:"roles,omitempty"\`
}`);
    expect(envelope).toContain(`func (e Envelope) Validate() error {`);
    const source = await Bun.file(output).text();
    expect(source).toContain(`type Envelope = listenerwire.Envelope`);
    expect(source).toContain(`type EnvelopeSender = listenerwire.EnvelopeSender`);
    expect(source).not.toContain(`type Envelope struct`);
    expect(await Bun.file(questionSchema).exists()).toBe(false);
  }, 120_000);

  // Each file the generator writes is checked, so a hand-edit of any of them fails CI naming the
  // file it found stale rather than passing because a sibling file still matches. The hand-edit
  // lands on a copy under GEN_GO_CHECK_ROOT, never on the tracked file this workspace shares.
  for (const stale of generated) {
    test(`a hand-edit of ${relative(repoRoot, stale)} makes --check fail, naming it`, async () => {
      const root = mkdtempSync(join(tmpdir(), "gen-go-check-"));
      const staleCopy = resolve(root, relative(repoRoot, stale));
      try {
        for (const source of generated) {
          const copy = resolve(root, relative(repoRoot, source));
          mkdirSync(dirname(copy), { recursive: true });
          await Bun.write(copy, await Bun.file(source).text());
        }
        await Bun.write(staleCopy, `${await Bun.file(stale).text()}\n// hand-edited\n`);

        const spawned = Bun.spawn(["bun", generator, "--check"], {
          cwd: repoRoot,
          env: { ...process.env, GEN_GO_CHECK_ROOT: root },
          stderr: "pipe",
          stdout: "pipe",
        });
        const stderr = await new Response(spawned.stderr).text();
        const exitCode = await spawned.exited;

        expect(exitCode).not.toBe(0);
        expect(stderr).toContain(`${staleCopy} is stale`);
      } finally {
        rmSync(root, { force: true, recursive: true });
      }
    }, 120_000);
  }
});
