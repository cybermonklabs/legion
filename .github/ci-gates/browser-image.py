# The browser_image filter, kept by hand: gates browser-image.yaml's image build; .github/scripts/check-image-trigger-paths.sh proves it covers every file the build reads.
EXCLUDED = excluded('browser_image', "gates browser-image.yaml's image build; .github/scripts/check-image-trigger-paths.sh proves it covers every file the build reads", [
    '.github/workflows/ci.yaml',
    '.github/ci-gates/browser-image.py',
    '.github/workflows/browser-image.yaml',
    '.github/docker/**',
    '.github/scripts/ci_images.py',
])
