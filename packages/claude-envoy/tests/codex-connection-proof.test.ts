// The Codex plugin's MCP server presents the session's broker token on its NATS CONNECT, with a
// proof of the key that token is bound to over the server's own nonce: the connection proof the
// NATS auth service checks (`brokertoken.VerifyConnectionProof`). The server runs in a process of
// its own (tests/codex-server-process.ts) and dials a stand-in hub that sends a nonce and keeps
// the CONNECT it is sent, after trading at a stand-in broker that keeps the DPoP key each trade
// proved. Both ways a Codex session is told to use its broker token run: `NATS_AUTH=broker` in
// the environment, and `natsAuth: broker` in its envoy.json.
import { afterEach, describe, expect, test } from "bun:test"
import { createHash, createPublicKey, type JsonWebKeyInput, randomBytes, verify } from "node:crypto"
import { writeFileSync } from "node:fs"
import { join } from "node:path"
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret"
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories"
import type { TCPSocketListener } from "bun"
import {
  type CodexServer,
  codexHomes,
  recordFor,
  startCodexServer,
  until,
} from "./codex-server-process"

const makeDirectory = temporaryDirectories("codex-connection-proof")
const stops: (() => Promise<void> | void)[] = []
afterEach(async () => {
  for (const stop of stops.splice(0).reverse()) await stop()
})

/** One part of a compact JWS as JSON. */
function part(compact: string, at: number): Record<string, unknown> {
  return JSON.parse(Buffer.from(compact.split(".")[at] ?? "", "base64url").toString("utf8"))
}

/** A JWK's RFC 7638 thumbprint, for the EC keys DPoP proofs carry. */
function thumbprint(jwk: Record<string, unknown>): string {
  const members = JSON.stringify({ crv: jwk["crv"], kty: jwk["kty"], x: jwk["x"], y: jwk["y"] })
  return createHash("sha256").update(members).digest("base64url")
}

/** A broker whose token exchange mints a random DPoP-bound token, keeping the key each trade's
 *  DPoP proof named, as the broker binds the token it mints to that key, and the session the trade
 *  named (its `session` form parameter), which the token it mints is for. */
function broker() {
  const minted: { token: string; jkt: string; session: string | null }[] = []
  const server = Bun.serve({
    hostname: "127.0.0.1",
    port: 0,
    fetch: async (request) => {
      const proof = request.headers.get("dpop") ?? ""
      const jwk = part(proof, 0)["jwk"] as Record<string, unknown> | undefined
      const session = new URLSearchParams(await request.text()).get("session")
      const token = fixtureSecret("access")
      minted.push({ token, jkt: jwk === undefined ? "" : thumbprint(jwk), session })
      return Response.json({
        access_token: token,
        issued_token_type: "urn:ietf:params:oauth:token-type:access_token",
        token_type: "DPoP",
        expires_in: 900,
      })
    },
  })
  stops.push(() => server.stop(true))
  return { minted, url: `http://127.0.0.1:${server.port}` }
}

/** A hub that runs an auth service as far as a client can tell: its INFO names a nonce of this
 *  connection's own. It keeps each CONNECT it is sent, with the nonce it sent that connection. */
function hub() {
  const connects: { nonce: string; connect: Record<string, unknown> }[] = []
  const listener: TCPSocketListener<{ nonce: string; buffered: string }> = Bun.listen({
    hostname: "127.0.0.1",
    port: 0,
    socket: {
      open(socket) {
        const nonce = randomBytes(16).toString("base64url")
        socket.data = { nonce, buffered: "" }
        const info = {
          server_id: "NCSHUB",
          server_name: "ncs-hub",
          version: "2.12.0",
          proto: 1,
          headers: true,
          max_payload: 1_048_576,
          auth_required: true,
          nonce,
        }
        socket.write(`INFO ${JSON.stringify(info)}\r\n`)
      },
      data(socket, bytes) {
        socket.data.buffered += bytes.toString()
        const match = /^CONNECT (\{.*\})\r\n/m.exec(socket.data.buffered)
        if (match === null) return
        connects.push({ nonce: socket.data.nonce, connect: JSON.parse(match[1] ?? "{}") })
        socket.end()
      },
    },
  })
  stops.push(() => listener.stop(true))
  return { connects, url: `nats://127.0.0.1:${listener.port}` }
}

describe("the Codex server's NATS CONNECT", () => {
  test.each([
    ["NATS_AUTH=broker in the environment", "env"],
    ["natsAuth: broker in its envoy.json", "file"],
  ] as const)(
    "carries the session's broker token and its key's proof over the server's nonce, told %s",
    async (_told, way) => {
      const root = makeDirectory()
      const trades = broker()
      const nats = hub()
      const session = crypto.randomUUID()
      const homes = codexHomes(root, {
        enabled: true,
        // No listener answers: the CONNECT is made before the session registers.
        url: "http://127.0.0.1:9",
        natsUrls: [nats.url],
        ...(way === "file" ? { natsAuth: "broker" } : {}),
      })
      const tokenFile = join(root, "pod-token")
      writeFileSync(tokenFile, `${fixtureSecret("pod")}\n`)
      await recordFor(homes.pluginData, session, homes)
      const server: CodexServer = startCodexServer(
        {
          HOME: homes.home,
          PLUGIN_DATA: homes.pluginData,
          AGENT_SECRETS_URL: trades.url,
          AGENT_SECRETS_MACHINE_IDENTITY: "kubernetes",
          AGENT_SECRETS_MACHINE_TOKEN_FILE: tokenFile,
          ...(way === "env" ? { NATS_AUTH: "broker" } : {}),
        },
        root,
      )
      stops.push(server.stop)
      await until(() => nats.connects.length > 0, "the server's CONNECT", 30_000, server.stderr)

      const { nonce, connect } = nats.connects[0] ?? { nonce: "", connect: {} }
      const token = connect["auth_token"]
      expect(typeof token).toBe("string")
      // The token the broker minted for this session's trade, and no other.
      const minted = trades.minted.find((entry) => entry.token === token)
      expect(minted).toBeDefined()
      // Traded for the session the Codex session's record names, and no other.
      expect(minted?.session).toBe(session)
      expect(typeof connect["sig"]).toBe("string")
      const proof = Buffer.from(String(connect["sig"]), "base64url").toString("utf8")
      const [header, claims, signature] = proof.split(".")
      expect(part(proof, 0)["typ"]).toBe("dpop+jwt")
      expect(part(proof, 0)["alg"]).toBe("ES256")
      const said = part(proof, 1)
      expect(said["htm"]).toBe("CONNECT")
      expect(said["htu"]).toBe("nats:connect")
      expect(said["nonce"]).toBe(nonce)
      expect(said["ath"]).toBe(createHash("sha256").update(String(token)).digest("base64url"))
      // Signed by the key the proof embeds, and that key is the one the token was bound to.
      // The proof's header names it as a JWK, which the key object reads.
      const jwk = part(proof, 0)["jwk"] as JsonWebKeyInput["key"] & Record<string, unknown>
      const signed = verify(
        "sha256",
        Buffer.from(`${header}.${claims}`),
        {
          key: createPublicKey({ key: jwk, format: "jwk" }),
          dsaEncoding: "ieee-p1363",
        },
        Buffer.from(signature ?? "", "base64url"),
      )
      expect(signed).toBe(true)
      expect(thumbprint(jwk)).toBe(minted?.jkt ?? "")
    },
    60_000,
  )
})
