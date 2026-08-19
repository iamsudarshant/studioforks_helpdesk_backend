package httpx

import (
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/go-playground/validator/v10"
)

var validate *validator.Validate

// Domain formats used across the Indian statutory fields this platform handles.
var (
	reUAN = regexp.MustCompile(`^\d{12}$`)
	rePAN = regexp.MustCompile(`^[A-Z]{5}\d{4}[A-Z]$`)
	// An EPFO member account number: region / office / establishment /
	// extension / member, e.g. MH/BAN/1234567/000/1234567. The slashes are the
	// canonical form; the run-together spelling is accepted too because that is
	// how the portal exports it, and NormalisePFNumber converts one to the
	// other so what is stored is always the readable form.
	rePF         = regexp.MustCompile(`^[A-Z]{2}/[A-Z]{3}/\d{1,7}/\d{3}/\d{1,7}$`)
	rePFCompact  = regexp.MustCompile(`^([A-Z]{2})([A-Z]{3})(\d{7})(\d{3})(\d{7})$`)
	reESIC       = regexp.MustCompile(`^\d{10,17}$`)
	reMobileIN   = regexp.MustCompile(`^[6-9]\d{9}$`)
	reSlug       = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,48}[a-z0-9])?$`)
	reKey        = regexp.MustCompile(`^[A-Z][A-Z0-9_]{1,48}$`)
	reFieldKey   = regexp.MustCompile(`^[a-z][a-z0-9_]{0,48}$`)
	reHexColor   = regexp.MustCompile(`^#(?:[0-9a-fA-F]{3}|[0-9a-fA-F]{6})$`)
	reEmployeeID = regexp.MustCompile(`^[A-Za-z0-9/\-_]{1,50}$`)
)

func init() {
	validate = validator.New(validator.WithRequiredStructEnabled())

	// Report the JSON name so error details match the wire format the client sent.
	validate.RegisterTagNameFunc(func(fld reflect.StructField) string {
		name := strings.SplitN(fld.Tag.Get("json"), ",", 2)[0]
		if name == "-" || name == "" {
			return fld.Name
		}
		return name
	})

	mustRegister("uan", func(fl validator.FieldLevel) bool {
		return matchOptional(reUAN, fl.Field().String())
	})
	mustRegister("pan", func(fl validator.FieldLevel) bool {
		return matchOptional(rePAN, strings.ToUpper(fl.Field().String()))
	})
	mustRegister("pfnumber", func(fl validator.FieldLevel) bool {
		v := strings.ToUpper(strings.TrimSpace(fl.Field().String()))
		if v == "" {
			return true
		}
		return rePF.MatchString(v) || rePFCompact.MatchString(v)
	})
	mustRegister("esic", func(fl validator.FieldLevel) bool {
		return matchOptional(reESIC, fl.Field().String())
	})
	mustRegister("mobile", func(fl validator.FieldLevel) bool {
		return matchOptional(reMobileIN, fl.Field().String())
	})
	mustRegister("slug", func(fl validator.FieldLevel) bool {
		return matchOptional(reSlug, fl.Field().String())
	})
	mustRegister("enumkey", func(fl validator.FieldLevel) bool {
		return matchOptional(reKey, fl.Field().String())
	})
	mustRegister("fieldkey", func(fl validator.FieldLevel) bool {
		return matchOptional(reFieldKey, fl.Field().String())
	})
	mustRegister("hexcolor", func(fl validator.FieldLevel) bool {
		return matchOptional(reHexColor, fl.Field().String())
	})
	mustRegister("employeeid", func(fl validator.FieldLevel) bool {
		return matchOptional(reEmployeeID, fl.Field().String())
	})
	mustRegister("notblank", func(fl validator.FieldLevel) bool {
		return strings.TrimSpace(fl.Field().String()) != ""
	})
	mustRegister("dateonly", func(fl validator.FieldLevel) bool {
		v := fl.Field().String()
		if v == "" {
			return true
		}
		_, err := time.Parse("2006-01-02", v)
		return err == nil
	})
	// Rejects control characters that would corrupt CSV/Excel exports or logs.
	mustRegister("safetext", func(fl validator.FieldLevel) bool {
		for _, r := range fl.Field().String() {
			if unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t' {
				return false
			}
		}
		return true
	})
}

func mustRegister(tag string, fn validator.Func) {
	if err := validate.RegisterValidation(tag, fn); err != nil {
		panic(fmt.Sprintf("registering validator %q: %v", tag, err))
	}
}

