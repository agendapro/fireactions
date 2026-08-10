package server

import (
	"time"

	"github.com/hostinger/fireactions/server/backend"
)

// Machine holds a backend VM handle plus the pool bookkeeping the server
// tracks alongside it.
type Machine struct {
	backend.Machine

	Name      string
	RunnerID  int64
	Pool      string
	CreatedAt time.Time
}
