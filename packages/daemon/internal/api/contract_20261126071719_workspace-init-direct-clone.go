package api

// 20261126071719 workspace-init-direct-clone: An issue pod runs one `workspace-init provision` init container. Its command loses `--feed`; its environment gains `LEGION_PROVISION_TOKEN_FILE`, `LEGION_REPOSITORY_CACHE_URL`, `LEGION_REPOSITORY_CACHE_TOKEN_FILE` and `TMPDIR`; `workspace-init fetch` and `LEGION_DEFAULT_BRANCH` go. An image built before this contract refuses the token in provision, and an image built after it has no fetch command, so the image probe must refuse a mismatched pair.
const workspaceInitDirectCloneContract = 20261126071719

func init() {
	registerEntry(contractEntry{id: workspaceInitDirectCloneContract, slug: "workspace-init-direct-clone"})
}
