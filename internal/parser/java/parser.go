package java

import (
	"context"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/java"

	"github.com/maraichr/lattice/internal/parser"
	"github.com/maraichr/lattice/internal/parser/sqlutil"
)

// Parser implements a tree-sitter based Java parser.
type Parser struct {
	tsParser *sitter.Parser
}

func New() *Parser {
	p := sitter.NewParser()
	p.SetLanguage(java.GetLanguage())
	return &Parser{tsParser: p}
}

func (p *Parser) Languages() []string {
	return []string{"java"}
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

	packageName := ""

	// Walk tree to extract symbols
	for i := 0; i < int(root.ChildCount()); i++ {
		child := root.Child(i)
		switch child.Type() {
		case "package_declaration":
			packageName = extractPackageName(child, input.Content)

		case "import_declaration":
			importPath := extractImportPath(child, input.Content)
			if importPath != "" {
				refs = append(refs, parser.RawReference{
					ToName:        importPath,
					ToQualified:   importPath,
					ReferenceType: "imports",
					Line:          int(child.StartPoint().Row) + 1,
				})
			}

		case "class_declaration":
			syms, rfs := extractClass(child, input.Content, packageName)
			symbols = append(symbols, syms...)
			refs = append(refs, rfs...)

		case "interface_declaration":
			syms, rfs := extractInterface(child, input.Content, packageName)
			symbols = append(symbols, syms...)
			refs = append(refs, rfs...)

		case "enum_declaration":
			syms := extractEnum(child, input.Content, packageName)
			symbols = append(symbols, syms...)
		}
	}

	// Process annotations for Spring/JPA detection
	annoRefs := extractAnnotationRefs(root, input.Content, packageName)
	refs = append(refs, annoRefs...)

	// JDBC PreparedStatement/prepareCall detection
	jdbcRefs := extractJDBCRefs(root, input.Content, symbols)
	refs = append(refs, jdbcRefs...)

	// General method call references
	callRefs := extractMethodCalls(root, input.Content, symbols)
	refs = append(refs, callRefs...)

	// @NamedQuery / @NamedNativeQuery detection
	namedQueryRefs := extractNamedQueryRefs(root, input.Content, packageName)
	refs = append(refs, namedQueryRefs...)

	// Spring MVC/WebFlux endpoint extraction (@RestController + @GetMapping etc.)
	endpointSyms := extractSpringEndpoints(root, input.Content, packageName)
	symbols = append(symbols, endpointSyms...)

	return &parser.ParseResult{
		Symbols:    symbols,
		References: refs,
	}, nil
}

func extractPackageName(node *sitter.Node, src []byte) string {
	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		if child.Type() == "scoped_identifier" || child.Type() == "identifier" {
			return child.Content(src)
		}
	}
	return ""
}

func extractImportPath(node *sitter.Node, src []byte) string {
	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		if child.Type() == "scoped_identifier" || child.Type() == "identifier" {
			return child.Content(src)
		}
	}
	return ""
}

func extractClass(node *sitter.Node, src []byte, pkg string) ([]parser.Symbol, []parser.RawReference) {
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

	qname := qualifyJava(pkg, name)
	classSym := parser.Symbol{
		Name:          name,
		QualifiedName: qname,
		Kind:          "class",
		Language:      "java",
		StartLine:     int(node.StartPoint().Row) + 1,
		EndLine:       int(node.EndPoint().Row) + 1,
	}

	// Check for superclass/interfaces
	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		if child.Type() == "superclass" {
			parent := extractTypeIdent(child, src)
			if parent != "" {
				refs = append(refs, parser.RawReference{
					FromSymbol:    qname,
					ToName:        parent,
					ReferenceType: "inherits",
					Line:          int(child.StartPoint().Row) + 1,
				})
			}
		}
		if child.Type() == "super_interfaces" {
			ifaces := extractTypeList(child, src)
			for _, iface := range ifaces {
				refs = append(refs, parser.RawReference{
					FromSymbol:    qname,
					ToName:        iface,
					ReferenceType: "implements",
					Line:          int(child.StartPoint().Row) + 1,
				})
			}
		}
	}

	symbols = append(symbols, classSym)

	// Extract members from class body
	body := findChild(node, "class_body")
	if body != nil {
		memberSyms, memberRefs := extractMembers(body, src, pkg, name)
		symbols = append(symbols, memberSyms...)
		refs = append(refs, memberRefs...)
	}

	return symbols, refs
}

