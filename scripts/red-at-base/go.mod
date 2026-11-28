// The two Go programs scripts/red-at-base.sh builds, each from a copy in its own directory: its
// header says what each does. No Go workspace of the repository lists this module, so a go command
// run here takes GOWORK=off, as `GOWORK=off go vet ./...`.
module redatbase

go 1.21
