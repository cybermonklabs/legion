#!/usr/bin/env bash
# No message states a length in characters that its counter does not count as characters.
#
# A character here is one Unicode code point. Dispatch's caps count UTF-16 code units
# (packages/contracts/AGENTS.md), where a character outside the Basic Multilingual Plane counts
# two, and Go's len() counts bytes, where every character outside ASCII counts two to four. A
# refusal that says "a title is at most 1000 characters, and this one is 1002" over a title of
# 501 emoji tells the writer something false. So a message gives its numbers with no unit
# ("title: 1002 of at most 1000"), or names the unit it counts ("UTF-16 code units").
#
# The check reads every file git lists (tracked, and untracked but not ignored). It leaves a file
# out only for a reason of LEFT_OUT below, a binary file (one holding a NUL byte), or a listed
# file that is not on disk, and it prints how many files it read and how many it left out, by
# reason. It reads a file as the program that shows its text builds it:
#
#   Go, TypeScript and JavaScript, Python and shell: each string literal outside comments, with
#     the literals and values a `+` joins to it on any line read as one message (and Python's
#     adjacent literals), each interpolation (`${n}`, an f-string's `{n}`, shell's `$n`) read as
#     its value, JSX text in `.tsx` and `.jsx`, unquoted shell words, and each shell
#     here-document, whose body is read in the language of the program it feeds.
#   Every other text file (markdown, YAML, JSON, plain text and the rest): its whole text outside
#     its comments, as prose a person or a model reads.
#
# A count beside the word is a hit: "%d characters", "2,000 characters", "%d-character",
# "${n} chars", "at character %d", "chars=%d", and a constant's name written as the count
# ("SEARCH_QUERY_MAX characters"). "char" and "chars" are the same word. A hit passes when one
# of these holds:
#
#   names its unit   the 40 characters after the word, inside its sentence, name UTF-16 or code
#                    units or points: "32,000 characters (UTF-16 units)" tells the truth.
#   ASCII grammar    the sentence that holds the count names a grammar that admits ASCII alone
#                    (hex, a-z, base64, uuid, SHA, DNS, ASCII, "lowercase letters, digits",
#                    "40-character commit"), where bytes, code points and UTF-16 units are one
#                    number.
#   exempt           a comment on the line or the two lines above the message reads
#                    `characters: code points, <reason>` or `characters: ASCII alone, <reason>`:
#                    the counter is where the check cannot follow it, and the comment says where.
#                    A marker with no reason passes nothing.
#   code points      the count's own value is counted with utf8.RuneCount, a []rune,
#                    [...s].length or Array.from(s).length, or shell's ${#s} or `wc -m` in a
#                    script that sets a UTF-8 locale itself, and with no other counter. The check
#                    follows the value, not the code around it: a format verb's own argument, an
#                    interpolation's expression, a number's bound in the `if` or `case` the message
#                    answers, and each name in them to its assignment in the same file.
#
# Every other hit is an instance, and the check fails naming each one with its reason: the
# counter it found (len() counts bytes, .length and Units16 count UTF-16 code units), or that it
# cannot judge the count, since it cannot follow it to a counter (a value from a function or a
# file it does not follow, a bound with no counter beside it, a count in prose). It also fails,
# printing UNKNOWN, when it read no file or found no hit, since a scan that saw nothing has not
# passed.
#
# Its controls run first on every run, through the same code as the scan (CONTROLS below), and a
# control that answers otherwise fails the check: each shape the scan must fire on and each it
# must pass, one per entry of OTHER_COUNTERS, the files it reads and leaves out, and UNKNOWN.
#
# Run from anywhere: .github/scripts/check-character-counts.sh [--list]
# --list prints every hit with its verdict. CI runs it in ci.yaml's lint job.
#
# The program is five Python files beside this one: character_counts_lexer.py reads each file's
# literals and comments and builds its messages, character_counts_analysis.py judges each count
# by its counter, character_counts_tree.py walks the files git lists and names each class it
# leaves out, character_counts_controls.py holds the controls, and character_counts.py runs the
# controls and then the walk.
set -euo pipefail

cd "$(dirname "$0")/../.."

exec python3 -B .github/scripts/character_counts.py "$@"
