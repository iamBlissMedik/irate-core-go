package idempotency

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/iamBlissMedik/irate-core-go/internal/apperr"
)

func TestValidateKey(t *testing.T) {
	assert.NoError(t, ValidateKey("3f1c2b9e-8a4d-4e1f-9b7a-2c6d8e0f1a2b"))
	assert.NoError(t, ValidateKey("idemp-1730000000-42"))

	err := ValidateKey("")
	ae, _ := apperr.As(err)
	assert.Equal(t, apperr.CodeIdempotencyRequired, ae.Code)

	assert.Error(t, ValidateKey(strings.Repeat("k", 256)))
	assert.Error(t, ValidateKey("has space"))
	assert.Error(t, ValidateKey("tab\tkey"))
	assert.Error(t, ValidateKey("ключ"), "non-ASCII")
}

func TestHashRequest(t *testing.T) {
	a := HashRequest("1000000002", "250000", "rent")
	assert.Equal(t, a, HashRequest("1000000002", "250000", "rent"), "deterministic")
	assert.NotEqual(t, a, HashRequest("1000000002", "250001", "rent"))
	assert.NotEqual(t, HashRequest("ab", "c"), HashRequest("a", "bc"), "field boundaries matter")
	assert.Len(t, a, 64)
}
