import { useQuery } from "@tanstack/react-query";
import { Fragment, type ReactNode, useRef, useState } from "react";

import { isText } from "../../api/artifact-version";
import { api } from "../../api/client";
import type { Artifact, Version } from "../../api/types";
import {
  focusVisibleRing,
  pageFailedText,
  pageField,
  pageHeaderControl,
  pageInkText,
  pageLineBorder,
  pageMutedText,
  pageQuietText,
  pageRowAct,
} from "../../theme/classes";
import { artifactVersionUrl, formatArtifactBytes } from "../artifacts/ArtifactHeader";
import { ChangeView } from "../doc/ChangeView";
import { AgentName } from "../refs/AgentName";
import { useListPages } from "./details-leaves";
import { shortDay } from "./node-time";
import { authorText, pageChrome } from "./page-structure";
import { ANSWER_LINES, CONTROL_LABELS, FILE_WORDS, sentenceCase } from "./page-words";

/** The name of the checksum a file's version carries, written before it as the checksum's own
 *  notation, "SHA-256 9f86d0…", which today's version list draws: it is part of the file's own
 *  fact and drawn with it as the file's, so it is no word of the page's lists. */
const CHECKSUM = "SHA-256";

/**
 * A version as the page names it (part N's R2, the prototype's `openDoc`): `word`, its number,
 * who wrote it and the day it was made, "version 3 · alice · 5 Nov". The number, the names and
 * the day are the document's own; the word and the dots are the page's. The head of an opened
 * document writes the word in lower case, as the prototype does; an entry of the version list
 * opens with it, so it is capitalised there.
 */
export function VersionWords({ version, word }: { version: Version; word: string }): ReactNode {
  const day = shortDay(version.created_at);
  const authors = version.authors ?? [];
  return (
    <>
      {word} <span {...authorText}>{version.number}</span>
      {authors.length === 0 ? null : (
        <>
          {" · "}
          <span {...authorText}>
            {authors.map((author, index) => (
              <Fragment key={`${author.kind}:${author.id}`}>
                {index === 0 ? null : ", "}
                <AgentName actor={author} author inline />
              </Fragment>
            ))}
          </span>
        </>
      )}
      {day === "" ? null : (
        <>
          {" · "}
          <span {...authorText}>{day}</span>
        </>
      )}
    </>
  );
}

/** A file's size and checksum, each the file's own fact, as today's version list draws them
 *  beside a version: a document is told apart by its text, so it draws its size alone. */
function fileFacts(artifact: Artifact, version: Version): string[] {
  const facts = version.size === undefined ? [] : [formatArtifactBytes(version.size)];
  if (artifact.kind !== "doc" && version.sha256 !== undefined) {
    facts.push(`${CHECKSUM} ${version.sha256}`);
  }
  return facts;
}

/**
 * The versions of a document or file opened in place of the outline, under its editor or its
 * picture (part N's R2): what today's version list and comparison carried, in the page's own
 * words and colours. The list draws every version, named or not, newest first, each by its name,
 * or by its number, author and day where it has none, with its "Download version N" and a file's
 * size and checksum: 20 at a time, with the control under them that says how many are left and
 * draws the next 20 (part M's `useListPages`, root R40), and no control that hides the unnamed
 * ones. The comparison of any two versions is folded under "Show comparison": a document's as the
 * change view, word by word, and a file's as each side's size and checksum. The version control
 * beside the views compares one version with the document as it stands (R3); this compares any
 * two.
 *
 * `versions` is newest first: the versions the document's editor read, newer than the issue's
 * own read once a version is named here.
 */
export function DocumentVersions({
  artifact,
  versions,
}: {
  artifact: Artifact;
  versions: readonly Version[];
}): ReactNode {
  const list = useRef<HTMLUListElement>(null);
  const pages = useListPages();
  return (
    <div className="space-y-4">
      <section aria-label={`${FILE_WORDS.versions_for} ${artifact.name}`} className="space-y-2">
        <h3 className={`text-sm font-semibold ${pageMutedText}`} {...pageChrome}>
          {FILE_WORDS.versions}
        </h3>
        <ul className="space-y-1" ref={list}>
          {versions.slice(0, pages.drawn).map((version) => {
            const facts = fileFacts(artifact, version);
            return (
              <li
                className="flex flex-wrap items-center justify-between gap-x-2 text-sm"
                key={version.number}
              >
                <span className={pageInkText} {...pageChrome}>
                  {version.named && version.summary !== null ? (
                    <span {...authorText}>{version.summary}</span>
                  ) : (
                    <VersionWords version={version} word={sentenceCase(FILE_WORDS.version)} />
                  )}
                </span>
                <a
                  className={`inline-flex min-h-6 items-center font-medium underline ${pageInkText} ${pageRowAct}`}
                  download=""
                  href={artifactVersionUrl(artifact.id, version.number, { download: true })}
                >
                  <span {...pageChrome}>{CONTROL_LABELS.download_version}</span>{" "}
                  <span {...authorText}>{version.number}</span>
                </a>
                {facts.length === 0 ? null : (
                  <span className={`w-full break-all text-xs ${pageQuietText}`} {...authorText}>
                    {facts.join(" · ")}
                  </span>
                )}
              </li>
            );
          })}
        </ul>
        {pages.more(versions.length, (index) => list.current?.children[index])}
      </section>
      {versions.length >= 2 ? <VersionComparison artifact={artifact} versions={versions} /> : null}
    </div>
  );
}

/** One side of the comparison: a picker over every version, newest first, named after the
 *  document and the side, "Compare notes.md from". */
