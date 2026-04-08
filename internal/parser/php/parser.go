package php

import (
	"context"
	"regexp"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/php"

	"github.com/maraichr/lattice/internal/parser"
	"github.com/maraichr/lattice/internal/parser/sqlutil"
)

// Parser implements a tree-sitter based PHP parser.
type Parser struct {
	tsParser *sitter.Parser
}

func New() *Parser {
	p := sitter.NewParser()
	p.SetLanguage(php.GetLanguage())
	return &Parser{tsParser: p}
}

func (p *Parser) Languages() []string {
	return []string{"php"}
}

func (p *Parser) Parse(input parser.FileInput) (*parser.ParseResult, error) {
	tree, err := p.tsParser.ParseCtx(context.Background(), nil, input.Content)
	if err != nil {
		return nil, err
	}
	defer tree.Close()

	root := tree.RootNode()
	src := input.Content

	var symbols []parser.Symbol
	var refs []parser.RawReference

	namespace := ""

	// First pass: extract namespace and use statements, then declarations
	walkTopLevel(root, func(node *sitter.Node) {
		switch node.Type() {
		case "namespace_definition":
			ns := extractNamespaceName(node, src)
			if ns != "" {
				namespace = ns
			}
			// Walk namespace body for declarations
			body := findChild(node, "compound_statement")
			if body != nil {
				syms, rfs := extractDeclarations(body, src, namespace)
				symbols = append(symbols, syms...)
				refs = append(refs, rfs...)
			}

		case "namespace_use_declaration":
			rfs := extractUseStatement(node, src)
			refs = append(refs, rfs...)

		case "class_declaration":
			syms, rfs := extractClass(node, src, namespace)
			symbols = append(symbols, syms...)
			refs = append(refs, rfs...)

		case "trait_declaration":
			syms, rfs := extractTrait(node, src, namespace)
			symbols = append(symbols, syms...)
			refs = append(refs, rfs...)

		case "interface_declaration":
			sym, rfs := extractInterface(node, src, namespace)
			if sym != nil {
				symbols = append(symbols, *sym)
			}
			refs = append(refs, rfs...)

		case "function_definition":
			sym := extractFunction(node, src, namespace)
			if sym != nil {
				symbols = append(symbols, *sym)
			}

		case "enum_declaration":
			sym, rfs := extractEnum(node, src, namespace)
			if sym != nil {
				symbols = append(symbols, *sym)
			}
			refs = append(refs, rfs...)
		}
	})

	// Second pass: extract references from expressions (new, type hints, method calls)
	classRanges := buildClassRanges(symbols)
	exprRefs := extractExpressionRefs(root, src, classRanges)
	refs = append(refs, exprRefs...)

	// Third pass: extract DB refs and endpoint symbols
	dbRefs := extractDatabaseRefs(root, src, classRanges)
	refs = append(refs, dbRefs...)

	endpointSyms := extractEndpoints(root, src, namespace)
	symbols = append(symbols, endpointSyms...)

	return &parser.ParseResult{
		Symbols:    symbols,
		References: refs,
	}, nil
}

// --- Namespace and use ---

func extractNamespaceName(node *sitter.Node, src []byte) string {
	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		if child.Type() == "namespace_name" || child.Type() == "qualified_name" || child.Type() == "name" {
			return child.Content(src)
		}
	}
	return ""
}

func extractUseStatement(node *sitter.Node, src []byte) []parser.RawReference {
	var refs []parser.RawReference
	walkTree(node, func(n *sitter.Node) {
		if n.Type() == "namespace_use_clause" || n.Type() == "namespace_name" || n.Type() == "qualified_name" {
			name := n.Content(src)
			if name != "" && n.Parent() == node {
				refs = append(refs, parser.RawReference{
					ToName:        name,
					ReferenceType: "imports",
					Line:          int(n.StartPoint().Row) + 1,
				})
			}
		}
	})
	// If no clauses found, get the direct namespace name
	if len(refs) == 0 {
		for i := 0; i < int(node.ChildCount()); i++ {
			child := node.Child(i)
			if child.Type() == "namespace_use_clause" {
				name := ""
				walkTree(child, func(n *sitter.Node) {
					if name == "" && (n.Type() == "namespace_name" || n.Type() == "qualified_name" || n.Type() == "name") {
						name = n.Content(src)
					}
				})
				if name != "" {
					refs = append(refs, parser.RawReference{
						ToName:        name,
						ReferenceType: "imports",
						Line:          int(child.StartPoint().Row) + 1,
					})
				}
			}
		}
	}
	return refs
}

