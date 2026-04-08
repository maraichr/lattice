package tsql

import (
	"strings"

	"github.com/maraichr/lattice/internal/parser"
)

// Parser implements a recursive-descent T-SQL parser that extracts symbols and references.
type Parser struct {
	tokens           []Token
	pos              int
	symbols          []parser.Symbol
	refs             []parser.RawReference
	colRefs          []parser.ColumnReference
	schema           string // current default schema
	skipColumnLineage bool  // when true, do not extract column-level lineage (migration/schema files)
	fileContext       string // synthetic context for top-level statements (e.g. "__file__:migrate")
}

// TSQLParser implements the parser.Parser interface.
type TSQLParser struct{}

func New() *TSQLParser {
	return &TSQLParser{}
}

func (t *TSQLParser) Languages() []string {
	return []string{"tsql", "sql"}
}

func (t *TSQLParser) Parse(input parser.FileInput) (*parser.ParseResult, error) {
	// Strip common template tokens (e.g. DNN Platform's {databaseOwner}, {objectQualifier})
	content := stripTemplateTokens(string(input.Content))
	lexer := NewLexer(content)
	tokens := lexer.Tokenize()

	// Split into batches by GO
	batches := splitBatches(tokens)

	// Derive a synthetic context name from the file path for top-level statements.
	syntheticContext := deriveFileContext(input.Path)

	var allSymbols []parser.Symbol
	var allRefs []parser.RawReference
	var allColRefs []parser.ColumnReference

	for _, batch := range batches {
		p := &Parser{
			tokens:            batch,
			schema:            "dbo",
			skipColumnLineage: input.SkipColumnLineage,
			fileContext:       syntheticContext,
		}
		p.parseBatch()
		allSymbols = append(allSymbols, p.symbols...)
		allRefs = append(allRefs, p.refs...)
		allColRefs = append(allColRefs, p.colRefs...)
	}

	return &parser.ParseResult{
		Symbols:          allSymbols,
		References:       allRefs,
		ColumnReferences: allColRefs,
	}, nil
}

// deriveFileContext creates a synthetic symbol name from a file path for use as
// the context of top-level SQL statements (EXEC, SELECT, etc.) outside procedures.
func deriveFileContext(path string) string {
	if path == "" {
		return ""
	}
	// Use just the filename without extension
	name := path
	if idx := strings.LastIndexAny(name, "/\\"); idx >= 0 {
		name = name[idx+1:]
	}
	if idx := strings.LastIndex(name, "."); idx >= 0 {
		name = name[:idx]
	}
	return "__file__:" + name
}

// stripTemplateTokens removes common SQL template placeholders used by frameworks
// like DNN Platform (e.g. {databaseOwner}, {objectQualifier}).
func stripTemplateTokens(content string) string {
	r := strings.NewReplacer(
		"{databaseOwner}", "dbo.",
		"{objectQualifier}", "",
	)
	return r.Replace(content)
}

func splitBatches(tokens []Token) [][]Token {
	var batches [][]Token
	var current []Token

	for _, tok := range tokens {
		if tok.Type == TokenGO {
			if len(current) > 0 {
				batches = append(batches, current)
				current = nil
			}
			continue
		}
		if tok.Type == TokenNewline || tok.Type == TokenComment {
			continue
		}
		current = append(current, tok)
	}
	if len(current) > 0 {
		batches = append(batches, current)
	}
	return batches
}

func (p *Parser) parseBatch() {
	// Use fileContext for top-level statements so EXEC/DML outside procedures
	// can still generate edges. A synthetic __file__ symbol is emitted if needed.
	ctx := p.fileContext
	emittedFileSymbol := false

	ensureFileSymbol := func() {
		if ctx != "" && !emittedFileSymbol {
			p.symbols = append(p.symbols, parser.Symbol{
				Name:          ctx,
				QualifiedName: ctx,
				Kind:          "script",
				Language:      "tsql",
				StartLine:     1,
				EndLine:       p.currentLine(),
			})
			emittedFileSymbol = true
		}
	}

	for p.pos < len(p.tokens) {
		tok := p.current()
		if tok.Type == TokenEOF {
			break
		}

		if tok.Type == TokenKeyword {
			switch tok.Value {
			case "CREATE":
				p.parseCreate()
			case "SELECT":
				p.parseSelect(ctx)
			case "INSERT":
				p.parseInsert(ctx)
			case "UPDATE":
				p.parseUpdate(ctx)
			case "DELETE":
				p.parseDelete(ctx)
			case "EXEC", "EXECUTE":
				ensureFileSymbol()
				p.parseExec(ctx)
			case "MERGE":
				p.parseMerge(ctx)
			default:
				p.advance()
			}
		} else {
			p.advance()
		}
	}
}

