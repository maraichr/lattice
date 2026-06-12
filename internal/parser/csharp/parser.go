package csharp

import (
	"context"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/csharp"

	"github.com/maraichr/lattice/internal/parser"
)

// Parser implements a tree-sitter based C# parser.
type Parser struct {
	tsParser *sitter.Parser
}

func New() *Parser {
	p := sitter.NewParser()
	p.SetLanguage(csharp.GetLanguage())
	return &Parser{tsParser: p}
}

func (p *Parser) Languages() []string {
	return []string{"csharp"}
}

func (p *Parser) Parse(input parser.FileInput) (*parser.ParseResult, error) {
	tree, err := p.tsParser.ParseCtx(context.Background(), nil, input.Content)
	if err != nil {
		return nil, err
	}
	defer tree.Close()

	root := tree.RootNode()

	var symbols []parser.Symbol
	var refs []parser.RawReference

	// First pass: extract namespace and using directives from root children
	namespace := ""
	for i := 0; i < int(root.ChildCount()); i++ {
		child := root.Child(i)
		switch child.Type() {
		case "using_directive":
			importPath := extractUsingDirective(child, input.Content)
			if importPath != "" {
				refs = append(refs, parser.RawReference{
					ToName:        importPath,
					ToQualified:   importPath,
					ReferenceType: "imports",
					Line:          int(child.StartPoint().Row) + 1,
				})
			}

		case "namespace_declaration":
			ns := extractNamespaceName(child, input.Content)
			if ns != "" {
				namespace = ns
			}
			body := findChild(child, "declaration_list")
			if body != nil {
				processDeclarationList(body, input.Content, ns, &symbols, &refs)
			}

		case "file_scoped_namespace_declaration":
			ns := extractNamespaceName(child, input.Content)
			if ns != "" {
				namespace = ns
			}
			// File-scoped: type declarations are root-level siblings, processed below

		default:
			// Root-level type declarations (with or without file-scoped namespace)
			processTopLevelDecl(child, input.Content, namespace, &symbols, &refs)
		}
	}

	// Extract method body references (calls, object creation, type references)
	methodBodyRefs := extractMethodBodyRefs(root, input.Content, namespace)
	refs = append(refs, methodBodyRefs...)

	// Build class ranges for enclosing-scope resolution (FromSymbol for SQL refs)
	classRanges := buildClassRanges(root, input.Content, namespace)

	// Extract attribute-based and inline SQL references (with FromSymbol set)
	attrRefs := extractAttributeRefs(root, input.Content, namespace, classRanges)
	refs = append(refs, attrRefs...)

	sqlRefs := extractInlineSQLRefs(root, input.Content, namespace, classRanges)
	refs = append(refs, sqlRefs...)

	procRefs := extractStoredProcRefs(root, input.Content, classRanges)
	refs = append(refs, procRefs...)

	varRefs := extractSQLVariableRefs(root, input.Content, classRanges)
	refs = append(refs, varRefs...)

	// Extract ASP.NET Core routing attributes ([Route], [HttpGet], [HttpPost], …)
	// and emit endpoint symbols for cross-language API matching.
	endpointSyms := extractASPNetEndpoints(root, input.Content, namespace)
	symbols = append(symbols, endpointSyms...)

	return &parser.ParseResult{
		Symbols:    symbols,
		References: refs,
	}, nil
}

func processDeclarationList(body *sitter.Node, src []byte, ns string, symbols *[]parser.Symbol, refs *[]parser.RawReference) {
	for i := 0; i < int(body.ChildCount()); i++ {
		child := body.Child(i)
		processTopLevelDecl(child, src, ns, symbols, refs)
	}
}

func processTopLevelDecl(node *sitter.Node, src []byte, ns string, symbols *[]parser.Symbol, refs *[]parser.RawReference) {
	switch node.Type() {
	case "class_declaration":
		syms, rfs := extractClass(node, src, ns)
		*symbols = append(*symbols, syms...)
		*refs = append(*refs, rfs...)

	case "interface_declaration":
		syms, rfs := extractInterface(node, src, ns)
		*symbols = append(*symbols, syms...)
		*refs = append(*refs, rfs...)

	case "struct_declaration":
		syms, rfs := extractStruct(node, src, ns)
		*symbols = append(*symbols, syms...)
		*refs = append(*refs, rfs...)

	case "enum_declaration":
		syms := extractEnum(node, src, ns)
		*symbols = append(*symbols, syms...)
	}
}

func extractNamespaceName(node *sitter.Node, src []byte) string {
	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		switch child.Type() {
		case "qualified_name", "identifier":
			return child.Content(src)
		}
	}
	return ""
}

func extractUsingDirective(node *sitter.Node, src []byte) string {
	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		switch child.Type() {
		case "qualified_name", "identifier":
			return child.Content(src)
		}
	}
	return ""
}

func extractClass(node *sitter.Node, src []byte, ns string) ([]parser.Symbol, []parser.RawReference) {
	var symbols []parser.Symbol
	var refs []parser.RawReference

	name := ""
	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		if child.Type() == "identifier" {
			name = child.Content(src)
			break
		}
	}
	if name == "" {
		return nil, nil
	}

	qname := qualifyCSharp(ns, name)
	symbols = append(symbols, parser.Symbol{
		Name:          name,
		QualifiedName: qname,
		Kind:          "class",
		Language:      "csharp",
		StartLine:     int(node.StartPoint().Row) + 1,
		EndLine:       int(node.EndPoint().Row) + 1,
	})

	// Check base_list for inheritance/implementation
	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		if child.Type() == "base_list" {
			baseRefs := extractBaseList(child, src, qname)
			refs = append(refs, baseRefs...)
		}
	}

	// Extract members from class body
	body := findChild(node, "declaration_list")
	if body != nil {
		memberSyms, memberRefs := extractMembers(body, src, ns, name)
		symbols = append(symbols, memberSyms...)
		refs = append(refs, memberRefs...)
	}

	return symbols, refs
}

func extractInterface(node *sitter.Node, src []byte, ns string) ([]parser.Symbol, []parser.RawReference) {
	var symbols []parser.Symbol
	var refs []parser.RawReference

	name := ""
	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		if child.Type() == "identifier" {
			name = child.Content(src)
			break
		}
	}
	if name == "" {
		return nil, nil
	}

	qname := qualifyCSharp(ns, name)
	symbols = append(symbols, parser.Symbol{
		Name:          name,
		QualifiedName: qname,
		Kind:          "interface",
		Language:      "csharp",
		StartLine:     int(node.StartPoint().Row) + 1,
		EndLine:       int(node.EndPoint().Row) + 1,
	})

	return symbols, refs
}

func extractStruct(node *sitter.Node, src []byte, ns string) ([]parser.Symbol, []parser.RawReference) {
	var symbols []parser.Symbol
	var refs []parser.RawReference

	name := ""
	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		if child.Type() == "identifier" {
			name = child.Content(src)
			break
		}
	}
	if name == "" {
		return nil, nil
	}

	qname := qualifyCSharp(ns, name)
	symbols = append(symbols, parser.Symbol{
		Name:          name,
		QualifiedName: qname,
		Kind:          "class",
		Language:      "csharp",
		StartLine:     int(node.StartPoint().Row) + 1,
		EndLine:       int(node.EndPoint().Row) + 1,
	})

	// Struct body members
	body := findChild(node, "declaration_list")
	if body != nil {
		memberSyms, memberRefs := extractMembers(body, src, ns, name)
		symbols = append(symbols, memberSyms...)
		refs = append(refs, memberRefs...)
	}

	return symbols, refs
}