func extractInterface(node *sitter.Node, src []byte, pkg string) ([]parser.Symbol, []parser.RawReference) {
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

	qname := qualifyJava(pkg, name)
	symbols = append(symbols, parser.Symbol{
		Name:          name,
		QualifiedName: qname,
		Kind:          "interface",
		Language:      "java",
		StartLine:     int(node.StartPoint().Row) + 1,
		EndLine:       int(node.EndPoint().Row) + 1,
	})

	// Detect Spring Data repository interfaces
	entityType := extractSpringDataEntity(node, src)
	if entityType != "" {
		refs = append(refs, parser.RawReference{
			FromSymbol:    qname,
			ToName:        entityType,
			ReferenceType: "uses_table",
			Confidence:    0.7,
			Line:          int(node.StartPoint().Row) + 1,
		})

		// Extract derived query methods (findBy*, countBy*, deleteBy*)
		body := findChild(node, "interface_body")
		if body != nil {
			derivedRefs := extractDerivedQueryMethods(body, src, qname, entityType)
			refs = append(refs, derivedRefs...)
		}
	}

	return symbols, refs
}

func extractEnum(node *sitter.Node, src []byte, pkg string) []parser.Symbol {
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
		QualifiedName: qualifyJava(pkg, name),
		Kind:          "enum",
		Language:      "java",
		StartLine:     int(node.StartPoint().Row) + 1,
		EndLine:       int(node.EndPoint().Row) + 1,
	}}
}

func extractMembers(body *sitter.Node, src []byte, pkg, className string) ([]parser.Symbol, []parser.RawReference) {
	var symbols []parser.Symbol
	var refs []parser.RawReference

	for i := 0; i < int(body.ChildCount()); i++ {
		child := body.Child(i)
		switch child.Type() {
		case "method_declaration":
			name, sig := extractMethodDecl(child, src)
			if name != "" {
				qname := qualifyJava(pkg, className+"."+name)
				symbols = append(symbols, parser.Symbol{
					Name:          name,
					QualifiedName: qname,
					Kind:          "method",
					Language:      "java",
					StartLine:     int(child.StartPoint().Row) + 1,
					EndLine:       int(child.EndPoint().Row) + 1,
					Signature:     sig,
				})
			}

		case "constructor_declaration":
			name := className
			qname := qualifyJava(pkg, className+"."+name)
			symbols = append(symbols, parser.Symbol{
				Name:          name,
				QualifiedName: qname,
				Kind:          "method",
				Language:      "java",
				StartLine:     int(child.StartPoint().Row) + 1,
				EndLine:       int(child.EndPoint().Row) + 1,
			})

		case "field_declaration":
			fieldName := extractFieldName(child, src)
			if fieldName != "" {
				symbols = append(symbols, parser.Symbol{
					Name:          fieldName,
					QualifiedName: qualifyJava(pkg, className+"."+fieldName),
					Kind:          "field",
					Language:      "java",
					StartLine:     int(child.StartPoint().Row) + 1,
					EndLine:       int(child.EndPoint().Row) + 1,
				})
			}

		case "class_declaration":
			nestedPkg := qualifyJava(pkg, className)
			nestedSyms, nestedRefs := extractClass(child, src, nestedPkg)
			symbols = append(symbols, nestedSyms...)
			refs = append(refs, nestedRefs...)

		case "interface_declaration":
			nestedPkg := qualifyJava(pkg, className)
			nestedSyms, nestedRefs := extractInterface(child, src, nestedPkg)
			symbols = append(symbols, nestedSyms...)
			refs = append(refs, nestedRefs...)

		case "enum_declaration":
			nestedPkg := qualifyJava(pkg, className)
			nestedSyms := extractEnum(child, src, nestedPkg)
			symbols = append(symbols, nestedSyms...)
		}
	}

	return symbols, refs
}

func extractMethodDecl(node *sitter.Node, src []byte) (string, string) {
	name := ""
	sig := ""
	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		if child.Type() == "identifier" && name == "" {
			name = child.Content(src)
		}
		if child.Type() == "formal_parameters" {
			sig = child.Content(src)
		}
	}
	return name, sig
}