// --- Declarations ---

func extractDeclarations(body *sitter.Node, src []byte, ns string) ([]parser.Symbol, []parser.RawReference) {
	var symbols []parser.Symbol
	var refs []parser.RawReference

	for i := 0; i < int(body.ChildCount()); i++ {
		child := body.Child(i)
		switch child.Type() {
		case "class_declaration":
			syms, rfs := extractClass(child, src, ns)
			symbols = append(symbols, syms...)
			refs = append(refs, rfs...)
		case "trait_declaration":
			syms, rfs := extractTrait(child, src, ns)
			symbols = append(symbols, syms...)
			refs = append(refs, rfs...)
		case "interface_declaration":
			sym, rfs := extractInterface(child, src, ns)
			if sym != nil {
				symbols = append(symbols, *sym)
			}
			refs = append(refs, rfs...)
		case "function_definition":
			sym := extractFunction(child, src, ns)
			if sym != nil {
				symbols = append(symbols, *sym)
			}
		case "enum_declaration":
			sym, rfs := extractEnum(child, src, ns)
			if sym != nil {
				symbols = append(symbols, *sym)
			}
			refs = append(refs, rfs...)
		}
	}

	return symbols, refs
}

func extractClass(node *sitter.Node, src []byte, ns string) ([]parser.Symbol, []parser.RawReference) {
	var symbols []parser.Symbol
	var refs []parser.RawReference

	name := findChildContent(node, "name", src)
	if name == "" {
		return nil, nil
	}

	qname := qualify(ns, name)
	symbols = append(symbols, parser.Symbol{
		Name:          name,
		QualifiedName: qname,
		Kind:          "class",
		Language:      "php",
		StartLine:     int(node.StartPoint().Row) + 1,
		EndLine:       int(node.EndPoint().Row) + 1,
	})

	// Heritage: extends, implements
	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		switch child.Type() {
		case "base_clause":
			baseName := extractFirstTypeName(child, src)
			if baseName != "" {
				refs = append(refs, parser.RawReference{
					FromSymbol:    qname,
					ToName:        baseName,
					ReferenceType: "inherits",
					Line:          int(child.StartPoint().Row) + 1,
				})
			}
		case "class_interface_clause":
			for j := 0; j < int(child.ChildCount()); j++ {
				gc := child.Child(j)
				if gc.Type() == "name" || gc.Type() == "qualified_name" || gc.Type() == "namespace_name" {
					refs = append(refs, parser.RawReference{
						FromSymbol:    qname,
						ToName:        gc.Content(src),
						ReferenceType: "implements",
						Line:          int(gc.StartPoint().Row) + 1,
					})
				}
			}
		}
	}

	// Class body members
	body := findChild(node, "declaration_list")
	if body != nil {
		memberSyms, memberRefs := extractClassMembers(body, src, name, qname)
		symbols = append(symbols, memberSyms...)
		refs = append(refs, memberRefs...)
	}

	return symbols, refs
}

func extractTrait(node *sitter.Node, src []byte, ns string) ([]parser.Symbol, []parser.RawReference) {
	var symbols []parser.Symbol
	var refs []parser.RawReference

	name := findChildContent(node, "name", src)
	if name == "" {
		return nil, nil
	}

	qname := qualify(ns, name)
	symbols = append(symbols, parser.Symbol{
		Name:          name,
		QualifiedName: qname,
		Kind:          "class", // Traits are stored as class kind
		Language:      "php",
		StartLine:     int(node.StartPoint().Row) + 1,
		EndLine:       int(node.EndPoint().Row) + 1,
	})

	body := findChild(node, "declaration_list")
	if body != nil {
		memberSyms, memberRefs := extractClassMembers(body, src, name, qname)
		symbols = append(symbols, memberSyms...)
		refs = append(refs, memberRefs...)
	}

	return symbols, refs
}

