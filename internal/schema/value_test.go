package schema

import (
	"encoding/json"
	"testing"
)

func TestCoerceValue(t *testing.T) {
	cases := []struct {
		name    string
		field   Field
		in      any
		want    any
		wantErr bool
	}{
		{"string ok", Field{Type: TypeString}, "hello", "hello", false},
		{"string empty is nil", Field{Type: TypeString}, "", nil, false},
		{"string wrong type", Field{Type: TypeString}, 5.0, nil, true},
		{"string too long default", Field{Type: TypeString}, string(make([]byte, 256)), nil, true},
		{"string max length", Field{Type: TypeString, MaxLength: ptr(3)}, "abcd", nil, true},
		{"string min length runes", Field{Type: TypeString, MinLength: ptr(2)}, "é", nil, true},
		{"string regex ok", Field{Type: TypeString, Regex: `^[a-z-]+$`}, "my-slug", "my-slug", false},
		{"string regex fail", Field{Type: TypeString, Regex: `^[a-z-]+$`}, "My Slug", nil, true},
		{"text long ok", Field{Type: TypeText}, string(make([]byte, 1000)), string(make([]byte, 1000)), false},
		{"email ok", Field{Type: TypeEmail}, " a@b.co ", "a@b.co", false},
		{"email bad", Field{Type: TypeEmail}, "not-an-email", nil, true},
		{"email with name rejected", Field{Type: TypeEmail}, "Bob <a@b.co>", nil, true},
		{"number ok", Field{Type: TypeNumber, Format: "decimal"}, 1.5, 1.5, false},
		{"number json.Number", Field{Type: TypeNumber}, json.Number("42"), 42.0, false},
		{"number string rejected", Field{Type: TypeNumber}, "42", nil, true},
		{"integer rejects decimal", Field{Type: TypeNumber, Format: "integer"}, 1.5, nil, true},
		{"number min", Field{Type: TypeNumber, Min: ptr(0.0)}, -1.0, nil, true},
		{"number max", Field{Type: TypeNumber, Max: ptr(10.0)}, 11.0, nil, true},
		{"boolean ok", Field{Type: TypeBoolean}, false, false, false},
		{"boolean string rejected", Field{Type: TypeBoolean}, "true", nil, true},
		{"date ok", Field{Type: TypeDate}, "2024-02-29", "2024-02-29", false},
		{"date invalid", Field{Type: TypeDate}, "2023-02-29", nil, true},
		{"datetime normalized to UTC", Field{Type: TypeDateTime}, "2024-05-01T11:30:00+02:00", "2024-05-01T09:30:00.000Z", false},
		{"datetime invalid", Field{Type: TypeDateTime}, "yesterday", nil, true},
		{"enum ok", Field{Type: TypeEnum, Enum: []string{"draft", "live"}}, "live", "live", false},
		{"enum invalid", Field{Type: TypeEnum, Enum: []string{"draft", "live"}}, "gone", nil, true},
		{"json object", Field{Type: TypeJSON}, map[string]any{"a": []any{1.0}}, map[string]any{"a": []any{1.0}}, false},
		{"nil is nil", Field{Type: TypeNumber}, nil, nil, false},
		{"richtext sanitized", Field{Type: TypeRichText}, `<p onclick="x()">Hi<script>alert(1)</script></p>`, "<p>Hi</p>", false},
		{"richtext empty paragraph is nil", Field{Type: TypeRichText}, "<p> </p>", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := CoerceValue(tc.field, tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			gj, _ := json.Marshal(got)
			wj, _ := json.Marshal(tc.want)
			if string(gj) != string(wj) {
				t.Fatalf("got %s, want %s", gj, wj)
			}
		})
	}
}
