package delphi

import (
	"path/filepath"
	"regexp"
	"strings"

	"github.com/maraichr/lattice/internal/parser"
	"github.com/maraichr/lattice/internal/parser/sqlutil"
)

// Parser implements a parser for Delphi/Object Pascal files.
type Parser struct{}

func New() *Parser {
	return &Parser{}
}

func (p *Parser) Languages() []string {
	return []string{"delphi", "pascal"}
}

func (p *Parser) Parse(input parser.FileInput) (*parser.ParseResult, error) {
	ext := strings.ToLower(filepath.Ext(input.Path))

	// DFM files get special handling
	if ext == ".dfm" {
		symbols, refs := ParseDFM(string(input.Content), 0)
		return &parser.ParseResult{
			Symbols:    symbols,
			References: refs,
		}, nil
	}

	// Pascal source files (.pas, .dpr)
	return parsePascal(input)
}

func parsePascal(input parser.FileInput) (*parser.ParseResult, error) {
	content := string(input.Content)
	lines := strings.Split(content, "\n")

	var symbols []parser.Symbol
	var refs []parser.RawReference

	unitName := ""
	inInterface := false
	inImplementation := false

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		lower := strings.ToLower(trimmed)
		lineNum := i + 1

		// Unit declaration
		if strings.HasPrefix(lower, "unit ") {
			name := extractIdentAfterKeyword(trimmed, "unit")
			if name != "" {
				unitName = name
				symbols = append(symbols, parser.Symbol{
					Name:          name,
					QualifiedName: name,
					Kind:          "module",
					Language:      "delphi",
					StartLine:     lineNum,
					EndLine:       len(lines),
				})
			}
		}

		// Program declaration
		if strings.HasPrefix(lower, "program ") {
			name := extractIdentAfterKeyword(trimmed, "program")
			if name != "" {
				unitName = name
				symbols = append(symbols, parser.Symbol{
					Name:          name,
					QualifiedName: name,
					Kind:          "module",
					Language:      "delphi",
					StartLine:     lineNum,
					EndLine:       len(lines),
				})
			}
		}

		// Track sections
		if lower == "interface" {
			inInterface = true
			inImplementation = false
		}
		if lower == "implementation" {
			inInterface = false
			inImplementation = true
		}

		// Uses clause
		if strings.HasPrefix(lower, "uses") {
			usesRefs := parseUsesClause(lines, i)
			refs = append(refs, usesRefs...)
		}

		// Type declarations
		// TMyClass = class(TParent)
		if classMatch := matchClassDecl(trimmed); classMatch != nil {
			qname := qualify(unitName, classMatch.name)
			sym := parser.Symbol{
				Name:          classMatch.name,
				QualifiedName: qname,
				Kind:          classMatch.kind,
				Language:      "delphi",
				StartLine:     lineNum,
				EndLine:       findPascalEnd(lines, i),
			}
			symbols = append(symbols, sym)

			if classMatch.parent != "" {
				refs = append(refs, parser.RawReference{
					FromSymbol:    qname,
					ToName:        classMatch.parent,
					ReferenceType: "inherits",
					Line:          lineNum,
				})
			}
		}

		// Procedure/function declarations
		if strings.HasPrefix(lower, "procedure ") || strings.HasPrefix(lower, "function ") ||
			strings.HasPrefix(lower, "class procedure ") || strings.HasPrefix(lower, "class function ") ||
			strings.HasPrefix(lower, "constructor ") || strings.HasPrefix(lower, "destructor ") {

			name, sig := parsePascalProcDecl(trimmed)
			if name != "" {
				kind := "procedure"
				if strings.Contains(lower, "function") {
					kind = "function"
				} else if strings.Contains(lower, "constructor") {
					kind = "method"
				} else if strings.Contains(lower, "destructor") {
					kind = "method"
				}

				qname := qualify(unitName, name)
				endLine := lineNum
				if inImplementation {
					endLine = findPascalProcEnd(lines, i)
				}
				symbols = append(symbols, parser.Symbol{
					Name:          name,
					QualifiedName: qname,
					Kind:          kind,
					Language:      "delphi",
					StartLine:     lineNum,
					EndLine:       endLine,
					Signature:     sig,
				})
			}
		}

		// Property declarations (in class body)
		if (inInterface || inImplementation) && strings.HasPrefix(lower, "property ") {
			name := extractIdentAfterKeyword(trimmed, "property")
			if name != "" {
				symbols = append(symbols, parser.Symbol{
					Name:          name,
					QualifiedName: qualify(unitName, name),
					Kind:          "property",
					Language:      "delphi",
					StartLine:     lineNum,
					EndLine:       lineNum,
				})
			}
		}

		// Include directives {$I filename.inc}
		if includeMatch := regexp.MustCompile(`\{\$[Ii]\s+(\S+)\}`).FindStringSubmatch(trimmed); len(includeMatch) >= 2 {
			refs = append(refs, parser.RawReference{
				ToName:        includeMatch[1],
				ReferenceType: "imports",
				Line:          lineNum,
			})
		}
	}

	_ = inInterface // used above

	// Post-pass: extract SQL references from Pascal source
	sqlRefs := extractPascalSQLRefs(lines, symbols)
	refs = append(refs, sqlRefs...)

	return &parser.ParseResult{
		Symbols:    symbols,
		References: refs,
	}, nil
}