func extractInterface(node *sitter.Node, src []byte, ns string) (*parser.Symbol, []parser.RawReference) {
	name := findChildContent(node, "name", src)
	if name == "" {
		return nil, nil
	}

	qname := qualify(ns, name)
	sym := &parser.Symbol{
		Name:          name,
		QualifiedName: qname,
		Kind:          "interface",
		Language:      "php",
		StartLine:     int(node.StartPoint().Row) + 1,
		EndLine:       int(node.EndPoint().Row) + 1,
	}

	var refs []parser.RawReference
	// extends clause for interfaces
	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		if child.Type() == "base_clause" {
			baseName := extractFirstTypeName(child, src)
			if baseName != "" {
				refs = append(refs, parser.RawReference{
					FromSymbol:    qname,
					ToName:        baseName,
					ReferenceType: "inherits",
					Line:          int(child.StartPoint().Row) + 1,
				})
			}
		}
	}

	return sym, refs
}

func extractFunction(node *sitter.Node, src []byte, ns string) *parser.Symbol {
	name := findChildContent(node, "name", src)
	if name == "" {
		return nil
	}

	sig := ""
	params := findChild(node, "formal_parameters")
	if params != nil {
		sig = params.Content(src)
	}

	return &parser.Symbol{
		Name:          name,
		QualifiedName: qualify(ns, name),
		Kind:          "function",
		Language:      "php",
		StartLine:     int(node.StartPoint().Row) + 1,
		EndLine:       int(node.EndPoint().Row) + 1,
		Signature:     sig,
	}
}

func extractEnum(node *sitter.Node, src []byte, ns string) (*parser.Symbol, []parser.RawReference) {
	name := findChildContent(node, "name", src)
	if name == "" {
		return nil, nil
	}

	qname := qualify(ns, name)
	sym := &parser.Symbol{
		Name:          name,
		QualifiedName: qname,
		Kind:          "enum",
		Language:      "php",
		StartLine:     int(node.StartPoint().Row) + 1,
		EndLine:       int(node.EndPoint().Row) + 1,
	}

	var refs []parser.RawReference
	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		if child.Type() == "class_interface_clause" {
			for j := 0; j < int(child.ChildCount()); j++ {
				gc := child.Child(j)
				if gc.Type() == "name" || gc.Type() == "qualified_name" {
					refs = append(refs, parser.RawReference{
						FromSymbol:    qname,
						ToName:        gc.Content(src),
						ReferenceType: "implements",
						Line:          int(gc.StartPoint().Row) + 1,
					})
				}
			}
		}
	}

	return sym, refs
}

// --- Class members ---

func extractClassMembers(body *sitter.Node, src []byte, className, classQName string) ([]parser.Symbol, []parser.RawReference) {
	var symbols []parser.Symbol
	var refs []parser.RawReference

	for i := 0; i < int(body.ChildCount()); i++ {
		child := body.Child(i)
		switch child.Type() {
		case "method_declaration":
			sym := extractMethod(child, src, className)
			if sym != nil {
				symbols = append(symbols, *sym)
			}

		case "property_declaration":
			name := extractPropertyName(child, src)
			if name != "" {
				symbols = append(symbols, parser.Symbol{
					Name:          name,
					QualifiedName: classQName + "." + name,
					Kind:          "property",
					Language:      "php",
					StartLine:     int(child.StartPoint().Row) + 1,
					EndLine:       int(child.EndPoint().Row) + 1,
				})
			}

		case "const_declaration":
			names := extractConstNames(child, src)
			for _, name := range names {
				symbols = append(symbols, parser.Symbol{
					Name:          name,
					QualifiedName: classQName + "." + name,
					Kind:          "constant",
					Language:      "php",
					StartLine:     int(child.StartPoint().Row) + 1,
					EndLine:       int(child.EndPoint().Row) + 1,
				})
			}

		case "use_declaration":
			// Trait use: use HasRoles, Notifiable;
			walkTree(child, func(n *sitter.Node) {
				if n.Type() == "name" || n.Type() == "qualified_name" {
					traitName := n.Content(src)
					if traitName != "" && traitName != "use" {
						refs = append(refs, parser.RawReference{
							FromSymbol:    classQName,
							ToName:        traitName,
							ReferenceType: "imports",
							Line:          int(n.StartPoint().Row) + 1,
						})
					}
				}
			})
		}
	}

	return symbols, refs
}