func extractEnum(node *sitter.Node, src []byte, ns string) []parser.Symbol {
	name := ""
	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		if child.Type() == "identifier" {
			name = child.Content(src)
			break
		}
	}
	if name == "" {
		return nil
	}

	return []parser.Symbol{{
		Name:          name,
		QualifiedName: qualifyCSharp(ns, name),
		Kind:          "enum",
		Language:      "csharp",
		StartLine:     int(node.StartPoint().Row) + 1,
		EndLine:       int(node.EndPoint().Row) + 1,
	}}
}

func extractMembers(body *sitter.Node, src []byte, ns, typeName string) ([]parser.Symbol, []parser.RawReference) {
	var symbols []parser.Symbol
	var refs []parser.RawReference

	for i := 0; i < int(body.ChildCount()); i++ {
		child := body.Child(i)
		switch child.Type() {
		case "method_declaration":
			name, sig := extractMethodDecl(child, src)
			if name != "" {
				qname := qualifyCSharp(ns, typeName+"."+name)
				symbols = append(symbols, parser.Symbol{
					Name:          name,
					QualifiedName: qname,
					Kind:          "method",
					Language:      "csharp",
					StartLine:     int(child.StartPoint().Row) + 1,
					EndLine:       int(child.EndPoint().Row) + 1,
					Signature:     sig,
				})
			}

		case "constructor_declaration":
			name := typeName
			qname := qualifyCSharp(ns, typeName+"."+name)
			symbols = append(symbols, parser.Symbol{
				Name:          name,
				QualifiedName: qname,
				Kind:          "method",
				Language:      "csharp",
				StartLine:     int(child.StartPoint().Row) + 1,
				EndLine:       int(child.EndPoint().Row) + 1,
			})

		case "property_declaration":
			propName := extractPropertyName(child, src)
			if propName != "" {
				symbols = append(symbols, parser.Symbol{
					Name:          propName,
					QualifiedName: qualifyCSharp(ns, typeName+"."+propName),
					Kind:          "property",
					Language:      "csharp",
					StartLine:     int(child.StartPoint().Row) + 1,
					EndLine:       int(child.EndPoint().Row) + 1,
				})

				// Check for DbSet<T> properties
				dbSetType := extractDbSetType(child, src)
				if dbSetType != "" {
					refs = append(refs, parser.RawReference{
						FromSymbol:    qualifyCSharp(ns, typeName),
						ToName:        dbSetType,
						ReferenceType: "uses_table",
						Line:          int(child.StartPoint().Row) + 1,
					})
				}

				// Check for EF navigation properties (virtual ICollection<T>, virtual T)
				navType := extractNavigationProperty(child, src)
				if navType != "" {
					refs = append(refs, parser.RawReference{
						FromSymbol:    qualifyCSharp(ns, typeName),
						ToName:        navType,
						ReferenceType: "references",
						Confidence:    0.85,
						Line:          int(child.StartPoint().Row) + 1,
					})
				}
			}

		case "field_declaration":
			fieldName := extractFieldName(child, src)
			if fieldName != "" {
				symbols = append(symbols, parser.Symbol{
					Name:          fieldName,
					QualifiedName: qualifyCSharp(ns, typeName+"."+fieldName),
					Kind:          "field",
					Language:      "csharp",
					StartLine:     int(child.StartPoint().Row) + 1,
					EndLine:       int(child.EndPoint().Row) + 1,
				})
			}

		// Nested types
		case "class_declaration":
			syms, rfs := extractClass(child, src, ns)
			symbols = append(symbols, syms...)
			refs = append(refs, rfs...)

		case "interface_declaration":
			syms, rfs := extractInterface(child, src, ns)
			symbols = append(symbols, syms...)
			refs = append(refs, rfs...)
		}
	}

	return symbols, refs
}

func extractMethodDecl(node *sitter.Node, src []byte) (string, string) {
	name := ""
	sig := ""
	
	nameNode := node.ChildByFieldName("name")
	if nameNode != nil {
		name = nameNode.Content(src)
	} else {
		for i := 0; i < int(node.ChildCount()); i++ {
			child := node.Child(i)
			if child.Type() == "identifier" && name == "" {
				name = child.Content(src)
			}
		}
	}

	paramNode := node.ChildByFieldName("parameters")
	if paramNode != nil {
		sig = paramNode.Content(src)
	} else {
		for i := 0; i < int(node.ChildCount()); i++ {
			child := node.Child(i)
			if child.Type() == "parameter_list" {
				sig = child.Content(src)
			}
		}
	}
	
	return name, sig
}

func extractPropertyName(node *sitter.Node, src []byte) string {
	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		if child.Type() == "identifier" {
			return child.Content(src)
		}
	}
	return ""
}

func extractFieldName(node *sitter.Node, src []byte) string {
	decl := findChild(node, "variable_declaration")
	if decl == nil {
		return ""
	}
	declarator := findChild(decl, "variable_declarator")
	if declarator != nil {
		for i := 0; i < int(declarator.ChildCount()); i++ {
			child := declarator.Child(i)
			if child.Type() == "identifier" {
				return child.Content(src)
			}
		}
	}
	return ""
}

func extractDbSetType(node *sitter.Node, src []byte) string {
	// Look for DbSet<T> in the property type
	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		if child.Type() == "generic_name" {
			text := child.Content(src)
			if strings.HasPrefix(text, "DbSet<") {
				// Extract T from DbSet<T>
				inner := text[6 : len(text)-1]
				return inner
			}
		}
	}
	return ""
}

// extractNavigationProperty detects EF navigation properties:
// - virtual ICollection<Order> Orders { get; set; }
// - virtual IEnumerable<Order> Orders { get; set; }
// - virtual List<Order> Orders { get; set; }
// - virtual Customer Customer { get; set; }
func extractNavigationProperty(node *sitter.Node, src []byte) string {
	text := node.Content(src)

	// Must have 'virtual' modifier
	if !strings.Contains(text, "virtual") {
		return ""
	}

	// Collection navigation: ICollection<T>, IEnumerable<T>, List<T>, IList<T>, HashSet<T>
	collectionTypes := []string{"ICollection<", "IEnumerable<", "List<", "IList<", "HashSet<"}
	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		if child.Type() == "generic_name" {
			childText := child.Content(src)
			for _, ct := range collectionTypes {
				if strings.HasPrefix(childText, ct) {
					inner := childText[len(ct) : len(childText)-1]
					return inner
				}
			}
		}
	}

	// Single navigation: virtual Customer Customer { get; set; }
	// Look for type_identifier that's a PascalCase name (not a primitive)
	hasVirtual := false
	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		if child.Type() == "modifier" && child.Content(src) == "virtual" {
			hasVirtual = true
		}
	}
	if hasVirtual {
		for i := 0; i < int(node.ChildCount()); i++ {
			child := node.Child(i)
			if child.Type() == "identifier" || child.Type() == "nullable_type" {
				typeName := child.Content(src)
				typeName = strings.TrimSuffix(typeName, "?")
				// Skip primitive types and common non-navigation types
				if !isPrimitiveType(typeName) && len(typeName) > 0 && typeName[0] >= 'A' && typeName[0] <= 'Z' {
					return typeName
				}
			}
		}
	}

	return ""
}

func isPrimitiveType(t string) bool {
	primitives := map[string]bool{
		"string": true, "String": true, "int": true, "Int32": true,
		"long": true, "Int64": true, "bool": true, "Boolean": true,
		"double": true, "Double": true, "float": true, "Single": true,
		"decimal": true, "Decimal": true, "DateTime": true, "DateTimeOffset": true,
		"Guid": true, "byte": true, "Byte": true, "char": true,
		"object": true, "Object": true, "void": true,
	}
	return primitives[t]
}

