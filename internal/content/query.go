package content

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"mino/internal/schema"
)

// Status selects entries by publication state.
type Status string

const (
	StatusAny       Status = ""          // every entry (admin default)
	StatusPublished Status = "published" // published entries only (API default)
	StatusDraft     Status = "draft"     // unpublished entries only
)

// Sort orders results by a field or system attribute.
type Sort struct {
	Field string
	Desc  bool
}

// Filter is a node of a filter tree. Leaves have Field and Op set; branches use And/Or/Not.
type Filter struct {
	And   []Filter
	Or    []Filter
	Not   *Filter
	Field string
	Op    string
	Value []string // raw values from the query; converted per field type when building SQL
}

// IsZero reports whether the filter matches everything.
func (f *Filter) IsZero() bool {
	return f == nil || (len(f.And) == 0 && len(f.Or) == 0 && f.Not == nil && f.Field == "")
}

// Query describes a list request.
type Query struct {
	Page     int
	PageSize int
	Sort     []Sort
	Filter   Filter
	Fields   []string // nil means all fields
	Populate []string // link fields to populate
	Status   Status
	Search   string // case-insensitive match on text fields (admin search)
}

// Limits bounds pagination.
type Limits struct {
	DefaultPageSize int
	MaxPageSize     int
}

// DefaultLimits are used when settings are unavailable.
var DefaultLimits = Limits{DefaultPageSize: 25, MaxPageSize: 100}

var operators = map[string]bool{
	"$eq": true, "$eqi": true, "$ne": true, "$lt": true, "$lte": true, "$gt": true, "$gte": true,
	"$in": true, "$notIn": true, "$contains": true, "$containsi": true, "$notContains": true,
	"$notContainsi": true, "$startsWith": true, "$endsWith": true, "$null": true, "$notNull": true, "$between": true,
}

// systemColumns maps API names of system attributes to SQL columns.
var systemColumns = map[string]string{
	"id":          "e.id",
	"createdAt":   "e.created_at",
	"updatedAt":   "e.updated_at",
	"publishedAt": "e.published_at",
}

// QueryError is a malformed query parameter.
type QueryError struct{ Msg string }

func (e *QueryError) Error() string { return e.Msg }

func qerr(format string, args ...any) error { return &QueryError{Msg: fmt.Sprintf(format, args...)} }

// ParseQuery reads Strapi-style list parameters:
//
//	pagination[page]=1&pagination[pageSize]=25
//	sort=title:asc,createdAt:desc   (or sort[0]=title:asc)
//	filters[title][$containsi]=hello&filters[$or][0][views][$gt]=10
//	fields=title,slug               (or fields[0]=title)
//	populate=*                      (or populate=author,cover / populate[0]=author)
//	status=published|draft
func ParseQuery(ct *schema.ContentType, v url.Values, lim Limits) (Query, error) {
	q := Query{Page: 1, PageSize: lim.DefaultPageSize, Status: StatusPublished}
	nested := map[string]any{}
	for key, vals := range v {
		path, ok := splitKey(key)
		if !ok {
			return q, qerr("malformed parameter %q", key)
		}
		insert(nested, path, vals)
	}

	if p, ok := nested["pagination"].(map[string]any); ok {
		var err error
		if q.Page, err = intParam(p["page"], 1, "pagination[page]"); err != nil {
			return q, err
		}
		if q.PageSize, err = intParam(p["pageSize"], lim.DefaultPageSize, "pagination[pageSize]"); err != nil {
			return q, err
		}
	}
	if q.Page < 1 {
		return q, qerr("pagination[page] must be at least 1")
	}
	if q.PageSize < 1 {
		return q, qerr("pagination[pageSize] must be at least 1")
	}
	q.PageSize = min(q.PageSize, lim.MaxPageSize)

	for _, s := range listParam(nested["sort"]) {
		name, dir, _ := strings.Cut(s, ":")
		desc := false
		switch strings.ToLower(dir) {
		case "", "asc":
		case "desc":
			desc = true
		default:
			return q, qerr("sort direction must be asc or desc")
		}
		if err := checkSortable(ct, name); err != nil {
			return q, err
		}
		q.Sort = append(q.Sort, Sort{Field: name, Desc: desc})
	}

	if raw, ok := nested["fields"]; ok {
		q.Fields = []string{}
		for _, name := range listParam(raw) {
			f, ok := ct.Field(name)
			if !ok && !slices.Contains(schema.SystemFields, name) {
				return q, qerr("unknown field %q in fields", name)
			}
			if ok && f.IsLink() {
				return q, qerr("%q is a link field; use populate instead of fields", name)
			}
			q.Fields = append(q.Fields, name)
		}
	}

	for _, name := range listParam(nested["populate"]) {
		if name == "*" {
			q.Populate = nil
			for _, f := range ct.Fields {
				if f.IsLink() {
					q.Populate = append(q.Populate, f.Name)
				}
			}
			break
		}
		f, ok := ct.Field(name)
		if !ok || !f.IsLink() {
			return q, qerr("cannot populate %q: not a relation or media field", name)
		}
		if !slices.Contains(q.Populate, name) {
			q.Populate = append(q.Populate, name)
		}
	}

	if raw, ok := nested["status"]; ok {
		switch s := firstString(raw); s {
		case "published":
			q.Status = StatusPublished
		case "draft":
			q.Status = StatusDraft
		default:
			return q, qerr("status must be published or draft")
		}
	}

	if raw, ok := nested["filters"]; ok {
		m, ok := raw.(map[string]any)
		if !ok {
			return q, qerr("filters must be an object, e.g. filters[title][$eq]=x")
		}
		f, err := buildFilter(ct, m, 0)
		if err != nil {
			return q, err
		}
		q.Filter = f
	}
	return q, nil
}