// extractPascalSQLRefs detects SQL patterns in Delphi/Pascal source code:
// SQL.Text assignment, SQL.Add accumulation, CommandText assignment, and
// stored procedure bindings (StoredProcName/ProcedureName). Component
// prefixes are optional so code inside with-blocks is also covered.
func extractPascalSQLRefs(lines []string, symbols []parser.Symbol) []parser.RawReference {
	var refs []parser.RawReference

	// Build procedure/function ranges for FromSymbol resolution
	findEnclosing := func(lineNum int) string {
		best := ""
		bestSpan := 1<<31 - 1
		for _, s := range symbols {
			if (s.Kind == "procedure" || s.Kind == "function" || s.Kind == "method") &&
				lineNum >= s.StartLine && lineNum <= s.EndLine {
				span := s.EndLine - s.StartLine
				if span < bestSpan {
					bestSpan = span
					best = s.QualifiedName
				}
			}
		}
		return best
	}

	// Regex patterns for SQL assignments
	sqlTextAssign := regexp.MustCompile(`(?i)(?:(\w+)\.)?SQL\.Text\s*:=\s*(.+)`)
	sqlAdd := regexp.MustCompile(`(?i)(?:(\w+)\.)?SQL\.Add\s*\(\s*(.+)\)`)
	commandTextAssign := regexp.MustCompile(`(?i)(?:(\w+)\.)?CommandText\s*:=\s*(.+)`)
	storedProcAssign := regexp.MustCompile(`(?i)(?:(\w+)\.)?(?:StoredProcName|ProcedureName)\s*:=\s*'([^']+)'`)

	// Multi-line SQL.Text concatenation tracking
	var sqlTextBuilder strings.Builder
	sqlTextStartLine := 0
	inSQLConcat := false

	// Consecutive SQL.Add calls build one statement, so clauses split across
	// calls (SELECT on one line, FROM on the next) keep their table refs.
	var addBuilder strings.Builder
	addComponent := ""
	addStartLine := 0
	addActive := false

	flushAdd := func() {
		if !addActive {
			return
		}
		addActive = false
		fullSQL := addBuilder.String()
		addBuilder.Reset()
		if sqlutil.LooksLikeSQL(fullSQL) {
			from := findEnclosing(addStartLine)
			tableRefs := sqlutil.ExtractTableRefs(fullSQL, addStartLine, from, "dbo")
			for j := range tableRefs {
				tableRefs[j].Confidence = 0.85
			}
			refs = append(refs, tableRefs...)
		}
	}

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		lineNum := i + 1

		// Continue multi-line SQL.Text concatenation
		if inSQLConcat {
			sqlStr := extractPascalStrings(trimmed)
			sqlTextBuilder.WriteString(" " + sqlStr)
			if !isContinued(trimmed) {
				inSQLConcat = false
				fullSQL := sqlTextBuilder.String()
				if sqlutil.LooksLikeSQL(fullSQL) {
					from := findEnclosing(sqlTextStartLine)
					tableRefs := sqlutil.ExtractTableRefs(fullSQL, sqlTextStartLine, from, "dbo")
					for j := range tableRefs {
						tableRefs[j].Confidence = 0.85
					}
					refs = append(refs, tableRefs...)
				}
			}
			continue
		}

		// SQL.Add('...') — accumulate consecutive calls to the same component
		if m := sqlAdd.FindStringSubmatch(trimmed); m != nil {
			component := strings.ToLower(m[1])
			if addActive && component != addComponent {
				flushAdd()
			}
			if !addActive {
				addActive = true
				addComponent = component
				addStartLine = lineNum
			}
			addBuilder.WriteString(extractPascalStrings(m[2]))
			addBuilder.WriteString(" ")
			continue
		}
		// Any other statement ends a SQL.Add accumulation.
		flushAdd()

		// StoredProc1.StoredProcName := 'GetUsers'
		// ADOStoredProc1.ProcedureName := 'GetUsers;1'
		if m := storedProcAssign.FindStringSubmatch(trimmed); m != nil {
			if ref, ok := procCallRef(m[2], findEnclosing(lineNum), lineNum); ok {
				refs = append(refs, ref)
			}
			continue
		}

		// SQL.Text := 'SELECT * FROM ...'
		// May be multi-line: SQL.Text := 'SELECT * ' + #13#10 + 'FROM table'
		if m := sqlTextAssign.FindStringSubmatch(trimmed); m != nil {
			rest := m[2]
			sqlStr := extractPascalStrings(rest)
			if isContinued(rest) {
				// Multi-line concatenation
				sqlTextBuilder.Reset()
				sqlTextBuilder.WriteString(sqlStr)
				sqlTextStartLine = lineNum
				inSQLConcat = true
			} else if sqlutil.LooksLikeSQL(sqlStr) {
				from := findEnclosing(lineNum)
				tableRefs := sqlutil.ExtractTableRefs(sqlStr, lineNum, from, "dbo")
				for j := range tableRefs {
					tableRefs[j].Confidence = 0.9
				}
				refs = append(refs, tableRefs...)
			}
			continue
		}

		// CommandText := 'EXEC dbo.GetUser', 'SELECT ...', or a bare proc name
		if m := commandTextAssign.FindStringSubmatch(trimmed); m != nil {
			sqlStr := extractPascalStrings(m[2])
			from := findEnclosing(lineNum)
			if sqlutil.LooksLikeSQL(sqlStr) {
				tableRefs := sqlutil.ExtractTableRefs(sqlStr, lineNum, from, "dbo")
				for j := range tableRefs {
					tableRefs[j].Confidence = 0.9
				}
				refs = append(refs, tableRefs...)
			} else if ref, ok := procCallRef(sqlStr, from, lineNum); ok {
				refs = append(refs, ref)
			}
		}
	}
	flushAdd()

	return refs
}

