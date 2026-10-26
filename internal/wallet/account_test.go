package wallet

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGenerateAccountNumber(t *testing.T) {
	seen := map[string]bool{}
	for range 2000 {
		n, err := GenerateAccountNumber()
		require.NoError(t, err)
		require.Len(t, n, 10)
		assert.NotEqual(t, byte('0'), n[0], "0xxxxxxxxx is reserved for system accounts")
		assert.True(t, isAccountNumber(n))
		seen[n] = true
	}
	assert.Greater(t, len(seen), 1990, "numbers are random, not sequential")
}

func TestDisplayName(t *testing.T) {
	name := func(s string) *string { return &s }
	assert.Equal(t, "Ada Okafor", DisplayName("ada@irate.dev", name("  Ada Okafor ")))
	assert.Equal(t, "Ada", DisplayName("ada@irate.dev", nil))
	assert.Equal(t, "Ada", DisplayName("ada+savings@irate.dev", nil), "+tags are dropped")
	assert.Equal(t, "Ada", DisplayName("ada@irate.dev", name("   ")), "blank verified name falls back")
	assert.Equal(t, "Émile", DisplayName("émile@x.fr", nil), "unicode-aware capitalisation")
	assert.Equal(t, "iRate User", DisplayName("@x.dev", nil))
}

func TestIsAccountNumber(t *testing.T) {
	assert.True(t, isAccountNumber("1000000001"))
	assert.False(t, isAccountNumber("100000000"))
	assert.False(t, isAccountNumber("10000000011"))
	assert.False(t, isAccountNumber("10000000a1"))
	assert.False(t, isAccountNumber("１０００００００01"), "full-width digits")
}
