package processexit

import (
	"os"
	"sync/atomic"
)

type exitFunc func(int)

type handler struct {
	exit exitFunc
}

var current atomic.Pointer[handler]

// Exit terminates the process through the active handler.
func Exit(code int) {
	value := current.Load()
	if value == nil {
		os.Exit(code)
	}
	value.exit(code)
}

// Set replaces the process exit handler and returns a restore function.
func Set(exit func(int)) func() {
	if exit == nil {
		panic("process exit function is nil")
	}

	previous := current.Swap(&handler{exit: exitFunc(exit)})

	return func() {
		current.Store(previous)
	}
}