func (p *Parser) parseCreate() {
	startLine := p.current().Line
	p.advance() // skip CREATE

	// optional OR ALTER
	if p.matchKeyword("OR") {
		p.advance()
		if p.matchKeyword("ALTER") {
			p.advance()
		}
	}

	tok := p.current()
	if tok.Type != TokenKeyword {
		return
	}

	switch tok.Value {
	case "TABLE":
		p.parseCreateTable(startLine)
	case "VIEW":
		p.parseCreateView(startLine)
	case "PROCEDURE", "PROC":
		p.parseCreateProcedure(startLine)
	case "FUNCTION":
		p.parseCreateFunction(startLine)
	case "TRIGGER":
		p.parseCreateTrigger(startLine)
	case "TYPE":
		p.parseCreateType(startLine)
	default:
		// skip unknown CREATE
	}
}

func (p *Parser) parseCreateTable(startLine int) {
	p.advance() // skip TABLE

	name := p.readQualifiedName()
	if name == "" {
		return
	}

	sym := parser.Symbol{
		Name:          unqualify(name),
		QualifiedName: name,
		Kind:          "table",
		Language:      "tsql",
		StartLine:     startLine,
	}

	// Parse columns
	if p.matchPunct("(") {
		p.advance() // skip (
		sym.Children = p.parseColumnDefs(name)
	}

	sym.EndLine = p.currentLine()
	p.symbols = append(p.symbols, sym)
}

func (p *Parser) parseColumnDefs(tableName string) []parser.Symbol {
	var cols []parser.Symbol
	depth := 1

	for p.pos < len(p.tokens) && depth > 0 {
		tok := p.current()
		if tok.Type == TokenEOF {
			break
		}

		if p.matchPunct("(") {
			depth++
			p.advance()
			continue
		}
		if p.matchPunct(")") {
			depth--
			p.advance()
			continue
		}

		// Skip constraints
		if tok.Type == TokenKeyword && (tok.Value == "CONSTRAINT" || tok.Value == "PRIMARY" ||
			tok.Value == "FOREIGN" || tok.Value == "UNIQUE" || tok.Value == "CHECK" || tok.Value == "INDEX") {
			p.skipToCommaOrParen(depth)
			continue
		}

		// Column: identifier followed by a type keyword or identifier
		if tok.Type == TokenIdent && p.pos+1 < len(p.tokens) {
			colName := tok.Value
			colLine := tok.Line
			p.advance()
			// Check if next is a type
			next := p.current()
			if next.Type == TokenKeyword || next.Type == TokenIdent {
				cols = append(cols, parser.Symbol{
					Name:          colName,
					QualifiedName: tableName + "." + colName,
					Kind:          "column",
					Language:      "tsql",
					StartLine:     colLine,
					EndLine:       colLine,
				})
			}
			p.skipToCommaOrParen(depth)
			continue
		}

		p.advance()
	}
	return cols
}

func (p *Parser) parseCreateView(startLine int) {
	p.advance() // skip VIEW
	name := p.readQualifiedName()
	if name == "" {
		return
	}

	sym := parser.Symbol{
		Name:          unqualify(name),
		QualifiedName: name,
		Kind:          "view",
		Language:      "tsql",
		StartLine:     startLine,
	}

	// Skip to AS keyword then parse the SELECT
	for p.pos < len(p.tokens) && !p.matchKeyword("AS") {
		p.advance()
	}
	if p.matchKeyword("AS") {
		p.advance()
		colRefsBefore := len(p.colRefs)
		p.parseSelect(name)

		// Create column children for the view from the SELECT output columns.
		// This ensures view columns exist as symbols so lineage edges can resolve.
		for _, ref := range p.colRefs[colRefsBefore:] {
			parts := strings.Split(ref.TargetColumn, ".")
			colName := parts[len(parts)-1]
			sym.Children = append(sym.Children, parser.Symbol{
				Name:          colName,
				QualifiedName: ref.TargetColumn,
				Kind:          "column",
				Language:      "tsql",
				StartLine:     ref.Line,
				EndLine:       ref.Line,
			})
		}
	}

	sym.EndLine = p.currentLine()
	p.symbols = append(p.symbols, sym)
}

