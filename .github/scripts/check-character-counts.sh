#!/usr/bin/env bash
# No message states a length in characters that its counter does not count as characters.
#
# A character here is one Unicode code point. Dispatch's caps count UTF-16 code units
# (packages/contracts/AGENTS.md), where a character outside the Basic Multilingual Plane counts
# two, and Go's len() counts bytes, where every character outside ASCII counts two to four. A
# refusal that mislabels a title's 1002 UTF-16 code units as a code-point count over a title of
# 501 emoji tells the writer something false. So a message gives its numbers with no unit
# ("title: 1002 of at most 1000"), or names the unit it counts ("UTF-16 code units").
#
# What this check holds: every count beside a unit word on one line has a reviewed counter and
# unit, and a new or edited such line fails CI until it is reviewed. It does not judge a count
# itself; a person decides each one in the ledger, .github/character-counts.ledger.
#
# The net (character_counts_net.py) finds each site: an occurrence of "character", "characters",
# "char" or "chars", in any case and as a whole word, on a line that also holds a count, in every
# file git lists (tracked, and untracked but not ignored). A count is a number ("2,000", "32k",
# "1-40", "UTF-16"), a number in words ("two", "a dozen", "a single"), a format directive ("%d",
# "%[2]d", "%*d", "%(n)d", "%v", "%s"), an interpolation ("{}", "{n:,}", "{s.length}",
# "{{.N}}", "${n}", "${#s}", "$n", "#{n}") or a length call ("len(", ".length", "RuneCount",
# "wc"). The net reads each line as written, comments and all, and the same line with its escapes
# decoded (\uXXXX, \u{X}, \UXXXXXXXX, \xXX, octal, \n, \t, \r, and &#NN; and &#xNN;), so an
# escaped word is the word. It parses no language and reads no grammar: no comment, quotation,
# qualifier, locale or counter makes an occurrence a site or keeps it from being one.
#
# The ledger holds one row per site, tab-separated: the path, the SHA-256 of the line's exact
# bytes, the occurrence's ordinal on the line, which copy of that line in the file it is (1 for
# the first from the top), the counter that computes the count (`<path>:<symbol>`, or `none` where
# no code counts it), the unit that counter counts (code points, bytes, UTF-16 code units, or not a
# length, where the word is no unit of a count), and one sentence of reason. A line that moves
# inside its file keeps its row; an edited line, a line that went and a file that went leave
# their rows with no site. The check fails on:
#   - a site with no row;
#   - a row with no site (the line changed, moved past a copy of itself, or went);
#   - two rows for one site;
#   - a row whose unit is bytes or UTF-16 code units: its line calls that count characters, so the
#     message names the unit its counter counts instead;
#   - a row that breaks the schema, or names a counter whose file the tree does not hold or whose
#     file does not hold the symbol.
# Write each row by hand from the counter you read: --list prints each site with its decision,
# and the key fields of each row a site needs.
#
# The check leaves a file out only for a reason of LEFT_OUT (character_counts_tree.py): the
# checker's own planted cases, the ledger (held to its schema and to the sites instead), dated
# records (any CHANGELOG.md, docs/plans, docs/superpowers, docs/research, docs/solutions) and
# fixtures whose bytes are the subject; a binary file (one holding a NUL byte); a symbolic link;
# or a listed file that is not on disk. It prints how many files it read and how many it left out,
# by reason. It also fails, printing UNKNOWN, when it read no file or found no site outside the
# checker's own files, since a scan that saw nothing has not passed.
#
# Its controls run first on every run, through the same code as the walk
# (character_counts_controls.py), and a control that answers otherwise fails the check: every
# planted shape of the gates of this check's first three rounds (character_counts_shapes.py) is a
# site that fails with no row and answers by its row; each ledger rule; each class of file it
# leaves out and each class it must read; UNKNOWN; and the listing of a real git repository.
#
# What the net cannot see: a unit word on a line with no count, where the count is on another line
# or the text names a cap in words ("too many characters", "the character cap", a table header
# over its numbers, a word held in a variable, JSX that a formatter wraps); a word assembled from
# fragments ("char" + "acters"); a word inside a name ("maxChars", "MAX_CHARS"); an escape it does
# not decode (percent-encoding, a named HTML entity); a count written in words outside its list
# ("a", "no", "several"); other spellings and languages; a text file in UTF-16, which holds NUL
# bytes and is left out as a binary; and a wrong decision in a row, which a reviewer reads.
#
# Run from anywhere: .github/scripts/check-character-counts.sh [--list]
# CI runs it in ci.yaml's lint job.
#
# The program is six Python files beside this one: character_counts_net.py finds the sites,
# character_counts_ledger.py reads the ledger and holds the sites to it, character_counts_tree.py
# walks the files git lists and names each class it leaves out, character_counts_controls.py and
# character_counts_shapes.py hold the controls and their planted shapes, and character_counts.py
# runs the controls and then the walk.
set -euo pipefail

cd "$(dirname "$0")/../.."

exec python3 -B .github/scripts/character_counts.py "$@"
