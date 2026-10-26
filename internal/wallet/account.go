package wallet

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"strings"
	"unicode"
	"unicode/utf8"
)

// GenerateAccountNumber returns a random 10-digit NUBAN-style account number
// whose first digit is 1-9 (the 0xxxxxxxxx range is reserved for system
// accounts).
//
// Numbers are random rather than sequential so they cannot be enumerated by
// incrementing. Uniqueness is guaranteed by the database; the caller retries
// on the (roughly 1 in 9 billion) chance of a collision.
func GenerateAccountNumber() (string, error) {
	// 1_000_000_000 + [0, 9_000_000_000) -> [1_000_000_000, 9_999_999_999]
	n, err := rand.Int(rand.Reader, big.NewInt(9_000_000_000))
	if err != nil {
		return "", fmt.Errorf("generate account number: %w", err)
	}
	return fmt.Sprintf("%010d", n.Int64()+1_000_000_000), nil
}

// DisplayName is the name shown to OTHER users (name enquiry, statements).
//
// Only a VERIFIED KYC name is trusted: showing a name a user merely typed into
// a pending KYC form would let anyone impersonate "Dangote Industries" before
// review. Without a verified name, it falls back to the capitalised email
// local part without any "+tag" ("ada+savings@irate.dev" -> "Ada").
func DisplayName(email string, verifiedName *string) string {
	if verifiedName != nil {
		if n := strings.TrimSpace(*verifiedName); n != "" {
			return n
		}
	}
	local, _, _ := strings.Cut(email, "@")
	local, _, _ = strings.Cut(local, "+")
	if local == "" {
		return "iRate User"
	}
	r, size := utf8.DecodeRuneInString(local)
	return string(unicode.ToUpper(r)) + local[size:]
}