func extractFieldName(node *sitter.Node, src []byte) string {
	decl := findChild(node, "variable_declarator")
	if decl != nil {
		for i := 0; i < int(decl.ChildCount()); i++ {
			child := decl.Child(i)
			if child.Type() == "identifier" {
				return child.Content(src)
			}
		}
	}
	return ""
}

func extractTypeIdent(node *sitter.Node, src []byte) string {
	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		if child.Type() == "type_identifier" || child.Type() == "identifier" {
			return child.Content(src)
		}
	}
	return ""
}

func extractTypeList(node *sitter.Node, src []byte) []string {
	var types []string
	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		if child.Type() == "type_list" {
			for j := 0; j < int(child.ChildCount()); j++ {
				grandchild := child.Child(j)
				if grandchild.Type() == "type_identifier" || grandchild.Type() == "generic_type" {
					types = append(types, grandchild.Content(src))
				}
			}
		}
	}
	return types
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

func qualifyJava(pkg, name string) string {
	if pkg != "" {
		return pkg + "." + name
	}
	return name
}

func qualifyAnnotated(pkg, className, refName string) string {
	if className != "" {
		return qualifyJava(pkg, className)
	}
	return refName
}

// extractAnnotationRefs walks the tree looking for Spring/JPA annotations.
func extractAnnotationRefs(root *sitter.Node, src []byte, pkg string) []parser.RawReference {
	var refs []parser.RawReference
	walkTree(root, func(node *sitter.Node) {
		if node.Type() != "marker_annotation" && node.Type() != "annotation" {
			return
		}

		annoText := node.Content(src)
		line := int(node.StartPoint().Row) + 1

		// Find the annotated class/method name
		parent := node.Parent()
		className := ""
		if parent != nil {
			for i := 0; i < int(parent.ChildCount()); i++ {
				child := parent.Child(i)
				if child.Type() == "identifier" {
					className = child.Content(src)
					break
				}
			}
		}

		// @Entity or @Table(name="...")
		if strings.Contains(annoText, "@Entity") || strings.Contains(annoText, "@Table") {
			tableName := extractAnnotationParam(annoText, "name")
			if tableName == "" {
				tableName = className
			}
			if tableName != "" {
				refs = append(refs, parser.RawReference{
					FromSymbol:    qualifyAnnotated(pkg, className, ""),
					ToName:        tableName,
					ReferenceType: "uses_table",
					Line:          line,
				})
			}
		}

		// @Query("SELECT ...")
		if strings.Contains(annoText, "@Query") {
			query := extractAnnotationStringParam(annoText)
			if query != "" && looksLikeSQL(query) {
				tableRefs := extractSQLTableRefs(query, line)
				refs = append(refs, tableRefs...)
			}
		}

		// @RequestMapping, @GetMapping, etc.
		if strings.Contains(annoText, "Mapping") {
			path := extractAnnotationStringParam(annoText)
			if path != "" {
				refs = append(refs, parser.RawReference{
					FromSymbol:    qualifyAnnotated(pkg, className, ""),
					ToName:        path,
					ReferenceType: "references",
					Line:          line,
				})
			}
		}
	})

	return refs
}

func walkTree(node *sitter.Node, fn func(*sitter.Node)) {
	fn(node)
	for i := 0; i < int(node.ChildCount()); i++ {
		walkTree(node.Child(i), fn)
	}
}

func extractAnnotationParam(text, param string) string {
	// Look for param = "value" or param = 'value'
	_, rest, found := strings.Cut(text, param)
	if !found {
		return ""
	}
	rest = strings.TrimSpace(rest)
	if len(rest) > 0 && rest[0] == '=' {
		rest = strings.TrimSpace(rest[1:])
		if len(rest) > 0 && (rest[0] == '"' || rest[0] == '\'') {
			end := strings.IndexByte(rest[1:], rest[0])
			if end >= 0 {
				return rest[1 : end+1]
			}
		}
	}
	return ""
}

