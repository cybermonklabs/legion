# Edits allowed in a copied file

A file under `src/` is a copy when the `sjawhar/proof-sdk` fork's commit 24a5fc94915cda5704897c497af6312c140db40d holds it at the same path; `src/directive-factories.ts` and `src/html-flow.ts` are copies from micromark (below), and the rest are legion's own.
That commit stays the audit source when the upstream pin (`docs/upstream.md`) moves.
The copies and the generated `upstream/` keep their MIT license (`LICENSE`); legion's own files, `scripts/` and `tests/` are Apache-2.0.

A file copied from the fork differs from its counterpart only by the kinds of edit below, and by no others.
Diff it against `jj --ignore-working-copy -R <proof-sdk> file show -r 24a5fc94 root:src/<file>`: every line that differs is one of these kinds.
Biome's formatter, linter and `assist` are off over the copies, in the first override of the root `biome.json`, so that this diff stays readable; an editor that organizes imports on save still reorders them.

1. The import specifiers of upstream modules.

2. `bun test` registration in the copied suites: `src/tests/harness.ts` replaces each file's own `test()` tally and its `process.exit` tail.

3. `src/tests/headless-no-dom.test.ts` imports `../lib-headless.js` in place of the fork's built `dist/headless.js`; its banner, the comment beside that import and the case's name follow.

4. `src/editor/schema/block-ids.ts` extends the upstream code-block and frontmatter schemas itself (`codeBlockSchemaExt`, `frontmatterSchema`) and reads `withDomAttributes` from `./dom-attributes`.

5. `src/editor/schema/block-ids.ts`'s default id generator is `uuidV4` from `./uuid`, in place of `crypto.randomUUID`.

6. The copy-attributes fix in `src/block-schema.ts` and `src/tests/block-schema.test.ts`: `typedBlockSpec` moved out of `blockSchemaPlugins`'s `$nodeSchema` callback and exported, `attributeText` lifted out of `markdownAttrs`, the new `domAttributeName`, `domAttrs` and `parsedDomAttrs`, the parse rule's `getAttrs` and `contentElement`, the `withDomAttributes` wrapper on `toDOM`, and the suite's cases for all of it.

7. `src/lib.ts` installs its peer cursors through `collabCursorPlugin` from `./collab-cursor-plugin` instead of calling `yCursorPlugin` itself.

8. `src/lib.ts` installs `trailingNewlineInputPlugin` from `./trailing-newline-input`.

9. `src/lib.ts` installs `recordMarkHistoryPlugin` from `./record-mark-history`.

10. `src/lib.ts` routes `removeMark` and the new `retypeMark` through `./record-mark-retype`.

11. `src/lib.ts` installs `editorHighlightsPlugin`, routes `setActiveMarks`, `setActiveBlocks`, the pulses of `focusMark` and `focusBlock`, and the handle's new `setBlockMarks` through `./editor-highlights`, and re-exports its `BlockMark`.

12. `src/lib.ts` installs `editorChangeDecorationsPlugin`, routes the handle's new `setChangeDecorations` through `./editor-change-decorations`, and re-exports its `ChangeDecorations`, `TextDecoration` and `BlockWidget`.

13. `src/lib.ts` installs `referenceDecorationsPlugin`, routes the handle's new `setReferences` through `./reference-decorations`, and re-exports its `EditorReferences`, `ReferenceCandidate` and `ReferenceTarget`; `src/lib.css` holds the rules that draw a reference's title on one line, cut with an ellipsis, and hide the stored text under it.

14. `src/dispatch-marks.ts`'s `dispatchAsk` schema declares `excludes: ''`, so two readers' asks may cover the same text; `removeAskMark` removes that mark's instance by id, not every `dispatchAsk` over its range, and the parse runner closes its own span of nested asks, not every open ask.

15. `src/dispatch-mark-events.ts` reads the mark a click or a hover names from `./record-mark-target` instead of taking the innermost mark span.

16. `src/dispatch-action-bar.ts`'s Comment, Suggest and Ask buttons and its exported `commentOnSelection` take one path, `ActionBarController.act`, which marks nothing for an empty or stale range or a read-only view, and `src/lib.ts` exposes `commentOnSelection` on the handle.

17. Comments on a read-only document.
    `src/lib.ts`'s `readOnly` option and `setReadOnly` give way to the `access` option and `setAccess` (`'edit' | 'comment' | 'read'`), which make the view editable for `'edit'` alone and hand the mode to the bar's own `setAccess`.
    The bar hides itself on a read-only view unless its access is `'comment'` and the selection holds text, and then offers Comment alone, which writes no mark and reports a `quote-comment` action with the selection's range.
    The bar's touch focus loss (`editorLostFocus`) hides it on an editable view alone.
    `src/dispatch-marks.ts`'s `MarkAction` carries the `quote-comment` kind.
    `src/tests/dispatch-action-bar-position.test.ts`'s view double declares `editable: true`, its element double declares `hidden`, and it has a case for a touch view that turns read-only with comment access after losing focus.

