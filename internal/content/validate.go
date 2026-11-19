package content

import (
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"slices"

	"mino/internal/schema"
)

func newValidationError() *schema.ValidationError {
	return &schema.ValidationError{Message: "Validation failed"}
}

// prepare merges input into the existing document (nil for a new entry), coercing each scalar
// value and parsing link ids. Keys mapped to nil remove a value. System attributes in input are
// ignored so clients can send back what they received.
func prepare(ct *schema.ContentType, input map[string]any, existing map[string]any) (map[string]any, map[string][]int64, *schema.ValidationError) {
	ve := newValidationError()
	data := map[string]any{}
	maps.Copy(data, existing)
	links := map[string][]int64{}

	for key, raw := range input {
		if slices.Contains(schema.SystemFields, key) {
			continue
		}
		f, ok := ct.Field(key)
		if !ok {
			ve.Add("Unknown field", key)
			continue
		}
		if f.IsLink() {
			ids, err := parseIDs(f, raw)
			if err != nil {
				ve.Add(err.Error(), key)
				continue
			}
			links[key] = ids
			continue
		}
		v, err := schema.CoerceValue(f, raw)
		if err != nil {
			ve.Add(err.Error(), key)
			continue
		}
		if v == nil {
			delete(data, key)
		} else {
			data[key] = v
		}
	}

	if existing == nil {
		for _, f := range ct.Fields {
			if f.IsLink() || f.Default == nil {
				continue
			}
			if _, given := input[f.Name]; given {
				continue
			}
			if v, err := schema.CoerceValue(f, f.Default); err == nil && v != nil {
				data[f.Name] = v
			}
		}
	}
	return data, links, ve
}

// checkRequired validates required fields. linkCounts gives the number of links per link field.
func checkRequired(ct *schema.ContentType, data map[string]any, linkCounts map[string]int, ve *schema.ValidationError) {
	for _, f := range ct.Fields {
		if !f.Required {
			continue
		}
		if f.IsLink() {
			if linkCounts[f.Name] == 0 {
				ve.Add("This field is required", f.Name)
			}
			continue
		}
		if _, ok := data[f.Name]; !ok && ve.For(f.Name) == "" {
			ve.Add("This field is required", f.Name)
		}
	}
}

// parseIDs accepts an id, {"id": n}, an array of those, or null/[] to clear.
func parseIDs(f schema.Field, raw any) ([]int64, error) {
	var items []any
	switch v := raw.(type) {
	case nil:
		return []int64{}, nil
	case []any:
		items = v
	case []int64:
		ids := slices.Clone(v)
		return dedupe(f, ids)
	default:
		items = []any{v}
	}
	ids := make([]int64, 0, len(items))
	for _, it := range items {
		if m, ok := it.(map[string]any); ok {
			it = m["id"]
		}
		id, ok := toID(it)
		if !ok {
			return nil, fmt.Errorf("expected an id or a list of ids")
		}
		ids = append(ids, id)
	}
	return dedupe(f, ids)
}

func dedupe(f schema.Field, ids []int64) ([]int64, error) {
	out := ids[:0]
	seen := map[int64]bool{}
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	if !f.IsMultiple() && len(out) > 1 {
		return nil, fmt.Errorf("accepts a single value")
	}
	return out, nil
}

func toID(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		if n > 0 && n == math.Trunc(n) && n < 1<<53 {
			return int64(n), true
		}
	case int64:
		return n, n > 0
	case int:
		return int64(n), n > 0
	case json.Number:
		i, err := n.Int64()
		return i, err == nil && i > 0
	}
	return 0, false
}