func (p *Parser) parseCreateProcedure(startLine int) {
	p.advance() // skip PROCEDURE/PROC
	name := p.readQualifiedName()
	if name == "" {
		return
	}

	sym := parser.Symbol{
		Name:          unqualify(name),
		QualifiedName: name,
		Kind:          "procedure",
		Language:      "tsql",
		StartLine:     startLine,
	}

	// Collect signature up to AS
	var sigParts []string
	if p.matchPunct("(") || p.matchPunct("@") || (p.current().Type == TokenIdent && strings.HasPrefix(p.current().Value, "@")) {
		sigParts = p.collectParamSignature()
	}
	if len(sigParts) > 0 {
		sig := strings.Join(sigParts, " ")
		sym.Signature = sig
	}

	// Skip to AS + BEGIN
	for p.pos < len(p.tokens) && !p.matchKeyword("AS") {
		p.advance()
	}
	if p.matchKeyword("AS") {
		p.advance()
	}

	// Parse body
	p.parseBody(name)

	sym.EndLine = p.currentLine()
	p.symbols = append(p.symbols, sym)
}

func (p *Parser) parseCreateFunction(startLine int) {
	p.advance() // skip FUNCTION
	name := p.readQualifiedName()
	if name == "" {
		return
	}

	sym := parser.Symbol{
		Name:          unqualify(name),
		QualifiedName: name,
		Kind:          "function",
		Language:      "tsql",
		StartLine:     startLine,
	}

	// Collect params
	if p.matchPunct("(") {
		sigParts := p.collectParamSignature()
		if len(sigParts) > 0 {
			sym.Signature = strings.Join(sigParts, " ")
		}
	}

	// RETURNS clause
	if p.matchKeyword("RETURNS") {
		p.advance()
		// skip return type
		for p.pos < len(p.tokens) && !p.matchKeyword("AS") && !p.matchKeyword("BEGIN") {
			p.advance()
		}
	}

	if p.matchKeyword("AS") {
		p.advance()
	}

	p.parseBody(name)

	sym.EndLine = p.currentLine()
	p.symbols = append(p.symbols, sym)
}

func (p *Parser) parseCreateTrigger(startLine int) {
	p.advance() // skip TRIGGER
	name := p.readQualifiedName()
	if name == "" {
		return
	}

	sym := parser.Symbol{
		Name:          unqualify(name),
		QualifiedName: name,
		Kind:          "trigger",
		Language:      "tsql",
		StartLine:     startLine,
	}

	// ON table_name
	if p.matchKeyword("ON") {
		p.advance()
		tableName := p.readQualifiedName()
		if tableName != "" {
			p.refs = append(p.refs, parser.RawReference{
				FromSymbol:    name,
				ToName:        unqualify(tableName),
				ToQualified:   tableName,
				ReferenceType: "uses_table",
				Line:          p.current().Line,
			})
		}
	}

	// Skip to AS
	for p.pos < len(p.tokens) && !p.matchKeyword("AS") {
		p.advance()
	}
	if p.matchKeyword("AS") {
		p.advance()
	}

	p.parseBody(name)

	sym.EndLine = p.currentLine()
	p.symbols = append(p.symbols, sym)
}

func (p *Parser) parseCreateType(startLine int) {
	p.advance() // skip TYPE
	name := p.readQualifiedName()
	if name == "" {
		return
	}

	sym := parser.Symbol{
		Name:          unqualify(name),
		QualifiedName: name,
		Kind:          "type",
		Language:      "tsql",
		StartLine:     startLine,
		EndLine:       p.currentLine(),
	}
	p.symbols = append(p.symbols, sym)
}