18. `src/block-schema.ts` exports `typedBlockRefusal`, the refusal `visit` throws, and `src/lib-headless.ts`'s `readOnly` option installs `remarkReadOnlyDirectives` from `./read-only-directives` in place of `remarkTypedBlocks`.

19. `src/lib-headless.ts` takes `renderBlock` and passes it to `blockSchemaPlugins`, as `src/lib.ts` does, and re-exports `HostBlockRenderer`, with the case in `src/tests/block-schema.test.ts` that a host drawing keeps the block's copy attributes.

20. `src/block-schema.ts`'s `remarkTypedBlocks` reads its source through `markdownSource` from `./typed-block-drawing`, and `src/lib-headless.ts`'s default branch installs `remarkUnfitTypedBlocks` after `remarkTypedBlocks`.

21. Asks whose content breaks the ask's rule.
    `src/lib.ts` installs `liveTypedBlocksRemarkPlugin` from `./read-only-directives` after the mark plugins in place of `typedBlockRemarkPlugin`, calls `relaxAskContentCheck(schema)` from `./ask-content` before it binds the shared document, and installs that module's `brokenAskInputGuards` (`view.setProps(brokenAskInputGuards)`) and `keepUnselectedAskBlocks` (`.use(keepUnselectedAskBlocks)`).
    `src/lib-headless.ts` calls `relaxAskContentCheck` after building its schema, and its `parseMarkdown` and `readOnly` comments name the live editor's step.
    `src/lib-remark-directive-plugin.ts` drops `typedBlockRemarkPlugin`.
    `src/typed-block-commands.ts`'s `retype` and `canRetype` require the typed node's content to match the body, so a block is never retyped into an ask it cannot be.

22. `src/lib-remark-directive-plugin.ts` registers `directiveContainerSyntax` from `./directive-container` in place of micromark-extension-directive's flow container; `containerDirectiveSyntax`, that package's import and the `micromark-util-types` import it needed are gone, and the plugin's doc comment says which construct it registers.

23. `src/block-schema.ts`'s default mode refuses a `:::name{...}` paragraph line standing inside `maxDirectiveNesting` directives (imported from `./directive-container`), an opener the tokenizer left as text: `rejectUnsupportedDirectiveSyntax` counts the `containerDirective` nodes around each paragraph, and `unsupportedDirectiveReason` takes that count.

24. `src/directive-factories.ts` joins micromark-extension-directive 4.0.0's `lib/factory-name.js`, `lib/factory-label.js` and `lib/factory-attributes.js` into one module, in that order, each after a comment naming its file, under the package's MIT notice, with the three files' imports in one block at the top and each JSDoc `@import` an `import type`.

25. `src/lib.ts` installs `mermaidCodeBlockPlugin` from `./mermaid-code-block-view` in place of upstream's `mermaidDiagramsPlugin` and takes the host's `drawMermaid` option (typed `MermaidDrawer`, imported from `./mermaid-svg`) for it, and `src/lib.css` drops its `.proof-mermaid-svg` rules, which that view's shadow-root sheet carries.

26. The `block[]` attribute kind, which reads, writes and checks its values through `./block-pointers`.
    In `src/block-schema.ts`: `BlockAttributeKind` gains `block[]`, `BlockAttributeSchema` gains `targets`, `validateBlockSchema` refuses `targets` on any other kind, and `parseAttribute`'s `block[]` case calls `parseBlockPointers` and `attributeText`, with its one conditional expression written as statements, joining the pointers with one space.
    In `src/typed-block-commands.ts`: `TypedBlockCommandContext.documentRef`; `validateValue`'s `block[]` case calling `@legion/contracts`' `blockPointersProblem`; `findBlock` moved to `./block-pointers`, which returns the first holder of a repeated id; and `insert`, `retype` and `setAttributes` building their transaction first and handing it, with the block it replaces or changes, to `dispatchUnlessRefused`, which throws `blockPointerRefusal`'s refusal before it dispatches, so a refused command leaves the document unchanged.
    `blockPointerRefusal` treats that block's pointers as already held only when its type is unchanged.
    In `src/lib.ts`: `CreateProofEditorOptions.documentRef`, passed to `createTypedBlockCommands`.
    In `src/tests/block-schema.test.ts`: the imports of `DISPATCH_BLOCK_POINTER_VALUES`, `EditorState` and `createTypedBlockCommands`, and the suite's four cases for the kind.