func extractAnnotationStringParam(text string) string {
	// Extract first string literal from annotation
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

func looksLikeSQL(s string) bool {
	upper := strings.ToUpper(strings.TrimSpace(s))
	for _, kw := range []string{"SELECT", "INSERT", "UPDATE", "DELETE", "FROM"} {
		if strings.Contains(upper, kw) {
			return true
		}
	}
	return false
}

func extractSQLTableRefs(sql string, line int) []parser.RawReference {
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
			// Extract table name (first word)
			end := strings.IndexAny(rest, " \t\n,;)")
			tableName := rest
			if end > 0 {
				tableName = rest[:end]
			}
			tableName = strings.TrimSpace(tableName)
			if tableName != "" && !isSQLKeyword(tableName) {
				refs = append(refs, parser.RawReference{
					ToName:        tableName,
					ReferenceType: "uses_table",
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

// extractJDBCRefs detects JDBC PreparedStatement and prepareCall patterns.
func extractJDBCRefs(root *sitter.Node, src []byte, symbols []parser.Symbol) []parser.RawReference {
	var refs []parser.RawReference

	walkTree(root, func(node *sitter.Node) {
		if node.Type() != "method_invocation" {
			return
		}

		line := int(node.StartPoint().Row) + 1

		// Get method name
		methodName := ""
		for i := 0; i < int(node.ChildCount()); i++ {
			child := node.Child(i)
			if child.Type() == "identifier" {
				methodName = child.Content(src)
			}
		}

		switch methodName {
		case "prepareStatement", "prepareCall":
			args := findChild(node, "argument_list")
			if args == nil {
				return
			}
			sqlStr := extractFirstStringLiteral(args, src)
			if sqlStr == "" {
				return
			}
			from := findEnclosingSymbol(line, symbols)
			if sqlutil.LooksLikeSQL(sqlStr) {
				tableRefs := sqlutil.ExtractTableRefs(sqlStr, line, from, "")
				for i := range tableRefs {
					tableRefs[i].Confidence = 0.9
				}
				refs = append(refs, tableRefs...)
			}
		}
	})

	return refs
}

// extractNamedQueryRefs detects @NamedQuery and @NamedNativeQuery annotations.
func extractNamedQueryRefs(root *sitter.Node, src []byte, pkg string) []parser.RawReference {
	var refs []parser.RawReference

	walkTree(root, func(node *sitter.Node) {
		if node.Type() != "annotation" {
			return
		}

		annoText := node.Content(src)
		if !strings.Contains(annoText, "NamedQuery") && !strings.Contains(annoText, "NamedNativeQuery") {
			return
		}

		line := int(node.StartPoint().Row) + 1
		query := extractAnnotationParam(annoText, "query")
		if query != "" && looksLikeSQL(query) {
			tableRefs := extractSQLTableRefs(query, line)
			for i := range tableRefs {
				tableRefs[i].Confidence = 0.9
			}
			refs = append(refs, tableRefs...)
		}
	})

	return refs
}

// extractSpringDataEntity detects if an interface extends JpaRepository<T, ID>
// or CrudRepository<T, ID> and returns the entity type name T.
func extractSpringDataEntity(node *sitter.Node, src []byte) string {
	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		if child.Type() == "extends_interfaces" {
			for j := 0; j < int(child.ChildCount()); j++ {
				typeNode := child.Child(j)
				text := typeNode.Content(src)
				// Check for JpaRepository<T, ...> or CrudRepository<T, ...>
				for _, repo := range []string{"JpaRepository", "CrudRepository", "PagingAndSortingRepository", "ReactiveCrudRepository"} {
					if strings.HasPrefix(text, repo+"<") {
						// Extract T from Repository<T, ID>
						start := strings.IndexByte(text, '<')
						end := strings.IndexAny(text[start+1:], ",>")
						if start >= 0 && end >= 0 {
							return strings.TrimSpace(text[start+1 : start+1+end])
						}
					}
				}
			}
		}
	}
	return ""
}

// extractDerivedQueryMethods parses Spring Data derived query method names.
func extractDerivedQueryMethods(body *sitter.Node, src []byte, fromSymbol, entityType string) []parser.RawReference {
	var refs []parser.RawReference

	for i := 0; i < int(body.ChildCount()); i++ {
		child := body.Child(i)
		if child.Type() != "method_declaration" {
			continue
		}
		name, _ := extractMethodDecl(child, src)
		if name == "" {
			continue
		}
		// Detect derived queries: findBy*, countBy*, deleteBy*, existsBy*
		for _, prefix := range []string{"findBy", "countBy", "deleteBy", "existsBy", "readBy", "getBy", "queryBy"} {
			if strings.HasPrefix(name, prefix) {
				refs = append(refs, parser.RawReference{
					FromSymbol:    fromSymbol + "." + name,
					ToName:        entityType,
					ReferenceType: "uses_table",
					Confidence:    0.7,
					Line:          int(child.StartPoint().Row) + 1,
				})
				break
			}
		}
	}

	return refs
}

// extractFirstStringLiteral returns the first string literal from an argument list.
func extractFirstStringLiteral(args *sitter.Node, src []byte) string {
	for i := 0; i < int(args.ChildCount()); i++ {
		child := args.Child(i)
		if child.Type() == "string_literal" {
			text := child.Content(src)
			if len(text) >= 2 {
				return text[1 : len(text)-1]
			}
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// Spring MVC/WebFlux endpoint extraction
// ---------------------------------------------------------------------------

// springVerbMappings maps Spring annotation names to HTTP verbs.
// An empty verb string means the annotation carries explicit method= attribute
// (i.e. @RequestMapping), so we use GET as the default.
var springVerbMappings = map[string]string{
	"GetMapping":      "GET",
	"PostMapping":     "POST",
	"PutMapping":      "PUT",
	"PatchMapping":    "PATCH",
	"DeleteMapping":   "DELETE",
	"RequestMapping":  "", // verb comes from method= attribute or defaults to GET
}

// extractSpringEndpoints walks the tree and emits endpoint symbols for every
// Spring controller method decorated with a mapping annotation.
//
// Strategy:
//  1. Find classes annotated with @RestController, @Controller, or @RequestMapping.
//  2. Collect the class-level base path from @RequestMapping.
//  3. For each method in the class body, look for HTTP mapping annotations.
//  4. Emit a Symbol with Kind "endpoint" and Signature "GET /api/orders/{id}".
func extractSpringEndpoints(root *sitter.Node, src []byte, pkg string) []parser.Symbol {
	var endpoints []parser.Symbol

	walkTree(root, func(node *sitter.Node) {
		if node.Type() != "class_declaration" {
			return
		}

		// Check whether this class is a Spring controller
		if !isSpringController(node, src) {
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

		qname := qualifyJava(pkg, className)

		// Collect class-level base paths from @RequestMapping in modifiers
		var basePaths []string
		classModifiers := findChild(node, "modifiers")
		if classModifiers != nil {
			for i := 0; i < int(classModifiers.ChildCount()); i++ {
				attr := classModifiers.Child(i)
				if attr.Type() != "marker_annotation" && attr.Type() != "annotation" {
					continue
				}
				annoText := attr.Content(src)
				if !strings.Contains(annoText, "RequestMapping") {
					continue
				}
				path := extractSpringMappingPath(attr, src, annoText)
				if path != "" {
					basePaths = append(basePaths, path)
				}
			}
		}
		if len(basePaths) == 0 {
			basePaths = []string{""}
		}

		// Walk class body for method declarations
		body := findChild(node, "class_body")
		if body == nil {
			return
		}

		for i := 0; i < int(body.ChildCount()); i++ {
			member := body.Child(i)
			if member.Type() != "method_declaration" {
				continue
			}

			methodName, _ := extractMethodDecl(member, src)
			if methodName == "" {
				continue
			}

			// Method-level annotations live inside a "modifiers" child
			methodModifiers := findChild(member, "modifiers")
			if methodModifiers == nil {
				continue
			}

			for j := 0; j < int(methodModifiers.ChildCount()); j++ {
				mc := methodModifiers.Child(j)
				if mc.Type() != "marker_annotation" && mc.Type() != "annotation" {
					continue
				}
				annoText := mc.Content(src)

				verb, isMapping := matchSpringMapping(annoText)
				if !isMapping {
					continue
				}

				methodPath := extractSpringMappingPath(mc, src, annoText)

				for _, basePath := range basePaths {
					route := joinSpringRoute(verb, basePath, methodPath)
					endpoints = append(endpoints, parser.Symbol{
						Name:          methodName,
						QualifiedName: qname + "." + methodName,
						Kind:          "endpoint",
						Language:      "java",
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

// isSpringController returns true if the class has a @RestController, @Controller,
// or @RequestMapping annotation. In Java's tree-sitter grammar, class-level
// annotations live inside a "modifiers" child of the class_declaration.
func isSpringController(classNode *sitter.Node, src []byte) bool {
	modifiers := findChild(classNode, "modifiers")
	if modifiers == nil {
		return false
	}
	for i := 0; i < int(modifiers.ChildCount()); i++ {
		child := modifiers.Child(i)
		if child.Type() != "marker_annotation" && child.Type() != "annotation" {
			continue
		}
		text := child.Content(src)
		if strings.Contains(text, "RestController") ||
			strings.Contains(text, "@Controller") ||
			strings.Contains(text, "RequestMapping") {
			return true
		}
	}
	return false
}

// matchSpringMapping returns the HTTP verb for a known Spring mapping annotation
// and true if the annotation is a known mapping type.
func matchSpringMapping(annoText string) (verb string, ok bool) {
	for name, v := range springVerbMappings {
		if strings.Contains(annoText, name) {
			if v == "" {
				// @RequestMapping: look for method= GET|POST etc.; default GET
				v = extractRequestMappingVerb(annoText)
			}
			return v, true
		}
	}
	return "", false
}

// extractRequestMappingVerb parses the method= attribute of @RequestMapping.
// Returns "GET" if not specified.
func extractRequestMappingVerb(annoText string) string {
	methods := []string{"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"}
	upper := strings.ToUpper(annoText)
	for _, m := range methods {
		if strings.Contains(upper, "METHOD."+m) || strings.Contains(upper, `"`+m+`"`) {
			return m
		}
	}
	return "GET"
}

// extractSpringMappingPath extracts the path value from a Spring mapping annotation.
// It handles: @GetMapping("/path"), @GetMapping(value="/path"), @RequestMapping(path="/path").
func extractSpringMappingPath(node *sitter.Node, src []byte, annoText string) string {
	// Try to find annotation_argument_list as a direct child for structured parsing
	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		if child.Type() == "annotation_argument_list" {
			return extractSpringMappingPathFromArgList(child, src)
		}
	}
	// Fallback: text-based extraction from the raw annotation string
	return extractAnnotationStringParam(annoText)
}

// extractSpringMappingPathFromArgList extracts the first path string from an
// annotation_argument_list node. Handles:
//   - positional: ("/{id}")
//   - named value: (value = "/{id}")
//   - named path: (path = "/{id}")
func extractSpringMappingPathFromArgList(node *sitter.Node, src []byte) string {
	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		switch child.Type() {
		case "string_literal":
			return extractJavaStringLiteralContent(child, src)
		case "element_value_pair":
			// Only use value= or path= named params (skip method=, etc.)
			name := ""
			for j := 0; j < int(child.ChildCount()); j++ {
				gc := child.Child(j)
				if gc.Type() == "identifier" {
					name = gc.Content(src)
					break
				}
			}
			if name == "" || name == "value" || name == "path" {
				for j := 0; j < int(child.ChildCount()); j++ {
					gc := child.Child(j)
					if gc.Type() == "string_literal" {
						return extractJavaStringLiteralContent(gc, src)
					}
				}
			}
		}
	}
	return ""
}

// extractJavaStringLiteralContent returns the unquoted content of a Java string_literal node.
// The tree-sitter Java grammar stores the content in a string_fragment child node.
func extractJavaStringLiteralContent(node *sitter.Node, src []byte) string {
	// Prefer the string_fragment child if present (tree-sitter Java grammar)
	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		if child.Type() == "string_fragment" {
			return child.Content(src)
		}
	}
	// Fallback: strip surrounding quotes from the raw content
	text := node.Content(src)
	if len(text) >= 2 {
		return text[1 : len(text)-1]
	}
	return ""
}

// joinSpringRoute builds a canonical route string like "GET /api/users/{id}".
func joinSpringRoute(verb, basePath, methodPath string) string {
	base := "/" + strings.Trim(basePath, "/")
	if base == "/" {
		base = ""
	}
	method := ""
	if methodPath != "" {
		method = "/" + strings.Trim(methodPath, "/")
	}
	combined := base + method
	if combined == "" {
		combined = "/"
	}
	// Normalise: strip trailing slash except root
	if len(combined) > 1 {
		combined = strings.TrimRight(combined, "/")
	}
	if verb == "" {
		return combined
	}
	return verb + " " + combined
}

// findEnclosingSymbol returns the qualified name of the narrowest symbol
// (method, function, or class) that encloses the given line.
func findEnclosingSymbol(line int, symbols []parser.Symbol) string {
	best := ""
	bestSpan := 1<<31 - 1
	for _, s := range symbols {
		if (s.Kind == "method" || s.Kind == "function" || s.Kind == "class") &&
			line >= s.StartLine && line <= s.EndLine {
			span := s.EndLine - s.StartLine
			if span < bestSpan {
				bestSpan = span
				best = s.QualifiedName
			}
		}
	}
	return best
}

// isCommonJavaMethod returns true for methods that are too common to be useful
// in a dependency call graph.
func isCommonJavaMethod(name string) bool {
	return commonJavaMethods[name]
}

var commonJavaMethods = map[string]bool{
	// Object
	"toString": true, "equals": true, "hashCode": true, "getClass": true,
	"clone": true, "wait": true, "notify": true, "notifyAll": true,
	// Collection / Map
	"add": true, "remove": true, "contains": true, "clear": true,
	"size": true, "isEmpty": true, "get": true, "put": true,
	"iterator": true, "forEach": true, "containsKey": true, "containsValue": true,
	"entrySet": true, "keySet": true, "values": true, "addAll": true,
	"removeAll": true, "retainAll": true, "toArray": true, "indexOf": true,
	"set": true, "subList": true,
	// Stream / functional
	"stream": true, "collect": true, "map": true, "filter": true,
	"flatMap": true, "reduce": true, "sorted": true, "distinct": true,
	"limit": true, "skip": true, "toList": true, "of": true,
	"anyMatch": true, "allMatch": true, "noneMatch": true, "findFirst": true,
	"findAny": true, "count": true, "min": true, "max": true,
	"peek": true, "mapToInt": true, "mapToLong": true, "mapToDouble": true,
	// I/O and logging
	"println": true, "print": true, "printf": true,
	"log": true, "info": true, "warn": true, "error": true, "debug": true, "trace": true,
	"close": true, "flush": true, "read": true, "write": true, "append": true,
	// String
	"length": true, "charAt": true, "substring": true, "trim": true,
	"valueOf": true, "format": true, "concat": true, "replace": true,
	"split": true, "startsWith": true, "endsWith": true, "matches": true,
	"toLowerCase": true, "toUpperCase": true, "strip": true, "isBlank": true,
	// JDBC (already handled by extractJDBCRefs)
	"prepareStatement": true, "prepareCall": true,
	"executeQuery": true, "executeUpdate": true, "execute": true,
	"getConnection": true, "setString": true, "setInt": true, "setLong": true,
	"getInt": true, "getString": true, "getLong": true, "getBoolean": true,
	"next": true, "getResultSet": true,
	// Optional
	"orElse": true, "orElseThrow": true, "orElseGet": true,
	"isPresent": true, "ifPresent": true, "empty": true,
	// Builder / common patterns
	"build": true, "builder": true, "with": true,
	"getLogger": true, "getName": true, "getMessage": true,
}

// extractMethodCalls walks the AST for method_invocation nodes and emits
// "calls" references for non-trivial method calls.
func extractMethodCalls(root *sitter.Node, src []byte, symbols []parser.Symbol) []parser.RawReference {
	var refs []parser.RawReference

	walkTree(root, func(node *sitter.Node) {
		if node.Type() != "method_invocation" {
			return
		}

		// Extract called method name — last identifier child before argument_list.
		methodName := ""
		for i := 0; i < int(node.ChildCount()); i++ {
			child := node.Child(i)
			if child.Type() == "identifier" {
				methodName = child.Content(src)
			}
		}
		if methodName == "" || isCommonJavaMethod(methodName) {
			return
		}

		line := int(node.StartPoint().Row) + 1
		from := findEnclosingSymbol(line, symbols)
		if from == "" {
			return
		}

		refs = append(refs, parser.RawReference{
			FromSymbol:    from,
			ToName:        methodName,
			ReferenceType: "calls",
			Confidence:    0.8,
			Line:          line,
		})
	})

	return refs
}