var keyPart = regexp.MustCompile(`\[([^\[\]]*)\]`)

// splitKey turns "filters[title][$eq]" into ["filters", "title", "$eq"].
func splitKey(key string) ([]string, bool) {
	head, rest, hasBracket := strings.Cut(key, "[")
	if !hasBracket {
		return []string{key}, true
	}
	rest = "[" + rest
	parts := []string{head}
	matches := keyPart.FindAllStringSubmatchIndex(rest, -1)
	pos := 0
	for _, m := range matches {
		if m[0] != pos {
			return nil, false
		}
		parts = append(parts, rest[m[2]:m[3]])
		pos = m[1]
	}
	return parts, pos == len(rest) && len(parts) <= 12
}

func insert(m map[string]any, path []string, vals []string) {
	for i, p := range path {
		if i == len(path)-1 {
			if existing, ok := m[p].([]string); ok {
				m[p] = append(existing, vals...)
			} else {
				m[p] = vals
			}
			return
		}
		next, ok := m[p].(map[string]any)
		if !ok {
			next = map[string]any{}
			m[p] = next
		}
		m = next
	}
}

// listParam accepts "a,b", repeated values, or indexed keys (x[0]=a&x[1]=b).
func listParam(raw any) []string {
	var out []string
	switch v := raw.(type) {
	case []string:
		for _, s := range v {
			for _, part := range strings.Split(s, ",") {
				if part = strings.TrimSpace(part); part != "" {
					out = append(out, part)
				}
			}
		}
	case map[string]any:
		for _, k := range sortedKeys(v) {
			out = append(out, listParam(v[k])...)
		}
	}
	return out
}

func firstString(raw any) string {
	if v, ok := raw.([]string); ok && len(v) > 0 {
		return v[0]
	}
	return ""
}

func intParam(raw any, def int, name string) (int, error) {
	s := firstString(raw)
	if s == "" {
		return def, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, qerr("%s must be an integer", name)
	}
	return n, nil
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, errA := strconv.Atoi(keys[i])
		b, errB := strconv.Atoi(keys[j])
		if errA == nil && errB == nil {
			return a < b
		}
		return keys[i] < keys[j]
	})
	return keys
}

func checkSortable(ct *schema.ContentType, name string) error {
	if _, ok := systemColumns[name]; ok {
		return nil
	}
	f, ok := ct.Field(name)
	if !ok {
		return qerr("unknown sort field %q", name)
	}
	if !f.Sortable() {
		return qerr("cannot sort by %q", name)
	}
	return nil
}

// buildFilter converts the nested parameter map into a Filter tree, validating field names.
func buildFilter(ct *schema.ContentType, m map[string]any, depth int) (Filter, error) {
	if depth > 6 {
		return Filter{}, qerr("filters are nested too deeply")
	}
	var out Filter
	for _, key := range sortedKeys(m) {
		val := m[key]
		switch key {
		case "$and", "$or":
			children, ok := val.(map[string]any)
			if !ok {
				return out, qerr("%s expects indexed conditions, e.g. filters[%s][0][title][$eq]=x", key, key)
			}
			var list []Filter
			for _, ck := range sortedKeys(children) {
				cm, ok := children[ck].(map[string]any)
				if !ok {
					return out, qerr("invalid %s condition", key)
				}
				f, err := buildFilter(ct, cm, depth+1)
				if err != nil {
					return out, err
				}
				list = append(list, f)
			}
			if key == "$and" {
				out.And = append(out.And, list...)
			} else {
				out.And = append(out.And, Filter{Or: list})
			}
		case "$not":
			cm, ok := val.(map[string]any)
			if !ok {
				return out, qerr("invalid $not condition")
			}
			f, err := buildFilter(ct, cm, depth+1)
			if err != nil {
				return out, err
			}
			out.And = append(out.And, Filter{Not: &f})
		default:
			leaves, err := fieldConditions(ct, key, val)
			if err != nil {
				return out, err
			}
			out.And = append(out.And, leaves...)
		}
	}
	return out, nil
}