27. The manifest's `since` and `creatable_since`, read by `src/block-schema.ts`, `src/typed-block-commands.ts` and `src/lib.ts`.
    The `BlockSchema` types name the manifest's nodes and marks and each element's numbers.
    A block the editor makes takes no default of an attribute the editor may not create yet (`typedBlockSpec`'s `version`).
    The commands offer, insert, retype into and write only the typed blocks, attributes and enum values the editor may create (`creatable`, `creatableValue`).
    `src/lib.ts`'s `installCreationGate` makes a markdown parse through `parserCtx` (a text/plain paste, a suggestion's accept) and an editable editor's `setMarkdown` throw for content holding a node, mark, typed block, attribute or enum value the editor may not create yet (`assertCreatable`), and puts `creatableDomParser` in `schema.cached`, so an HTML paste or drop reads no such node or mark, and `parsedDomAttrs` reads no typed block's section holding such an attribute or value.
    A read-only editor's `setMarkdown`, the collab sync and the headless engine still read all of them.

28. Editors that write no marks of Proof's own.
    `src/lib.ts`'s `proofMarks` option (true by default): set to `false`, the editor installs neither `authoredTrackerPlugin`, nor `markPopoverPlugin` and `arrowCommentPlugin`, nor `dispatchActionBarPlugin`, and registers no popover hook.
    So it offers no Comment, Suggest or Ask on a selection and writes no mark from one, records no authorship on what a person types, and its `commentOnSelection` answers false; `CreateProofEditorOptions` and `commentOnSelection`'s comment say so.
    Dispatch sets it for a priority's description, which it saves as markdown; `tests/proof-marks.test.ts` holds both settings.

29. `src/lib-headless.ts`'s parse processor installs `remarkKeepAddressesWhole` from `./whole-addresses` right after `remarkGfm`, and `src/lib.ts` installs that module's `libraryRemarkKeepAddressesWholePlugin` right after `gfm`, each with a comment saying why, so a bare `dispatch://` address is read whole by the headless engine and by the live editor's paste and `setMarkdown`.

30. Annotations, casts and assertions that make a file type-check, each of which erases before runtime.

31. The `minutes` attribute kind, a duration a plan task's `size` is written in (`45m`, `2h`, `1h30m`).
    In `src/block-schema.ts`: `minutesValueProblem`, the twin of pmdoc's `Minutes`, with its pattern and its bound, and `parseAttribute`'s `minutes` case calling it.
    In `src/typed-block-commands.ts`: `validateValue`'s `minutes` case calling it.
    In `src/tests/block-schema.test.ts`: its import and the suite's case for the kind.