func extractBaseList(node *sitter.Node, src []byte, fromQName string) []parser.RawReference {
	var refs []parser.RawReference
	line := int(node.StartPoint().Row) + 1

	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		typeName := ""

		switch child.Type() {
		case "identifier", "qualified_name":
			typeName = child.Content(src)
		case "generic_name":
			// Extract the base name before <T>
			for j := 0; j < int(child.ChildCount()); j++ {
				gc := child.Child(j)
				if gc.Type() == "identifier" {
					typeName = gc.Content(src)
					break
				}
			}
		case "simple_base_type":
			// simple_base_type wraps the actual type
			for j := 0; j < int(child.ChildCount()); j++ {
				gc := child.Child(j)
				switch gc.Type() {
				case "identifier", "qualified_name":
					typeName = gc.Content(src)
				case "generic_name":
					for k := 0; k < int(gc.ChildCount()); k++ {
						ggc := gc.Child(k)
						if ggc.Type() == "identifier" {
							typeName = ggc.Content(src)
							break
						}
					}
				}
				if typeName != "" {
					break
				}
			}
		}

		if typeName == "" {
			continue
		}

		if isInterfaceName(typeName) {
			refs = append(refs, parser.RawReference{
				FromSymbol:    fromQName,
				ToName:        typeName,
				ReferenceType: "implements",
				Line:          line,
			})
		} else {
			refs = append(refs, parser.RawReference{
				FromSymbol:    fromQName,
				ToName:        typeName,
				ReferenceType: "inherits",
				Line:          line,
			})
		}
	}

	return refs
}

// classRange holds byte range and qualified name for a class (used to resolve FromSymbol).
type classRange struct {
	start, end uint32
	qname      string
}

// buildClassRanges collects all class declarations with their ranges and qualified names.
func buildClassRanges(root *sitter.Node, src []byte, namespace string) []classRange {
	var ranges []classRange
	walkTree(root, func(node *sitter.Node) {
		if node.Type() != "class_declaration" {
			return
		}
		name := ""
		for i := 0; i < int(node.ChildCount()); i++ {
			child := node.Child(i)
			if child.Type() == "identifier" {
				name = child.Content(src)
				break
			}
		}
		if name == "" {
			return
		}
		qname := qualifyCSharp(namespace, name)
		ranges = append(ranges, classRange{
			start: node.StartByte(),
			end:   node.EndByte(),
			qname: qname,
		})
	})
	return ranges
}

// findEnclosingClass returns the qualified name of the innermost class containing the given node.
func findEnclosingClass(node *sitter.Node, classRanges []classRange) string {
	start := node.StartByte()
	end := node.EndByte()
	var best *classRange
	for i := range classRanges {
		r := &classRanges[i]
		if r.start <= start && end <= r.end {
			if best == nil || (r.end-r.start) < (best.end-best.start) {
				best = r
			}
		}
	}
	if best == nil {
		return ""
	}
	return best.qname
}

func extractAttributeRefs(root *sitter.Node, src []byte, _ string, classRanges []classRange) []parser.RawReference {
	var refs []parser.RawReference

	walkTree(root, func(node *sitter.Node) {
		if node.Type() != "attribute" {
			return
		}

		text := node.Content(src)
		line := int(node.StartPoint().Row) + 1
		fromSymbol := findEnclosingClass(node, classRanges)

		// [Table("Users")]
		if strings.Contains(text, "Table") {
			tableName := extractAttributeStringParam(text)
			if tableName != "" {
				refs = append(refs, parser.RawReference{
					FromSymbol:    fromSymbol,
					ToName:        tableName,
					ToQualified:   "dbo." + tableName,
					ReferenceType: "uses_table",
					Line:          line,
				})
			}
		}
	})

	return refs
}

func extractInlineSQLRefs(root *sitter.Node, src []byte, _ string, classRanges []classRange) []parser.RawReference {
	var refs []parser.RawReference

	// Methods that take SQL statement strings (SELECT, INSERT, etc.)
	sqlStatementMethods := map[string]bool{
		"FromSqlRaw":             true,
		"FromSqlInterpolated":    true,
		"ExecuteSqlRaw":          true,
		"ExecuteSqlInterpolated": true,
		"SqlQuery":               true,
		"Query":                  true,
		"QueryFirst":             true,
		"QuerySingle":            true,
		"QueryFirstOrDefault":    true,
		"QueryAsync":             true,
		"QueryMultiple":          true,
		"QueryFirstAsync":        true,
		"QuerySingleAsync":       true,
	}

	// Methods that take a stored procedure NAME as first string arg
	procNameMethods := map[string]bool{
		"ExecuteNonQuery": true,
		"ExecuteReader":   true,
		"ExecuteScalar":   true,
		"Execute":         true,
		"ExecuteAsync":    true,
		"GetDataReader":   true,
		"GetData":         true,
		"BulkInsert":      true,
		"IDataReader":     true,
	}

	walkTree(root, func(node *sitter.Node) {
		if node.Type() != "invocation_expression" {
			return
		}

		line := int(node.StartPoint().Row) + 1
		fromSymbol := findEnclosingClass(node, classRanges)

		// Check if invocation calls a SQL method
		memberAccess := findChild(node, "member_access_expression")
		if memberAccess == nil {
			return
		}

		// The method name is the last identifier in the member access
		methodName := ""
		for i := 0; i < int(memberAccess.ChildCount()); i++ {
			child := memberAccess.Child(i)
			if child.Type() == "identifier" {
				methodName = child.Content(src)
			}
		}

		// Extract string literal argument
		argList := findChild(node, "argument_list")
		if argList == nil {
			return
		}

		if sqlStatementMethods[methodName] {
			// Existing behavior: extract SQL string, parse table refs
			for i := 0; i < int(argList.ChildCount()); i++ {
				arg := argList.Child(i)
				sqlStr := extractStringLiteral(arg, src)
				if sqlStr != "" && looksLikeSQL(sqlStr) {
					tableRefs := extractSQLTableRefs(sqlStr, line, fromSymbol)
					refs = append(refs, tableRefs...)
				}
			}
		} else if procNameMethods[methodName] {
			// First string arg is the proc name (or inline SQL)
			firstStr := extractFirstStringArg(argList, src)
			if firstStr == "" {
				return
			}
			if looksLikeSQL(firstStr) {
				// It's an inline SQL statement, extract table refs
				tableRefs := extractSQLTableRefs(firstStr, line, fromSymbol)
				refs = append(refs, tableRefs...)
			} else {
				// It's a stored procedure name
				procName := strings.TrimPrefix(firstStr, "dbo.")
				refs = append(refs, parser.RawReference{
					FromSymbol:    fromSymbol,
					ToName:        procName,
					ToQualified:   "dbo." + procName,
					ReferenceType: "calls",
					Line:          line,
				})
			}
		} else if methodName == "Include" || methodName == "ThenInclude" {
			// .Include("Orders") or .Include("Customer")
			firstStr := extractFirstStringArg(argList, src)
			if firstStr != "" {
				refs = append(refs, parser.RawReference{
					FromSymbol:    fromSymbol,
					ToName:        firstStr,
					ReferenceType: "references",
					Confidence:    0.8,
					Line:          line,
				})
			}
		}
	})

	return refs
}

// extractFirstStringArg returns the first string literal found in an argument list.
func extractFirstStringArg(argList *sitter.Node, src []byte) string {
	for i := 0; i < int(argList.ChildCount()); i++ {
		arg := argList.Child(i)
		if s := extractStringLiteral(arg, src); s != "" {
			return s
		}
	}
	return ""
}

