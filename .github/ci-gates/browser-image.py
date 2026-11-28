# The browser_image filter, kept by hand: gates browser-image.yaml's image build; .github/scripts/check-image-trigger-paths.sh proves it covers every file the build reads.
EXCLUDED = excluded('browser_image', "gates browser-image.yaml's image build; .github/scripts/check-image-trigger-paths.sh proves it covers every file the build reads", [
    '.github/workflows/ci.yaml',
    '.github/ci-gates/browser-image.py',
    '.github/workflows/browser-image.yaml',
    '.github/docker/**',
    '.github/scripts/ci_images.py',
    # The build job's `Decide whether to publish` and `Tag and record the image this run leaves`
    # steps source it and read the image's config and manifests through retry_fetch, which the
    # image build itself never reads, so check-image-trigger-paths.sh's proof does not cover it.
    '.github/scripts/ci-retry.sh',
    # ci-retry.sh sources it from its own directory: retry_fetch runs each of those reads'
    # attempts inside its stop-commands block.
    '.github/scripts/ci-log-guard.sh',
])