// parseBody parses the body of a procedure/function/trigger, extracting DML references.
func (p *Parser) parseBody(context string) {
	depth := 0
	for p.pos < len(p.tokens) {
		tok := p.current()
		if tok.Type == TokenEOF {
			break
		}

		if tok.Type == TokenKeyword {
			switch tok.Value {
			case "BEGIN":
				depth++
				p.advance()
			case "END":
				if depth > 0 {
					depth--
				}
				p.advance()
				if depth == 0 {
					return
				}
			case "SELECT":
				p.parseSelect(context)
			case "INSERT":
				p.parseInsert(context)
			case "UPDATE":
				p.parseUpdate(context)
			case "DELETE":
				p.parseDelete(context)
			case "EXEC", "EXECUTE":
				p.parseExec(context)
			case "MERGE":
				p.parseMerge(context)
			default:
				p.advance()
			}
		} else {
			p.advance()
		}
	}
}

func (p *Parser) parseSelect(context string) {
	selectLine := p.current().Line
	p.advance() // skip SELECT

	// Parse select columns before FROM
	selectItems := p.parseSelectColumns()

	// Collect FROM tables with aliases for column qualification
	fromTables := make(map[string]string)
	if p.matchKeyword("FROM") {
		p.advance()
		fromTables = p.collectFromTables(context, "reads_from")
	}

	// Process JOINs — also collect table aliases
	for p.pos < len(p.tokens) && !p.matchPunct(";") && !p.matchKeyword("UNION") {
		if p.matchKeyword("JOIN") || p.matchKeyword("INNER") || p.matchKeyword("LEFT") || p.matchKeyword("RIGHT") || p.matchKeyword("CROSS") || p.matchKeyword("OUTER") || p.matchKeyword("FULL") {
			// Advance past join type keywords until we get past JOIN
			for p.matchKeyword("INNER") || p.matchKeyword("LEFT") || p.matchKeyword("RIGHT") || p.matchKeyword("CROSS") || p.matchKeyword("OUTER") || p.matchKeyword("FULL") || p.matchKeyword("JOIN") {
				if p.matchKeyword("JOIN") {
					p.advance()
					break
				}
				p.advance()
			}
			name, alias := p.readTableWithAlias()
			if name != "" {
				fromTables[strings.ToLower(alias)] = name
				if context != "" {
					p.refs = append(p.refs, parser.RawReference{
						FromSymbol:    context,
						ToName:        unqualify(name),
						ToQualified:   name,
						ReferenceType: "joins",
						Line:          p.currentLine(),
					})
				}
			}
		} else if p.matchPunct("(") {
			p.skipParens()
		} else {
			p.advance()
		}
	}

	// Generate column references from parsed select items with qualified source columns
	if context != "" && !p.skipColumnLineage {
		for _, item := range selectItems {
			if item.sourceColumn == "" {
				continue
			}
			p.colRefs = append(p.colRefs, parser.ColumnReference{
				SourceColumn:   qualifyColumn(item.sourceColumn, fromTables),
				TargetColumn:   context + "." + item.alias,
				DerivationType: item.derivationType,
				Expression:     item.expression,
				Context:        context,
				Line:           selectLine,
			})
		}
	}
}

// selectItem represents a parsed SELECT column expression.
type selectItem struct {
	sourceColumn   string // source column reference (may be qualified)
	alias          string // output alias or column name
	derivationType string // direct_copy, transform, aggregate
	expression     string // original expression text
}

// parseSelectColumns reads tokens between SELECT and FROM and extracts column items.
func (p *Parser) parseSelectColumns() []selectItem {
	var items []selectItem
	var currentTokens []string
	parenDepth := 0

	for p.pos < len(p.tokens) {
		tok := p.current()
		if tok.Type == TokenEOF {
			break
		}

		// Stop at FROM (not inside parens)
		if parenDepth == 0 && p.matchKeyword("FROM") {
			break
		}

		// Stop at semicolon
		if p.matchPunct(";") {
			break
		}

		// Skip TOP N
		if p.matchKeyword("TOP") {
			p.advance()
			// Skip the number or parens after TOP
			if p.matchPunct("(") {
				p.skipParens()
			} else {
				p.advance()
			}
			continue
		}

		// Skip DISTINCT
		if p.matchKeyword("DISTINCT") {
			p.advance()
			continue
		}

		if p.matchPunct("(") {
			parenDepth++
			currentTokens = append(currentTokens, tok.Value)
			p.advance()
			continue
		}
		if p.matchPunct(")") {
			if parenDepth > 0 {
				parenDepth--
			}
			currentTokens = append(currentTokens, tok.Value)
			p.advance()
			continue
		}

		// Comma separates select items (only at top level)
		if parenDepth == 0 && p.matchPunct(",") {
			if len(currentTokens) > 0 {
				items = append(items, classifySelectItem(currentTokens))
				currentTokens = nil
			}
			p.advance()
			continue
		}

		currentTokens = append(currentTokens, tok.Value)
		p.advance()
	}

	// Last item
	if len(currentTokens) > 0 {
		items = append(items, classifySelectItem(currentTokens))
	}

	return items
}

