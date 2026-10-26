package httpx

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"reflect"
	"strings"

	"github.com/iamBlissMedik/irate-core-go/internal/apperr"
	"github.com/iamBlissMedik/irate-core-go/internal/money"
)

// MaxBodyBytes caps every JSON request body. API payloads here are tiny; a
// cap stops a client from making the server buffer gigabytes.
const MaxBodyBytes = 1 << 20 // 1 MiB

// Normalizer is implemented by request types that clean their fields (trim
// whitespace, lower-case emails) before validation.
type Normalizer interface {
	Normalize()
}

// Decode strictly parses a JSON body into dst, normalises it, then validates
// it. It rejects: a wrong Content-Type, an empty body, malformed JSON, unknown
// fields (catching misspelt field names), wrong types, trailing data and
// bodies over MaxBodyBytes. Every failure is a client-safe *apperr.Error.
func Decode(w http.ResponseWriter, r *http.Request, dst any) error {
	return decode(w, r, dst, false)
}

// DecodeOptional is Decode for endpoints where the body may be omitted
// entirely (dst keeps its zero/default values).
func DecodeOptional(w http.ResponseWriter, r *http.Request, dst any) error {
	return decode(w, r, dst, true)
}

func decode(w http.ResponseWriter, r *http.Request, dst any, optional bool) error {
	if optional && r.ContentLength == 0 && r.Header.Get("Transfer-Encoding") == "" {
		return finish(dst)
	}

	if ct := r.Header.Get("Content-Type"); ct != "" {
		// Some proxies merge duplicated headers into "application/json,
		// application/json"; judge the first value.
		first, _, _ := strings.Cut(ct, ",")
		mediaType, _, err := mime.ParseMediaType(first)
		if err != nil || mediaType != "application/json" {
			return apperr.New(http.StatusUnsupportedMediaType, apperr.CodeUnsupportedMedia, "Content-Type must be application/json")
		}
	} else if !optional {
		return apperr.New(http.StatusUnsupportedMediaType, apperr.CodeUnsupportedMedia, "Content-Type must be application/json")
	}

	r.Body = http.MaxBytesReader(w, r.Body, MaxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(dst); err != nil {
		if optional && errors.Is(err, io.EOF) {
			return finish(dst)
		}
		return decodeError(err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return apperr.BadRequest(apperr.CodeBadRequest, "Request body must contain a single JSON object")
	}
	return finish(dst)
}

func finish(dst any) error {
	if n, ok := dst.(Normalizer); ok {
		n.Normalize()
	}
	return Validate(dst)
}

func decodeError(err error) error {
	var (
		syntaxErr   *json.SyntaxError
		typeErr     *json.UnmarshalTypeError
		maxBytesErr *http.MaxBytesError
	)
	switch {
	case errors.Is(err, io.EOF):
		return apperr.BadRequest(apperr.CodeBadRequest, "Request body must not be empty")
	case errors.As(err, &syntaxErr):
		return apperr.BadRequest(apperr.CodeBadRequest, fmt.Sprintf("Malformed JSON at position %d", syntaxErr.Offset))
	case errors.Is(err, io.ErrUnexpectedEOF):
		return apperr.BadRequest(apperr.CodeBadRequest, "Malformed JSON: unexpected end of body")
	case errors.As(err, &typeErr):
		if typeErr.Field == "" {
			return apperr.BadRequest(apperr.CodeBadRequest, "Request body must be a JSON object")
		}
		return apperr.Field(typeErr.Field, typeMessage(typeErr.Type))
	case errors.As(err, &maxBytesErr):
		return apperr.New(http.StatusRequestEntityTooLarge, apperr.CodePayloadTooLarge,
			fmt.Sprintf("Request body must not exceed %d bytes", maxBytesErr.Limit))
	case strings.HasPrefix(err.Error(), "json: unknown field "):
		field := strings.Trim(strings.TrimPrefix(err.Error(), "json: unknown field "), `"`)
		return apperr.Field(field, "is not a recognised field")
	default:
		return apperr.BadRequest(apperr.CodeBadRequest, "Invalid request body")
	}
}

func typeMessage(t reflect.Type) string {
	if t == reflect.TypeFor[money.Minor]() {
		return "must be a positive whole number of minor units, e.g. 150000 for 1,500.00"
	}
	switch t.Kind() {
	case reflect.String:
		return "must be a string"
	case reflect.Bool:
		return "must be a boolean"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "must be an integer"
	case reflect.Float32, reflect.Float64:
		return "must be a number"
	case reflect.Slice, reflect.Array:
		return "must be an array"
	default:
		return "has an invalid type"
	}
}
