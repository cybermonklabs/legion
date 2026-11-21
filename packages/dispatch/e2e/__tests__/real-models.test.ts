import { expect, test } from "bun:test";
import { tmpdir } from "node:os";
import { fileURLToPath } from "node:url";

import { rowLimit, runBounded } from "./bounded-subprocess";

// `DISPATCH_E2E_MODELS=real` (e2e/harness-models.ts): run-server.sh starts the server on the chat
// model and the decision model the caller's own settings name, and with any of those settings
// missing it starts nothing and names each one. Each run here gives run-server.sh every input it
// reads before the models, a database it never opens, and a server binary that is no file, so a
// run that got past the models' check stops at that binary rather than building a server.

const runServer = fileURLToPath(new URL("../run-server.sh", import.meta.url));

const inputs = {
  DATABASE_URL: "postgres://postgres@127.0.0.1:1/never-opened",
  DISPATCH_E2E_AGENT_SECRETS_URL: "",
  DISPATCH_E2E_MODELS: "real",
  DISPATCH_E2E_PORT: "9041",
  DISPATCH_E2E_SERVER_BIN: "/nonexistent/envoy-dispatch",
  FAKE_ENVOY_PORT: "9042",
  FAKE_GITHUB_PORT: "9043",
};

function runReal(settings: Record<string, string>) {
  return runBounded({
    argv: ["bash", runServer],
    bound: "short",
    // TMPDIR: the child writes under its temporary directory, which on a machine whose TMPDIR
    // names the only writable volume is not /tmp.
    env: { PATH: process.env.PATH ?? "", TMPDIR: tmpdir(), ...inputs, ...settings },
    from: import.meta.url,
  });
}

const decisionModel = {
  DISPATCH_DECISION_MODEL: "jev",
  DISPATCH_DECISION_MODEL_ENDPOINT: "https://decide.example",
  DISPATCH_DECISION_MODEL_TOKEN_FILE: "/run/secrets/decision-token",
};

test(
  "with the real models and no model settings run-server.sh starts nothing and names each missing setting",
  async () => {
    const none = await runReal({});
    expect(none.code).toBe(2);
    expect(none.stderr).toContain(
      "these settings of theirs are missing: DISPATCH_ANSWERER (azure, with the DISPATCH_AZURE_CHAT_* settings and the Azure sign-in, or endpoint, with the DISPATCH_CHAT_* settings); DISPATCH_DECISION_MODEL; DISPATCH_DECISION_MODEL_ENDPOINT; DISPATCH_DECISION_MODEL_TOKEN_FILE"
    );

    // A place named, and some of its settings given, names the rest of that place's and of the
    // decision model's.
    const partial = await runReal({
      DISPATCH_ANSWERER: "azure",
      DISPATCH_AZURE_CHAT_ENDPOINT: "https://chat.example",
      DISPATCH_DECISION_MODEL: "jev",
    });
    expect(partial.code).toBe(2);
    expect(partial.stderr).toContain(
      "these settings of theirs are missing: DISPATCH_AZURE_CHAT_DEPLOYMENT; DISPATCH_AZURE_CHAT_TOKENS_PER_MINUTE; DISPATCH_AZURE_TENANT_ID; DISPATCH_AZURE_CLIENT_ID; DISPATCH_AZURE_FEDERATED_TOKEN_FILE; DISPATCH_DECISION_MODEL_ENDPOINT; DISPATCH_DECISION_MODEL_TOKEN_FILE"
    );

    // Every setting given, the check passes and the run goes on to the server binary.
    const whole = await runReal({
      ...decisionModel,
      DISPATCH_ANSWERER: "endpoint",
      DISPATCH_CHAT_ENDPOINT: "https://chat.example",
      DISPATCH_CHAT_MODEL: "a-chat-model",
      DISPATCH_CHAT_TOKEN_FILE: "/run/secrets/chat-token",
    });
    expect(whole.stderr).not.toContain("missing");
    expect(whole.stderr).toContain(
      "DISPATCH_E2E_SERVER_BIN=/nonexistent/envoy-dispatch is not an executable file"
    );
  },
  rowLimit("short", "short", "short")
);