// mergeQualifiedTokens joins adjacent ident.ident sequences into single tokens.
// e.g. ["o", ".", "OrderID"] → ["o.OrderID"]
func mergeQualifiedTokens(tokens []string) []string {
	if len(tokens) == 0 {
		return tokens
	}
	var result []string
	i := 0
	for i < len(tokens) {
		name := tokens[i]
		for i+2 < len(tokens) && tokens[i+1] == "." {
			name += "." + tokens[i+2]
			i += 2
		}
		result = append(result, name)
		i++
	}
	return result
}

// classifySelectItem takes a token slice for one SELECT item and determines derivation type.
func classifySelectItem(tokens []string) selectItem {
	tokens = mergeQualifiedTokens(tokens)
	if len(tokens) == 0 {
		return selectItem{}
	}

	expr := strings.Join(tokens, " ")
	item := selectItem{expression: expr}

	// Check for AS alias
	alias := ""
	colTokens := tokens
	for i, t := range tokens {
		if strings.EqualFold(t, "AS") && i+1 < len(tokens) {
			alias = tokens[i+1]
			colTokens = tokens[:i]
			break
		}
	}
	// If no AS, last token might be alias if preceded by an expression
	if alias == "" && len(tokens) > 1 {
		// Simple heuristic: if not a function call and last token is an ident, it's an alias
		last := tokens[len(tokens)-1]
		if !strings.Contains(last, "(") && !strings.Contains(last, ")") && !strings.Contains(last, ".") {
			prevTokenStr := strings.Join(tokens[:len(tokens)-1], " ")
			if strings.ContainsAny(prevTokenStr, "()+*-/") || strings.Contains(prevTokenStr, ".") {
				alias = last
				colTokens = tokens[:len(tokens)-1]
			}
		}
	}

	exprStr := strings.Join(colTokens, " ")
	exprUpper := strings.ToUpper(exprStr)

	// Check for aggregate functions
	aggregates := []string{"COUNT(", "SUM(", "AVG(", "MIN(", "MAX(", "COUNT (", "SUM (", "AVG (", "MIN (", "MAX ("}
	for _, agg := range aggregates {
		if strings.Contains(exprUpper, agg) {
			item.derivationType = "aggregate"
			item.sourceColumn = extractFirstColumn(colTokens)
			if alias != "" {
				item.alias = alias
			}
			return item
		}
	}

	// Check for function calls or expressions (transform)
	if strings.Contains(exprStr, "(") || strings.ContainsAny(exprStr, "+-*/") {
		item.derivationType = "transform"
		item.sourceColumn = extractFirstColumn(colTokens)
		if alias != "" {
			item.alias = alias
		}
		return item
	}

	// Simple column reference (direct_copy)
	item.derivationType = "direct_copy"
	item.sourceColumn = exprStr
	if alias != "" {
		item.alias = alias
	} else {
		// Alias is the column name itself
		parts := strings.Split(exprStr, ".")
		item.alias = parts[len(parts)-1]
	}

	return item
}

// extractFirstColumn finds the first column reference in an expression.
func extractFirstColumn(tokens []string) string {
	for _, t := range tokens {
		// Skip function names and keywords
		upper := strings.ToUpper(t)
		if upper == "(" || upper == ")" || upper == "," || upper == "+" || upper == "-" || upper == "*" || upper == "/" {
			continue
		}
		if isAggFunc(upper) || upper == "CASE" || upper == "WHEN" || upper == "THEN" || upper == "ELSE" || upper == "END" || upper == "AS" || upper == "CAST" || upper == "CONVERT" {
			continue
		}
		// Looks like a column ref
		if strings.Contains(t, ".") || (len(t) > 0 && t[0] != '\'') {
			return t
		}
	}
	return ""
}

