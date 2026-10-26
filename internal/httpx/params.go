package httpx

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/iamBlissMedik/irate-core-go/internal/apperr"
)

const (
	DefaultPageLimit = 10
	MaxPageLimit     = 100
	maxPage          = 100_000 // keeps OFFSET bounded (and inside int32)
)

// Page is a validated offset-pagination request.
type Page struct {
	Page  int
	Limit int
}

func (p Page) Offset() int32 { return int32((p.Page - 1) * p.Limit) }
func (p Page) Lim() int32    { return int32(p.Limit) }

// ParsePage reads ?page=&limit=. Like the original API it is lenient: missing
// or junk values fall back to defaults, and limit is clamped to MaxPageLimit
// so a client cannot request a million rows in one call.
func ParsePage(r *http.Request) Page {
	q := r.URL.Query()
	page := atoiDefault(q.Get("page"), 1)
	limit := atoiDefault(q.Get("limit"), DefaultPageLimit)
	if page < 1 {
		page = 1
	}
	if page > maxPage {
		page = maxPage
	}
	if limit < 1 {
		limit = DefaultPageLimit
	}
	if limit > MaxPageLimit {
		limit = MaxPageLimit
	}
	return Page{Page: page, Limit: limit}
}

// PageMeta is the pagination block returned by every list endpoint.
type PageMeta struct {
	Total       int64 `json:"total"`
	Page        int   `json:"page"`
	Limit       int   `json:"limit"`
	TotalPages  int   `json:"totalPages"`
	HasNextPage bool  `json:"hasNextPage"`
	HasPrevPage bool  `json:"hasPrevPage"`
}

func NewPageMeta(total int64, p Page) PageMeta {
	totalPages := int((total + int64(p.Limit) - 1) / int64(p.Limit))
	return PageMeta{
		Total:       total,
		Page:        p.Page,
		Limit:       p.Limit,
		TotalPages:  totalPages,
		HasNextPage: p.Page < totalPages,
		HasPrevPage: p.Page > 1,
	}
}

// UUIDParam parses a UUID path parameter, returning a field error if malformed.
func UUIDParam(r *http.Request, name string) (uuid.UUID, error) {
	raw := chi.URLParam(r, name)
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, apperr.Field(name, "must be a valid UUID")
	}
	return id, nil
}

// LikePattern turns user input into a safe "contains" ILIKE pattern: the LIKE
// wildcards % and _ (and the escape character) are escaped so that searching
// for "100%" does not match everything. Returns nil for blank input.
func LikePattern(input string) *string {
	s := strings.TrimSpace(input)
	if s == "" {
		return nil
	}
	if r := []rune(s); len(r) > 100 {
		s = string(r[:100]) // truncate on rune boundaries, never mid-character
	}
	s = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
	p := "%" + s + "%"
	return &p
}

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}
