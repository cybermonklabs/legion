# Architecture components

Part of `skill://legion-architect/references/decomposition.md`. Read this before you decompose a
root whose project has architecture components or has none yet, and before you give a child its
own `--components-json`.

A project's components are the `.dispatch/architecture/` files Dispatch imports from its source
repositories (`skill://dispatch`).

- They exist: attach the root before you decompose, so every child inherits them:
  `dispatch issue-update --issue <root> --components-json '{"mode":"explicit","ids":[…]}'`.
- They do not: the root's first pull request writes `.dispatch/architecture/<id>.md` for each
  planned component (front matter `title`, `parent`, `depends_on`, `external`; no `paths`) in the
  branch of the source whose repository holds it. `parent` and `depends_on` name components of
  that same source only, and its id is one no other source of the project uses (a repeated id
  fails that source's import). Leave the root on `inherit` until it merges (an earlier attach
  answers `400 COMPONENTS_INPUT`), then run `dispatch architecture-sync --project <KEY>` (every
  source; `--repo <owner/name>` syncs that one alone) and attach the root; children created
  before then inherit it.
- Give a child its own `--components-json` only for a narrower set, and
  `{"mode":"none","reason":"…"}` when it changes no code or file a component describes.
