# The captured diffs of the survival computation's tests

Each `.diff` here is the answer GitHub's diff media type gave for one commit of cybermonklabs/legion, read with plain `gh api` on 2026-11-26, and each `<name>.json` beside it is its golden.

`TestParseDiffReadsEveryCapturedShape` reads every `.diff` with `survival.ParseDiff`, compares its files and hunks with the golden, holds it to the shape the table below names, and fails when the directory holds fewer than the 13 diffs and the compare-route copy.

`TestTheMergeCommitsTwoRoutesParseAlike` reads the merge commit through both routes, and `TestFilesPatchReportsEveryWithheldFile` reads `files-page.json`.

## The golden

Each golden is what `parse_git_diff` of `scripts-feasibility/patchlib.py` reads of the same file, so the Go parser is compared with the reference and not with itself.

The script is that of dispatch://INFRA-3117/artifact/survival-scripts-tar-gz version 5, whose `patchlib.py` has the sha256 `4e7a2e2c85c0e478f5d4ebdff925d6d2314045c2a1f4a902b051aa2f172d64b5`.

A golden is a list of files, each `{old_path, new_path, status, binary, hunks}` with `null` for a missing path and each hunk `[old_start, removed, new_start, added]`.

They were written with this command, run in this directory with the archive's `scripts-feasibility` directory at `$S`:

```sh
python3 - "$S" $(ls *.diff | sed 's/\.diff$//') <<'PY'
import json, sys
sys.path.insert(0, sys.argv[1])
import patchlib
for name in sys.argv[2:]:
    text = open(name + ".diff", encoding="utf-8", errors="replace").read()
    files = [{"old_path": f.old_path, "new_path": f.new_path, "status": f.status, "binary": f.binary,
              "hunks": [[h.old_start, len(h.removed), h.new_start, len(h.added)] for h in f.hunks]}
             for f in patchlib.parse_git_diff(text)]
    with open(name + ".json", "w", encoding="utf-8") as out:
        out.write("[\n" + ",\n".join(json.dumps(f, ensure_ascii=False) for f in files) + "\n]\n")
PY
```

Biome then formats them, since the lint job checks every JSON file of the tree with it (`.github/scripts/check-biome-tree.sh`), and the tests read them through `json.Unmarshal`, so the format changes nothing they read.
The command and then this one, run from the repository's root with the Biome that `bun.lock` pins, write each golden again byte for byte:

```sh
node_modules/.bin/biome format --write packages/envoy/internal/dispatch/delivery/survival/testdata/diffs/*.json
```

`ParseDiff` reads every golden as the reference does but one path: `quoted-path.json` holds `b/packages/envoy/internal/dispatch/delivery/survival/testdata/shapes/naïve "name".txt"`, with the prefix and a closing quote, because `patchlib.py` strips the prefix before it unquotes and its unquote drops the TAB git writes after a path holding a space in place of the closing quote.

`ParseDiff` reads `packages/envoy/internal/dispatch/delivery/survival/testdata/shapes/naïve "name".txt`, and the test names that departure (`pathDepartures` in `diff_test.go`).

## The fixtures

Each was captured with `gh api repos/cybermonklabs/legion/commits/<sha> -H 'Accept: application/vnd.github.diff' > <name>.diff` but the last two rows.

The ten shapes from `main` are single-parent first-parent commits of `main@origin` at `f69d69d2f71055c68240ead1ade97d37327060b0`, found with `git -C "$LEGION_WORKSPACE" log --first-parent --max-parents=1 --min-parents=1 f69d69d2f71055c68240ead1ade97d37327060b0` and the filter the table names.

For each shape the search took a recent commit of at most 4 files and 80 changed lines; for the three shapes no such commit has (`rename-exact`, `binary` and `removed-dashdash`), it took one of the commits of that shape with the fewest files.