func fieldConditions(ct *schema.ContentType, field string, val any) ([]Filter, error) {
	_, isSystem := systemColumns[field]
	f, ok := ct.Field(field)
	if !isSystem && !ok {
		return nil, qerr("unknown filter field %q", field)
	}
	if ok && f.Private {
		return nil, qerr("unknown filter field %q", field)
	}
	if ok && f.Type == schema.TypeRelation {
		// filters[author][id][$eq]=3, filters[author][id]=3, or filters[author][$null]=true
		if m, isMap := val.(map[string]any); isMap {
			if idVal, hasID := m["id"]; hasID {
				return opConditions(field, idVal)
			}
		}
		return opConditions(field, val)
	}
	return opConditions(field, val)
}

func opConditions(field string, val any) ([]Filter, error) {
	switch v := val.(type) {
	case []string:
		if len(v) > 1 {
			return []Filter{{Field: field, Op: "$in", Value: v}}, nil
		}
		return []Filter{{Field: field, Op: "$eq", Value: v}}, nil
	case map[string]any:
		var out []Filter
		for _, op := range sortedKeys(v) {
			if !operators[op] {
				return nil, qerr("unknown operator %q on %q", op, field)
			}
			vals := listValues(v[op])
			if len(vals) == 0 {
				return nil, qerr("operator %s on %q needs a value", op, field)
			}
			out = append(out, Filter{Field: field, Op: op, Value: vals})
		}
		return out, nil
	}
	return nil, qerr("invalid filter on %q", field)
}

// listValues flattens repeated or indexed values without splitting on commas (values may contain them).
func listValues(raw any) []string {
	switch v := raw.(type) {
	case []string:
		return v
	case map[string]any:
		var out []string
		for _, k := range sortedKeys(v) {
			out = append(out, listValues(v[k])...)
		}
		return out
	}
	return nil
}

// sqlBuilder accumulates positional arguments.
type sqlBuilder struct{ args []any }

func (b *sqlBuilder) arg(v any) string {
	b.args = append(b.args, v)
	return "$" + strconv.Itoa(len(b.args))
}

// lit quotes a validated field name as a SQL string literal. Names reaching this point have been
// checked against the schema and match ^[a-zA-Z][a-zA-Z0-9_]*$; quoting is defense in depth.
// Literal keys (rather than bind parameters) let PostgreSQL match expression indexes.
func lit(name string) string { return "'" + strings.ReplaceAll(name, "'", "''") + "'" }

func (b *sqlBuilder) filterSQL(ct *schema.ContentType, f Filter) (string, error) {
	var parts []string
	for _, c := range f.And {
		s, err := b.filterSQL(ct, c)
		if err != nil {
			return "", err
		}
		if s != "" {
			parts = append(parts, s)
		}
	}
	if len(f.Or) > 0 {
		var ors []string
		for _, c := range f.Or {
			s, err := b.filterSQL(ct, c)
			if err != nil {
				return "", err
			}
			if s == "" {
				s = "TRUE"
			}
			ors = append(ors, s)
		}
		parts = append(parts, "("+strings.Join(ors, " OR ")+")")
	}
	if f.Not != nil {
		s, err := b.filterSQL(ct, *f.Not)
		if err != nil {
			return "", err
		}
		if s != "" {
			parts = append(parts, "NOT COALESCE(("+s+"), false)")
		}
	}
	if f.Field != "" {
		s, err := b.leafSQL(ct, f)
		if err != nil {
			return "", err
		}
		parts = append(parts, s)
	}
	if len(parts) == 0 {
		return "", nil
	}
	return "(" + strings.Join(parts, " AND ") + ")", nil
}

func parseBoolValue(s string) (bool, error) {
	b, err := strconv.ParseBool(s)
	if err != nil {
		return false, qerr("expected true or false, got %q", s)
	}
	return b, nil
}

func likePattern(s, prefix, suffix string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return prefix + r.Replace(s) + suffix
}

