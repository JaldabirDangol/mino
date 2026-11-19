package content

import "mino/internal/schema"

// BuildWhereForTest exposes filter SQL generation so tests can check value conversion errors.
func BuildWhereForTest(ct *schema.ContentType, q Query) (string, error) {
	b := &sqlBuilder{}
	return b.filterSQL(ct, q.Filter)
}
