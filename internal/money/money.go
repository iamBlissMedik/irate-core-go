// Package money defines the representation of monetary amounts.
//
// All amounts are integer MINOR units (kobo for NGN: 1 NGN = 100 kobo), never
// floats, so every operation is exact. Amounts are int64 in Go and BIGINT in
// Postgres.
//
// On the wire, amounts are JSON STRINGS ("150000"), because JavaScript numbers
// lose precision above 2^53 and money must survive a round-trip through any
// client. Requests may send either a JSON integer (150000) or an integer
// string ("150000"); decimals, exponents and other forms are rejected.
package money

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
)

// Minor is an amount in minor units.
type Minor int64

// maxDigits bounds parsing work; int64 has at most 19 digits.
const maxDigits = 19

func (m Minor) Int64() int64 { return int64(m) }

func (m Minor) String() string { return strconv.FormatInt(int64(m), 10) }

// MarshalJSON renders the amount as a decimal string, e.g. "150000".
func (m Minor) MarshalJSON() ([]byte, error) {
	return []byte(`"` + m.String() + `"`), nil
}

// UnmarshalJSON accepts 150000 or "150000". It deliberately rejects 1500.5,
// 1.5e5, "+5", " 5", hex and anything that does not fit in an int64.
//
// Failures are returned as *json.UnmarshalTypeError so encoding/json attaches
// the offending field name, which the HTTP layer turns into a field error.
func (m *Minor) UnmarshalJSON(b []byte) error {
	s := string(b)
	if s == "null" {
		return nil // left as zero; "required"/"gt=0" validation reports it
	}
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		s = s[1 : len(s)-1]
	}
	n, ok := parseInteger(s)
	if !ok {
		return &json.UnmarshalTypeError{Value: "amount " + strconv.Quote(string(b)), Type: reflect.TypeFor[Minor]()}
	}
	*m = Minor(n)
	return nil
}

// Parse parses a base-10 integer string of minor units.
func Parse(s string) (Minor, bool) {
	n, ok := parseInteger(s)
	return Minor(n), ok
}

func parseInteger(s string) (int64, bool) {
	digits := s
	if len(digits) > 0 && digits[0] == '-' {
		digits = digits[1:]
	}
	if len(digits) == 0 || len(digits) > maxDigits {
		return 0, false
	}
	for i := 0; i < len(digits); i++ {
		if digits[i] < '0' || digits[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, false // overflow
	}
	return n, true
}

// Format renders minor units for humans, e.g. Format(5000000, "NGN") ==
// "NGN 50,000.00". It assumes two decimal places (true for NGN, USD, GBP,
// EUR); currencies like JPY (0) or KWD (3) would need a per-currency exponent.
func Format(m Minor, currency string) string {
	n := int64(m)
	sign := ""
	if n < 0 {
		sign = "-"
	}
	// Work in uint64 so math.MinInt64 does not overflow when negated.
	u := uint64(n)
	if n < 0 {
		u = uint64(-(n + 1)) + 1
	}
	major, minor := u/100, u%100

	digits := strconv.FormatUint(major, 10)
	var grouped []byte
	for i := range len(digits) {
		if i > 0 && (len(digits)-i)%3 == 0 {
			grouped = append(grouped, ',')
		}
		grouped = append(grouped, digits[i])
	}
	return currency + " " + sign + string(grouped) + "." + fmt.Sprintf("%02d", minor)
}