| File | Commit | Shape | Filter |
| --- | --- | --- | --- |
| `rename-exact.diff` | `8c41b214454eeea889be78354826eff38dfb6850` | a rename with no edit, `similarity index 100%` and no hunk, among 11 files | `--diff-filter=R --find-renames=100%` |
| `rename-edit.diff` | `9033d26f648c8ced69c3dc1d96c2680ac06018bf` | a rename with an edit, `similarity index 96%` | `-M --raw`, a status `R` below `R100` |
| `new-file.diff` | `8279140f6544553e40e1769f0ece50b5649be088` | a new file of 9 lines | `--diff-filter=A` |
| `deleted-file.diff` | `e745ed0c09936d48a8a7bb575b7590c282409995` | two deleted files | `--diff-filter=D` |
| `binary.diff` | `a15b95e665530d0950e1e3153bf0f51915356d98` | ten binary files deleted, `Binary files … differ` | `--numstat`, a file of `-` added and removed |
| `symlink.diff` | `8d309a4ff2eaed8ce4d4897045032a7ab38e4250` | a symbolic link added, `new file mode 120000`, one line with no newline | `--raw`, a mode `120000` |
| `no-newline.diff` | `a8d75ff257e035d3b6b638983172a33eb418cdf0` | `\ No newline at end of file` | the patch holds that line |
| `empty-file.diff` | `94fdf0276743ea20a021873f81b8fddf27169311` | an empty file added, no hunk | `--raw`, an added blob `e69de29` |
| `removed-dashdash.diff` | `2802db4c929bf703e46685535f89f989b319a55f` | three removed SQL comment lines, each written `--- …` inside a hunk | a `--- ` line after a file's first `@@` |
| `split-hunk.diff` | `1040e01dbda994b4b9ca0e9e414ac842c46e6261` | 4 hunk headers whose context lines split them into 8 hunks | a hunk with a change, context, then a change |
| `mode-only.diff` | `d807d22c0fbc6005855569d4d9a1589e31f3b3f1` | `old mode 100755` and `new mode 100644`, no hunk | none: the plan's review counted no first-parent commit of `main` that changes a mode alone; this one is on `legion/INFRA-2709` |
| `quoted-path.diff` | `9e916ad2c31ee107d64929696c3fb8cd6783f538` | a path git quotes, with C escapes and a TAB after it | none: the plan's review counted no quoted path in a first-parent commit of `main` |
| `merge.diff` | `604ff0971f8b7afd4368134ab35cd1c1648775c4` | a merge commit of two parents, 3 files, through the commit route | named by plan task T2 |
| `merge-compare.diff` | `604ff0971f8b7afd4368134ab35cd1c1648775c4` | the same commit through the compare route from its first parent `ec3862972a80940126bc43622a2f3cd313eafb92` | named by plan task T2 |
| `files-page.json` | `1ffc8dec7e789c9cb2374f10a8d69069edff73ed` | page 1 of the commit route's `files[]` of a first-parent commit of 414 files: 100 files, 3 of them with changes and no `patch` | `--shortstat`, over 300 files; of the 18 such commits' first pages read, the smallest that withholds a patch |

`merge-compare.diff` was captured with `gh api repos/cybermonklabs/legion/compare/ec3862972a80940126bc43622a2f3cd313eafb92...604ff0971f8b7afd4368134ab35cd1c1648775c4 -H 'Accept: application/vnd.github.diff' > merge-compare.diff`; both routes answered the same 14,635 bytes.

`files-page.json` was captured with `gh api 'repos/cybermonklabs/legion/commits/1ffc8dec7e789c9cb2374f10a8d69069edff73ed?per_page=100&page=1' > files-page.json`, and Biome formatted it with the goldens.

The quoted path is a scratch commit made for this capture on 2026-11-26 with the git data API and no clone, on the ref `scratch/INFRA-3206-quoted-path`: one tree on `main`'s adding `packages/envoy/internal/dispatch/delivery/survival/testdata/shapes/naïve "name".txt` with two lines, and one commit of that tree whose parent is `main`'s tip then, `22843f79a9d7b72394e6c1f0eca3f8f5c45713ea`.

The ref is deleted at the issue's retro; GitHub keeps serving the commit by its SHA after that, so the capture command still reads it.
