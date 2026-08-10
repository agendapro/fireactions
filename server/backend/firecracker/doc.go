// Package firecracker implements backend.Backend on top of Firecracker
// microVMs and containerd. Every implementation file carries the "_linux"
// filename suffix, since Firecracker only runs on linux; this file carries
// no build tag so the package is never empty on other platforms, which
// keeps "go build ./server/backend/..." and "go vet ./server/backend/..."
// usable from a darwin development machine.
package firecracker