// adoCommandTypes are ADO.NET command/adapter types whose first string
// constructor argument is a SQL statement or stored procedure name. Includes
// the legacy OleDb/Odbc providers common in .NET Framework-era enterprise apps.
var adoCommandTypes = map[string]bool{
	"SqlCommand":       true,
	"OleDbCommand":     true,
	"OdbcCommand":      true,
	"SqlCeCommand":     true,
	"SqlDataAdapter":   true,
	"OleDbDataAdapter": true,
	"OdbcDataAdapter":  true,
}

// extractStoredProcRefs detects ADO.NET command constructor and CommandText assignment patterns.
func extractStoredProcRefs(root *sitter.Node, src []byte, classRanges []classRange) []parser.RawReference {
	var refs []parser.RawReference

	walkTree(root, func(node *sitter.Node) {
		line := int(node.StartPoint().Row) + 1
		fromSymbol := findEnclosingClass(node, classRanges)

		switch node.Type() {
		case "object_creation_expression":
			// new SqlCommand("ProcName", ...), new SqlDataAdapter("SELECT ...", ...)
			typeName := ""
			for i := 0; i < int(node.ChildCount()); i++ {
				child := node.Child(i)
				if child.Type() == "identifier" || child.Type() == "qualified_name" {
					typeName = child.Content(src)
					break
				}
			}
			// Allow qualified names like System.Data.SqlClient.SqlCommand
			if idx := strings.LastIndex(typeName, "."); idx >= 0 {
				typeName = typeName[idx+1:]
			}
			if !adoCommandTypes[typeName] {
				return
			}
			argList := findChild(node, "argument_list")
			if argList == nil {
				return
			}
			firstStr := extractFirstStringArg(argList, src)
			if firstStr == "" {
				return
			}
			if looksLikeSQL(firstStr) {
				tableRefs := extractSQLTableRefs(firstStr, line, fromSymbol)
				refs = append(refs, tableRefs...)
			} else {
				procName := strings.TrimPrefix(firstStr, "dbo.")
				refs = append(refs, parser.RawReference{
					FromSymbol:    fromSymbol,
					ToName:        procName,
					ToQualified:   "dbo." + procName,
					ReferenceType: "calls",
					Line:          line,
				})
			}

		case "assignment_expression":
			// cmd.CommandText = "ProcName"
			left := node.Child(0)
			if left == nil {
				return
			}
			leftText := left.Content(src)
			if !strings.HasSuffix(leftText, ".CommandText") && leftText != "CommandText" {
				return
			}
			// Right side is the value after '='
			for i := 0; i < int(node.ChildCount()); i++ {
				child := node.Child(i)
				valStr := extractStringLiteral(child, src)
				if valStr == "" {
					continue
				}
				if looksLikeSQL(valStr) {
					tableRefs := extractSQLTableRefs(valStr, line, fromSymbol)
					refs = append(refs, tableRefs...)
				} else {
					procName := strings.TrimPrefix(valStr, "dbo.")
					refs = append(refs, parser.RawReference{
						FromSymbol:    fromSymbol,
						ToName:        procName,
						ToQualified:   "dbo." + procName,
						ReferenceType: "calls",
						Line:          line,
					})
				}
				return
			}
		}
	})

	return refs
}

// extractSQLVariableRefs finds SQL statements assigned to local variables or
// fields — the dominant ADO.NET pattern in .NET Framework-era code:
//
//	string sql = "SELECT * FROM Customers WHERE Id = " + id;
//	sql += " ORDER BY Name";
//	cmd.CommandText = sql;
//
// A variable becomes a SQL source when first assigned a string starting with a
// SQL verb; later += / self-concat appends to known SQL variables contribute
// additional fragments. Only plain string expressions (literals and
// concatenations) are considered; invocation results are left to
// extractInlineSQLRefs to avoid double counting.
func extractSQLVariableRefs(root *sitter.Node, src []byte, classRanges []classRange) []parser.RawReference {
	var refs []parser.RawReference
	sqlVars := map[string]bool{} // lowercased variable names known to hold SQL

	emit := func(node *sitter.Node, name string, valueNode *sitter.Node, isAppend bool) {
		if valueNode == nil || !isStringExpression(valueNode) {
			return
		}
		sqlStr := collectStringParts(valueNode, src)
		if sqlStr == "" {
			return
		}
		key := strings.ToLower(name)
		if isAppend {
			// Appends only count for variables already known to hold SQL —
			// "log += \" from the server\"" must not produce table refs.
			if !sqlVars[key] {
				return
			}
		} else {
			if !startsWithSQLVerb(sqlStr) {
				return
			}
			sqlVars[key] = true
		}
		line := int(node.StartPoint().Row) + 1
		fromSymbol := findEnclosingClass(node, classRanges)
		refs = append(refs, extractSQLTableRefs(sqlStr, line, fromSymbol)...)
	}

	walkTree(root, func(node *sitter.Node) {
		switch node.Type() {
		case "variable_declarator":
			// shapes: (identifier "=" value) or (identifier (equals_value_clause value))
			name := ""
			var valueNode *sitter.Node
			for i := 0; i < int(node.ChildCount()); i++ {
				child := node.Child(i)
				switch child.Type() {
				case "identifier":
					if name == "" {
						name = child.Content(src)
					}
				case "equals_value_clause":
					if child.ChildCount() > 0 {
						valueNode = child.Child(int(child.ChildCount()) - 1)
					}
				case "string_literal", "verbatim_string_literal", "binary_expression":
					valueNode = child
				}
			}
			if name != "" {
				emit(node, name, valueNode, false)
			}

		case "assignment_expression":
			// sql = "..." / sql += "..." / sql = sql + "..." — plain identifiers
			// only; member targets like cmd.CommandText are handled by
			// extractStoredProcRefs.
			left := node.Child(0)
			if left == nil || left.Type() != "identifier" {
				return
			}
			name := left.Content(src)
			op := ""
			if opNode := node.Child(1); opNode != nil {
				op = opNode.Content(src)
			}
			valueNode := node.Child(int(node.ChildCount()) - 1)
			isAppend := op == "+=" || isSelfConcat(valueNode, name, src)
			emit(node, name, valueNode, isAppend)
		}
	})

	return refs
}

// isSelfConcat reports whether value has the shape "name + ..." (the VB/C#
// self-append idiom written without +=).
func isSelfConcat(value *sitter.Node, name string, src []byte) bool {
	if value == nil || value.Type() != "binary_expression" {
		return false
	}
	left := value.Child(0)
	return left != nil && left.Type() == "identifier" && left.Content(src) == name
}

// startsWithSQLVerb reports whether s begins with a SQL statement keyword.
// This is stricter than looksLikeSQL, which matches keywords anywhere and
// would treat prose like "loaded from the cache" as SQL.
func startsWithSQLVerb(s string) bool {
	upper := strings.ToUpper(strings.TrimSpace(s))
	for _, kw := range []string{"SELECT", "INSERT", "UPDATE", "DELETE", "EXEC", "EXECUTE", "MERGE", "TRUNCATE", "WITH"} {
		if strings.HasPrefix(upper, kw+" ") {
			return true
		}
	}
	return false
}

// isStringExpression reports whether node is a string literal or a
// concatenation expression (rather than e.g. an invocation result).
func isStringExpression(node *sitter.Node) bool {
	switch node.Type() {
	case "string_literal", "verbatim_string_literal", "binary_expression":
		return true
	}
	return false
}