func extractMethod(node *sitter.Node, src []byte, className string) *parser.Symbol {
	name := findChildContent(node, "name", src)
	if name == "" {
		return nil
	}

	sig := ""
	params := findChild(node, "formal_parameters")
	if params != nil {
		sig = params.Content(src)
	}

	return &parser.Symbol{
		Name:          name,
		QualifiedName: className + "." + name,
		Kind:          "method",
		Language:      "php",
		StartLine:     int(node.StartPoint().Row) + 1,
		EndLine:       int(node.EndPoint().Row) + 1,
		Signature:     sig,
	}
}

func extractPropertyName(node *sitter.Node, src []byte) string {
	var name string
	walkTree(node, func(n *sitter.Node) {
		if name == "" && n.Type() == "property_element" {
			varNode := findChild(n, "variable_name")
			if varNode != nil {
				name = strings.TrimPrefix(varNode.Content(src), "$")
			}
		}
	})
	return name
}

func extractConstNames(node *sitter.Node, src []byte) []string {
	var names []string
	walkTree(node, func(n *sitter.Node) {
		if n.Type() == "const_element" {
			nameNode := findChild(n, "name")
			if nameNode != nil {
				names = append(names, nameNode.Content(src))
			}
		}
	})
	return names
}

// --- Expression references ---

type classRange struct {
	qname     string
	startLine int
	endLine   int
}

func buildClassRanges(symbols []parser.Symbol) []classRange {
	var ranges []classRange
	for _, s := range symbols {
		if s.Kind == "class" || s.Kind == "function" || s.Kind == "method" {
			ranges = append(ranges, classRange{s.QualifiedName, s.StartLine, s.EndLine})
		}
	}
	return ranges
}

func findEnclosing(line int, ranges []classRange) string {
	best := ""
	bestSpan := 1<<31 - 1
	for _, r := range ranges {
		if line >= r.startLine && line <= r.endLine {
			span := r.endLine - r.startLine
			if span < bestSpan {
				bestSpan = span
				best = r.qname
			}
		}
	}
	return best
}

func extractExpressionRefs(root *sitter.Node, src []byte, ranges []classRange) []parser.RawReference {
	var refs []parser.RawReference

	walkTree(root, func(node *sitter.Node) {
		line := int(node.StartPoint().Row) + 1

		switch node.Type() {
		case "object_creation_expression":
			// new UserService()
			nameNode := findChild(node, "name")
			if nameNode == nil {
				nameNode = findChild(node, "qualified_name")
			}
			if nameNode != nil {
				refs = append(refs, parser.RawReference{
					FromSymbol:    findEnclosing(line, ranges),
					ToName:        nameNode.Content(src),
					ReferenceType: "references",
					Line:          line,
				})
			}

		case "simple_parameter":
			// Type-hinted parameters: function(User $user)
			typeNode := findTypeHint(node, src)
			if typeNode != "" && !isPHPBuiltin(typeNode) {
				refs = append(refs, parser.RawReference{
					FromSymbol:    findEnclosing(line, ranges),
					ToName:        typeNode,
					ReferenceType: "references",
					Line:          line,
				})
			}
		}
	})

	return refs
}

