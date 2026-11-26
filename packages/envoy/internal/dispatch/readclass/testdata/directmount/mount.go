// Package directmount is TestEveryMountedPatternHasAReadClass's fixture: one direct mount on a
// ServeMux, which the walk must name, and a Handle method of a type that is no ServeMux, which it
// must leave alone.
package directmount

import "net/http"

func mount(mux *http.ServeMux) {
	mux.HandleFunc("GET /fixture", func(http.ResponseWriter, *http.Request) {})
}

type hub struct{}

func (hub) Handle(channel string, notified func(string)) {}

func listen(h hub) {
	h.Handle("fixture_channel", func(string) {})
}
