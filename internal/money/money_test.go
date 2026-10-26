package money

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMinorUnmarshalJSON(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    Minor
		wantErr bool
	}{
		{"json integer", `150000`, 150000, false},
		{"integer string", `"150000"`, 150000, false},
		{"zero", `0`, 0, false},
		{"negative parses (validation rejects later)", `-5`, -5, false},
		{"null leaves zero", `null`, 0, false},
		{"max int64", `9223372036854775807`, math.MaxInt64, false},
		{"decimal", `1500.50`, 0, true},
		{"decimal string", `"1500.50"`, 0, true},
		{"exponent", `1.5e5`, 0, true},
		{"explicit plus", `"+5"`, 0, true},
		{"leading space", `" 5"`, 0, true},
		{"hex", `"0x10"`, 0, true},
		{"empty string", `""`, 0, true},
		{"overflow", `9223372036854775808`, 0, true},
		{"bool", `true`, 0, true},
		{"bare minus", `"-"`, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var m Minor
			err := json.Unmarshal([]byte(tt.in), &m)
			if tt.wantErr {
				require.Error(t, err)
				var typeErr *json.UnmarshalTypeError
				assert.ErrorAs(t, err, &typeErr, "errors must be UnmarshalTypeError so the field name is attached")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, m)
		})
	}
}

func TestMinorUnmarshalReportsFieldName(t *testing.T) {
	var body struct {
		Amount Minor `json:"amount"`
	}
	err := json.Unmarshal([]byte(`{"amount": 10.5}`), &body)
	var typeErr *json.UnmarshalTypeError
	require.ErrorAs(t, err, &typeErr)
	assert.Equal(t, "amount", typeErr.Field)
}

func TestMinorMarshalJSONIsString(t *testing.T) {
	b, err := json.Marshal(struct {
		Balance Minor `json:"balance"`
	}{Balance: 9007199254740993}) // > 2^53: would lose precision as a JS number
	require.NoError(t, err)
	assert.JSONEq(t, `{"balance":"9007199254740993"}`, string(b))
}

func TestFormat(t *testing.T) {
	tests := []struct {
		in   Minor
		want string
	}{
		{0, "NGN 0.00"},
		{5, "NGN 0.05"},
		{100, "NGN 1.00"},
		{5_000_000, "NGN 50,000.00"},
		{123_456_789, "NGN 1,234,567.89"},
		{-250_000, "NGN -2,500.00"},
		{math.MinInt64, "NGN -92,233,720,368,547,758.08"},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, Format(tt.in, "NGN"))
	}
}

func TestParse(t *testing.T) {
	m, ok := Parse("42")
	assert.True(t, ok)
	assert.Equal(t, Minor(42), m)
	_, ok = Parse("4.2")
	assert.False(t, ok)
}