// collectStringParts concatenates the contents of all string literals in an
// expression, skipping literals inside argument lists (method call arguments).
func collectStringParts(node *sitter.Node, src []byte) string {
	var parts []string
	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		switch n.Type() {
		case "argument_list":
			return // don't pick up strings passed to nested calls
		case "string_literal", "verbatim_string_literal":
			if s := extractStringLiteral(n, src); s != "" {
				parts = append(parts, s)
			}
			return
		}
		for i := 0; i < int(n.ChildCount()); i++ {
			walk(n.Child(i))
		}
	}
	walk(node)
	return strings.Join(parts, " ")
}

func extractStringLiteral(node *sitter.Node, src []byte) string {
	// Walk into argument node to find string_literal or interpolated_string
	var result string
	walkTree(node, func(n *sitter.Node) {
		if result != "" {
			return
		}
		if n.Type() == "string_literal" || n.Type() == "verbatim_string_literal" {
			content := n.Content(src)
			// Strip quotes
			if len(content) >= 2 {
				if content[0] == '@' && len(content) >= 3 {
					result = content[2 : len(content)-1] // @"..."
				} else {
					result = content[1 : len(content)-1] // "..."
				}
			}
		}
	})
	return result
}

func extractAttributeStringParam(text string) string {
	idx := strings.IndexByte(text, '"')
	if idx < 0 {
		return ""
	}
	end := strings.IndexByte(text[idx+1:], '"')
	if end < 0 {
		return ""
	}
	return text[idx+1 : idx+1+end]
}

func qualifyCSharp(namespace, name string) string {
	if namespace != "" {
		return namespace + "." + name
	}
	return name
}

func isInterfaceName(name string) bool {
	// C# convention: interfaces start with 'I' followed by an uppercase letter
	if len(name) < 2 {
		return false
	}
	return name[0] == 'I' && name[1] >= 'A' && name[1] <= 'Z'
}

func findChild(node *sitter.Node, nodeType string) *sitter.Node {
	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		if child.Type() == nodeType {
			return child
		}
	}
	return nil
}

func walkTree(node *sitter.Node, fn func(*sitter.Node)) {
	fn(node)
	for i := 0; i < int(node.ChildCount()); i++ {
		walkTree(node.Child(i), fn)
	}
}

func looksLikeSQL(s string) bool {
	upper := strings.ToUpper(strings.TrimSpace(s))
	// Check for SQL keywords that should appear as whole words at the start
	// or preceded by whitespace (not as substrings of identifiers like "DeleteUser")
	for _, kw := range []string{"SELECT", "INSERT", "UPDATE", "DELETE", "FROM", "EXEC", "EXECUTE"} {
		if containsSQLKeyword(upper, kw) {
			return true
		}
	}
	return false
}

