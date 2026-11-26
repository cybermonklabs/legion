#!/usr/bin/env bun
import { randomUUID } from "node:crypto";
import { appendFileSync, readFileSync, writeFileSync } from "node:fs";
import path from "node:path";
import { DISPATCH_FIRST_MARKER } from "@legion/envoy-client/dispatch-first";
import { z } from "zod";

const MODEL = "claude-opus-5-5";
const THINKING = "xhigh";
const Config = z.looseObject({
  providers: z.record(
    z.string(),
    z.looseObject({
      api: z.string().optional(),
      baseUrl: z.url(),
      models: z.array(z.looseObject({ id: z.string() })).optional(),
    })
  ),
});
const RequestBody = z.looseObject({
  model: z.string(),
  metadata: z.record(z.string(), z.unknown()).optional(),
  messages: z.array(
    z.looseObject({
      role: z.string(),
      content: z.union([z.string(), z.array(z.looseObject({ text: z.string().optional() }))]),
    })
  ),
});

/** Use the copied profile's gateway credentials, not the caller's. Only keep request metadata
 * needed to identify what was served: no prompt, headers with credentials, or response text. */
export function caseGateway(dir: string, agentDir: string) {
  const models = path.join(agentDir, "models.yml");
  const settings = path.join(agentDir, "config.yml");
  const config = Config.parse(Bun.YAML.parse(readFileSync(models, "utf8")));
  const candidates = Object.entries(config.providers);
  const [name, provider] = candidates[0] ?? [];
  if (
    !name ||
    !provider ||
    candidates.length !== 1 ||
    (provider.api ?? "anthropic-messages") !== "anthropic-messages"
  ) {
    throw new Error("case profiles require one Anthropic Messages gateway provider");
  }
  const upstream = new URL(provider.baseUrl);
  if (
    !["https:", "http:"].includes(upstream.protocol) ||
    upstream.username ||
    upstream.password ||
    upstream.search ||
    upstream.hash
  )
    throw new Error("case gateway URL must be HTTP(S), without credentials, query or fragment");
  const userId = randomUUID();
  const sessionId = randomUUID();
  const log = path.join(dir, "deployments.jsonl");
  let seq = 0;
  const server = Bun.serve({
    hostname: "127.0.0.1",
    port: 0,
    idleTimeout: 0,
    async fetch(request) {
      const url = new URL(request.url);
      if (request.method !== "POST" || !/^\/v1\/messages(?:\/count_tokens)?$/.test(url.pathname))
        return new Response(null, { status: 404 });
      const parsed = RequestBody.safeParse(await request.json());
      if (!parsed.success || parsed.data.model !== MODEL)
        return new Response("Expected the pinned case model", { status: 400 });
      const headers = new Headers(request.headers);
      for (const key of ["host", "connection", "content-length", "transfer-encoding"])
        headers.delete(key);
      headers.set("x-litellm-session-id", sessionId);
      const body = parsed.data;
      const dispatchFirst = body.messages.some(
        ({ role, content }) =>
          role === "user" &&
          (typeof content === "string" ? content : content[0]?.text)?.startsWith(
            DISPATCH_FIRST_MARKER
          )
      );
      const at = new Date().toISOString();
      const requestSeq = ++seq;
      const response = await fetch(
        `${upstream.href.replace(/\/$/, "")}${url.pathname}${url.search}`,
        {
          method: "POST",
          headers,
          body: JSON.stringify({ ...body, metadata: { ...body.metadata, user_id: userId } }),
          redirect: "manual",
          signal: request.signal,
        }
      );
      appendFileSync(
        log,
        `${JSON.stringify({
          seq: requestSeq,
          at,
          status: response.status,
          dispatchFirst,
          deployment:
            response.headers.get("x-litellm-model-id") ??
            response.headers.get("x-litellm-deployment-id"),
        })}\n`
      );
      const responseHeaders = new Headers(response.headers);
      for (const key of ["connection", "transfer-encoding", "content-length", "content-encoding"])
        responseHeaders.delete(key);
      return new Response(response.body, { status: response.status, headers: responseHeaders });
    },
  });
  try {
    const model = `${name}/${MODEL}`;
    const { modelOverrides: _overrides, discovery: _discovery, ...connection } = provider;
    const original = provider.models?.find(({ id }) => id === MODEL);
    const changed = {
      ...connection,
      baseUrl: server.url.origin,
      models: [{ ...original, id: MODEL }],
    };
    const originalSettings = z
      .record(z.string(), z.unknown())
      .parse(Bun.YAML.parse(readFileSync(settings, "utf8")));
    const roles =
      z.record(z.string(), z.unknown()).safeParse(originalSettings.modelRoles).data ?? {};
    writeFileSync(models, `${JSON.stringify({ providers: { [name]: changed } }, null, 2)}\n`, {
      mode: 0o600,
    });
    writeFileSync(
      settings,
      `${JSON.stringify(
        {
          ...originalSettings,
          enabledModels: [model],
          defaultThinkingLevel: THINKING,
          modelRoles: Object.fromEntries(
            [
              ...new Set([
                ...Object.keys(roles),
                "default",
                "task",
                "advisor",
                "slow",
                "smol",
                "plan",
                "review",
                "oracle",
                "deep",
              ]),
            ].map((role) => [role, model])
          ),
        },
        null,
        2
      )}\n`
    );
    writeFileSync(
      path.join(dir, "gateway-run.json"),
      `${JSON.stringify({ userId, sessionId, model, thinking: THINKING })}\n`
    );
    return server;
  } catch (error) {
    server.stop(true);
    throw error;
  }
}

if (import.meta.main) {
  const [dir, agentDir] = Bun.argv.slice(2);
  if (!dir || !agentDir)
    throw new Error("usage: case-gateway.ts <run dir> <copied profile's agent dir>");
  const server = caseGateway(dir, agentDir);
  console.log(`listening on ${server.url.origin}`);
}