31. Single-block editors (the `element` option, decision 544).
    `src/lib.ts`'s `CreateProofEditorOptions` gains `element?: Y.XmlElement`.
    When `element` is given, `createProofEditor` refuses a document-level node (`XmlFragment` or `doc`), wraps `serializerCtx` before `.use(listener)` so a non-`doc` root serializes inside `doc`, sets `editorStateOptionsCtx` and `schema.topNodeType` to the element's own node type and attributes, and binds `collabService` to `opts.element` instead of `opts.ydoc.getXmlFragment(opts.fragment ?? 'prosemirror')` (the one rewritten line).
    The `element` change in `t17` added 47 lines and rewrote one line in `src/lib.ts`.

    Decision 551's rebind adds 34 lines and rewrites two in `src/lib.ts` (`+36 / −2` against `f885e9ba`):

    | Lines after the rebind change | Count | Change |
    | --- | ---: | --- |
    | 39 | 1 added | Import `editorStateCtx`. |
    | 63 | 1 added | Import `EditorState` to replace the state in the kept view. |
    | 243–244 | 2 added | Declare `ProofEditorHandle.rebindElement`. |
    | 341 | 1 added | Default `rememberAwareness` to `true` in the cursor installer. |
    | 366 | 1 rewritten | Set the collaboration service's awareness only when `rememberAwareness` is true. |
    | 614 | 1 added | Hold the bind generation, initially `0`. |
    | 632 | 1 rewritten | Guard the initial cursor install by that generation and preserve the service's awareness for an editor made without `element`. |
    | 657–669 | 13 added | Validate the target, construct its root with its own attributes, and advance the generation. |
    | 670–684 | 15 added | Copy the live state plugins into Milkdown, disconnect, switch the root and state, and connect to the new element. Install cursors with the new generation and leave the view's own plugins untouched. |

    `src/tests/element-option.test.ts` exercises the target's type and `blockId`, both places a host installs plugins, the old cursor install, and the refusal to rebind a full-document editor.

    Decision 551's cut adds 6 lines to `src/lib.ts` beyond the lines above and rewrites none (`+6 / −0`); each acts only where `element` is given, so an editor made without it builds as before:

    | Lines after the cut | Count | Change |
    | --- | ---: | --- |
    | 35 | 1 added | Import `createBlockBuildTiming` from `./block-build-timing`. |
    | 460 | 1 added | Make the build's timing (`blockBuild`) for a block editor alone; it is `undefined` without `element`. |
    | 542 | 1 added | Install its plugin, which gates Milkdown's dependency timers. |
    | 593 | 1 added | Yield after `.create()`, before the parser guards and the view's setup (the `view setup` phase). |
    | 611 | 1 added | Yield after `applyAccess`, before the collaboration binding (the `collaboration` phase). |
    | 660 | 1 added | End the last phase's measure before the handle is returned. |

    `src/block-build-timing.ts`, Dispatch's own file beside the copies, adds a gate to each of Milkdown's `initTimerCtx`, `schemaTimerCtx`, `editorStateTimerCtx` and `editorViewTimerCtx` and releases it in a task of its own, by a zero-delay timer, once every other timer in that list has resolved; Milkdown's plugins and their order are unchanged.
    Each phase records one `block-editor-task` measure, from when its task begins to the next yield.
    A hidden page releases each phase at once, with its measure: browsers run a hidden page's timers at most once a second, and Milkdown fails a timer still waited for 3 s after its first wait.
    `src/tests/block-build-timing.test.ts` holds a build with `element`, a build without it, and a build on a hidden page.

    A collaborator's caret label in a single-block editor hangs below its caret's line where it would pass the top of the editable, since the host draws that editor in a box of the block's own height (the issue page's row, R7); an editor whose root is `doc` places every label as before.
    `src/lib.css` adds 9 lines: the `.proof-collab-cursor__label--below` rule, `top: 100%`, with its comment.
    `src/collab-cursor-plugin.ts`, Dispatch's own file beside the copies, adds 24 lines and rewrites none: the class's name, the doc comment's paragraph, the read of each label's place before any class is written (`hangsAbove`, read from the caret and the label's own height, so the class it sets does not change what it reads), and the class's write, for an editor whose root is no `doc` alone.

    A single-block editor whose root is a paragraph or a heading draws the titles of the references its words hold, as a document's text block does.
    `src/reference-decorations.ts`, Dispatch's own file, adds 8 lines and rewrites none: `candidatesBetween` reads such a root as the one text block it is, from 0, where `nodesBetween` visits no text block, and `nextCandidates` reads it again whole at each edit; an editor whose root is `doc` reads as before.
    `src/tests/element-option.test.ts` holds the case for a paragraph root and a heading root.

32. A `block[]` attribute's `items`, the item kinds its values may point at besides a block of a document - an issue's ask, comment or message, a block of one, or an issue - with their `item_versions`, and the `parent` attribute.
    In `src/block-schema.ts`: the import of `PARENT_ATTRIBUTE` and `pointerItemKind` from `@legion/contracts`; `creatableValue` reading `item_versions` for each item a `block[]` value points at; `validateBlockSchema` refusing `items` on any other kind, an item kind `ITEM_KINDS` does not name or one named twice, and a `parent` attribute of any kind but `block[]`; and `parseAttribute`'s `block[]` case passing `attribute.items` to `parseBlockPointers`.
    In `src/typed-block-commands.ts`: the import of `parentProblem` from `./block-pointers`, and `validateValue`'s `block[]` case passing `attribute.items` to `blockPointersProblem` and refusing what `parentProblem` refuses.
    In `src/tests/block-schema.test.ts`: the import of `validateBlockSchema`, the shared table's case building each row's attribute with that row's `items`, and the suite's cases for a parent, a size and a needs, and for an issue in a needs.

33. An editor bound to one XmlFragment of the host's Yjs document: `src/lib.ts`'s `CreateProofEditorOptions.fragment` (`'prosemirror'` where it names none), the `ydoc` option's comment, and `collabService.bindXmlFragment` of that fragment in place of `bindDoc`, with a comment saying why: an issue's room holds one fragment per document.

## The two micromark copies

`src/directive-factories.ts` and `src/html-flow.ts` have no counterpart in the fork.
Each function of `src/directive-factories.ts` differs from its source in micromark-extension-directive 4.0.0 (`node_modules/.bun/micromark-extension-directive@4.0.0` at the repository root, or the package's npm tarball) by the join and the type annotations and casts alone.
`src/html-flow.ts` differs from `node_modules/.bun/micromark-core-commonmark@2.0.3/node_modules/micromark-core-commonmark/dev/lib/html-flow.js` by the construct's name, the one line its notice quotes, the import of `blankLine` from the package rather than a copy of it, and the type annotations alone.
