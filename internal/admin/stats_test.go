package admin

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestPercentChange(t *testing.T) {
	tests := []struct {
		cur, prev int64
		trend     float64
		trendType string
	}{
		{0, 0, 0, "neutral"},
		{5, 0, 100, "up"},
		{0, 5, -100, "down"},
		{150, 100, 50, "up"},
		{100, 150, -33.3, "down"},
		{100, 100, 0, "neutral"},
		{1, 3, -66.7, "down"},
	}
	for _, tt := range tests {
		trend, typ := PercentChange(tt.cur, tt.prev)
		assert.Equal(t, tt.trend, trend, "%d vs %d", tt.cur, tt.prev)
		assert.Equal(t, tt.trendType, typ)
	}
}
