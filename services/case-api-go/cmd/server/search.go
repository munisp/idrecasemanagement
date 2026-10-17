package main

import (
	"fmt"
	"strings"
)

// Search on server-paged lists (?q=): one ILIKE pattern matched against the columns a list names. LIKE wildcards
// in the term are taken literally, and the term is capped at 100 characters.

// searchLike: the term as an ILIKE pattern, "" when there is no term.
func searchLike(raw string) string {
	t := strings.TrimSpace(raw)
	if t == "" {
		return ""
	}
	if len(t) > 100 {
		t = t[:100]
	}
	return "%" + strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(t) + "%"
}

// searchClause: " AND (col ILIKE $n OR ...)" -- n is the position of the searchLike pattern in the query's args.
// cols are SQL expressions (text, or cast to text by the caller).
func searchClause(n int, cols ...string) string {
	parts := make([]string, len(cols))
	for i, c := range cols {
		parts[i] = fmt.Sprintf("coalesce(%s,'') ILIKE $%d", c, n)
	}
	return " AND (" + strings.Join(parts, " OR ") + ")"
}