var procNameRe = regexp.MustCompile(`^[A-Za-z_][\w$]*(?:\.[A-Za-z_][\w$]*)*$`)

// procCallRef builds a "calls" reference to a stored procedure. ADO appends
// an overload ordinal to procedure names ('GetUsers;1') which is stripped.
func procCallRef(name, from string, line int) (parser.RawReference, bool) {
	name = strings.TrimSpace(name)
	if idx := strings.IndexByte(name, ';'); idx >= 0 {
		name = name[:idx]
	}
	if !procNameRe.MatchString(name) {
		return parser.RawReference{}, false
	}
	ref := parser.RawReference{
		FromSymbol:    from,
		ToName:        name,
		ReferenceType: "calls",
		Line:          line,
	}
	if strings.Contains(name, ".") {
		ref.ToQualified = name
	} else {
		ref.ToQualified = "dbo." + name
	}
	return ref, true
}

// extractPascalStrings extracts string content from Pascal string expressions.
// Handles: 'string literal', concatenation with +, #13#10 line breaks.
func extractPascalStrings(expr string) string {
	var result strings.Builder
	i := 0
	for i < len(expr) {
		if expr[i] == '\'' {
			// Find closing quote (Pascal uses '' for escaped single quote)
			end := i + 1
			for end < len(expr) {
				if expr[end] == '\'' {
					if end+1 < len(expr) && expr[end+1] == '\'' {
						end += 2 // escaped quote
						continue
					}
					break
				}
				end++
			}
			if end < len(expr) {
				result.WriteString(strings.ReplaceAll(expr[i+1:end], "''", "'"))
			}
			i = end + 1
		} else {
			i++
		}
	}
	return result.String()
}

// isContinued checks if a Pascal line continues (ends with + or has continuation chars).
func isContinued(line string) bool {
	trimmed := strings.TrimRight(strings.TrimSpace(line), " ;")
	return strings.HasSuffix(trimmed, "+")
}

type classDecl struct {
	name   string
	kind   string // class, record, interface
	parent string
}