func isAggFunc(s string) bool {
	switch s {
	case "COUNT", "SUM", "AVG", "MIN", "MAX", "UPPER", "LOWER", "TRIM", "LTRIM", "RTRIM",
		"COALESCE", "ISNULL", "NULLIF", "SUBSTRING", "LEN", "LEFT", "RIGHT", "REPLACE",
		"CHARINDEX", "STUFF", "CONCAT", "FORMAT", "DATEPART", "DATEDIFF", "DATEADD",
		"GETDATE", "GETUTCDATE", "YEAR", "MONTH", "DAY":
		return true
	}
	return false
}

func (p *Parser) parseInsert(context string) {
	insertLine := p.current().Line
	p.advance() // skip INSERT

	if p.matchKeyword("INTO") {
		p.advance()
	}

	targetTable := p.readQualifiedName()
	if targetTable != "" && context != "" {
		p.refs = append(p.refs, parser.RawReference{
			FromSymbol:    context,
			ToName:        unqualify(targetTable),
			ToQualified:   targetTable,
			ReferenceType: "writes_to",
			Line:          p.current().Line,
		})
	}

	// Check for column list: (col1, col2, ...)
	var targetCols []string
	if p.matchPunct("(") {
		p.advance()
		for p.pos < len(p.tokens) && !p.matchPunct(")") {
			tok := p.current()
			if tok.Type == TokenIdent || tok.Type == TokenKeyword {
				targetCols = append(targetCols, tok.Value)
			}
			p.advance()
			if p.matchPunct(",") {
				p.advance()
			}
		}
		if p.matchPunct(")") {
			p.advance()
		}
	}

	// If followed by SELECT, correlate columns positionally.
	// Allow both top-level (context="") and in-body (context=procName) INSERT...SELECT.
	if p.matchKeyword("SELECT") && targetTable != "" && len(targetCols) > 0 {
		p.advance() // skip SELECT
		selectItems := p.parseSelectColumns()

		// Read FROM tables for source column qualification
		fromTables := make(map[string]string)
		if p.matchKeyword("FROM") {
			p.advance()
			fromTables = p.collectFromTables(context, "reads_from")
		}

		// Use target table as context for top-level statements
		effectiveContext := context
		if effectiveContext == "" {
			effectiveContext = targetTable
		}

		if !p.skipColumnLineage {
			for i, col := range targetCols {
				if i < len(selectItems) {
					srcCol := selectItems[i].sourceColumn
					if srcCol == "" {
						srcCol = selectItems[i].expression
					}
					p.colRefs = append(p.colRefs, parser.ColumnReference{
						SourceColumn:   qualifyColumn(srcCol, fromTables),
						TargetColumn:   targetTable + "." + col,
						DerivationType: selectItems[i].derivationType,
						Expression:     selectItems[i].expression,
						Context:        effectiveContext,
						Line:           insertLine,
					})
				}
			}
		}
	}
}

func (p *Parser) parseUpdate(context string) {
	updateLine := p.current().Line
	p.advance() // skip UPDATE
	targetTable := p.readQualifiedName()
	if targetTable != "" && context != "" {
		p.refs = append(p.refs, parser.RawReference{
			FromSymbol:    context,
			ToName:        unqualify(targetTable),
			ToQualified:   targetTable,
			ReferenceType: "writes_to",
			Line:          p.current().Line,
		})
	}

	// Parse SET clause for column references
	if p.matchKeyword("SET") && context != "" && targetTable != "" {
		p.advance()
		p.parseSetClause(context, targetTable, updateLine)
	}
}

