package pgsql

import (
	"fmt"
	"strings"

	pg_query "github.com/pganalyze/pg_query_go/v6"

	"github.com/maraichr/lattice/internal/parser"
)

// PgSQLParser implements the parser.Parser interface using pg_query_go.
type PgSQLParser struct{}

func New() *PgSQLParser {
	return &PgSQLParser{}
}

func (p *PgSQLParser) Languages() []string {
	return []string{"pgsql", "plpgsql", "sql"}
}

func (p *PgSQLParser) Parse(input parser.FileInput) (*parser.ParseResult, error) {
	src := string(input.Content)
	tree, err := pg_query.Parse(src)
	if err != nil {
		return nil, fmt.Errorf("pg_query parse: %w", err)
	}

	w := &walker{
		symbols: make([]parser.Symbol, 0),
		refs:    make([]parser.RawReference, 0),
		colRefs: make([]parser.ColumnReference, 0),
		src:     src,
	}

	for _, stmt := range tree.Stmts {
		w.walkStatement(stmt)
	}

	return &parser.ParseResult{
		Symbols:          w.symbols,
		References:       w.refs,
		ColumnReferences: w.colRefs,
	}, nil
}

type walker struct {
	symbols []parser.Symbol
	refs    []parser.RawReference
	colRefs []parser.ColumnReference
	context string // current symbol context for references
	src     string // original source, for offset→line conversion
}

// lineOf converts a byte offset (pg_query locations are offsets, not lines)
// into a 1-based line number. Statement locations point just past the previous
// statement's terminator, so leading whitespace is skipped first.
func (w *walker) lineOf(offset int) int {
	if offset < 0 {
		offset = 0
	}
	if offset > len(w.src) {
		offset = len(w.src)
	}
	for offset < len(w.src) {
		c := w.src[offset]
		if c != ' ' && c != '\t' && c != '\n' && c != '\r' {
			break
		}
		offset++
	}
	line := 1
	for i := 0; i < offset; i++ {
		if w.src[i] == '\n' {
			line++
		}
	}
	return line
}

func (w *walker) walkStatement(rawStmt *pg_query.RawStmt) {
	if rawStmt.Stmt == nil {
		return
	}

	node := rawStmt.Stmt
	startLine := w.lineOf(int(rawStmt.StmtLocation))

	switch {
	case node.GetCreateStmt() != nil:
		w.walkCreateTable(node.GetCreateStmt(), startLine)
	case node.GetViewStmt() != nil:
		w.walkCreateView(node.GetViewStmt(), startLine)
	case node.GetCreateFunctionStmt() != nil:
		w.walkCreateFunction(node.GetCreateFunctionStmt(), startLine)
	case node.GetCreateTrigStmt() != nil:
		w.walkCreateTrigger(node.GetCreateTrigStmt(), startLine)
	case node.GetSelectStmt() != nil:
		w.walkSelect(node.GetSelectStmt(), "")
	case node.GetInsertStmt() != nil:
		w.walkInsert(node.GetInsertStmt(), "")
	case node.GetUpdateStmt() != nil:
		w.walkUpdate(node.GetUpdateStmt(), "")
	case node.GetDeleteStmt() != nil:
		w.walkDelete(node.GetDeleteStmt(), "")
	}
}

func (w *walker) walkCreateTable(stmt *pg_query.CreateStmt, startLine int) {
	name := rangeVarToQualified(stmt.Relation)
	sym := parser.Symbol{
		Name:          stmt.Relation.Relname,
		QualifiedName: name,
		Kind:          "table",
		Language:      "pgsql",
		StartLine:     startLine,
	}

	// Extract columns
	for _, elt := range stmt.TableElts {
		if colDef := elt.GetColumnDef(); colDef != nil {
			col := parser.Symbol{
				Name:          colDef.Colname,
				QualifiedName: name + "." + colDef.Colname,
				Kind:          "column",
				Language:      "pgsql",
				StartLine:     w.lineOf(int(colDef.Location)),
				EndLine:       w.lineOf(int(colDef.Location)),
			}
			sym.Children = append(sym.Children, col)
		}
	}

	sym.EndLine = sym.StartLine // approximate
	w.symbols = append(w.symbols, sym)
}

func (w *walker) walkCreateView(stmt *pg_query.ViewStmt, startLine int) {
	name := rangeVarToQualified(stmt.View)
	sym := parser.Symbol{
		Name:          stmt.View.Relname,
		QualifiedName: name,
		Kind:          "view",
		Language:      "pgsql",
		StartLine:     startLine,
	}

	// Extract references and column lineage from the view query
	if stmt.Query != nil {
		if sel := stmt.Query.GetSelectStmt(); sel != nil {
			w.walkSelect(sel, name)

			// Extract view column lineage from target list
			w.extractSelectColumnLineage(sel, name)
		}
	}

	sym.EndLine = sym.StartLine
	w.symbols = append(w.symbols, sym)
}

