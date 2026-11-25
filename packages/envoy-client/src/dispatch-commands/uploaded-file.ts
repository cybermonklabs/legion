import type { Artifact } from "@legion/contracts";
import type { DispatchClient } from "../dispatch-http";
import { shownPictureLine } from "../dispatch-picture-tools";
import {
  oversizePictureText,
  PICTURE_SHOWN_MAX_BYTES,
  sniffPictureType,
  toolImage,
} from "../dispatch-pictures";
import type { DispatchToolResult } from "./context";

/**
 * dispatch_doc_read of an uploaded file or image, or of a document's version stored as bytes
 * before the document became one. Dispatch serves a file's content only as the bytes of one of its
 * versions (its `/text` route answers a file 400 NOT_DOCUMENT), so this reads the version asked
 * for, or the latest. A picture a model takes (an image artifact whose bytes are a PNG, JPEG, GIF
 * or WebP of at most `PICTURE_SHOWN_MAX_BYTES`) comes back as an image block with its name, type,
 * version and size as the text, then the line naming it by its address
 * (`dispatch://<owner>/artifact/<slug>@v<N>`, `owner` an issue key, a project key or
 * `agent/<session id>`) as `dispatch_read` does, which a host reads back from a transcript; a
 * larger picture is described without its bytes being fetched when Dispatch states its size. Any
 * other file comes back as text when it is UTF-8, and is described with the route that serves its
 * bytes when it is not. The header names the version read by its hash, on the line under its
 * first: the latest version's `Document token:` line, as a document read prints its token, which
 * the upload of the next version names as its precondition; a named version's `Content: sha256:`
 * line, as a named document version's read prints its hash. It goes in the header, never after
 * the text, so the text after the header is the stored file unchanged, a leading byte order mark
 * included (the decoder keeps it), and a script or patch read back ends where the file ends. The
 * dispatch command changes two things, as it does for every output it prints: it adds one line
 * feed after a file that ends without one, and under Claude Code or Codex it cuts an output past
 * 25,000 characters and writes the whole of it to a file it names (`fitOutput` in
 * `../dispatch-cli.ts`). Dispatch states the token and the hash beside the version in the
 * artifact's version list, so no other read is made for them, and a Dispatch or a version that
 * states none prints no line: such a Dispatch takes no token on a file's upload. A file carries no
 * anchors or approval, so nothing else is read. A picture is shown whatever `shown` holds, and
 * joins it under its address, so the session's later reads name it instead of sending it.
 */
export async function readUploadedFile(
  client: DispatchClient,
  artifact: Artifact,
  details: Record<string, unknown>,
  requested: number | undefined,
  owner: string,
  shown: Set<string> | undefined
): Promise<DispatchToolResult> {
  const versions = artifact.versions ?? [];
  const latest = Math.max(0, ...versions.map((version) => version.number));
  const number = requested ?? latest;
  const of = number === latest ? "" : ` of ${latest}`;
  const bytesRoute = `GET /api/v1/artifacts/${artifact.id}/versions/${number} serves its bytes.`;
  const stated = versions.find((version) => version.number === number);
  // Keep the token in the header so the file's text can be copied back byte for byte.
  const hashLine =
    requested === undefined
      ? stated?.token === undefined
        ? ""
        : `\nDocument token: ${stated.token}`
      : stated?.sha256 === undefined
        ? ""
        : `\nContent: sha256:${stated.sha256}`;
  const tooLarge = (mime: string, bytes: number): DispatchToolResult => ({
    text:
      `${oversizePictureText(artifact.name, bytes)}, so dispatch doc-read cannot show this ` +
      `uploaded ${mime} picture (version ${number}${of}). ${bytesRoute}${hashLine}`,
    details,
  });
  if (
    artifact.kind === "image" &&
    stated?.size !== undefined &&
    stated.size > PICTURE_SHOWN_MAX_BYTES
  ) {
    return tooLarge(stated.mime ?? "image", stated.size);
  }
  const file = await client.fileVersion(artifact.id, number);
  const size = `${file.bytes.length.toLocaleString("en-US")} bytes`;
  if (artifact.kind === "image") {
    const image = toolImage(file.bytes);
    if (image !== undefined) {
      const address = `dispatch://${owner}/artifact/${artifact.slug}@v${number}`;
      shown?.add(address);
      return {
        text:
          `Picture ${artifact.name}: ${image.mimeType}, version ${number}${of}, ${size}.${hashLine}\n` +
          shownPictureLine(1, address, artifact.name, image.mimeType, file.bytes.length),
        details,
        images: [image],
      };
    }
    if (sniffPictureType(file.bytes) !== undefined) return tooLarge(file.mime, file.bytes.length);
  }
  let text: string;
  try {
    text = new TextDecoder("utf-8", { fatal: true, ignoreBOM: true }).decode(file.bytes);
  } catch {
    return {
      text:
        `${artifact.name} is an uploaded ${file.mime} file (version ${number}${of}, ${size}) that is not ` +
        `UTF-8 text, so dispatch doc-read cannot show it. ${bytesRoute}${hashLine}`,
      details,
    };
  }
  return {
    text: `File ${artifact.name}: ${file.mime}, version ${number}${of}, ${size}.${hashLine}\n\n${text}`,
    details,
  };
}
