# The ci-images job: the image copy, the package mode of the image gate, which reads
# ghcr.io, and the tool copy, which fills the second source of every tool install. These are
# the files that decide what those three steps do. The image gate's tree mode is a step of
# lint, which runs on every change, and no filter gates it.
GATE = gate("ci_images", "ci-images", reads=reads(
    (".github/ci-images.yaml", f"{at('ci-images', COPY_STEP)} copies the rows it holds, and "
                               f"{at('ci-images', PACKAGE_STEP)} reads each row's digest"),
    (".github/scripts/mirror-ci-images.sh", f"{at('ci-images', COPY_STEP)} runs it"),
    (".github/scripts/check-ci-images.sh", f"{at('ci-images', PACKAGE_STEP)} runs it"),
    (".github/scripts/ci_images.py", "every reader of a row goes through it, the recipe hash "
                                     "rule 10 compares among them"),
    (".github/docker/**", f"{at('ci-images', PACKAGE_STEP)} hashes every file of the build "
                          f"context, which is this directory, and refuses the built row whose "
                          f"published image carries another recipe's sha256 in its "
                          f"legion.browser-image.source-sha256 label"),
    (".github/workflows/browser-image.yaml", f"the other half of that recipe: "
                                             f"{at('ci-images', PACKAGE_STEP)} hashes this file "
                                             f"too, since its labels, outputs, platforms and "
                                             f"action pins decide the bytes the build pushes"),
    (".github/ci-tools.yaml", f"{at('ci-images', TOOL_COPY_STEP)} fills the copy of every ghcr-mirror row it holds"),
    (".github/scripts/mirror-ci-tools.sh", f"{at('ci-images', TOOL_COPY_STEP)} runs it"),
    (".github/scripts/ci-tools.sh", f"{at('ci-images', TOOL_COPY_STEP)}: the filler reads a row's fields and "
                                    "its seams the way that helper does, and the copy it fills is what the "
                                    "helper then reads"),
    (".github/actions/install-crane/**", at("ci-images", "./.github/actions/install-crane")),
))
