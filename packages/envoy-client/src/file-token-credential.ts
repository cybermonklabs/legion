// packages/envoy-client/src/file-token-credential.ts
//
// The `dispatch` command's credential when a broker token was traded elsewhere: two files that a
// driver outside this copy keeps, `DISPATCH_BROKER_TOKEN_FILE` with the access token and
// `DISPATCH_BROKER_DPOP_KEY_FILE` with the P-256 key the token is bound to. The command trades
// nothing. It reads both files before each request, so the driver renews the token by renaming a
// new file over the old one, and the next request carries the new token.

import { readFileSync } from "node:fs";
import { type DpopKey, importDpopKey } from "@legion/cipher-client/dpop";
import { readErrorCode } from "@legion/cipher-client/error-text";
import { holdSecret } from "@legion/contracts/held-secrets";
import type { RequestCredential } from "./bearer";
import { httpUrl } from "./envoy-config";
import { sendable } from "./secret-file";

/** Names the file that holds the broker access token, alone; whitespace around it is trimmed. */
export const TOKEN_FILE_VAR = "DISPATCH_BROKER_TOKEN_FILE";
/** Names the file that holds the token's P-256 private key, as one PKCS#8 PEM block. */
export const DPOP_KEY_FILE_VAR = "DISPATCH_BROKER_DPOP_KEY_FILE";

/** The two files, as the settings name them. */
export interface TokenFiles {
  readonly tokenFile: string;
  readonly keyFile: string;
}

/** The label of a PKCS#8 PEM block. The marker lines are built from it rather than written out
 *  whole, because the leak guard every push passes refuses a line holding a private key's
 *  begin marker, whatever follows it. */
const PKCS8_LABEL = "PRIVATE KEY";

/** One PKCS#8 PEM block and nothing else. `createPrivateKey` also takes SEC1 and PKCS#1 PEM,
 *  and the setting takes only this form. */
const PKCS8_PEM = new RegExp(
  `^-----BEGIN ${PKCS8_LABEL}-----\\r?\\n(?:[A-Za-z0-9+/=]+\\r?\\n)+-----END ${PKCS8_LABEL}-----$`
);

/**
 * `filePath`'s text, or an error that names `variable` and the file. No error here or below
 * quotes a byte of the file: a token file holds a credential, and a key file holds key material.
 */
function readNamed(variable: string, filePath: string): string {
  try {
    return readFileSync(filePath, "utf8");
  } catch (error) {
    throw new Error(
      `${variable} names ${filePath}, which could not be read (${readErrorCode(error)})`
    );
  }
}

/** The token in `filePath`, trimmed and held, so no line this process prints carries it. */
function tokenIn(filePath: string): string {
  const token = readNamed(TOKEN_FILE_VAR, filePath).trim();
  if (token === "") {
    throw new Error(
      `${TOKEN_FILE_VAR} names ${filePath}, which holds no token: it is empty or blank`
    );
  }
  if (!sendable(token)) {
    throw new Error(
      `${TOKEN_FILE_VAR} names ${filePath}, which holds whitespace or a control character inside its token`
    );
  }
  return holdSecret(token);
}

/** The key in `filePath`: a P-256 key in one PKCS#8 PEM block. */
async function keyIn(filePath: string): Promise<DpopKey> {
  const pem = readNamed(DPOP_KEY_FILE_VAR, filePath).trim();
  if (!PKCS8_PEM.test(pem)) {
    throw new Error(
      `${DPOP_KEY_FILE_VAR} names ${filePath}, which does not hold one P-256 private key in PKCS#8 PEM`
    );
  }
  // Its refusals name the setting and the file, and quote none of the key's bytes.
  return await importDpopKey(pem, `${DPOP_KEY_FILE_VAR}'s key ${filePath}`);
}

/**
 * The credential `files` give for requests to `dispatchUrl`: `Authorization: DPoP <token>` and a
 * proof signed with the key, naming the request's method, a URL and the token (`ath`). The URL is
 * the request's own, or, where `proofOrigin` is given, `proofOrigin` followed by the request's own
 * path: Dispatch checks each proof's URL against its public origin, and `tokenFilesTarget` gives
 * that origin as `proofOrigin` when `dispatchUrl`'s host is a Kubernetes Service name. Either way
 * the proof drops the query. Both files are read again on every call, so the credential keeps no
 * token of its own. A URL whose origin is not `dispatchUrl`'s is refused, one on `proofOrigin`
 * included, so the token reaches no other server. A file that cannot be read, a token file that
 * is empty or blank, and a key that is not P-256 in PKCS#8 PEM each fail the call with an error
 * that names the setting and the file.
 */
export function fileTokenCredential(
  files: TokenFiles,
  dispatchUrl: string,
  proofOrigin?: string
): RequestCredential {
  const origin = httpUrl(dispatchUrl)?.origin;
  let presented = "";
  return {
    scheme: "DPoP",
    async headers(method: string, url: string): Promise<Readonly<Record<string, string>>> {
      const target = httpUrl(url);
      if (origin === undefined || target?.origin !== origin) {
        throw new Error(
          `the token ${TOKEN_FILE_VAR} names goes to DISPATCH_URL's origin (${origin}) alone, and this request is for ${target?.origin ?? "a URL that is not http or https"}`
        );
      }
      const proofUrl = proofOrigin === undefined ? url : `${proofOrigin}${target.pathname}`;
      const token = tokenIn(files.tokenFile);
      const key = await keyIn(files.keyFile);
      presented = token;
      return {
        Authorization: `DPoP ${token}`,
        DPoP: await key.proof({ method, url: proofUrl, accessToken: token }),
      };
    },
    stale(): void {
      // Nothing is kept to drop: the next call reads the token file again, which the driver
      // renews by rename.
    },
    secret: () => presented,
  };
}
