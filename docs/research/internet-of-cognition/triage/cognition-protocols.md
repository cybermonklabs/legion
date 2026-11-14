# Triage: ioc-project/cognition-protocols

Repository: github.com/ioc-project/cognition-protocols.

Clone commit: e3c7bf10f676de37ac08d2fe97e7de982f74629a, of 2026-11-11.

Clone path in this pod: $IOC_CLONES/cognition-protocols, where IOC_CLONES is /legion/scratch/tmp/ioc-clones.

This is a triage.
It does not run their code [t-triage-depth].

## What the repository is

It is an umbrella repository for two protocols, ACCORD and CONCORD [t-cp-identity].

ACCORD is for shared intent.
CONCORD is for a decision under conflict.

Each protocol ships as prompt-shaped skills, a Python package, metric code and documents [t-cp-cross-skills] [t-cp-drop-packages] [t-cp-drop-metrics].

## Maturity

The clone is shallow, at depth one, so `git log` over it returns one commit [t-cp-clone-shallow] and `git tag` over it returns nothing [t-cp-tag-local].

Maturity is read over the GitHub REST API instead.

The repository holds nine tags, and every version in them is below 1.0.0 [t-cp-tags-api].

It has eight published releases, and each is for one skill or one package [t-cp-releases].

The first release is of 2026-10-22, which is 20 days before the pinned commit [t-cp-releases].

The default branch holds at least 85 commits, and the repository was made on 2026-10-12 [t-cp-commits].

It is one month old [t-all-age].

The public following is small: 30 stars [t-org-stars].

## Licence

The licence is the Apache License 2.0 [t-cp-licence].

The file is named LICENSE.txt, not LICENSE [t-cp-licence].

A tool that looks for a file named LICENSE finds nothing [t-cp-licence].

Each skill directory carries its own copy of the licence [t-cp-licence].

## Size

The repository tracks 216 files and 56144 lines [t-cp-size].

It is the smallest of the three cloned repositories [t-cp-size].

Four lock files hold a large share of the lines [t-cp-size].

## Version range measured

protocols/accord/skills/accord-receiver/SKILL.md declares version 0.1.0 [t-cp-version-accord-receiver].

This agrees with the version the plan cites.

The four skills are not at one version: accord-receiver is 0.1.0, accord-initiator is 0.1.1, and both concord skills are 0.2.0 [t-cp-version-range].

The Python packages are all at 0.1.0 [t-cp-version-range].

The measured range for the skills is 0.1.0 to 0.2.0 [t-cp-version-range].

## What could cross into our runtime

Two classes.

Prompt-shaped skill: the four SKILL.md files, each with YAML front matter that holds a name, a description, a licence and metadata [t-cp-cross-skills].

Specification document: protocols/concord/spec/CONCORD-SPEC.md, 608 lines, at version 0.1.0 [t-cp-cross-spec].

There is no third class here.
ACCORD has no specification document, so a reader must take the prompt as the specification [t-cp-no-accord-spec].

There is no machine-readable schema of the wire format [t-cp-no-wire-schema].

The only file named schema is Python code [t-cp-no-wire-schema].

## What is dropped, and why

packages/ioc-concord, 22 files: the Python engine, state, store and wire format of CONCORD.
It would be a rebuild in Go or TypeScript [t-cp-drop-packages].

The two mce-metrics packages, 41 files: Python metric code that reads OpenTelemetry spans.
It is a service beside Legion, and it needs a trace store this issue does not run [t-cp-drop-metrics].

The scripts beside three of the four skills: Python helpers the skill calls [t-cp-drop-scripts].

A skill that crosses without its script loses the behaviour the script carries.
The deep read must size that gap [t-cp-drop-scripts].

docs/, tools/, .claude-plugin/ and the CONCORD test harness, 110 files: a documentation site, a bundler, a plugin manifest and test transcripts.
The plugin manifest is not one of the four crossing classes [t-cp-drop-rest].

## One fact that limits every import

The repository holds no Go source file [t-all-no-go-ts-runtime].
