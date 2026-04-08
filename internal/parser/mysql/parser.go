package mysql

import (
	"regexp"
	"strings"

	"github.com/maraichr/lattice/internal/parser"
)

// Parser implements a regex-based MySQL parser that extracts symbols and references.
type Parser struct{}

func New() *Parser {
	return &Parser{}
}

func (p *Parser) Languages() []string {
	return []string{"mysql"}
}

func (p *Parser) Parse(input parser.FileInput) (*parser.ParseResult, error) {
	content := string(input.Content)

	// Split by DELIMITER blocks first, then process each batch
	batches := splitDelimiterBatches(content)

	var allSymbols []parser.Symbol
	var allRefs []parser.RawReference
	var allColRefs []parser.ColumnReference

	syntheticContext := deriveFileContext(input.Path)

	for _, batch := range batches {
		syms, refs, colRefs := parseBatch(batch, syntheticContext, input.SkipColumnLineage)
		allSymbols = append(allSymbols, syms...)
		allRefs = append(allRefs, refs...)
		allColRefs = append(allColRefs, colRefs...)
	}

	return &parser.ParseResult{
		Symbols:          allSymbols,
		References:       allRefs,
		ColumnReferences: allColRefs,
	}, nil
}

// deriveFileContext creates a synthetic symbol name from a file path.
func deriveFileContext(path string) string {
	if path == "" {
		return ""
	}
	name := path
	if idx := strings.LastIndexAny(name, "/\\"); idx >= 0 {
		name = name[idx+1:]
	}
	if idx := strings.LastIndex(name, "."); idx >= 0 {
		name = name[:idx]
	}
	return "__file__:" + name
}

// splitDelimiterBatches splits MySQL scripts by DELIMITER and statement terminators.
func splitDelimiterBatches(content string) []string {
	var batches []string
	delimiter := ";"
	remaining := content

	delimRe := regexp.MustCompile(`(?im)^\s*DELIMITER\s+(\S+)\s*$`)

	for remaining != "" {
		loc := delimRe.FindStringIndex(remaining)
		if loc == nil {
			// No more DELIMITER directives — split by current delimiter
			stmts := splitByDelimiter(remaining, delimiter)
			batches = append(batches, stmts...)
			break
		}

		// Process content before the DELIMITER directive
		before := remaining[:loc[0]]
		if strings.TrimSpace(before) != "" {
			stmts := splitByDelimiter(before, delimiter)
			batches = append(batches, stmts...)
		}

		// Extract new delimiter
		match := delimRe.FindStringSubmatch(remaining[loc[0]:])
		newDelim := match[1]
		remaining = remaining[loc[1]:]

		if newDelim == ";" {
			delimiter = ";"
			continue
		}

		// Find the closing DELIMITER ; and extract the block
		closeLoc := delimRe.FindStringIndex(remaining)
		if closeLoc != nil {
			block := remaining[:closeLoc[0]]
			if strings.TrimSpace(block) != "" {
				// Split by custom delimiter
				stmts := splitByDelimiter(block, newDelim)
				batches = append(batches, stmts...)
			}
			remaining = remaining[closeLoc[1]:]
			delimiter = ";"
		} else {
			// No closing DELIMITER — treat rest as one batch
			stmts := splitByDelimiter(remaining, newDelim)
			batches = append(batches, stmts...)
			break
		}
	}

	return batches
}

