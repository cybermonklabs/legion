package workflow

import "fmt"

// offHeadWaitReason is why the testing move waits where the tester's completion named head, at
// which Legion holds no set of check states, and the open pull request stands at prHead, another
// commit: Dispatch computes states only at a pull request's heads and former heads, so no state
// comes at a commit that is neither, such as one of a step's own branch beside a fix, and the
// tester is to record its results at the pull request's head and complete again.
func offHeadWaitReason(head, prHead string) string {
	return fmt.Sprintf("the move waits for Dispatch's states at %s, which is not the head of the pull request (%s) and at which Legion holds no state. "+
		"Dispatch computes states only at a pull request's heads and former heads, so where %s is neither, none comes: "+
		"the tester records its results at the pull request's head and completes again", head, prHead, head)
}