// containsSQLKeyword checks if kw appears as a word boundary in s
// (at start of string or after whitespace, followed by end/whitespace/punctuation).
func containsSQLKeyword(upper, kw string) bool {
	idx := 0
	for {
		pos := strings.Index(upper[idx:], kw)
		if pos < 0 {
			return false
		}
		absPos := idx + pos
		// Check left boundary: must be at start or preceded by whitespace/punctuation
		if absPos > 0 {
			ch := upper[absPos-1]
			if ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '_' {
				idx = absPos + len(kw)
				continue
			}
		}
		// Check right boundary: must be at end or followed by whitespace/punctuation
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

func extractSQLTableRefs(sql string, line int, fromSymbol string) []parser.RawReference {
	var refs []parser.RawReference
	upper := strings.ToUpper(sql)
	keywords := []string{"FROM", "JOIN", "INTO", "UPDATE"}

	for _, kw := range keywords {
		idx := 0
		for {
			pos := strings.Index(upper[idx:], kw+" ")
			if pos < 0 {
				break
			}
			pos += idx + len(kw) + 1
			rest := strings.TrimSpace(sql[pos:])
			end := strings.IndexAny(rest, " \t\n,;)")
			tableName := rest
			if end > 0 {
				tableName = rest[:end]
			}
			tableName = strings.TrimSpace(tableName)
			if tableName != "" && !isSQLKeyword(tableName) {
				refs = append(refs, parser.RawReference{
					FromSymbol:    fromSymbol,
					ToName:        tableName,
					ToQualified:   "dbo." + tableName,
					ReferenceType: "uses_table",
					Line:          line,
				})
			}
			idx = pos
		}
	}

	// Extract EXEC/EXECUTE proc references
	for _, execKw := range []string{"EXEC ", "EXECUTE "} {
		idx := 0
		for {
			pos := strings.Index(upper[idx:], execKw)
			if pos < 0 {
				break
			}
			pos += idx + len(execKw)
			rest := strings.TrimSpace(sql[pos:])
			end := strings.IndexAny(rest, " \t\n,;(@")
			procName := rest
			if end > 0 {
				procName = rest[:end]
			}
			procName = strings.TrimSpace(procName)
			if procName != "" && !isSQLKeyword(procName) {
				refs = append(refs, parser.RawReference{
					FromSymbol:    fromSymbol,
					ToName:        procName,
					ToQualified:   "dbo." + procName,
					ReferenceType: "calls",
					Line:          line,
				})
			}
			idx = pos
		}
	}

	return refs
}

func isSQLKeyword(s string) bool {
	kw := map[string]bool{
		"SELECT": true, "FROM": true, "WHERE": true, "AND": true,
		"OR": true, "SET": true, "VALUES": true, "AS": true,
		"ON": true, "IN": true, "NOT": true, "NULL": true,
	}
	return kw[strings.ToUpper(s)]
}

// ---------------------------------------------------------------------------
// Method body reference extraction (calls, object creation, type references)
// ---------------------------------------------------------------------------

// extractMethodBodyRefs walks the entire tree looking for method invocations and
// object creation expressions inside method/constructor bodies, emitting "calls"
// and "references" edges. It also extracts type references from method parameters,
// return types, and field/property types.
func extractMethodBodyRefs(root *sitter.Node, src []byte, ns string) []parser.RawReference {
	var refs []parser.RawReference

	// Build method ranges: method_declaration / constructor_declaration → enclosing class qname + method name
	type methodInfo struct {
		qname     string // qualified name of the method
		classQN   string // qualified name of the enclosing class
		startByte uint32
		endByte   uint32
	}
	var methods []methodInfo

	walkTree(root, func(node *sitter.Node) {
		switch node.Type() {
		case "method_declaration", "constructor_declaration":
			// Find enclosing class
			classQN := findEnclosingClassNode(node, src, ns)
			if classQN == "" {
				return
			}

			methodName := ""
			if node.Type() == "constructor_declaration" {
				// Constructor name = class short name
				parts := strings.Split(classQN, ".")
				methodName = parts[len(parts)-1]
			} else {
				nameNode := node.ChildByFieldName("name")
				if nameNode != nil {
					methodName = nameNode.Content(src)
				} else {
					for i := 0; i < int(node.ChildCount()); i++ {
						c := node.Child(i)
						if c.Type() == "identifier" {
							methodName = c.Content(src)
							break
						}
					}
				}
			}
			if methodName == "" {
				return
			}

			qname := classQN + "." + methodName
			methods = append(methods, methodInfo{
				qname:     qname,
				classQN:   classQN,
				startByte: node.StartByte(),
				endByte:   node.EndByte(),
			})

			// Extract return type reference (for method_declaration only)
			if node.Type() == "method_declaration" {
				retType := extractReturnType(node, src)
				if retType != "" && !isPrimitiveType(retType) && !isCommonFrameworkType(retType) {
					refs = append(refs, parser.RawReference{
						FromSymbol:    qname,
						ToName:        retType,
						ReferenceType: "references",
						Confidence:    0.7,
						Line:          int(node.StartPoint().Row) + 1,
					})
				}
			}

			// Extract parameter type references
			paramTypes := extractParamTypes(node, src)
			for _, pt := range paramTypes {
				if !isPrimitiveType(pt) && !isCommonFrameworkType(pt) {
					refs = append(refs, parser.RawReference{
						FromSymbol:    qname,
						ToName:        pt,
						ReferenceType: "references",
						Confidence:    0.7,
						Line:          int(node.StartPoint().Row) + 1,
					})
				}
			}

		case "field_declaration", "property_declaration":
			classQN := findEnclosingClassNode(node, src, ns)
			if classQN == "" {
				return
			}
			typeName := extractDeclaredType(node, src)
			if typeName != "" && !isPrimitiveType(typeName) && !isCommonFrameworkType(typeName) {
				refs = append(refs, parser.RawReference{
					FromSymbol:    classQN,
					ToName:        typeName,
					ReferenceType: "references",
					Confidence:    0.7,
					Line:          int(node.StartPoint().Row) + 1,
				})
			}
		}
	})

	// Find the enclosing method for a given byte position
	findMethod := func(bytePos uint32) *methodInfo {
		var best *methodInfo
		for i := range methods {
			m := &methods[i]
			if m.startByte <= bytePos && bytePos <= m.endByte {
				if best == nil || (m.endByte-m.startByte) < (best.endByte-best.startByte) {
					best = m
				}
			}
		}
		return best
	}

	// Walk for invocation_expression and object_creation_expression
	walkTree(root, func(node *sitter.Node) {
		switch node.Type() {
		case "invocation_expression":
			m := findMethod(node.StartByte())
			if m == nil {
				return
			}
			calledMethod := extractInvocationTarget(node, src)
			if calledMethod == "" {
				return
			}
			// Skip common framework/language methods
			if isCommonMethod(calledMethod) {
				return
			}
			refs = append(refs, parser.RawReference{
				FromSymbol:    m.qname,
				ToName:        calledMethod,
				ReferenceType: "calls",
				Confidence:    0.8,
				Line:          int(node.StartPoint().Row) + 1,
			})

		case "object_creation_expression":
			m := findMethod(node.StartByte())
			if m == nil {
				return
			}
			typeName := extractCreatedType(node, src)
			if typeName == "" || isPrimitiveType(typeName) || isCommonFrameworkType(typeName) {
				return
			}
			refs = append(refs, parser.RawReference{
				FromSymbol:    m.qname,
				ToName:        typeName,
				ReferenceType: "references",
				Confidence:    0.8,
				Line:          int(node.StartPoint().Row) + 1,
			})
		}
	})

	return refs
}

// findEnclosingClassNode walks up the parent chain to find the enclosing class.
func findEnclosingClassNode(node *sitter.Node, src []byte, ns string) string {
	for p := node.Parent(); p != nil; p = p.Parent() {
		if p.Type() == "class_declaration" || p.Type() == "struct_declaration" {
			name := ""
			for i := 0; i < int(p.ChildCount()); i++ {
				c := p.Child(i)
				if c.Type() == "identifier" {
					name = c.Content(src)
					break
				}
			}
			if name != "" {
				return qualifyCSharp(ns, name)
			}
		}
	}
	return ""
}

// extractInvocationTarget extracts the method name from an invocation expression.
// For `obj.Method()` returns "Method", for `Method()` returns "Method".
func extractInvocationTarget(node *sitter.Node, src []byte) string {
	if node.ChildCount() == 0 {
		return ""
	}
	first := node.Child(0)

	switch first.Type() {
	case "member_access_expression":
		// Get the last identifier (the method name)
		nameNode := first.ChildByFieldName("name")
		if nameNode != nil {
			return nameNode.Content(src)
		}
		// Fallback: last identifier child
		for i := int(first.ChildCount()) - 1; i >= 0; i-- {
			c := first.Child(i)
			if c.Type() == "identifier" || c.Type() == "generic_name" {
				name := c.Content(src)
				// Strip generic params
				if idx := strings.IndexByte(name, '<'); idx > 0 {
					name = name[:idx]
				}
				return name
			}
		}
	case "identifier":
		return first.Content(src)
	case "generic_name":
		name := first.Content(src)
		if idx := strings.IndexByte(name, '<'); idx > 0 {
			name = name[:idx]
		}
		return name
	}
	return ""
}

// extractCreatedType extracts the type name from `new TypeName(...)`.
func extractCreatedType(node *sitter.Node, src []byte) string {
	for i := 0; i < int(node.ChildCount()); i++ {
		c := node.Child(i)
		switch c.Type() {
		case "identifier":
			return c.Content(src)
		case "qualified_name":
			// Return last part
			text := c.Content(src)
			parts := strings.Split(text, ".")
			return parts[len(parts)-1]
		case "generic_name":
			// Extract base name before <
			for j := 0; j < int(c.ChildCount()); j++ {
				gc := c.Child(j)
				if gc.Type() == "identifier" {
					return gc.Content(src)
				}
			}
		}
	}
	return ""
}

// extractReturnType extracts the return type from a method declaration.
func extractReturnType(node *sitter.Node, src []byte) string {
	retNode := node.ChildByFieldName("type")
	if retNode != nil {
		return extractTypeName(retNode, src)
	}
	// Fallback: scan children for the return type.
	// In tree-sitter C#, method_declaration children are typically:
	//   [modifiers...] [return_type] [identifier(name)] [parameter_list] [body]
	// We look for a type-like node that is followed by the method name identifier.
	for i := 0; i < int(node.ChildCount()); i++ {
		c := node.Child(i)
		switch c.Type() {
		case "predefined_type", "generic_name", "qualified_name", "nullable_type", "array_type":
			return extractTypeName(c, src)
		case "identifier":
			// Could be a return type identifier. Check if the next sibling is
			// another identifier (the method name) or a parameter_list.
			if i+1 < int(node.ChildCount()) {
				next := node.Child(i + 1)
				if next.Type() == "identifier" || next.Type() == "parameter_list" {
					return c.Content(src)
				}
			}
		}
	}
	return ""
}

// extractParamTypes extracts type names from method parameters.
func extractParamTypes(node *sitter.Node, src []byte) []string {
	paramList := node.ChildByFieldName("parameters")
	if paramList == nil {
		paramList = findChild(node, "parameter_list")
	}
	if paramList == nil {
		return nil
	}

	var types []string
	for i := 0; i < int(paramList.ChildCount()); i++ {
		param := paramList.Child(i)
		if param.Type() != "parameter" {
			continue
		}
		typeNode := param.ChildByFieldName("type")
		if typeNode == nil {
			// Fallback: first type-like child
			for j := 0; j < int(param.ChildCount()); j++ {
				c := param.Child(j)
				if c.Type() == "identifier" || c.Type() == "generic_name" || c.Type() == "qualified_name" || c.Type() == "predefined_type" || c.Type() == "nullable_type" {
					typeNode = c
					break
				}
			}
		}
		if typeNode != nil {
			typeName := extractTypeName(typeNode, src)
			if typeName != "" {
				types = append(types, typeName)
			}
		}
	}
	return types
}

// extractTypeName extracts a clean type name from a type node.
func extractTypeName(node *sitter.Node, src []byte) string {
	switch node.Type() {
	case "predefined_type":
		return node.Content(src)
	case "identifier":
		return node.Content(src)
	case "qualified_name":
		text := node.Content(src)
		parts := strings.Split(text, ".")
		return parts[len(parts)-1]
	case "generic_name":
		for i := 0; i < int(node.ChildCount()); i++ {
			c := node.Child(i)
			if c.Type() == "identifier" {
				return c.Content(src)
			}
		}
	case "nullable_type":
		// T? → extract T
		for i := 0; i < int(node.ChildCount()); i++ {
			c := node.Child(i)
			return extractTypeName(c, src)
		}
	case "array_type":
		// T[] → extract T
		for i := 0; i < int(node.ChildCount()); i++ {
			c := node.Child(i)
			t := extractTypeName(c, src)
			if t != "" {
				return t
			}
		}
	}
	return ""
}

// extractDeclaredType extracts the type from a field or property declaration.
func extractDeclaredType(node *sitter.Node, src []byte) string {
	// For property_declaration: the type is typically the first type-like child
	// For field_declaration: look inside variable_declaration
	if node.Type() == "field_declaration" {
		varDecl := findChild(node, "variable_declaration")
		if varDecl != nil {
			for i := 0; i < int(varDecl.ChildCount()); i++ {
				c := varDecl.Child(i)
				t := extractTypeName(c, src)
				if t != "" && !isPrimitiveType(t) {
					return t
				}
			}
		}
		return ""
	}

	// property_declaration
	for i := 0; i < int(node.ChildCount()); i++ {
		c := node.Child(i)
		switch c.Type() {
		case "identifier", "generic_name", "qualified_name", "nullable_type", "predefined_type", "array_type":
			t := extractTypeName(c, src)
			if t != "" && !isPrimitiveType(t) {
				return t
			}
		}
	}
	return ""
}

// isCommonFrameworkType returns true for types that are part of .NET framework/runtime.
func isCommonFrameworkType(t string) bool {
	common := map[string]bool{
		"Task": true, "IActionResult": true, "ActionResult": true,
		"IEnumerable": true, "ICollection": true, "IList": true, "List": true,
		"Dictionary": true, "HashSet": true, "ILogger": true, "CancellationToken": true,
		"HttpResponseMessage": true, "IHttpActionResult": true,
		"IDisposable": true, "EventArgs": true, "Exception": true,
		"StringBuilder": true, "Stream": true, "IConfiguration": true,
		"IServiceProvider": true, "Type": true, "Attribute": true,
		"var": true, "dynamic": true, "IFormFile": true,
	}
	return common[t]
}

// isCommonMethod returns true for method names that are too generic to be useful edges.
func isCommonMethod(name string) bool {
	common := map[string]bool{
		"ToString": true, "GetType": true, "Equals": true, "GetHashCode": true,
		"ReferenceEquals": true, "MemberwiseClone": true,
		"Add": true, "Remove": true, "Contains": true, "Clear": true,
		"Count": true, "ToList": true, "ToArray": true,
		"Select": true, "Where": true, "FirstOrDefault": true, "First": true,
		"SingleOrDefault": true, "Single": true, "Any": true, "All": true,
		"OrderBy": true, "OrderByDescending": true, "GroupBy": true,
		"Skip": true, "Take": true, "Aggregate": true, "Distinct": true,
		"Concat": true, "Join": true, "Zip": true,
		"Wait": true, "Result": true, "ConfigureAwait": true,
		"Dispose": true, "Close": true, "Write": true, "Read": true,
		"Log": true, "LogInformation": true, "LogWarning": true, "LogError": true, "LogDebug": true,
		"Format": true, "Append": true, "AppendLine": true,
		"Ok": true, "BadRequest": true, "NotFound": true, "StatusCode": true,
		"Request": true, "Response": true,
		// ADO.NET / data access methods (already handled by extractInlineSQLRefs/extractStoredProcRefs)
		"ExecuteNonQuery": true, "ExecuteReader": true, "ExecuteScalar": true,
		"Execute": true, "ExecuteAsync": true,
		"FromSqlRaw": true, "FromSqlInterpolated": true,
		"ExecuteSqlRaw": true, "ExecuteSqlInterpolated": true,
		"SqlQuery": true, "Query": true, "QueryFirst": true, "QuerySingle": true,
		"QueryFirstOrDefault": true, "QueryAsync": true, "QueryMultiple": true,
		"QueryFirstAsync": true, "QuerySingleAsync": true,
		"GetDataReader": true, "GetData": true, "BulkInsert": true, "IDataReader": true,
		"Include": true, "ThenInclude": true,
		// Common framework methods
		"CreateResponse": true, "CreateErrorResponse": true,
	}
	return common[name]
}

// ---------------------------------------------------------------------------
// ASP.NET Core endpoint extraction
// ---------------------------------------------------------------------------

// aspVerbAttrs maps attribute names to HTTP verbs. An empty string means the
// verb must be determined from the attribute value (e.g. [Route]).
var aspVerbAttrs = map[string]string{
	"HttpGet":     "GET",
	"HttpPost":    "POST",
	"HttpPut":     "PUT",
	"HttpPatch":   "PATCH",
	"HttpDelete":  "DELETE",
	"HttpHead":    "HEAD",
	"HttpOptions": "OPTIONS",
	"Route":       "", // verb-agnostic; used at class level for base path
}

// extractASPNetEndpoints walks the tree and emits endpoint symbols for every
// ASP.NET Core controller method decorated with a routing attribute.
//
// Each symbol has:
//   Kind:          "endpoint"
//   QualifiedName: "<Namespace>.<Controller>.<Method>"
//   Signature:     "GET /api/orders/{id}"  — the normalized route
func extractASPNetEndpoints(root *sitter.Node, src []byte, ns string) []parser.Symbol {
	var endpoints []parser.Symbol

	// DNN controller base classes that indicate an API controller.
	dnnControllerBases := map[string]bool{
		"DnnApiController":      true,
		"DnnController":         true,
		"ServicesApiController":  true,
		"DnnModuleController":   true,
	}

	// DNN return types that indicate an API endpoint method.
	dnnReturnTypes := map[string]bool{
		"HttpResponseMessage": true,
		"IHttpActionResult":   true,
	}

	// Collect class-level base routes: class_declaration → attributes → [Route("base")]
	type controllerInfo struct {
		qname     string
		basePaths []string // may be multiple [Route] attributes
		isDNN     bool     // true if inherits from a DNN controller base
	}
	controllers := map[string]*controllerInfo{} // name → info

	walkTree(root, func(node *sitter.Node) {
		if node.Type() != "class_declaration" {
			return
		}

		className := ""
		for i := 0; i < int(node.ChildCount()); i++ {
			child := node.Child(i)
			if child.Type() == "identifier" {
				className = child.Content(src)
				break
			}
		}
		if className == "" {
			return
		}

		// Check for DNN controller base classes
		isDNN := false
		baseList := findChild(node, "base_list")
		if baseList != nil {
			walkTree(baseList, func(n *sitter.Node) {
				if n.Type() == "identifier" {
					if dnnControllerBases[n.Content(src)] {
						isDNN = true
					}
				}
			})
		}

		// Only process Controller classes (ASP.NET convention or DNN base)
		if !isDNN && !strings.HasSuffix(className, "Controller") && !hasAttributeOnNode(node, src, "ApiController") {
			return
		}

		qname := qualifyCSharp(ns, className)
		info := &controllerInfo{qname: qname, isDNN: isDNN}
		controllers[className] = info

		// Collect class-level [Route("...")] attributes
		for i := 0; i < int(node.ChildCount()); i++ {
			child := node.Child(i)
			if child.Type() == "attribute_list" {
				walkTree(child, func(attr *sitter.Node) {
					if attr.Type() != "attribute" {
						return
					}
					attrName := extractAttrName(attr, src)
					if attrName == "Route" || attrName == "RoutePrefix" {
						path := extractAttrStringParam(attr, src)
						if path != "" {
							info.basePaths = append(info.basePaths, path)
						}
					}
				})
			}
		}

		// Default base path: [controller] → lowercase controller name without suffix
		if len(info.basePaths) == 0 {
			info.basePaths = []string{"[controller]"}
		}

		// Walk class body looking for method declarations with HTTP verb attributes
		body := findChild(node, "declaration_list")
		if body == nil {
			return
		}

		for i := 0; i < int(body.ChildCount()); i++ {
			member := body.Child(i)
			if member.Type() != "method_declaration" {
				continue
			}

			methodName := ""
			nameNode := member.ChildByFieldName("name")
			if nameNode != nil {
				methodName = nameNode.Content(src)
			} else {
				// Fallback: find the identifier that precedes a parameter_list
				for j := 0; j < int(member.ChildCount()); j++ {
					mc := member.Child(j)
					if mc.Type() == "identifier" && j+1 < int(member.ChildCount()) {
						next := member.Child(j + 1)
						if next.Type() == "parameter_list" {
							methodName = mc.Content(src)
							break
						}
					}
				}
			}
			if methodName == "" {
				continue
			}

			// Collect all routing attributes on this method
			hasRoutingAttr := false
			for j := 0; j < int(member.ChildCount()); j++ {
				mc := member.Child(j)
				if mc.Type() != "attribute_list" {
					continue
				}
				walkTree(mc, func(attr *sitter.Node) {
					if attr.Type() != "attribute" {
						return
					}
					attrName := extractAttrName(attr, src)
					verb, isRoutingAttr := aspVerbAttrs[attrName]
					if !isRoutingAttr {
						return
					}
					hasRoutingAttr = true

					methodPath := extractAttrStringParam(attr, src)

					// Build final routes by combining base paths with method path
					for _, basePath := range info.basePaths {
						route := buildRoute(verb, basePath, methodPath, strings.TrimSuffix(className, "Controller"), methodName)
						sig := strings.TrimSpace(route)
						sym := parser.Symbol{
							Name:          methodName,
							QualifiedName: qname + "." + methodName,
							Kind:          "endpoint",
							Language:      "csharp",
							StartLine:     int(member.StartPoint().Row) + 1,
							EndLine:       int(member.EndPoint().Row) + 1,
							Signature:     sig,
						}
						endpoints = append(endpoints, sym)
					}
				})
			}

			// DNN convention-based routing: public methods returning HttpResponseMessage
			// or IHttpActionResult without explicit routing attributes get convention routes.
			if !hasRoutingAttr && isDNN && isPublicMethod(member, src) {
				retType := extractReturnType(member, src)
				if dnnReturnTypes[retType] {
					controllerShort := strings.TrimSuffix(className, "Controller")
					route := "GET /api/" + strings.ToLower(controllerShort) + "/" + strings.ToLower(methodName)
					endpoints = append(endpoints, parser.Symbol{
						Name:          methodName,
						QualifiedName: qname + "." + methodName,
						Kind:          "endpoint",
						Language:      "csharp",
						StartLine:     int(member.StartPoint().Row) + 1,
						EndLine:       int(member.EndPoint().Row) + 1,
						Signature:     route,
					})
				}
			}
		}
	})

	return endpoints
}

// buildRoute combines an HTTP verb, a controller base path, and a method-level
// path into a single canonical route string like "GET /api/users/{id}".
func buildRoute(verb, basePath, methodPath, controllerName, methodName string) string {
	// Expand [controller] and [action] tokens
	base := strings.ReplaceAll(basePath, "[controller]", strings.ToLower(controllerName))
	base = strings.ReplaceAll(base, "[Controller]", strings.ToLower(controllerName))
	base = strings.ReplaceAll(base, "[action]", strings.ToLower(methodName))
	base = strings.ReplaceAll(base, "[Action]", strings.ToLower(methodName))
	
	methodPath = strings.ReplaceAll(methodPath, "[action]", strings.ToLower(methodName))
	methodPath = strings.ReplaceAll(methodPath, "[Action]", strings.ToLower(methodName))

	// Prefix "api/" if the base path starts with [controller] expansion
	if !strings.HasPrefix(base, "/") && !strings.HasPrefix(base, "api/") {
		base = "api/" + base
	}

	// Join base and method paths
	combined := "/" + strings.Trim(base, "/")
	if methodPath != "" {
		combined = combined + "/" + strings.Trim(methodPath, "/")
	}

	// Normalise route parameters: {id:int} → {id}
	combined = normalizeRouteParams(combined)

	if verb == "" {
		return combined
	}
	return verb + " " + combined
}

// normalizeRouteParams strips type constraints from ASP.NET route parameters.
// e.g. {id:int} → {id}, {name:alpha:minlength(3)} → {name}
func normalizeRouteParams(path string) string {
	var out strings.Builder
	i := 0
	for i < len(path) {
		if path[i] == '{' {
			out.WriteByte('{')
			i++
			// Read until } keeping only the param name (before any :)
			name := []byte{}
			for i < len(path) && path[i] != '}' {
				if path[i] == ':' {
					// skip constraint
					for i < len(path) && path[i] != '}' {
						i++
					}
					break
				}
				name = append(name, path[i])
				i++
			}
			out.Write(name)
			out.WriteByte('}')
			if i < len(path) {
				i++ // skip '}'
			}
		} else {
			out.WriteByte(path[i])
			i++
		}
	}
	return out.String()
}

// extractAttrName returns the name of an attribute node (e.g. "HttpGet" from [HttpGet("path")]).
func extractAttrName(attr *sitter.Node, src []byte) string {
	for i := 0; i < int(attr.ChildCount()); i++ {
		child := attr.Child(i)
		if child.Type() == "identifier" || child.Type() == "qualified_name" {
			return child.Content(src)
		}
	}
	return ""
}

// extractAttrStringParam extracts the first string literal parameter of an attribute.
// Handles both positional ([HttpGet("/path")]) and named (value="...") forms.
func extractAttrStringParam(attr *sitter.Node, src []byte) string {
	for i := 0; i < int(attr.ChildCount()); i++ {
		child := attr.Child(i)
		if child.Type() == "attribute_argument_list" {
			for j := 0; j < int(child.ChildCount()); j++ {
				arg := child.Child(j)
				if s := extractStringLiteral(arg, src); s != "" {
					return s
				}
				// named argument: value = "..."
				if arg.Type() == "attribute_argument" {
					for k := 0; k < int(arg.ChildCount()); k++ {
						if s := extractStringLiteral(arg.Child(k), src); s != "" {
							return s
						}
					}
				}
			}
		}
	}
	return ""
}

// isPublicMethod returns true if a method_declaration has the "public" modifier.
func isPublicMethod(node *sitter.Node, src []byte) bool {
	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		if child.Type() == "modifier" && child.Content(src) == "public" {
			return true
		}
	}
	return false
}

// hasAttributeOnNode returns true if the node has an attribute list containing an attribute with the given name.
func hasAttributeOnNode(node *sitter.Node, src []byte, attrName string) bool {
	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		if child.Type() == "attribute_list" {
			found := false
			walkTree(child, func(attr *sitter.Node) {
				if attr.Type() == "attribute" && extractAttrName(attr, src) == attrName {
					found = true
				}
			})
			if found {
				return true
			}
		}
	}
	return false
}
