// Controller-liveprobe is controller-start-tmux.sh's one reading of the daemon's controller
// liveness probe: controller.Prober, the daemon's own, asked about each session it is given
// against a live Envoy listener. It belongs to no module: the script runs it with `go run
// -overlay` as packages/daemon/cmd/liveprobe-accept/main.go inside the checkout's packages/daemon,
// the one place the daemon's internal packages can be imported from.
//
//	controller-liveprobe <envoy url> <envoy token file> <project> <session>...
//
// prints one `<session>=<verdict>` line for each session, in the order given.
package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/controller"
)

func main() {
	token, _ := os.ReadFile(os.Args[2])
	p := controller.NewProber(controller.ProberOptions{EnvoyURL: os.Args[1], EnvoyToken: strings.TrimSpace(string(token)), Project: os.Args[3]})
	for _, session := range os.Args[4:] {
		fmt.Printf("%s=%s\n", session, p.Probe(context.Background(), session))
	}
}
