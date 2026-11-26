package api

import "testing"

// PLANTED, never merged: a failing test no group of the API legs names. go_legs.py places it
// in one leg by its name; that leg, envoy-go-race-api / envoy-go-race-api and ci-ok must read red.
func TestAPlantedFailureInOneAPIGroup(t *testing.T) { t.Fatal("planted failure") }
