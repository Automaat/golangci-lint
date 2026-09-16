package cache

import (
	"log"

	"github.com/golangci/golangci-lint/v2/internal/processexit"
)

func fatalf(format string, args ...any) {
	log.Printf(format, args...)
	processexit.Exit(1)
}