function VersionPicker({
  label,
  name,
  onChange,
  value,
  versions,
}: {
  label: string;
  name: string;
  onChange: (version: number) => void;
  value: number | undefined;
  versions: readonly Version[];
}): ReactNode {
  return (
    <label className={`text-xs font-medium ${pageMutedText}`}>
      <span {...pageChrome}>{label}</span>
      <select
        aria-label={name}
        className={`mt-1 block min-h-6 w-full min-w-0 rounded-[7px] border px-2 text-sm ${pageField} ${focusVisibleRing}`}
        onChange={(event) => onChange(Number(event.target.value))}
        value={value}
      >
        {versions.map((version) => (
          <option key={version.number} value={version.number}>
            {CONTROL_LABELS.version} {version.number}
            {version.named && version.summary !== null ? ` — ${version.summary}` : ""}
          </option>
        ))}
      </select>
    </label>
  );
}

/** One file version of the comparison: its side and number, "From · Version 2", then its size
 *  and checksum. */
function FileSide({
  artifact,
  side,
  version,
}: {
  artifact: Artifact;
  side: string;
  version: Version;
}): ReactNode {
  return (
    <div className={`min-w-0 rounded-lg border p-3 ${pageLineBorder}`}>
      <p className={`text-xs font-medium ${pageMutedText}`} {...pageChrome}>
        {side} · {CONTROL_LABELS.version} <span {...authorText}>{version.number}</span>
      </p>
      {fileFacts(artifact, version).map((fact) => (
        <p className={`mt-1 break-all text-xs ${pageQuietText}`} key={fact} {...authorText}>
          {fact}
        </p>
      ))}
    </div>
  );
}

/**
 * The comparison of any two versions, folded until the reader opens it, since a change view is
 * long and reads both versions' text: From the second newest, To the newest, until the reader
 * picks others. A file has no text to compare, so its comparison is each side's size and
 * checksum, and says so where the two hold the same bytes.
 */
function VersionComparison({
  artifact,
  versions,
}: {
  artifact: Artifact;
  versions: readonly Version[];
}): ReactNode {
  const [open, setOpen] = useState(false);
  const [before, setBefore] = useState<number | undefined>();
  const [after, setAfter] = useState<number | undefined>();
  const beforeNumber = before ?? versions[1]?.number;
  const afterNumber = after ?? versions[0]?.number;
  const isDocument = artifact.kind === "doc";
  const beforeText = useQuery({
    enabled: open && isDocument && beforeNumber !== undefined,
    queryKey: ["artifact", artifact.id, "version", beforeNumber],
    queryFn: () => api.getArtifactVersion(artifact.id, beforeNumber ?? 0),
  });
  const afterText = useQuery({
    enabled: open && isDocument && afterNumber !== undefined,
    queryKey: ["artifact", artifact.id, "version", afterNumber],
    queryFn: () => api.getArtifactVersion(artifact.id, afterNumber ?? 0),
  });
  const beforeVersion = versions.find((version) => version.number === beforeNumber);
  const afterVersion = versions.find((version) => version.number === afterNumber);
  // Each picker is named after the document and its side, "Compare notes.md from".
  const compareName = `${CONTROL_LABELS.compare} ${artifact.name}`;
  return (
    <section
      aria-label={`${CONTROL_LABELS.compare_versions_for} ${artifact.name}`}
      className="space-y-2"
    >
      <div className="flex flex-wrap items-center justify-between gap-3">
        <h3 className={`text-sm font-semibold ${pageMutedText}`} {...pageChrome}>
          {CONTROL_LABELS.compare_versions}
        </h3>
        <button
          aria-expanded={open}
          className={pageHeaderControl}
          onClick={() => setOpen((shown) => !shown)}
          type="button"
          {...pageChrome}
        >
          {open ? CONTROL_LABELS.hide_comparison : CONTROL_LABELS.show_comparison}
        </button>
      </div>
      {open ? (
        <>
          <div className="grid grid-cols-2 gap-2">
            <VersionPicker
              label={CONTROL_LABELS.compare_from}
              name={`${compareName} ${CONTROL_LABELS.compare_from.toLowerCase()}`}
              onChange={setBefore}
              value={beforeNumber}
              versions={versions}
            />
            <VersionPicker
              label={CONTROL_LABELS.compare_to}
              name={`${compareName} ${CONTROL_LABELS.compare_to.toLowerCase()}`}
              onChange={setAfter}
              value={afterNumber}
              versions={versions}
            />
          </div>
          {isDocument ? (
            beforeText.isError || afterText.isError ? (
              <p className={`text-sm ${pageFailedText}`} {...pageChrome}>
                {ANSWER_LINES.versions_unreadable}
              </p>
            ) : isText(beforeText.data) && isText(afterText.data) ? (
              // The change view draws the two versions' text, which is the document's own.
              <div {...authorText}>
                <ChangeView after={afterText.data.markdown} before={beforeText.data.markdown} />
              </div>
            ) : (
              <p className={`text-sm ${pageQuietText}`} {...pageChrome}>
                {ANSWER_LINES.versions_loading}
              </p>
            )
          ) : beforeVersion === undefined || afterVersion === undefined ? null : (
            <div className="space-y-2">
              <div className="grid grid-cols-2 gap-2">
                <FileSide
                  artifact={artifact}
                  side={CONTROL_LABELS.compare_from}
                  version={beforeVersion}
                />
                <FileSide
                  artifact={artifact}
                  side={CONTROL_LABELS.compare_to}
                  version={afterVersion}
                />
              </div>
              {beforeVersion.size === afterVersion.size &&
              beforeVersion.sha256 === afterVersion.sha256 ? (
                <p className={`text-sm ${pageQuietText}`} {...pageChrome}>
                  {ANSWER_LINES.versions_same}
                </p>
              ) : null}
            </div>
          )}
        </>
      ) : null}
    </section>
  );
}