// matchOptional treats an empty value as valid; use `required` to demand it.
func matchOptional(re *regexp.Regexp, v string) bool {
	if strings.TrimSpace(v) == "" {
		return true
	}
	return re.MatchString(v)
}

// Validate runs struct validation and converts failures into the field-level
// error details the frontend maps onto form inputs.
func Validate(v any) error {
	if err := validate.Struct(v); err != nil {
		var invalid *validator.InvalidValidationError
		if errors.As(err, &invalid) {
			return ErrInternal(err)
		}

		var verrs validator.ValidationErrors
		if errors.As(err, &verrs) {
			details := make([]FieldError, 0, len(verrs))
			for _, fe := range verrs {
				details = append(details, FieldError{
					Field:   fieldPath(fe),
					Code:    strings.ToUpper(fe.Tag()),
					Message: messageFor(fe),
				})
			}
			return ErrValidation(details...)
		}
		return ErrInternal(err)
	}
	return nil
}

// fieldPath renders nested struct paths as dotted JSON paths.
func fieldPath(fe validator.FieldError) string {
	ns := fe.Namespace()
	if i := strings.Index(ns, "."); i >= 0 {
		ns = ns[i+1:]
	}
	return ns
}

func messageFor(fe validator.FieldError) string {
	switch fe.Tag() {
	case "required", "notblank":
		return "This field is required."
	case "email":
		return "Enter a valid email address."
	case "min":
		if fe.Kind() == reflect.String {
			return fmt.Sprintf("Must be at least %s characters.", fe.Param())
		}
		return fmt.Sprintf("Must be at least %s.", fe.Param())
	case "max":
		if fe.Kind() == reflect.String {
			return fmt.Sprintf("Must be at most %s characters.", fe.Param())
		}
		return fmt.Sprintf("Must be at most %s.", fe.Param())
	case "len":
		return fmt.Sprintf("Must be exactly %s characters.", fe.Param())
	case "oneof":
		return fmt.Sprintf("Must be one of: %s.", strings.ReplaceAll(fe.Param(), " ", ", "))
	case "uan":
		return "UAN must be exactly 12 digits."
	case "pan":
		return "PAN must be in the format ABCDE1234F."
	case "pfnumber":
		return "PF number must look like MH/BAN/1234567/000/1234567."
	case "esic":
		return "ESIC number must be 10 to 17 digits."
	case "mobile":
		return "Enter a valid 10-digit mobile number."
	case "slug":
		return "Use lowercase letters, digits and hyphens only."
	case "enumkey":
		return "Use uppercase letters, digits and underscores, starting with a letter."
	case "fieldkey":
		return "Use lowercase letters, digits and underscores, starting with a letter."
	case "hexcolor":
		return "Enter a valid hex colour such as #1A73E8."
	case "employeeid":
		return "Employee ID contains unsupported characters."
	case "dateonly":
		return "Enter a date in YYYY-MM-DD format."
	case "safetext":
		return "This value contains characters that are not allowed."
	case "url":
		return "Enter a valid URL."
	case "gt":
		return fmt.Sprintf("Must be greater than %s.", fe.Param())
	case "gte":
		return fmt.Sprintf("Must be %s or more.", fe.Param())
	case "lt":
		return fmt.Sprintf("Must be less than %s.", fe.Param())
	case "lte":
		return fmt.Sprintf("Must be %s or less.", fe.Param())
	case "eqfield":
		return fmt.Sprintf("Must match %s.", strings.ToLower(fe.Param()))
	case "dive", "required_if", "required_with", "required_without":
		return "This field is required in the current context."
	default:
		return "This value is invalid."
	}
}

// NormalisePFNumber renders a PF account number in its canonical, readable form.
//
// EPFO's own portal exports the number run together — MHBAN00123450000012345 —
// while people write and read it with separators. Storing one shape means a
// search for either finds the record, and a list does not mix the two.
//
// Anything that is neither shape is returned untouched: validation has already
// refused it, and silently rewriting an unrecognised value would be worse than
// storing what was typed.
func NormalisePFNumber(value string) string {
	v := strings.ToUpper(strings.TrimSpace(value))
	if v == "" || rePF.MatchString(v) {
		return v
	}
	if m := rePFCompact.FindStringSubmatch(v); m != nil {
		return strings.Join(m[1:], "/")
	}
	return v
}