func (b *sqlBuilder) leafSQL(ct *schema.ContentType, f Filter) (string, error) {
	if col, ok := systemColumns[f.Field]; ok {
		return b.systemSQL(f, col)
	}
	field, _ := ct.Field(f.Field)
	switch field.Type {
	case schema.TypeRelation:
		return b.linkSQL(f, `SELECT 1 FROM relations r WHERE r.source_id = e.id AND r.field = `+lit(field.Name), "r.target_id")
	case schema.TypeMedia:
		return b.linkSQL(f, `SELECT 1 FROM media_links ml WHERE ml.entry_id = e.id AND ml.field = `+lit(field.Name), "ml.media_id")
	case schema.TypeJSON:
		if f.Op != "$null" && f.Op != "$notNull" {
			return "", qerr("JSON field %q only supports $null and $notNull", field.Name)
		}
	}

	key := lit(field.Name)
	text := "e.data->>" + key
	jsonv := "e.data->" + key

	switch f.Op {
	case "$null", "$notNull":
		want, err := parseBoolValue(f.Value[0])
		if err != nil {
			return "", err
		}
		isNull := (f.Op == "$null") == want
		if isNull {
			return "NOT (e.data ? " + key + ")", nil
		}
		return "(e.data ? " + key + ")", nil
	case "$contains", "$containsi", "$notContains", "$notContainsi", "$startsWith", "$endsWith":
		if !field.IsTextual() {
			return "", qerr("%s only applies to text fields", f.Op)
		}
		op := "LIKE"
		if strings.HasSuffix(f.Op, "i") {
			op = "ILIKE"
		}
		pre, suf := "%", "%"
		switch f.Op {
		case "$startsWith":
			pre = ""
		case "$endsWith":
			suf = ""
		}
		cond := text + " " + op + " " + b.arg(likePattern(f.Value[0], pre, suf))
		if strings.HasPrefix(f.Op, "$not") {
			return "NOT COALESCE(" + cond + ", false)", nil
		}
		return cond, nil
	case "$eqi":
		if !field.IsTextual() {
			return "", qerr("$eqi only applies to text fields")
		}
		return "lower(" + text + ") = lower(" + b.arg(f.Value[0]) + ")", nil
	}

	vals := make([]any, len(f.Value))
	for i, raw := range f.Value {
		v, err := filterValue(field, raw)
		if err != nil {
			return "", err
		}
		vals[i] = v
	}

	switch f.Op {
	case "$eq":
		// JSONB containment is served by the GIN index on entries.data.
		doc, _ := json.Marshal(map[string]any{field.Name: vals[0]})
		return "e.data @> " + b.arg(string(doc)) + "::jsonb", nil
	case "$ne":
		doc, _ := json.Marshal(map[string]any{field.Name: vals[0]})
		return "NOT (e.data @> " + b.arg(string(doc)) + "::jsonb)", nil
	case "$in", "$notIn":
		docs := make([]string, len(vals))
		for i, v := range vals {
			j, _ := json.Marshal(v)
			docs[i] = string(j)
		}
		cond := jsonv + " = ANY(" + b.arg(docs) + "::jsonb[])"
		if f.Op == "$notIn" {
			return "NOT COALESCE(" + cond + ", false)", nil
		}
		return cond, nil
	case "$lt", "$lte", "$gt", "$gte", "$between":
		if field.Type == schema.TypeBoolean {
			return "", qerr("%s does not apply to boolean fields", f.Op)
		}
		expr := text
		cast := ""
		if field.Type == schema.TypeNumber {
			expr = "(" + text + ")::float8"
			cast = "::float8"
		}
		if f.Op == "$between" {
			if len(vals) != 2 {
				return "", qerr("$between needs exactly two values")
			}
			return expr + " BETWEEN " + b.arg(vals[0]) + cast + " AND " + b.arg(vals[1]) + cast, nil
		}
		ops := map[string]string{"$lt": "<", "$lte": "<=", "$gt": ">", "$gte": ">="}
		return expr + " " + ops[f.Op] + " " + b.arg(vals[0]) + cast, nil
	}
	return "", qerr("unsupported operator %s", f.Op)
}

// filterValue converts a raw query string to the stored representation of field.
func filterValue(f schema.Field, raw string) (any, error) {
	switch f.Type {
	case schema.TypeNumber:
		n, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return nil, qerr("%q expects a number", f.Name)
		}
		return n, nil
	case schema.TypeBoolean:
		return parseBoolValue(raw)
	case schema.TypeDateTime:
		t, err := parseTime(raw)
		if err != nil {
			return nil, qerr("%q expects a date-time", f.Name)
		}
		return t.UTC().Format(schema.DateTimeLayout), nil
	}
	return raw, nil
}

