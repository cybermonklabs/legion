package plan

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// TaskSlug is the name a task's id gives its branch and its directory (claim.TaskBranch): the id
// itself when it has a slug's shape (claim.IsTaskSlug), else `t-` and the first 12 lower-case
// hexadecimal characters of the SHA-256 of the id. A block id can be a UUID, and FromBlocks holds an
// id to no grammar, so an id with a capital letter, a slash, two dots or more than 40 characters
// would make no valid branch or no stable directory; its hash makes both, and two ids give one
// slug only by a collision of the hash.
func TaskSlug(id string) string {
	if claim.IsTaskSlug(id) {
		return id
	}
	sum := sha256.Sum256([]byte(id))
	return "t-" + hex.EncodeToString(sum[:6])
}
