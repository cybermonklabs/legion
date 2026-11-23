import { expect, test } from "bun:test";
import type { LineHost } from "../src/line-hosts";
import { withMarksEditor } from "./marks-editor";

const LIST = "dispatch://issues?label=overnight";
const DONE = "dispatch://issues?label=overnight&status=done";

/** Every element the editor draws at the top of the document, as its tag and its text: a host's
 *  element stands among the blocks, so this says where each was drawn. */
function topOfDocument(root: HTMLElement): string[] {
  return Array.from(
    root.children,
    (element) =>
      `${element.tagName.toLowerCase()}${element.hasAttribute("data-line-host") ? "#host" : ""}:${element.textContent ?? ""}`
  );
}

// A list of issues a document holds is drawn below the line that writes it, and the line keeps
// its text (dispatch://INFRA-4935/spec, "The reference"): a host's element stands below each
// top-level paragraph whose whole text its test accepts, and nowhere else.
test("a host is drawn below each line its test accepts, and the line keeps its text", async () => {
  await withMarksEditor(
    `${LIST}\n\nSee ${LIST} in a sentence.\n\n${DONE}`,
    ({ handle, view }) => {
      const told: (readonly LineHost[])[] = [];
      handle.setLineHosts({
        accepts: (text) => text.startsWith("dispatch://issues?"),
        onHosts: (hosts) => told.push(hosts),
      });
      expect(topOfDocument(view.dom)).toEqual([
        `p:${LIST}`,
        "div#host:",
        `p:See ${LIST} in a sentence.`,
        `p:${DONE}`,
        "div#host:",
      ]);
      const hosts = told.at(-1) ?? [];
      expect(hosts.map(({ index, text }) => [index, text])).toEqual([
        [0, LIST],
        [1, DONE],
      ]);
      // Each element told is the one drawn.
      expect(hosts.map(({ element }) => element.isConnected)).toEqual([true, true]);
      expect(view.state.doc.textContent).toContain(LIST);

      // A host drawing into its element changes no line of the document.
      hosts[0]?.element.append("drawn by the host");
      expect(view.state.doc.childCount).toBe(3);

      handle.setLineHosts(null);
      expect(view.dom.querySelectorAll("[data-line-host]")).toHaveLength(0);
    },
    { access: "read" }
  );
});
