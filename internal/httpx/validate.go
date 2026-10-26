package httpx

import (
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"unicode"

	"github.com/go-playground/validator/v10"

	"github.com/iamBlissMedik/irate-core-go/internal/apperr"
)

var (
	validate         = newValidator()
	accountNumberRe  = regexp.MustCompile(`^[0-9]{10}$`)
	passwordMinRunes = 8
)

func newValidator() *validator.Validate {
	v := validator.New(validator.WithRequiredStructEnabled())

	// Report JSON field names ("toAccountNumber"), not Go names.
	v.RegisterTagNameFunc(func(f reflect.StructField) string {
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "-" {
			return ""
		}
		return name
	})

	must(v.RegisterValidation("accountnumber", func(fl validator.FieldLevel) bool {
		return accountNumberRe.MatchString(fl.Field().String())
	}))
	// Password policy (mirrors @irate/contracts): 8-128 chars with at least
	// one upper-case letter, one lower-case letter and one digit.
	must(v.RegisterValidation("password", func(fl validator.FieldLevel) bool {
		return IsStrongPassword(fl.Field().String())
	}))
	// Free text shown to other people (names, notes) must not carry control
	// characters (newline injection into statements, terminal escapes in logs).
	must(v.RegisterValidation("printable", func(fl validator.FieldLevel) bool {
		for _, r := range fl.Field().String() {
			if unicode.IsControl(r) {
				return false
			}
		}
		return true
	}))
	return v
}

// IsStrongPassword reports whether pw satisfies the password policy.
func IsStrongPassword(pw string) bool {
	n := len([]rune(pw))
	if n < passwordMinRunes || n > 128 {
		return false
	}
	var upper, lower, digit bool
	for _, r := range pw {
		switch {
		case unicode.IsUpper(r):
			upper = true
		case unicode.IsLower(r):
			lower = true
		case unicode.IsDigit(r):
			digit = true
		}
	}
	return upper && lower && digit
}

// Validate runs struct-tag validation and converts failures into a 400 with
// one field error per problem.
func Validate(v any) error {
	err := validate.Struct(v)
	if err == nil {
		return nil
	}
	var verrs validator.ValidationErrors
	if !errors.As(err, &verrs) {
		return err // programmer error (e.g. not a struct): surfaces as a 500
	}
	fields := make([]apperr.FieldError, 0, len(verrs))
	for _, fe := range verrs {
		fields = append(fields, apperr.FieldError{Field: fieldPath(fe), Message: message(fe)})
	}
	return apperr.Validation(fields...)
}

// fieldPath strips the root struct name: "TransferRequest.amount" -> "amount".
func fieldPath(fe validator.FieldError) string {
	ns := fe.Namespace()
	if _, rest, ok := strings.Cut(ns, "."); ok {
		return rest
	}
	return fe.Field()
}

func message(fe validator.FieldError) string {
	isString := fe.Kind() == reflect.String
	switch fe.Tag() {
	case "required":
		return "is required"
	case "email":
		return "must be a valid email address"
	case "url", "http_url":
		return "must be a valid URL"
	case "min":
		if isString {
			return fmt.Sprintf("must be at least %s characters", fe.Param())
		}
		return "must be at least " + fe.Param()
	case "max":
		if isString {
			return fmt.Sprintf("must be at most %s characters", fe.Param())
		}
		return "must be at most " + fe.Param()
	case "gt":
		return "must be greater than " + fe.Param()
	case "gte":
		return "must be at least " + fe.Param()
	case "oneof":
		return "must be one of: " + strings.ReplaceAll(fe.Param(), " ", ", ")
	case "len":
		return fmt.Sprintf("must be exactly %s characters", fe.Param())
	case "alpha":
		return "must contain only letters"
	case "uppercase":
		return "must be upper-case"
	case "accountnumber":
		return "must be exactly 10 digits"
	case "password":
		return "must be 8-128 characters and include an upper-case letter, a lower-case letter and a number"
	case "printable":
		return "must not contain control characters"
	default:
		return "is invalid"
	}
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
