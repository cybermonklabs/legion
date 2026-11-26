# @legion/proof-editor

- Part of `src/` is copied from the `sjawhar/proof-sdk` fork, and the rest is legion's own: a file under `src/` that the first override of the root `biome.json` does not exclude is a copy.
  `docs/copied-files.md` holds the only kinds of edit a copy may take.
  Put new behaviour in a module of legion's own that the copy imports, and add that module to the override's exclusions so that Biome checks it.

- Before you bump a micromark package, read "Parser patches" in `docs/upstream.md`: its patches and the tokenizer hooks the directive container relies on must carry over.

Where this file and the code disagree, the code is right: follow the code and correct this file.