func parseTime(raw string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return t, nil
	}
	return time.Parse(time.DateOnly, raw)
}

func (b *sqlBuilder) systemSQL(f Filter, col string) (string, error) {
	switch f.Op {
	case "$null", "$notNull":
		want, err := parseBoolValue(f.Value[0])
		if err != nil {
			return "", err
		}
		if (f.Op == "$null") == want {
			return col + " IS NULL", nil
		}
		return col + " IS NOT NULL", nil
	}
	vals := make([]any, len(f.Value))
	for i, raw := range f.Value {
		if col == "e.id" {
			n, err := strconv.ParseInt(raw, 10, 64)
			if err != nil {
				return "", qerr("id expects an integer")
			}
			vals[i] = n
		} else {
			t, err := parseTime(raw)
			if err != nil {
				return "", qerr("%s expects a date-time", f.Field)
			}
			vals[i] = t
		}
	}
	ops := map[string]string{"$eq": "=", "$ne": "<>", "$lt": "<", "$lte": "<=", "$gt": ">", "$gte": ">="}
	// Array parameters must be typed slices for pgx to encode them.
	typed := func() any {
		if col == "e.id" {
			out := make([]int64, len(vals))
			for i, v := range vals {
				out[i] = v.(int64)
			}
			return out
		}
		out := make([]time.Time, len(vals))
		for i, v := range vals {
			out[i] = v.(time.Time)
		}
		return out
	}
	switch f.Op {
	case "$in":
		return col + " = ANY(" + b.arg(typed()) + ")", nil
	case "$notIn":
		return "NOT (" + col + " = ANY(" + b.arg(typed()) + "))", nil
	case "$between":
		if len(vals) != 2 {
			return "", qerr("$between needs exactly two values")
		}
		return col + " BETWEEN " + b.arg(vals[0]) + " AND " + b.arg(vals[1]), nil
	}
	if op, ok := ops[f.Op]; ok {
		return col + " " + op + " " + b.arg(vals[0]), nil
	}
	return "", qerr("%s does not apply to %s", f.Op, f.Field)
}

// linkSQL filters on related ids: $eq, $ne, $in, $notIn, $null, $notNull.
func (b *sqlBuilder) linkSQL(f Filter, sub, idCol string) (string, error) {
	switch f.Op {
	case "$null", "$notNull":
		want, err := parseBoolValue(f.Value[0])
		if err != nil {
			return "", err
		}
		if (f.Op == "$null") == want {
			return "NOT EXISTS (" + sub + ")", nil
		}
		return "EXISTS (" + sub + ")", nil
	}
	ids := make([]int64, len(f.Value))
	for i, raw := range f.Value {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return "", qerr("%q expects ids", f.Field)
		}
		ids[i] = n
	}
	switch f.Op {
	case "$eq":
		return "EXISTS (" + sub + " AND " + idCol + " = " + b.arg(ids[0]) + ")", nil
	case "$ne":
		return "NOT EXISTS (" + sub + " AND " + idCol + " = " + b.arg(ids[0]) + ")", nil
	case "$in":
		return "EXISTS (" + sub + " AND " + idCol + " = ANY(" + b.arg(ids) + "))", nil
	case "$notIn":
		return "NOT EXISTS (" + sub + " AND " + idCol + " = ANY(" + b.arg(ids) + "))", nil
	}
	return "", qerr("%s does not apply to %q", f.Op, f.Field)
}

func orderSQL(ct *schema.ContentType, sorts []Sort) string {
	if len(sorts) == 0 {
		return "e.id ASC"
	}
	var parts []string
	hasID := false
	for _, s := range sorts {
		dir := " ASC NULLS LAST"
		if s.Desc {
			dir = " DESC NULLS LAST"
		}
		if col, ok := systemColumns[s.Field]; ok {
			parts = append(parts, col+dir)
			hasID = hasID || s.Field == "id"
			continue
		}
		f, ok := ct.Field(s.Field)
		if !ok {
			continue
		}
		switch f.Type {
		case schema.TypeString, schema.TypeText, schema.TypeEmail, schema.TypeEnum:
			// Case-insensitive ordering regardless of the database collation.
			parts = append(parts, "lower(e.data->>"+lit(s.Field)+")"+dir)
		default:
			// JSONB ordering: numbers numerically, booleans false<true, ISO dates lexically.
			parts = append(parts, "e.data->"+lit(s.Field)+dir)
		}
	}
	if !hasID {
		parts = append(parts, "e.id ASC") // stable pagination
	}
	return strings.Join(parts, ", ")
}
