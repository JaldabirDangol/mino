package schema

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/mail"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"mino/internal/sanitize"
)

// DateTimeLayout is the storage format for datetime values. Fixed-width UTC timestamps
// sort correctly as text, which keeps range filters and ordering simple.
const DateTimeLayout = "2006-01-02T15:04:05.000Z"

// CoerceValue validates a scalar value against a field and returns its canonical stored form.
// A nil result means "no value": empty strings and JSON null are normalized to nil so that
// required and unique checks behave consistently. Link fields (media, relations) are not handled here.
func CoerceValue(f Field, v any) (any, error) {
	if v == nil {
		return nil, nil
	}
	switch f.Type {
	case TypeString, TypeText, TypeEmail:
		s, ok := v.(string)
		if !ok {
			return nil, errors.New("must be a string")
		}
		if f.Type == TypeEmail {
			s = strings.TrimSpace(s)
		}
		if s == "" {
			return nil, nil
		}
		n := utf8.RuneCountInString(s)
		if f.MinLength != nil && n < *f.MinLength {
			return nil, fmt.Errorf("must be at least %d characters", *f.MinLength)
		}
		if f.MaxLength != nil && n > *f.MaxLength {
			return nil, fmt.Errorf("must be at most %d characters", *f.MaxLength)
		}
		if f.Type == TypeString && f.MaxLength == nil && n > 255 {
			return nil, errors.New("must be at most 255 characters (use Long Text for more)")
		}
		if f.Type == TypeEmail {
			a, err := mail.ParseAddress(s)
			if err != nil || a.Address != s || !strings.Contains(s, ".") {
				return nil, errors.New("must be a valid email address")
			}
		}
		if f.Regex != "" {
			re, err := regexp.Compile(f.Regex)
			if err == nil && !re.MatchString(s) {
				return nil, errors.New("does not match the required format")
			}
		}
		return s, nil

	case TypeRichText:
		s, ok := v.(string)
		if !ok {
			return nil, errors.New("must be an HTML string")
		}
		s = sanitize.HTML(s)
		if sanitize.IsEmpty(s) {
			return nil, nil
		}
		return s, nil

	case TypeNumber:
		n, ok := toFloat(v)
		if !ok {
			return nil, errors.New("must be a number")
		}
		if math.IsNaN(n) || math.IsInf(n, 0) {
			return nil, errors.New("must be a finite number")
		}
		if f.Format == "integer" && n != math.Trunc(n) {
			return nil, errors.New("must be an integer")
		}
		if f.Min != nil && n < *f.Min {
			return nil, fmt.Errorf("must be at least %v", *f.Min)
		}
		if f.Max != nil && n > *f.Max {
			return nil, fmt.Errorf("must be at most %v", *f.Max)
		}
		return n, nil

	case TypeBoolean:
		b, ok := v.(bool)
		if !ok {
			return nil, errors.New("must be true or false")
		}
		return b, nil

	case TypeDate:
		s, ok := v.(string)
		if !ok {
			return nil, errors.New("must be a date string (YYYY-MM-DD)")
		}
		if s == "" {
			return nil, nil
		}
		if _, err := time.Parse(time.DateOnly, s); err != nil {
			return nil, errors.New("must be a date (YYYY-MM-DD)")
		}
		return s, nil

	case TypeDateTime:
		s, ok := v.(string)
		if !ok {
			return nil, errors.New("must be a date-time string (RFC 3339)")
		}
		if s == "" {
			return nil, nil
		}
		t, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			return nil, errors.New("must be a date-time such as 2024-05-01T09:30:00Z")
		}
		return t.UTC().Format(DateTimeLayout), nil

	case TypeEnum:
		s, ok := v.(string)
		if !ok {
			return nil, errors.New("must be a string")
		}
		if s == "" {
			return nil, nil
		}
		if !slices.Contains(f.Enum, s) {
			return nil, fmt.Errorf("must be one of: %s", strings.Join(f.Enum, ", "))
		}
		return s, nil

	case TypeJSON:
		// Round-trip to guarantee the value is representable as JSON.
		b, err := json.Marshal(v)
		if err != nil {
			return nil, errors.New("must be valid JSON")
		}
		var out any
		_ = json.Unmarshal(b, &out)
		return out, nil
	}
	return nil, fmt.Errorf("field type %s does not hold a scalar value", f.Type)
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case int32:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}