func findTypeHint(node *sitter.Node, src []byte) string {
	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		switch child.Type() {
		case "name", "qualified_name":
			return child.Content(src)
		case "named_type", "type_name":
			return child.Content(src)
		case "union_type", "intersection_type", "nullable_type":
			// Get the first concrete type
			for j := 0; j < int(child.ChildCount()); j++ {
				gc := child.Child(j)
				if gc.Type() == "name" || gc.Type() == "qualified_name" || gc.Type() == "named_type" {
					return gc.Content(src)
				}
			}
		}
	}
	return ""
}

// --- Database references ---

// Patterns for PHP DB access
var (
	rePDOQuery    = regexp.MustCompile(`(?i)\$(?:pdo|db|conn|connection)\s*->\s*(?:query|prepare|exec)\s*\(`)
	reMySQLiQuery = regexp.MustCompile(`(?i)(?:\$mysqli\s*->\s*query|mysqli_query)\s*\(`)
	reWPDBQuery   = regexp.MustCompile(`(?i)\$wpdb\s*->\s*(?:query|get_results|get_var|get_row|get_col|prepare)\s*\(`)
	reLaravelDB   = regexp.MustCompile(`(?i)DB\s*::\s*(?:select|insert|update|delete|statement|unprepared)\s*\(`)
	reLaravelTbl  = regexp.MustCompile(`(?i)(?:DB\s*::\s*table|->table)\s*\(\s*['"](\w+)['"]`)
	reWPDBPrefix  = regexp.MustCompile(`(?i)\$wpdb\s*->\s*prefix\s*\.\s*['"](\w+)['"]`)
)

func extractDatabaseRefs(root *sitter.Node, src []byte, ranges []classRange) []parser.RawReference {
	var refs []parser.RawReference

	walkTree(root, func(node *sitter.Node) {
		if node.Type() != "member_call_expression" && node.Type() != "scoped_call_expression" && node.Type() != "function_call_expression" {
			return
		}

		line := int(node.StartPoint().Row) + 1
		from := findEnclosing(line, ranges)
		text := node.Content(src)

		// PDO/mysqli/wpdb query with SQL string
		if rePDOQuery.MatchString(text) || reMySQLiQuery.MatchString(text) || reWPDBQuery.MatchString(text) || reLaravelDB.MatchString(text) {
			// Extract SQL string argument
			sqlStr := extractFirstStringArg(node, src)
			if sqlStr != "" && sqlutil.LooksLikeSQL(sqlStr) {
				tableRefs := sqlutil.ExtractTableRefs(sqlStr, line, from, "")
				for i := range tableRefs {
					tableRefs[i].Confidence = 0.9
				}
				refs = append(refs, tableRefs...)
			}
		}

		// DB::table('users') / ->table('orders')
		if matches := reLaravelTbl.FindStringSubmatch(text); len(matches) > 1 {
			refs = append(refs, parser.RawReference{
				FromSymbol:    from,
				ToName:        matches[1],
				ReferenceType: "uses_table",
				Confidence:    0.9,
				Line:          line,
			})
		}

		// $wpdb->prefix . 'users'
		if matches := reWPDBPrefix.FindStringSubmatch(text); len(matches) > 1 {
			refs = append(refs, parser.RawReference{
				FromSymbol:    from,
				ToName:        matches[1],
				ReferenceType: "uses_table",
				Confidence:    0.85,
				Line:          line,
			})
		}
	})

	return refs
}

func extractFirstStringArg(node *sitter.Node, src []byte) string {
	args := findChild(node, "arguments")
	if args == nil {
		return ""
	}
	for i := 0; i < int(args.ChildCount()); i++ {
		child := args.Child(i)
		if child.Type() == "argument" {
			for j := 0; j < int(child.ChildCount()); j++ {
				gc := child.Child(j)
				if gc.Type() == "string" || gc.Type() == "encapsed_string" {
					return extractStringContent(gc, src)
				}
			}
		}
		if child.Type() == "string" || child.Type() == "encapsed_string" {
			return extractStringContent(child, src)
		}
	}
	return ""
}

// --- Endpoint extraction ---