func (w *walker) walkCreateFunction(stmt *pg_query.CreateFunctionStmt, startLine int) {
	parts := make([]string, len(stmt.Funcname))
	var funcName string
	for i, n := range stmt.Funcname {
		parts[i] = n.GetString_().Sval
	}
	qualifiedName := strings.Join(parts, ".")
	if len(parts) > 0 {
		funcName = parts[len(parts)-1]
	}

	kind := "function"
	// Check if it's a procedure (PostgreSQL 11+)
	if stmt.IsProcedure {
		kind = "procedure"
	}

	sym := parser.Symbol{
		Name:          funcName,
		QualifiedName: qualifiedName,
		Kind:          kind,
		Language:      "pgsql",
		StartLine:     startLine,
	}

	// Build signature from parameters
	var paramParts []string
	for _, param := range stmt.Parameters {
		if fp := param.GetFunctionParameter(); fp != nil {
			paramType := ""
			if fp.ArgType != nil {
				paramType = typeNameToString(fp.ArgType)
			}
			if fp.Name != "" {
				paramParts = append(paramParts, fp.Name+" "+paramType)
			} else {
				paramParts = append(paramParts, paramType)
			}
		}
	}
	if len(paramParts) > 0 {
		sym.Signature = "(" + strings.Join(paramParts, ", ") + ")"
	}

	// Parse PL/pgSQL body for references
	for _, opt := range stmt.Options {
		if defElem := opt.GetDefElem(); defElem != nil && defElem.Defname == "as" {
			if defElem.Arg != nil {
				// The function body is typically a list with one string element
				if list := defElem.Arg.GetList(); list != nil && len(list.Items) > 0 {
					if s := list.Items[0].GetString_(); s != nil {
						w.parsePLpgSQLBody(s.Sval, qualifiedName)
					}
				}
			}
		}
	}

	sym.EndLine = sym.StartLine
	w.symbols = append(w.symbols, sym)
}

func (w *walker) walkCreateTrigger(stmt *pg_query.CreateTrigStmt, startLine int) {
	name := stmt.Trigname
	qualifiedName := name
	if stmt.Relation != nil {
		qualifiedName = rangeVarToQualified(stmt.Relation) + "." + name
	}

	sym := parser.Symbol{
		Name:          name,
		QualifiedName: qualifiedName,
		Kind:          "trigger",
		Language:      "pgsql",
		StartLine:     startLine,
		EndLine:       startLine,
	}

	// Reference the table the trigger is ON
	if stmt.Relation != nil {
		tableName := rangeVarToQualified(stmt.Relation)
		w.refs = append(w.refs, parser.RawReference{
			FromSymbol:    qualifiedName,
			ToName:        stmt.Relation.Relname,
			ToQualified:   tableName,
			ReferenceType: "uses_table",
		})
	}

	// Reference the trigger function
	if len(stmt.Funcname) > 0 {
		funcParts := make([]string, len(stmt.Funcname))
		for i, n := range stmt.Funcname {
			funcParts[i] = n.GetString_().Sval
		}
		funcName := strings.Join(funcParts, ".")
		w.refs = append(w.refs, parser.RawReference{
			FromSymbol:    qualifiedName,
			ToName:        funcParts[len(funcParts)-1],
			ToQualified:   funcName,
			ReferenceType: "calls",
		})
	}

	w.symbols = append(w.symbols, sym)
}

func (w *walker) walkSelect(stmt *pg_query.SelectStmt, context string) {
	for _, from := range stmt.FromClause {
		w.extractTableRefs(from, context, "reads_from")
	}
}

func (w *walker) walkInsert(stmt *pg_query.InsertStmt, context string) {
	if stmt.Relation != nil && context != "" {
		name := rangeVarToQualified(stmt.Relation)
		w.refs = append(w.refs, parser.RawReference{
			FromSymbol:    context,
			ToName:        stmt.Relation.Relname,
			ToQualified:   name,
			ReferenceType: "writes_to",
		})

		// Column-level lineage: correlate INSERT columns with SELECT columns
		if stmt.SelectStmt != nil && len(stmt.Cols) > 0 {
			targetCols := make([]string, 0, len(stmt.Cols))
			for _, col := range stmt.Cols {
				if rt := col.GetResTarget(); rt != nil {
					targetCols = append(targetCols, rt.Name)
				}
			}

			if sel := stmt.SelectStmt.GetSelectStmt(); sel != nil {
				srcItems := w.extractTargetListItems(sel)
				for i, tgtCol := range targetCols {
					if i < len(srcItems) {
						w.colRefs = append(w.colRefs, parser.ColumnReference{
							SourceColumn:   srcItems[i].sourceColumn,
							TargetColumn:   name + "." + tgtCol,
							DerivationType: srcItems[i].derivationType,
							Expression:     srcItems[i].expression,
							Context:        context,
						})
					}
				}
			}
		}
	}
}

