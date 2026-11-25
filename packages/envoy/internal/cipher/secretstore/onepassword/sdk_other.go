//go:build !(darwin || linux || windows)

// Where the 1Password SDK does not build with CGO_ENABLED=0 (sdk.go says why), this file stands
// in for it: it imports no SDK package, and agent-secrets runs op there.

package onepassword

import (
	"context"
	"fmt"
	"runtime"
)

// sdkSupported is whether NewSDKClient can start the SDK in this build.
const sdkSupported = false

// NewSDKClient refuses: this build holds no 1Password SDK.
func NewSDKClient(context.Context, string, string) (Client, error) {
	return nil, fmt.Errorf("the 1Password SDK does not build for %s; agent-secrets uses op there", runtime.GOOS)
}