var (
	reLaravelRoute = regexp.MustCompile(`(?i)Route\s*::\s*(get|post|put|patch|delete|options|any|resource|apiResource)\s*\(\s*['"]([^'"]+)['"]`)
	reWPRestRoute  = regexp.MustCompile(`(?i)register_rest_route\s*\(\s*['"]([^'"]+)['"]\s*,\s*['"]([^'"]+)['"]`)
	reWPAjax       = regexp.MustCompile(`(?i)add_action\s*\(\s*['"](wp_ajax(?:_nopriv)?_\w+)['"]`)
)

func extractEndpoints(root *sitter.Node, src []byte, ns string) []parser.Symbol {
	var symbols []parser.Symbol
	content := string(src)

	// Laravel routes: Route::get('/api/users', ...)
	for _, m := range reLaravelRoute.FindAllStringSubmatch(content, -1) {
		verb := strings.ToUpper(m[1])
		path := m[2]
		sig := verb + " " + path
		line := countLinesBefore(content, strings.Index(content, m[0])) + 1
		symbols = append(symbols, parser.Symbol{
			Name:          sig,
			QualifiedName: sig,
			Kind:          "endpoint",
			Language:      "php",
			StartLine:     line,
			EndLine:       line,
			Signature:     sig,
		})
	}

	// WordPress REST routes: register_rest_route('wp/v2', '/posts', ...)
	for _, m := range reWPRestRoute.FindAllStringSubmatch(content, -1) {
		prefix := strings.TrimRight(m[1], "/")
		path := m[2]
		if !strings.HasPrefix(path, "/") {
			path = "/" + path
		}
		sig := "GET /" + prefix + path
		line := countLinesBefore(content, strings.Index(content, m[0])) + 1
		symbols = append(symbols, parser.Symbol{
			Name:          sig,
			QualifiedName: sig,
			Kind:          "endpoint",
			Language:      "php",
			StartLine:     line,
			EndLine:       line,
			Signature:     sig,
		})
	}

	// WordPress AJAX: add_action('wp_ajax_my_action', ...)
	for _, m := range reWPAjax.FindAllStringSubmatch(content, -1) {
		action := m[1]
		line := countLinesBefore(content, strings.Index(content, m[0])) + 1
		symbols = append(symbols, parser.Symbol{
			Name:          action,
			QualifiedName: action,
			Kind:          "endpoint",
			Language:      "php",
			StartLine:     line,
			EndLine:       line,
			Signature:     action,
		})
	}

	return symbols
}

// --- Helpers ---

func qualify(ns, name string) string {
	if ns != "" {
		return ns + `\` + name
	}
	return name
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

func findChildContent(node *sitter.Node, nodeType string, src []byte) string {
	child := findChild(node, nodeType)
	if child != nil {
		return child.Content(src)
	}
	return ""
}

func walkTree(node *sitter.Node, fn func(*sitter.Node)) {
	fn(node)
	for i := 0; i < int(node.ChildCount()); i++ {
		walkTree(node.Child(i), fn)
	}
}

func walkTopLevel(node *sitter.Node, fn func(*sitter.Node)) {
	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		fn(child)
		// Also check inside program node (PHP tree-sitter wraps in program)
		if child.Type() == "program" {
			walkTopLevel(child, fn)
		}
	}
}

func extractFirstTypeName(node *sitter.Node, src []byte) string {
	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		if child.Type() == "name" || child.Type() == "qualified_name" || child.Type() == "namespace_name" {
			return child.Content(src)
		}
	}
	return ""
}

func extractStringContent(node *sitter.Node, src []byte) string {
	text := node.Content(src)
	if len(text) >= 2 {
		return strings.Trim(text, `"'`)
	}
	return ""
}

func countLinesBefore(text string, pos int) int {
	if pos < 0 {
		return 0
	}
	return strings.Count(text[:pos], "\n")
}

func isPHPBuiltin(name string) bool {
	builtins := map[string]bool{
		"int": true, "float": true, "string": true, "bool": true,
		"array": true, "object": true, "null": true, "void": true,
		"mixed": true, "callable": true, "iterable": true, "self": true,
		"static": true, "parent": true, "never": true, "false": true,
		"true": true,
	}
	return builtins[strings.ToLower(name)]
}