// parseSetClause parses UPDATE ... SET col1 = expr1, col2 = expr2 ...
func (p *Parser) parseSetClause(context, targetTable string, line int) {
	for p.pos < len(p.tokens) {
		// Stop at WHERE, FROM, OUTPUT, or semicolon
		tok := p.current()
		if tok.Type == TokenEOF {
			break
		}
		if p.matchKeyword("WHERE") || p.matchKeyword("FROM") || p.matchKeyword("OUTPUT") || p.matchPunct(";") {
			break
		}

		// Read column name
		if tok.Type != TokenIdent && tok.Type != TokenKeyword {
			p.advance()
			continue
		}
		colName := tok.Value
		p.advance()

		// Expect =
		if !p.matchPunct("=") {
			continue
		}
		p.advance()

		// Read expression tokens until comma or stop keyword
		var exprTokens []string
		parenDepth := 0
		for p.pos < len(p.tokens) {
			t := p.current()
			if t.Type == TokenEOF {
				break
			}
			if parenDepth == 0 {
				if p.matchPunct(",") {
					p.advance()
					break
				}
				if p.matchKeyword("WHERE") || p.matchKeyword("FROM") || p.matchKeyword("OUTPUT") || p.matchPunct(";") {
					break
				}
			}
			if p.matchPunct("(") {
				parenDepth++
			}
			if p.matchPunct(")") {
				if parenDepth > 0 {
					parenDepth--
				}
			}
			exprTokens = append(exprTokens, t.Value)
			p.advance()
		}

		if len(exprTokens) > 0 && !p.skipColumnLineage {
			merged := mergeQualifiedTokens(exprTokens)
			exprStr := strings.Join(merged, " ")
			derivation := "direct_copy"
			if strings.Contains(exprStr, "(") || strings.ContainsAny(exprStr, "+-*/") {
				derivation = "transform"
			}
			srcCol := extractFirstColumn(merged)
			if srcCol == "" {
				srcCol = exprStr
			}
			p.colRefs = append(p.colRefs, parser.ColumnReference{
				SourceColumn:   srcCol,
				TargetColumn:   targetTable + "." + colName,
				DerivationType: derivation,
				Expression:     exprStr,
				Context:        context,
				Line:           line,
			})
		}
	}
}

func (p *Parser) parseDelete(context string) {
	p.advance() // skip DELETE

	if p.matchKeyword("FROM") {
		p.advance()
	}

	name := p.readQualifiedName()
	if name != "" && context != "" {
		p.refs = append(p.refs, parser.RawReference{
			FromSymbol:    context,
			ToName:        unqualify(name),
			ToQualified:   name,
			ReferenceType: "writes_to",
			Line:          p.current().Line,
		})
	}
}

func (p *Parser) parseExec(context string) {
	p.advance() // skip EXEC/EXECUTE
	name := p.readQualifiedName()
	if name != "" && context != "" {
		p.refs = append(p.refs, parser.RawReference{
			FromSymbol:    context,
			ToName:        unqualify(name),
			ToQualified:   name,
			ReferenceType: "calls",
			Line:          p.current().Line,
		})
	}
}

func (p *Parser) parseMerge(context string) {
	p.advance() // skip MERGE

	if p.matchKeyword("INTO") {
		p.advance()
	}

	name := p.readQualifiedName()
	if name != "" && context != "" {
		p.refs = append(p.refs, parser.RawReference{
			FromSymbol:    context,
			ToName:        unqualify(name),
			ToQualified:   name,
			ReferenceType: "writes_to",
			Line:          p.current().Line,
		})
	}
}

// Helper methods

func (p *Parser) current() Token {
	if p.pos >= len(p.tokens) {
		return Token{Type: TokenEOF}
	}
	return p.tokens[p.pos]
}

func (p *Parser) advance() {
	if p.pos < len(p.tokens) {
		p.pos++
	}
}

func (p *Parser) matchKeyword(kw string) bool {
	return p.current().Type == TokenKeyword && p.current().Value == kw
}

func (p *Parser) matchPunct(val string) bool {
	return p.current().Type == TokenPunctuation && p.current().Value == val
}

func (p *Parser) currentLine() int {
	if p.pos > 0 && p.pos <= len(p.tokens) {
		return p.tokens[p.pos-1].Line
	}
	return p.current().Line
}