func (w *walker) walkUpdate(stmt *pg_query.UpdateStmt, context string) {
	if stmt.Relation != nil && context != "" {
		name := rangeVarToQualified(stmt.Relation)
		w.refs = append(w.refs, parser.RawReference{
			FromSymbol:    context,
			ToName:        stmt.Relation.Relname,
			ToQualified:   name,
			ReferenceType: "writes_to",
		})

		// Column-level lineage from SET clause
		for _, target := range stmt.TargetList {
			if rt := target.GetResTarget(); rt != nil && rt.Val != nil {
				srcCol, derivation, expr := w.analyzeExpression(rt.Val)
				w.colRefs = append(w.colRefs, parser.ColumnReference{
					SourceColumn:   srcCol,
					TargetColumn:   name + "." + rt.Name,
					DerivationType: derivation,
					Expression:     expr,
					Context:        context,
				})
			}
		}
	}
}

func (w *walker) walkDelete(stmt *pg_query.DeleteStmt, context string) {
	if stmt.Relation != nil && context != "" {
		name := rangeVarToQualified(stmt.Relation)
		w.refs = append(w.refs, parser.RawReference{
			FromSymbol:    context,
			ToName:        stmt.Relation.Relname,
			ToQualified:   name,
			ReferenceType: "writes_to",
		})
	}
}

func (w *walker) extractTableRefs(node *pg_query.Node, context, refType string) {
	if node == nil || context == "" {
		return
	}

	if rv := node.GetRangeVar(); rv != nil {
		name := rangeVarToQualified(rv)
		w.refs = append(w.refs, parser.RawReference{
			FromSymbol:    context,
			ToName:        rv.Relname,
			ToQualified:   name,
			ReferenceType: refType,
		})
	}

	if jt := node.GetJoinExpr(); jt != nil {
		w.extractTableRefs(jt.Larg, context, refType)
		w.extractTableRefs(jt.Rarg, context, "joins")
	}

	if sub := node.GetRangeSubselect(); sub != nil {
		if sel := sub.Subquery.GetSelectStmt(); sel != nil {
			w.walkSelect(sel, context)
		}
	}
}

// extractSelectColumnLineage generates column references from a SELECT statement's target list.
func (w *walker) extractSelectColumnLineage(stmt *pg_query.SelectStmt, context string) {
	if context == "" {
		return
	}

	items := w.extractTargetListItems(stmt)
	for _, item := range items {
		if item.sourceColumn != "" {
			w.colRefs = append(w.colRefs, parser.ColumnReference{
				SourceColumn:   item.sourceColumn,
				TargetColumn:   item.alias,
				DerivationType: item.derivationType,
				Expression:     item.expression,
				Context:        context,
			})
		}
	}
}

type selectItemInfo struct {
	sourceColumn   string
	alias          string
	derivationType string
	expression     string
}

func (w *walker) extractTargetListItems(stmt *pg_query.SelectStmt) []selectItemInfo {
	var items []selectItemInfo

	// Build alias map from FROM clause
	aliasMap := make(map[string]string) // alias → qualified table name
	for _, from := range stmt.FromClause {
		w.buildAliasMap(from, aliasMap)
	}

	for _, target := range stmt.TargetList {
		rt := target.GetResTarget()
		if rt == nil {
			continue
		}

		item := selectItemInfo{}

		// Output alias
		if rt.Name != "" {
			item.alias = rt.Name
		}

		if rt.Val != nil {
			srcCol, derivation, expr := w.analyzeExpression(rt.Val)
			item.sourceColumn = resolveColumnAlias(srcCol, aliasMap)
			item.derivationType = derivation
			item.expression = expr

			// If no explicit alias, use the column name
			if item.alias == "" && derivation == "direct_copy" {
				parts := strings.Split(srcCol, ".")
				item.alias = parts[len(parts)-1]
			}
		}

		items = append(items, item)
	}

	return items
}

func (w *walker) buildAliasMap(node *pg_query.Node, aliasMap map[string]string) {
	if node == nil {
		return
	}
	if rv := node.GetRangeVar(); rv != nil {
		name := rangeVarToQualified(rv)
		if rv.Alias != nil && rv.Alias.Aliasname != "" {
			aliasMap[rv.Alias.Aliasname] = name
		}
	}
	if jt := node.GetJoinExpr(); jt != nil {
		w.buildAliasMap(jt.Larg, aliasMap)
		w.buildAliasMap(jt.Rarg, aliasMap)
	}
}