func splitByDelimiter(content, delim string) []string {
	parts := strings.Split(content, delim)
	var result []string
	for _, p := range parts {
		trimmed := strings.TrimSpace(p)
		if trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}

// Regex patterns for MySQL DDL
var (
	reCreateTable = regexp.MustCompile(`(?i)CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?` + "`?" + `(\w+)` + "`?" + `(?:\.` + "`?" + `(\w+)` + "`?" + `)?\s*\(`)
	reCreateView  = regexp.MustCompile(`(?i)CREATE\s+(?:OR\s+REPLACE\s+)?(?:ALGORITHM\s*=\s*\w+\s+)?(?:DEFINER\s*=\s*\S+\s+)?(?:SQL\s+SECURITY\s+\w+\s+)?VIEW\s+` + "`?" + `(\w+)` + "`?" + `(?:\.` + "`?" + `(\w+)` + "`?" + `)?\s+AS\s+`)
	reCreateProc  = regexp.MustCompile(`(?i)CREATE\s+(?:DEFINER\s*=\s*\S+\s+)?PROCEDURE\s+` + "`?" + `(\w+)` + "`?" + `(?:\.` + "`?" + `(\w+)` + "`?" + `)?\s*\(`)
	reCreateFunc  = regexp.MustCompile(`(?i)CREATE\s+(?:DEFINER\s*=\s*\S+\s+)?FUNCTION\s+` + "`?" + `(\w+)` + "`?" + `(?:\.` + "`?" + `(\w+)` + "`?" + `)?\s*\(`)
	reCreateTrig  = regexp.MustCompile(`(?i)CREATE\s+(?:DEFINER\s*=\s*\S+\s+)?TRIGGER\s+` + "`?" + `(\w+)` + "`?" + `(?:\.` + "`?" + `(\w+)` + "`?" + `)?\s+`)

	// DML reference patterns
	reSelectFrom  = regexp.MustCompile(`(?i)\bFROM\s+` + tableNamePattern())
	reJoin        = regexp.MustCompile(`(?i)\bJOIN\s+` + tableNamePattern())
	reInsertInto  = regexp.MustCompile(`(?i)\bINSERT\s+(?:INTO\s+)?` + tableNamePattern())
	reUpdate      = regexp.MustCompile(`(?i)\bUPDATE\s+` + tableNamePattern())
	reDeleteFrom  = regexp.MustCompile(`(?i)\bDELETE\s+FROM\s+` + tableNamePattern())
	reCall        = regexp.MustCompile(`(?i)\bCALL\s+` + tableNamePattern())

	// Trigger ON table
	reTriggerOn = regexp.MustCompile(`(?i)\bON\s+` + tableNamePattern())
)

func tableNamePattern() string {
	return "`?" + `(\w+)` + "`?" + `(?:\.` + "`?" + `(\w+)` + "`?" + `)?`
}

func parseBatch(batch, fileContext string, skipColumnLineage bool) ([]parser.Symbol, []parser.RawReference, []parser.ColumnReference) {
	var symbols []parser.Symbol
	var refs []parser.RawReference
	var colRefs []parser.ColumnReference

	upper := strings.ToUpper(batch)

	switch {
	case strings.Contains(upper, "CREATE") && reCreateTable.MatchString(batch):
		sym, cr := parseCreateTable(batch)
		if sym != nil {
			symbols = append(symbols, *sym)
		}
		colRefs = append(colRefs, cr...)

	case strings.Contains(upper, "CREATE") && reCreateView.MatchString(batch):
		sym, r, cr := parseCreateView(batch, skipColumnLineage)
		if sym != nil {
			symbols = append(symbols, *sym)
		}
		refs = append(refs, r...)
		colRefs = append(colRefs, cr...)

	case strings.Contains(upper, "CREATE") && reCreateProc.MatchString(batch):
		sym, r, cr := parseCreateProcOrFunc(batch, "procedure", reCreateProc, skipColumnLineage)
		if sym != nil {
			symbols = append(symbols, *sym)
		}
		refs = append(refs, r...)
		colRefs = append(colRefs, cr...)

	case strings.Contains(upper, "CREATE") && reCreateFunc.MatchString(batch):
		sym, r, cr := parseCreateProcOrFunc(batch, "function", reCreateFunc, skipColumnLineage)
		if sym != nil {
			symbols = append(symbols, *sym)
		}
		refs = append(refs, r...)
		colRefs = append(colRefs, cr...)

	case strings.Contains(upper, "CREATE") && reCreateTrig.MatchString(batch):
		sym, r := parseCreateTrigger(batch)
		if sym != nil {
			symbols = append(symbols, *sym)
		}
		refs = append(refs, r...)

	default:
		// Top-level DML — extract refs with file context
		if fileContext != "" {
			r := extractBodyRefs(batch, fileContext)
			if len(r) > 0 {
				refs = append(refs, r...)
			}
		}
	}

	return symbols, refs, colRefs
}

func parseCreateTable(batch string) (*parser.Symbol, []parser.ColumnReference) {
	m := reCreateTable.FindStringSubmatch(batch)
	if m == nil {
		return nil, nil
	}

	name := resolveTableName(m[1], m[2])
	line := countLinesBefore(batch, reCreateTable.FindStringIndex(batch)[0]) + 1

	sym := &parser.Symbol{
		Name:          stripBackticks(unqualify(name)),
		QualifiedName: name,
		Kind:          "table",
		Language:      "mysql",
		StartLine:     line,
	}

	// Extract columns from the CREATE TABLE body
	parenStart := strings.Index(batch, "(")
	if parenStart >= 0 {
		body := extractParenBody(batch[parenStart:])
		sym.Children = parseColumnDefs(body, name)
	}

	sym.EndLine = line + strings.Count(batch, "\n")
	return sym, nil
}

func parseCreateView(batch string, skipColumnLineage bool) (*parser.Symbol, []parser.RawReference, []parser.ColumnReference) {
	m := reCreateView.FindStringSubmatch(batch)
	if m == nil {
		return nil, nil, nil
	}

	name := resolveTableName(m[1], m[2])
	line := countLinesBefore(batch, reCreateView.FindStringIndex(batch)[0]) + 1

	sym := &parser.Symbol{
		Name:          stripBackticks(unqualify(name)),
		QualifiedName: name,
		Kind:          "view",
		Language:      "mysql",
		StartLine:     line,
		EndLine:       line + strings.Count(batch, "\n"),
	}

	// Extract references from the AS SELECT body
	asIdx := reCreateView.FindStringIndex(batch)
	body := batch[asIdx[1]:]
	refs := extractBodyRefs(body, name)

	return sym, refs, nil
}

func parseCreateProcOrFunc(batch, kind string, re *regexp.Regexp, skipColumnLineage bool) (*parser.Symbol, []parser.RawReference, []parser.ColumnReference) {
	m := re.FindStringSubmatch(batch)
	if m == nil {
		return nil, nil, nil
	}

	name := resolveTableName(m[1], m[2])
	line := countLinesBefore(batch, re.FindStringIndex(batch)[0]) + 1

	// Extract signature (parameters between first parentheses)
	sig := ""
	if parenStart := strings.Index(batch, "("); parenStart >= 0 {
		sigBody := extractParenBody(batch[parenStart:])
		sig = "(" + sigBody + ")"
	}

	sym := &parser.Symbol{
		Name:          stripBackticks(unqualify(name)),
		QualifiedName: name,
		Kind:          kind,
		Language:      "mysql",
		StartLine:     line,
		EndLine:       line + strings.Count(batch, "\n"),
		Signature:     sig,
	}

	// Extract body refs — everything after BEGIN (or after param list for functions)
	bodyStart := findBodyStart(batch)
	body := ""
	if bodyStart >= 0 {
		body = batch[bodyStart:]
	}

	refs := extractBodyRefs(body, name)

	// Column-level lineage from INSERT...SELECT
	var colRefs []parser.ColumnReference
	if !skipColumnLineage {
		colRefs = extractInsertSelectLineage(body, name)
	}

	return sym, refs, colRefs
}

func parseCreateTrigger(batch string) (*parser.Symbol, []parser.RawReference) {
	m := reCreateTrig.FindStringSubmatch(batch)
	if m == nil {
		return nil, nil
	}

	name := resolveTableName(m[1], m[2])
	line := countLinesBefore(batch, reCreateTrig.FindStringIndex(batch)[0]) + 1

	sym := &parser.Symbol{
		Name:          stripBackticks(unqualify(name)),
		QualifiedName: name,
		Kind:          "trigger",
		Language:      "mysql",
		StartLine:     line,
		EndLine:       line + strings.Count(batch, "\n"),
	}

	var refs []parser.RawReference

	// Find ON table
	trigEnd := reCreateTrig.FindStringIndex(batch)[1]
	remaining := batch[trigEnd:]
	onMatch := reTriggerOn.FindStringSubmatch(remaining)
	if onMatch != nil {
		tableName := resolveTableName(onMatch[1], onMatch[2])
		refs = append(refs, parser.RawReference{
			FromSymbol:    name,
			ToName:        stripBackticks(unqualify(tableName)),
			ToQualified:   tableName,
			ReferenceType: "uses_table",
			Line:          line,
		})
	}

	// Extract body refs
	bodyStart := findBodyStart(batch)
	if bodyStart >= 0 {
		bodyRefs := extractBodyRefs(batch[bodyStart:], name)
		refs = append(refs, bodyRefs...)
	}

	return sym, refs
}

// extractBodyRefs extracts DML references from a SQL body.
func extractBodyRefs(body, context string) []parser.RawReference {
	if body == "" || context == "" {
		return nil
	}

	var refs []parser.RawReference
	seen := make(map[string]bool)

	addRef := func(matches [][]string, refType string) {
		for _, m := range matches {
			name := resolveTableName(m[1], m[2])
			key := refType + ":" + name
			if seen[key] || isMySQLKeyword(stripBackticks(unqualify(name))) {
				continue
			}
			seen[key] = true
			refs = append(refs, parser.RawReference{
				FromSymbol:    context,
				ToName:        stripBackticks(unqualify(name)),
				ToQualified:   name,
				ReferenceType: refType,
			})
		}
	}

	addRef(reSelectFrom.FindAllStringSubmatch(body, -1), "reads_from")
	addRef(reJoin.FindAllStringSubmatch(body, -1), "joins")
	addRef(reInsertInto.FindAllStringSubmatch(body, -1), "writes_to")
	addRef(reUpdate.FindAllStringSubmatch(body, -1), "writes_to")
	addRef(reDeleteFrom.FindAllStringSubmatch(body, -1), "writes_to")
	addRef(reCall.FindAllStringSubmatch(body, -1), "calls")

	return refs
}

// extractInsertSelectLineage extracts column-level lineage from INSERT INTO ... SELECT patterns.
func extractInsertSelectLineage(body, context string) []parser.ColumnReference {
	if body == "" {
		return nil
	}

	re := regexp.MustCompile(`(?i)INSERT\s+INTO\s+` + "`?" + `(\w+)` + "`?" + `\s*\(([^)]+)\)\s*SELECT\s+(.+?)\s+FROM\s+`)
	matches := re.FindAllStringSubmatch(body, -1)
	if len(matches) == 0 {
		return nil
	}

	var colRefs []parser.ColumnReference
	for _, m := range matches {
		targetTable := stripBackticks(m[1])
		targetCols := splitAndTrim(m[2])
		selectCols := splitAndTrim(m[3])

		for i, tc := range targetCols {
			if i < len(selectCols) {
				sc := stripBackticks(strings.TrimSpace(selectCols[i]))
				derivation := "direct_copy"
				if strings.Contains(sc, "(") || strings.ContainsAny(sc, "+-*/") {
					derivation = "transform"
				}
				colRefs = append(colRefs, parser.ColumnReference{
					SourceColumn:   sc,
					TargetColumn:   targetTable + "." + stripBackticks(tc),
					DerivationType: derivation,
					Expression:     sc,
					Context:        context,
				})
			}
		}
	}

	return colRefs
}

// --- Helpers ---

func resolveTableName(first, second string) string {
	first = stripBackticks(first)
	second = stripBackticks(second)
	if second != "" {
		return first + "." + second
	}
	return first
}

func stripBackticks(s string) string {
	return strings.Trim(s, "`")
}

func unqualify(name string) string {
	parts := strings.Split(name, ".")
	return parts[len(parts)-1]
}

func parseColumnDefs(body, tableName string) []parser.Symbol {
	var cols []parser.Symbol
	lines := strings.Split(body, ",")

	colRe := regexp.MustCompile(`(?i)^\s*` + "`?" + `(\w+)` + "`?" + `\s+(\w+)`)
	constraintRe := regexp.MustCompile(`(?i)^\s*(PRIMARY\s+KEY|UNIQUE\s+KEY|KEY|INDEX|CONSTRAINT|FOREIGN\s+KEY|CHECK)`)

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		// Skip constraints
		if constraintRe.MatchString(trimmed) {
			continue
		}
		m := colRe.FindStringSubmatch(trimmed)
		if m != nil {
			colName := stripBackticks(m[1])
			if !isMySQLKeyword(colName) {
				cols = append(cols, parser.Symbol{
					Name:          colName,
					QualifiedName: tableName + "." + colName,
					Kind:          "column",
					Language:      "mysql",
				})
			}
		}
	}

	return cols
}