func (p *Parser) readQualifiedName() string {
	tok := p.current()
	if tok.Type != TokenIdent && tok.Type != TokenKeyword {
		return ""
	}

	var parts []string
	parts = append(parts, tok.Value)
	p.advance()

	for p.matchPunct(".") {
		p.advance() // skip .
		tok = p.current()
		if tok.Type == TokenIdent || tok.Type == TokenKeyword {
			parts = append(parts, tok.Value)
			p.advance()
		} else {
			break
		}
	}

	return strings.Join(parts, ".")
}

func (p *Parser) collectParamSignature() []string {
	var parts []string
	depth := 0
	if p.matchPunct("(") {
		depth = 1
		p.advance()
	}

	for p.pos < len(p.tokens) {
		tok := p.current()
		if tok.Type == TokenEOF {
			break
		}
		if p.matchPunct("(") {
			depth++
		}
		if p.matchPunct(")") {
			if depth > 0 {
				depth--
			}
			if depth == 0 {
				p.advance()
				break
			}
		}
		if p.matchKeyword("AS") || p.matchKeyword("BEGIN") {
			break
		}
		parts = append(parts, tok.Value)
		p.advance()
	}
	return parts
}

func (p *Parser) skipParens() {
	depth := 1
	p.advance() // skip (
	for p.pos < len(p.tokens) && depth > 0 {
		if p.matchPunct("(") {
			depth++
		} else if p.matchPunct(")") {
			depth--
		}
		p.advance()
	}
}

func (p *Parser) skipToCommaOrParen(depth int) {
	for p.pos < len(p.tokens) {
		if p.matchPunct(",") && depth <= 1 {
			p.advance()
			return
		}
		if p.matchPunct(")") {
			return // don't consume - let caller handle
		}
		if p.matchPunct("(") {
			p.skipParens()
			continue
		}
		p.advance()
	}
}

func unqualify(name string) string {
	parts := strings.Split(name, ".")
	return parts[len(parts)-1]
}

// readTableWithAlias reads a qualified table name optionally followed by [AS] alias.
// Returns (qualifiedName, alias) where alias defaults to the unqualified table name.
func (p *Parser) readTableWithAlias() (string, string) {
	name := p.readQualifiedName()
	if name == "" {
		return "", ""
	}
	alias := unqualify(name)

	if p.matchKeyword("AS") {
		p.advance()
	}
	tok := p.current()
	if tok.Type == TokenIdent {
		alias = tok.Value
		p.advance()
	}

	return name, alias
}

// collectFromTables reads the FROM clause (including comma-separated tables) and
// returns an alias→qualifiedName map. Also appends reads_from references if context is set.
func (p *Parser) collectFromTables(context, refType string) map[string]string {
	fromTables := make(map[string]string)

	name, alias := p.readTableWithAlias()
	if name == "" {
		return fromTables
	}
	fromTables[strings.ToLower(alias)] = name
	if context != "" {
		p.refs = append(p.refs, parser.RawReference{
			FromSymbol:    context,
			ToName:        unqualify(name),
			ToQualified:   name,
			ReferenceType: refType,
			Line:          p.currentLine(),
		})
	}

	// Handle comma-separated tables: FROM dbo.Users u, dbo.Roles r
	for p.matchPunct(",") {
		p.advance()
		name, alias = p.readTableWithAlias()
		if name == "" {
			break
		}
		fromTables[strings.ToLower(alias)] = name
		if context != "" {
			p.refs = append(p.refs, parser.RawReference{
				FromSymbol:    context,
				ToName:        unqualify(name),
				ToQualified:   name,
				ReferenceType: refType,
				Line:          p.currentLine(),
			})
		}
	}

	return fromTables
}

// qualifyColumn resolves a column reference using FROM-clause table aliases.
// "alias.Col" → "schema.table.Col", bare "Col" → "schema.table.Col" if single FROM table.
func qualifyColumn(col string, fromTables map[string]string) string {
	if col == "" || len(fromTables) == 0 {
		return col
	}

	parts := strings.SplitN(col, ".", 2)
	if len(parts) == 2 {
		// Has a table qualifier like "t.PortalID" — resolve alias
		if table, ok := fromTables[strings.ToLower(parts[0])]; ok {
			return table + "." + parts[1]
		}
		return col
	}

	// Bare name — qualify with the single FROM table if unambiguous
	if len(fromTables) == 1 {
		for _, table := range fromTables {
			return table + "." + col
		}
	}

	return col
}