func resolveColumnAlias(col string, aliasMap map[string]string) string {
	parts := strings.SplitN(col, ".", 2)
	if len(parts) == 2 {
		if resolved, ok := aliasMap[parts[0]]; ok {
			return resolved + "." + parts[1]
		}
	}
	return col
}

// analyzeExpression determines the source column, derivation type, and expression text.
func (w *walker) analyzeExpression(node *pg_query.Node) (srcCol, derivationType, expression string) {
	if node == nil {
		return "", "direct_copy", ""
	}

	// Column reference
	if cr := node.GetColumnRef(); cr != nil {
		parts := make([]string, 0, len(cr.Fields))
		for _, f := range cr.Fields {
			if s := f.GetString_(); s != nil {
				parts = append(parts, s.Sval)
			}
		}
		col := strings.Join(parts, ".")
		return col, "direct_copy", col
	}

	// Function call
	if fc := node.GetFuncCall(); fc != nil {
		funcParts := make([]string, 0, len(fc.Funcname))
		for _, n := range fc.Funcname {
			if s := n.GetString_(); s != nil {
				if s.Sval != "pg_catalog" {
					funcParts = append(funcParts, s.Sval)
				}
			}
		}
		funcName := strings.Join(funcParts, ".")
		funcUpper := strings.ToUpper(funcName)

		derivation := "transform"
		switch funcUpper {
		case "COUNT", "SUM", "AVG", "MIN", "MAX", "ARRAY_AGG", "STRING_AGG", "BOOL_AND", "BOOL_OR":
			derivation = "aggregate"
		}

		// Get first column arg
		firstCol := ""
		for _, arg := range fc.Args {
			col, _, _ := w.analyzeExpression(arg)
			if col != "" {
				firstCol = col
				break
			}
		}

		expr := funcUpper + "(...)"
		return firstCol, derivation, expr
	}

	// Type cast
	if tc := node.GetTypeCast(); tc != nil {
		return w.analyzeExpression(tc.Arg)
	}

	// A_Expr (arithmetic/comparison)
	if ae := node.GetAExpr(); ae != nil {
		leftCol, _, _ := w.analyzeExpression(ae.Lexpr)
		if leftCol != "" {
			return leftCol, "transform", "expression"
		}
		rightCol, _, _ := w.analyzeExpression(ae.Rexpr)
		return rightCol, "transform", "expression"
	}

	// CASE expression
	if ce := node.GetCaseExpr(); ce != nil {
		// Try to get column from first WHEN or default
		if ce.Defresult != nil {
			col, _, _ := w.analyzeExpression(ce.Defresult)
			if col != "" {
				return col, "conditional", "CASE"
			}
		}
		for _, when := range ce.Args {
			if cw := when.GetCaseWhen(); cw != nil && cw.Result != nil {
				col, _, _ := w.analyzeExpression(cw.Result)
				if col != "" {
					return col, "conditional", "CASE"
				}
			}
		}
		return "", "conditional", "CASE"
	}

	// Constant
	if node.GetAConst() != nil {
		return "", "direct_copy", "constant"
	}

	return "", "direct_copy", ""
}

// parsePLpgSQLBody does a best-effort secondary parse of PL/pgSQL function body.
func (w *walker) parsePLpgSQLBody(body, context string) {
	tree, err := pg_query.Parse(body)
	if err != nil {
		// PL/pgSQL often can't be parsed directly; that's OK
		return
	}

	for _, stmt := range tree.Stmts {
		if stmt.Stmt == nil {
			continue
		}
		node := stmt.Stmt
		switch {
		case node.GetSelectStmt() != nil:
			w.walkSelect(node.GetSelectStmt(), context)
		case node.GetInsertStmt() != nil:
			w.walkInsert(node.GetInsertStmt(), context)
		case node.GetUpdateStmt() != nil:
			w.walkUpdate(node.GetUpdateStmt(), context)
		case node.GetDeleteStmt() != nil:
			w.walkDelete(node.GetDeleteStmt(), context)
		}
	}
}

// Helpers

func rangeVarToQualified(rv *pg_query.RangeVar) string {
	if rv.Schemaname != "" {
		return rv.Schemaname + "." + rv.Relname
	}
	return rv.Relname
}

func typeNameToString(tn *pg_query.TypeName) string {
	parts := make([]string, 0, len(tn.Names))
	for _, n := range tn.Names {
		if s := n.GetString_(); s != nil {
			// Skip "pg_catalog" prefix
			if s.Sval == "pg_catalog" {
				continue
			}
			parts = append(parts, s.Sval)
		}
	}
	return strings.Join(parts, ".")
}