func extractParenBody(s string) string {
	depth := 0
	start := -1
	for i, ch := range s {
		if ch == '(' {
			if depth == 0 {
				start = i + 1
			}
			depth++
		} else if ch == ')' {
			depth--
			if depth == 0 {
				return s[start:i]
			}
		}
	}
	if start >= 0 {
		return s[start:]
	}
	return ""
}

func findBodyStart(batch string) int {
	upper := strings.ToUpper(batch)
	idx := strings.Index(upper, "\nBEGIN")
	if idx >= 0 {
		return idx + 6
	}
	idx = strings.Index(upper, " BEGIN")
	if idx >= 0 {
		return idx + 6
	}
	return -1
}

func countLinesBefore(text string, pos int) int {
	return strings.Count(text[:pos], "\n")
}

func splitAndTrim(s string) []string {
	parts := strings.Split(s, ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

func isMySQLKeyword(s string) bool {
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
		"DECLARE": true, "TABLE": true, "WITH": true, "IF": true,
		"EACH": true, "ROW": true, "FOR": true, "NEW": true,
		"OLD": true, "BEFORE": true, "AFTER": true, "INSERT": true,
		"UPDATE": true, "DELETE": true, "LOW_PRIORITY": true,
		"DUAL": true,
	}
	return kw[strings.ToUpper(s)]
}
