// packages/cipher-client/src/projected-token.ts

import { readFile } from "node:fs/promises";
import { readErrorCode } from "./error-text";
import type { SubjectToken, TokenSource } from "./source";
import { SUBJECT_TOKEN_TYPE_KUBERNETES } from "./wire";

/**
 * The identity of a pod: the projected Kubernetes service-account token its runtime mounts at
 * `path`, minted for the broker's exchange audience. The kubelet rewrites that file in place well
 * before the token expires, so it is read at every trade and never once at start. `variable` is
 * the setting that named the path, which its texts name in place of the path: a token pasted into
 * that setting is the path, and a trade's error is printed in a session's notices.
 */
export function projectedToken(path: string, variable: string): TokenSource {
  const named = `the projected service-account token ${variable} names`;
  return {
    async read(signal?: AbortSignal): Promise<SubjectToken> {
      let contents: string;
      try {
        contents = await readFile(path, {
          encoding: "utf8",
          ...(signal === undefined ? {} : { signal }),
        });
      } catch (error) {
        throw new Error(`read ${named}: ${readErrorCode(error)}`);
      }
      const token = contents.trim();
      if (token === "") throw new Error(`${named} is empty`);
      return { token, tokenType: SUBJECT_TOKEN_TYPE_KUBERNETES };
    },
    describe: () => named,
  };
}
