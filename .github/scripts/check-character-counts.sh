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
# out only for a reason of LEFT_OUT (character_counts_tree.py), a binary file (one holding a NUL
# byte), or a listed file that is not on disk, and it prints how many files it read and how many
# it left out, by reason. It reads a file as the program that shows its text builds it, and
# writes each escape as the character it stands for ("\u0063haracters" is the word):
#
#   Go, TypeScript and JavaScript, Python and shell: each string literal outside comments, with
#     the literals and values a `+` joins to it on any line read as one message (and Python's
#     adjacent literals), each interpolation (`${n}`, an f-string's `{n}`, shell's `$n`) and each
#     field of Python's str.format or a % mapping read as its value, a %s whose argument is the
#     word read as the word, the separate arguments of a call that writes them as one text
#     (fmt.Println, print, console.log, an array's join), each shell command's words, JSX text in
#     `.tsx` and `.jsx`, and each shell here-document, whose body is read in the language of the
#     program it feeds.
#   Every other text file (markdown, YAML, JSON, plain text and the rest): its whole text outside
#     its comments, as prose a person or a model reads.
#
# The word is "character", "char" and their plurals, in any case. A count beside the word is a
# hit, wherever it stands in the sentence: "%d characters", "2,000 characters", "%d-character",
# "32k", "1-40", "two", "over-40-character", `2000` and **2,000**, with up to three words
# between the count and the word ("%d Unicode characters", "five or more characters"); "at
# character %d", "chars=%d", "the character limit of 200", "characters used: %d" and "its length
# in characters is %d"; a constant's name written as the count ("SEARCH_QUERY_MAX characters");
# and a cap the word names with no number ("the character cap", "the characters left for it").
# Each count is judged by its counter, wherever it stands, and a hit passes when one of these
# holds:
#
#   code points      the count's own value is counted with utf8.RuneCount, a []rune,
#                    [...s].length or Array.from(s).length, or shell's ${#s} or `wc -m` in a
#                    script that sets a UTF-8 LC_ALL itself before the count, and with no other
#                    counter. The check follows the value, not the code around it: a format verb's
#                    own argument, an interpolation's expression, a bound's counter in the `if` or
#                    `case` the message answers or in each comparison of the constant it names, and
#                    each name in them to every assignment of it that can reach the count.
#   names its unit   the hit names a unit, between the count and the word or in the 40
#                    characters after the word inside its sentence ("32,000 characters (UTF-16
#                    units)", "256 Unicode characters"), and the counter counts that unit. In
#                    prose, which names no counter, the unit it names is taken as written.
#   ASCII grammar    the clause that holds the count names a grammar that admits ASCII alone
#                    (hex, a-z, base32, base64, uuid, SHA, DNS, ASCII, "lowercase letters,
#                    digits", "40-character commit"), where bytes, code points and UTF-16 units
#                    are one number. A grammar word in another clause of the sentence passes
#                    nothing; a count inside a parenthesis is of the clause before it.
#   not empty        "at least one character" and "one or more characters", which every unit
#                    agrees on.
#   exempt           a comment on the line or the two lines above the message, with no other
#                    message between, reads `characters: ASCII alone, <reason>` (a text of ASCII
#                    alone is one length in every unit, so it passes any counter), or, where the
#                    check cannot follow the counter, `characters: code points, <reason>` or, for a
#                    message that names UTF-16 code units, `characters: UTF-16 code units,
#                    <reason>`; the reason is at least 10 characters and says where the counter is.
#
# Every other hit is an instance, and the check fails naming each one with its reason: the
# counter it found (len() counts bytes, .length and Units16 count UTF-16 code units), the unit it
# names that its counter does not count, or that it cannot judge the count, since it cannot
# follow it to a counter (a value from a function or a file it does not follow, a bound with no
# counter beside it, a count in prose). It also fails, printing UNKNOWN, when it read no file or
# found no hit, since a scan that saw nothing has not passed.
#
# Its controls run first on every run, through the same code as the scan
# (character_counts_controls.py), and a control that answers otherwise fails the check: each
# shape the scan must fire on and each it must pass, one per entry of OTHER_COUNTERS, one per rule
# of LEFT_OUT with the paths the check must read, and UNKNOWN.
#
# What it cannot see: a comment, which it does not read; a count written as a word other than a
# number ("a single character", "a dozen"); a count more than three words from the word, or after
# it in another form than those above; the word a helper writes from its own body (`plural(n)`,
# with no word in the call); in prose, a stated unit's truth, since prose names no counter. A
# value the check cannot follow (another file's, a function's) fails as one it cannot judge.
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
