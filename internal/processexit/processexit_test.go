package processexit

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSet(t *testing.T) {
	var codes []int
	restoreOuter := Set(func(code int) { codes = append(codes, code) })
	restoreInner := Set(func(code int) { codes = append(codes, code*10) })

	Exit(2)
	restoreInner()
	Exit(3)
	restoreOuter()

	assert.Equal(t, []int{20, 3}, codes)
}
