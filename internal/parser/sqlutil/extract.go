package sqlutil

import (
	"strings"

	"github.com/maraichr/lattice/internal/parser"
)

// ExtractTableRefs parses SQL text and returns RawReferences for table names
// found after FROM, JOIN, INTO, UPDATE, DELETE, MERGE keywords.
// fromSymbol is the qualified name of the enclosing symbol.
// defaultSchema is prepended to ToQualified (e.g. "dbo").
func ExtractTableRefs(sql string, line int, fromSymbol, defaultSchema string) []parser.RawReference {
	var refs []parser.RawReference
	upper := strings.ToUpper(sql)
	keywords := []string{"FROM", "JOIN", "INTO", "UPDATE", "DELETE", "MERGE"}

	for _, kw := range keywords {
		idx := 0
		for {
			pos := strings.Index(upper[idx:], kw+" ")
			if pos < 0 {
				break
			}
			pos += idx + len(kw) + 1
			rest := strings.TrimSpace(sql[pos:])
			end := strings.IndexAny(rest, " \t\n\r,;)(")
			tableName := rest
			if end > 0 {
				tableName = rest[:end]
			}
			tableName = strings.TrimSpace(tableName)
			if tableName != "" && !IsSQLKeyword(tableName) {
				ref := parser.RawReference{
					FromSymbol:    fromSymbol,
					ToName:        tableName,
					ReferenceType: inferEdgeType(kw),
					Line:          line,
				}
				if strings.Contains(tableName, ".") {
					ref.ToQualified = tableName
				} else if defaultSchema != "" {
					ref.ToQualified = defaultSchema + "." + tableName
				}
				refs = append(refs, ref)
			}
			idx = pos
		}
	}

	// Handle EXEC/EXECUTE patterns for stored procedure calls
	for _, execKw := range []string{"EXEC ", "EXECUTE "} {
		idx := 0
		for {
			pos := strings.Index(upper[idx:], execKw)
			if pos < 0 {
				break
			}
			absPos := idx + pos
			// Check word boundary on left
			if absPos > 0 {
				ch := upper[absPos-1]
				if ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '_' {
					idx = absPos + len(execKw)
					continue
				}
			}
			pos = absPos + len(execKw)
			rest := strings.TrimSpace(sql[pos:])
			end := strings.IndexAny(rest, " \t\n\r,;)(")
			procName := rest
			if end > 0 {
				procName = rest[:end]
			}
			procName = strings.TrimSpace(procName)
			if procName != "" && !IsSQLKeyword(procName) {
				ref := parser.RawReference{
					FromSymbol:    fromSymbol,
					ToName:        procName,
					ReferenceType: "calls",
					Line:          line,
				}
				if defaultSchema != "" && !strings.Contains(procName, ".") {
					ref.ToQualified = defaultSchema + "." + procName
				} else {
					ref.ToQualified = procName
				}
				refs = append(refs, ref)
			}
			idx = pos
		}
	}

	return refs
}

// inferEdgeType returns the appropriate edge type based on the SQL keyword context.
func inferEdgeType(keyword string) string {
	switch keyword {
	case "INTO", "UPDATE", "DELETE", "MERGE":
		return "writes_to"
	default:
		return "uses_table"
	}
}

// LooksLikeSQL returns true if the string contains SQL keywords.
// Uses word-boundary checking to avoid false positives on identifiers like "DeleteUser".
func LooksLikeSQL(s string) bool {
	upper := strings.ToUpper(strings.TrimSpace(s))
	for _, kw := range []string{"SELECT", "INSERT", "UPDATE", "DELETE", "FROM", "EXEC", "EXECUTE", "CREATE", "ALTER", "DROP", "MERGE"} {
		if containsSQLKeyword(upper, kw) {
			return true
		}
	}
	return false
}

// containsSQLKeyword checks if kw appears as a word boundary in s.
func containsSQLKeyword(upper, kw string) bool {
	idx := 0
	for {
		pos := strings.Index(upper[idx:], kw)
		if pos < 0 {
			return false
		}
		absPos := idx + pos
		// Left boundary: must be at start or preceded by non-alnum
		if absPos > 0 {
			ch := upper[absPos-1]
			if ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '_' {
				idx = absPos + len(kw)
				continue
			}
		}
		// Right boundary
		endPos := absPos + len(kw)
		if endPos < len(upper) {
			ch := upper[endPos]
			if ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '_' {
				idx = absPos + len(kw)
				continue
			}
		}
		return true
	}
}

// IsSQLKeyword returns true if the given string is a common SQL keyword.
func IsSQLKeyword(s string) bool {
	kw := map[string]bool{
		"SELECT": true, "FROM": true, "WHERE": true, "AND": true,
		"OR": true, "SET": true, "VALUES": true, "AS": true,
		"ON": true, "IN": true, "NOT": true, "NULL": true,
		"INTO": true, "JOIN": true, "LEFT": true, "RIGHT": true,
		"INNER": true, "OUTER": true, "CROSS": true, "FULL": true,
		"GROUP": true, "ORDER": true, "BY": true, "HAVING": true,
		"UNION": true, "ALL": true, "EXISTS": true, "BETWEEN": true,
		"LIKE": true, "IS": true, "CASE": true, "WHEN": true,
		"THEN": true, "ELSE": true, "END": true, "BEGIN": true,
		"DECLARE": true, "TABLE": true, "WITH": true, "TOP": true,
		"DISTINCT": true, "INSERT": true, "UPDATE": true, "DELETE": true,
	}
	return kw[strings.ToUpper(s)]
}