func matchClassDecl(line string) *classDecl {
	// TMyClass = class(TParent) or TMyRecord = record
	patterns := []struct {
		re   *regexp.Regexp
		kind string
	}{
		{regexp.MustCompile(`(?i)^\s*(\w+)\s*=\s*class\s*\(\s*(\w+)\s*\)`), "class"},
		{regexp.MustCompile(`(?i)^\s*(\w+)\s*=\s*class\b`), "class"},
		{regexp.MustCompile(`(?i)^\s*(\w+)\s*=\s*record\b`), "type"},
		{regexp.MustCompile(`(?i)^\s*(\w+)\s*=\s*interface\s*\(\s*(\w+)\s*\)`), "interface"},
		{regexp.MustCompile(`(?i)^\s*(\w+)\s*=\s*interface\b`), "interface"},
	}

	for _, p := range patterns {
		m := p.re.FindStringSubmatch(line)
		if len(m) >= 2 {
			decl := &classDecl{name: m[1], kind: p.kind}
			if len(m) >= 3 {
				decl.parent = m[2]
			}
			return decl
		}
	}
	return nil
}

func parsePascalProcDecl(line string) (string, string) {
	lower := strings.ToLower(line)
	for _, prefix := range []string{"class procedure ", "class function ", "procedure ", "function ", "constructor ", "destructor "} {
		if strings.HasPrefix(lower, prefix) {
			rest := line[len(prefix):]
			// Extract name (may be ClassName.MethodName)
			if idx := strings.IndexAny(rest, "(;"); idx >= 0 {
				name := strings.TrimSpace(rest[:idx])
				sig := ""
				if rest[idx] == '(' {
					endIdx := strings.Index(rest, ")")
					if endIdx > idx {
						sig = rest[idx : endIdx+1]
					}
				}
				return name, sig
			}
			return strings.TrimSpace(strings.TrimRight(rest, ";")), ""
		}
	}
	return "", ""
}

func parseUsesClause(lines []string, startIdx int) []parser.RawReference {
	var refs []parser.RawReference

	// Collect the full uses statement (may span multiple lines)
	var uses strings.Builder
	for i := startIdx; i < len(lines); i++ {
		uses.WriteString(lines[i])
		if strings.Contains(lines[i], ";") {
			break
		}
		uses.WriteString(" ")
	}

	text := uses.String()
	if idx := strings.IndexByte(text, ';'); idx >= 0 {
		text = text[:idx]
	}

	// Remove "uses" keyword
	lower := strings.ToLower(text)
	if idx := strings.Index(lower, "uses"); idx >= 0 {
		text = text[idx+4:]
	}

	// Split by comma and extract unit names (ignore "in 'path'" parts)
	for _, part := range strings.Split(text, ",") {
		part = strings.TrimSpace(part)
		if inIdx := strings.Index(strings.ToLower(part), " in "); inIdx >= 0 {
			part = part[:inIdx]
		}
		part = strings.TrimSpace(part)
		if part != "" {
			refs = append(refs, parser.RawReference{
				ToName:        part,
				ReferenceType: "imports",
				Line:          startIdx + 1,
			})
		}
	}

	return refs
}

func extractIdentAfterKeyword(line, keyword string) string {
	lower := strings.ToLower(line)
	idx := strings.Index(lower, strings.ToLower(keyword))
	if idx < 0 {
		return ""
	}
	rest := strings.TrimSpace(line[idx+len(keyword):])
	rest = strings.TrimRight(rest, ";")
	rest = strings.TrimSpace(rest)
	if spaceIdx := strings.IndexAny(rest, " \t(;"); spaceIdx >= 0 {
		return rest[:spaceIdx]
	}
	return rest
}

func qualify(unitName, name string) string {
	if unitName != "" {
		return unitName + "." + name
	}
	return name
}

func findPascalEnd(lines []string, startIdx int) int {
	depth := 0
	for i := startIdx; i < len(lines); i++ {
		lower := strings.ToLower(strings.TrimSpace(lines[i]))
		if lower == "end;" || lower == "end." {
			if depth <= 0 {
				return i + 1
			}
			depth--
		}
		// Count nested begin/record/class blocks
		if strings.HasPrefix(lower, "begin") || strings.HasSuffix(lower, "= record") ||
			strings.Contains(lower, "= class") {
			depth++
		}
	}
	return startIdx + 1
}

func findPascalProcEnd(lines []string, startIdx int) int {
	depth := 0
	foundBegin := false
	for i := startIdx + 1; i < len(lines); i++ {
		lower := strings.ToLower(strings.TrimSpace(lines[i]))
		if strings.HasPrefix(lower, "begin") {
			foundBegin = true
			depth++
		}
		if (lower == "end;" || lower == "end.") && foundBegin {
			depth--
			if depth <= 0 {
				return i + 1
			}
		}
	}
	return startIdx + 1
}
