# mas-lab `library-standard/`

Subject: the directory `library-standard/` of the repository `mas-lab`.

Clone commit: `477441ddb82d1b07c76c36f1426d8a5be0b6194b`.

Our tree: `/legion/workspaces/cybermonklabs/legion/infra-3134` at commit `9542c6fbbecd184ccee73523ab3defa39d6cf23a`.

Requirement: r3.

## What it is

`library-standard` is one Python distribution, `mas-lab-library-standard` version 0.2.0, built with hatchling [m-std-package].

Its `pyproject.toml` description is `Standard library for MAS — infra bundles, tools, and pattern aliases.` [m-std-desc].

The plan's measurement of that description is correct, word for word [m-std-desc].

The package is a plugin set for the mas runtime, not a specification [m-std-library-yaml].

## What it holds

`library.yaml` is the plugin registry of the package [m-std-library-yaml].

It declares 23 plugin types and 56 plugin entries, each entry a module path and a class name [m-std-library-yaml] [m-std-plugin-count].

The source tree holds 125 Python files and 21 YAML files [m-std-python-count].

The Python files are the plugin bodies: design patterns, governance, context managers, memory, observability, control protocol, agent communication, and infra middleware [m-std-plugin-count].

The YAML files are manifest instances, not formats: a Control [m-std-control-file], an Overlay [m-std-overlay-file], and 12 infra bundles of kind `LLMProxy` under `libs/standard` [m-std-infra-bundle].

The source tree also holds 18 markdown plugin cards next to the implementations [m-std-cards].

The directory holds exactly one `SKILL.md` file, and that file sits inside a governance example [m-std-skill-file].

## How a reader uses it

A reader installs the package, then names a packaged Control on the `mas-ctl` command line with a `pkg://` reference [m-std-use].

The package has one public entry point: the entry-point group `mas.runtime.manifest_libraries`, name `standard`, value `mas.library.standard` [m-std-entrypoint].

The module `mas.library.standard` exports one function, `package_root`, which finds the directory that holds `library.yaml` [m-std-init].

The runtime reads that entry-point group to find the installed library root [m-std-runtime-loads].

So `ctl` is the command surface and `runtime` is the loader; the library itself is the content [m-std-ctl-validates] [m-std-runtime-loads].

## Dependencies

The package needs Python 3.11 or later [m-std-deps].

It depends on `mas-lab-runtime`, `ddgs>=9.14.0` and `httpx>=0.27` [m-std-deps].

`mas-lab-runtime` is a uv workspace member, so the package does not install outside the mas-lab repo without that sibling [m-std-deps].

## Is any part of it a specification format

No part of it is [m-std-no-schema].

No file under `library-standard/` matches `*.schema.yaml` or `*.schema.json` [m-std-no-schema].

The manifest schemas of the stack live in `docs/schemas` of mas-lab: `agent.schema.yaml`, `control.schema.yaml`, `overlay.schema.yaml` and `library.schema.yaml` [m-std-schema-elsewhere].

The schema loader that reads them lives in `ctl` [m-std-ctl-validates].

The issue's specification assigned the specification format to this directory.

That assignment is wrong: the format is in `docs/schemas`, and `library-standard` only holds instances of it [m-std-no-schema] [m-std-schema-elsewhere].

The plan's second measurement, that the format is used by `ctl` and by `runtime`, holds for the schema loader in `ctl` and the library loader in `runtime` [m-std-ctl-validates] [m-std-runtime-loads].

## What could cross into our runtime

Only one artifact class in this directory crosses unchanged: the prompt-shaped skill [m-std-cross-yaml].

That is the single `SKILL.md` file [m-std-skill-file].

Our skills use the same shape, a markdown file with `name` and `description` front matter at `skills/<name>/SKILL.md` [m-std-cross-skill].

The value of that crossing is near zero, because the one file exists to make a model name a tool that is not in the tools list, as an example of a governance block [m-std-skill-file].

Nothing here is a specification document, a vocabulary or ontology, or a schema [m-std-cross-yaml].

Every other part is a Python plugin bound to the mas runtime plugin interface, so it is a service beside Legion or a rebuild in Go or TypeScript [m-std-cross-python].

## Doubt found on reading

The user guide names shipped tools, such as `WebSearchTool`, that the source does not define [m-std-tools-empty].

The name appears only in the guide and in a test fixture [m-std-tools-empty].

The project's own prose says the directory holds "plugins, tools, skills, prompts, flavours, overlays and Controls" [m-std-readme-purpose].

That is a claim of the project, not of its code; the code supports plugins, flavours, overlays and Controls, but not the tools and not more than one skill [m-std-readme-purpose] [m-std-tools-empty] [m-std-skill-file].
