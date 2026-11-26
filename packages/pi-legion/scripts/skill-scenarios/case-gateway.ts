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
      content: z.union([
        z.string(),
        z.array(z.looseObject({ type: z.string().optional(), text: z.string().optional() })),
      ]),
    })
  ),
});

/** The Google metadata server's identity token for `audience`, as a keyless gateway route reads
 * the pod's own identity: minted on demand and kept in this process alone until ten minutes
 * before it expires. No file and no environment variable holds it. */
export function metadataIdentity(
  audience: string,
  read: typeof fetch = fetch
): () => Promise<string> {
  let held: { token: string; expires: number } | undefined;
  return async () => {
    if (held !== undefined && held.expires - Date.now() > 600_000) return held.token;
    const url = new URL(
      "http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/identity"
    );
    url.searchParams.set("audience", audience);
    url.searchParams.set("format", "full");
    const response = await read(url, { headers: { "Metadata-Flavor": "Google" } });
    if (!response.ok) throw new Error(`the metadata server answered ${response.status}`);
    const token = (await response.text()).trim();
    const claims = z
      .object({ aud: z.string(), exp: z.number() })
      .safeParse(
        JSON.parse(Buffer.from(token.split(".")[1] ?? "", "base64url").toString() || "null")
      );
    if (!claims.success || claims.data.aud !== audience)
      throw new Error(`the metadata server's token is no identity token for ${audience}`);
    held = { token, expires: claims.data.exp * 1000 };
    return token;
  };
}

/** Use the copied profile's gateway credentials, not the caller's. Only keep request metadata
 * needed to identify what was served: no prompt, headers with credentials, or response text.
 * With `identity`, the upstream is a keyless route: each request goes with that bearer in place
 * of the key the profile's provider names, which then is a placeholder that reaches no one. */
export function caseGateway(dir: string, agentDir: string, identity?: () => Promise<string>) {
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
      if (identity !== undefined) {
        headers.delete("x-api-key");
        headers.set("authorization", `Bearer ${await identity()}`);
      }
      const body = parsed.data;
      // The skill's own message: a user message's text that opens a line with the marker. Oh My
      // Pi puts its dated system reminder ahead of it in the first user message, so the marker
      // need not open the message; a tool result is a block of another type.
      const opensLine = (text: string | undefined) =>
        text !== undefined &&
        (text.startsWith(DISPATCH_FIRST_MARKER) || text.includes(`\n${DISPATCH_FIRST_MARKER}`));
      const dispatchFirst = body.messages.some(
        ({ role, content }) =>
          role === "user" &&
          (typeof content === "string"
            ? opensLine(content)
            : content.some(({ type, text }) => type === "text" && opensLine(text)))
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
          path: url.pathname,
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
  // SKILL_SCENARIOS_GATEWAY_AUDIENCE names a keyless route: the audience of the identity token
  // the pod's metadata server mints for it (a builds Job's model-gateway route, for one).
  const audience = process.env.SKILL_SCENARIOS_GATEWAY_AUDIENCE;
  const server = caseGateway(
    dir,
    agentDir,
    audience === undefined || audience === "" ? undefined : metadataIdentity(audience)
  );
  console.log(`listening on ${server.url.origin}`);
}
