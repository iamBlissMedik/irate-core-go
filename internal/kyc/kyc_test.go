package kyc

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamBlissMedik/irate-core-go/internal/apperr"
)

func TestParseDateOfBirth(t *testing.T) {
	lagos, err := time.LoadLocation("Africa/Lagos")
	require.NoError(t, err)
	s := &Service{loc: lagos, now: func() time.Time { return time.Date(2026, 10, 26, 12, 0, 0, 0, lagos) }}

	valid := map[string]string{
		"1994-05-17":                "1994-05-17",
		"2008-10-26":                "2008-10-26", // turns 18 today: allowed
		"1990-01-15T00:00:00.000Z":  "1990-01-15",
		"1990-01-15T23:30:00+01:00": "1990-01-15",
	}
	for in, want := range valid {
		dob, err := s.parseDateOfBirth(in)
		require.NoError(t, err, in)
		assert.Equal(t, want, dob.Format(dateLayout))
	}

	for _, in := range []string{"2008-10-27", "2030-01-01", "1890-01-01", "17/05/1994", "", "1994-02-30"} {
		_, err := s.parseDateOfBirth(in)
		ae, ok := apperr.As(err)
		require.True(t, ok, in)
		assert.Equal(t, "dateOfBirth", ae.Fields[0].Field, in)
	}
}

func TestSubmitRequestNormalize(t *testing.T) {
	r := SubmitRequest{FullName: "  Ada   Ngozi  Okafor ", IDType: " nin ", IDNumber: " 123 456 789 01 "}
	r.Normalize()
	assert.Equal(t, "Ada Ngozi Okafor", r.FullName)
	assert.Equal(t, "NIN", r.IDType)
	assert.Equal(t, "12345678901", r.IDNumber)
}
