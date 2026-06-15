package javascript

import (
	"context"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/javascript"
	"github.com/smacker/go-tree-sitter/typescript/typescript"

	"github.com/maraichr/lattice/internal/parser"
	"github.com/maraichr/lattice/internal/parser/sqlutil"
)

// Parser implements a tree-sitter based JavaScript/TypeScript parser.
type Parser struct {
	tsParser *sitter.Parser
	lang     string // "javascript" or "typescript"
}

func NewJS() *Parser {
	p := sitter.NewParser()
	p.SetLanguage(javascript.GetLanguage())
	return &Parser{tsParser: p, lang: "javascript"}
}

func NewTS() *Parser {
	p := sitter.NewParser()
	p.SetLanguage(typescript.GetLanguage())
	return &Parser{tsParser: p, lang: "typescript"}
}

func (p *Parser) Languages() []string {
	return []string{p.lang}
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

	// JavaScript/TypeScript have no language-level namespace: the module (file)
	// is the scope. Without a per-file prefix, short top-level names like
	// `rootReducer`, `index`, or an `actions` object collide across the hundreds
	// of files that legitimately reuse them, and the (project_id, qualified_name,
	// kind) upsert collapses them to a single row. We scope bare top-level names
	// by the module path. Names that are already dotted (legacy global namespaces
	// like `dnn.dom.positioning`, prototype methods) are left global.
	moduleScope := moduleScopeFromPath(input.Path)

	for i := 0; i < int(root.ChildCount()); i++ {
		child := root.Child(i)
		syms, rfs := p.extractTopLevel(child, input.Content, moduleScope)
		symbols = append(symbols, syms...)
		refs = append(refs, rfs...)
	}

	// Post-extraction pass: detect ORM/SQL database references
	dbRefs := p.extractDatabaseRefs(root, input.Content, symbols)
	refs = append(refs, dbRefs...)

	// Post-extraction pass: detect outbound HTTP API calls (fetch, axios, http, etc.)
	apiRefs := p.extractAPICallRefs(root, input.Content, symbols)

	// Post-extraction passes: reconstruct routes for configured service clients
	// (axios instances, base-path objects, in-file factories) and for the
	// URL-builder dialect (http.verb(getApiUrl(base, action))). These can emit a
	// fuller route for a call the generic pass also matched, so all three are
	// reconciled keeping the more-qualified (more path segments) reference.
	clientRefs := p.extractServiceClientAPIRefs(root, input.Content, symbols)
	urlBuilderRefs := p.extractURLBuilderAPIRefs(root, input.Content, symbols)
	reconstructed := append(clientRefs, urlBuilderRefs...)
	refs = append(refs, dedupeAPIRefs(apiRefs, reconstructed)...)

	return &parser.ParseResult{
		Symbols:    symbols,
		References: refs,
	}, nil
}

// dedupeAPIRefs merges generic and reconstructed calls_api references. When both
// passes produced a reference for the same caller and line, the one with more
// path segments wins (the reconstructed route carries the base path the generic
// pass could not see). Non-calls_api refs and refs at distinct sites pass through.
func dedupeAPIRefs(generic, reconstructed []parser.RawReference) []parser.RawReference {
	type key struct {
		from string
		line int
	}
	best := map[key]parser.RawReference{}
	var order []key

	consider := func(r parser.RawReference) {
		if r.ReferenceType != "calls_api" {
			return
		}
		k := key{r.FromSymbol, r.Line}
		existing, ok := best[k]
		if !ok {
			best[k] = r
			order = append(order, k)
			return
		}
		if routeSegmentCount(r.ToName) > routeSegmentCount(existing.ToName) {
			best[k] = r
		}
	}
	for _, r := range generic {
		consider(r)
	}
	for _, r := range reconstructed {
		consider(r)
	}

	out := make([]parser.RawReference, 0, len(order))
	for _, k := range order {
		out = append(out, best[k])
	}
	return out
}

// routeSegmentCount counts path segments in a "VERB path/with/segments" route.
func routeSegmentCount(route string) int {
	if idx := strings.IndexByte(route, ' '); idx >= 0 {
		route = route[idx+1:]
	}
	n := 0
	for _, s := range strings.Split(route, "/") {
		if strings.TrimSpace(s) != "" {
			n++
		}
	}
	return n
}

func (p *Parser) extractTopLevel(node *sitter.Node, src []byte, scope string) ([]parser.Symbol, []parser.RawReference) {
	switch node.Type() {
	case "function_declaration":
		sym, rfs := p.extractFunctionDecl(node, src, scope)
		return []parser.Symbol{sym}, rfs

	case "class_declaration":
		return p.extractClassDecl(node, src, scope)

	case "lexical_declaration", "variable_declaration":
		return p.extractVarDecl(node, src, scope)

	case "export_statement":
		return p.extractExportStatement(node, src, scope)

	case "import_statement":
		ref := p.extractImportStatement(node, src)
		return nil, ref

	case "interface_declaration":
		sym, rfs := p.extractInterfaceDecl(node, src, scope)
		return []parser.Symbol{sym}, rfs

	case "type_alias_declaration":
		sym := p.extractTypeAlias(node, src, scope)
		return []parser.Symbol{sym}, nil

	case "enum_declaration":
		sym := p.extractEnumDecl(node, src, scope)
		return []parser.Symbol{sym}, nil

	case "expression_statement":
		return p.extractExpressionStatement(node, src, scope)
	}

	return nil, nil
}

// extractExpressionStatement handles top-level statements that define symbols
// without declaration syntax: namespace assignments (dnn.controls.x = {...}),
// prototype methods (Foo.prototype.bar = function), CommonJS exports, IIFE
// modules ((function($){...})(jQuery)), and AMD define() calls. These are the
// dominant idioms in pre-ES6 codebases.
func (p *Parser) extractExpressionStatement(node *sitter.Node, src []byte, scope string) ([]parser.Symbol, []parser.RawReference) {
	// require() side-effects anywhere in the statement
	refs := p.extractRequireFromExpression(node, src)
	var symbols []parser.Symbol

	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		switch child.Type() {
		case "assignment_expression":
			syms, rfs := p.extractAssignment(child, src, scope)
			symbols = append(symbols, syms...)
			refs = append(refs, rfs...)
		case "call_expression":
			syms, rfs := p.extractModuleCall(child, src, scope)
			symbols = append(symbols, syms...)
			refs = append(refs, rfs...)
		}
	}

	return symbols, refs
}

// extractAssignment turns `lhs = function/object` statements into symbols.
func (p *Parser) extractAssignment(node *sitter.Node, src []byte, scope string) ([]parser.Symbol, []parser.RawReference) {
	left := node.ChildByFieldName("left")
	right := node.ChildByFieldName("right")
	if left == nil || right == nil {
		return nil, nil
	}

	name, qname, kind := assignmentTarget(left, src, scope)
	if name == "" {
		return nil, nil
	}

	startLine := int(node.StartPoint().Row) + 1
	endLine := int(node.EndPoint().Row) + 1

	switch right.Type() {
	case "arrow_function", "function", "function_expression":
		sig := ""
		if params := findChild(right, "formal_parameters"); params != nil {
			sig = params.Content(src)
		}
		if kind == "" {
			kind = "function"
		}
		return []parser.Symbol{{
			Name:          name,
			QualifiedName: qname,
			Kind:          kind,
			Language:      p.lang,
			StartLine:     startLine,
			EndLine:       endLine,
			Signature:     sig,
		}}, nil

	case "object":
		// Namespace object: emit the container plus its function-valued members.
		symbols := []parser.Symbol{{
			Name:          name,
			QualifiedName: qname,
			Kind:          "module",
			Language:      p.lang,
			StartLine:     startLine,
			EndLine:       endLine,
		}}
		symbols = append(symbols, p.extractObjectMembers(right, src, qname)...)
		return symbols, nil

	case "call_expression":
		// Revealing module: ns.x = (function(){ ... })(). Emit the container and
		// recurse into the IIFE body so inner declarations become its members.
		if body := iifeBody(right); body != nil {
			symbols := []parser.Symbol{{
				Name:          name,
				QualifiedName: qname,
				Kind:          "module",
				Language:      p.lang,
				StartLine:     startLine,
				EndLine:       endLine,
			}}
			var refs []parser.RawReference
			for i := 0; i < int(body.ChildCount()); i++ {
				syms, rfs := p.extractTopLevel(body.Child(i), src, qname)
				symbols = append(symbols, syms...)
				refs = append(refs, rfs...)
			}
			return symbols, refs
		}
	}

	return nil, nil
}

// assignmentTarget interprets the LHS of a top-level assignment. Returns empty
// name for targets that don't define a durable symbol (this.x, computed, locals).
func assignmentTarget(left *sitter.Node, src []byte, scope string) (name, qname, kind string) {
	switch left.Type() {
	case "member_expression":
		full := left.Content(src)
		if strings.Contains(full, "this.") || strings.Contains(full, "[") {
			return "", "", ""
		}
		parts := strings.Split(full, ".")
		// CommonJS: module.exports / exports.foo
		if parts[0] == "module" && len(parts) >= 2 && parts[1] == "exports" {
			if len(parts) == 2 {
				return "", "", "" // anonymous module.exports = ... (members still useful via object path below, but skip container)
			}
			name = parts[len(parts)-1]
			return name, qualify(scope, name), ""
		}
		if parts[0] == "exports" && len(parts) == 2 {
			return parts[1], qualify(scope, parts[1]), ""
		}
		// Prototype method: Foo.prototype.bar → Foo.bar (method)
		for i, seg := range parts {
			if seg == "prototype" && i >= 1 && i < len(parts)-1 {
				owner := strings.Join(parts[:i], ".")
				name = parts[len(parts)-1]
				return name, qualify(scope, owner+"."+name), "method"
			}
		}
		// window.Foo → Foo
		if parts[0] == "window" && len(parts) >= 2 {
			parts = parts[1:]
		}
		name = parts[len(parts)-1]
		return name, qualify(scope, strings.Join(parts, ".")), ""

	case "identifier":
		// Bare `foo = ...` at top level: only meaningful if it defines a function.
		name = left.Content(src)
		return name, qualify(scope, name), ""
	}
	return "", "", ""
}

// extractModuleCall handles top-level call statements that wrap module code:
// IIFEs and AMD define([deps], factory).
func (p *Parser) extractModuleCall(node *sitter.Node, src []byte, scope string) ([]parser.Symbol, []parser.RawReference) {
	var symbols []parser.Symbol
	var refs []parser.RawReference

	// AMD: define(["dep", ...], function(...) { body })
	if fn := findChild(node, "identifier"); fn != nil && fn.Content(src) == "define" {
		if args := findChild(node, "arguments"); args != nil {
			for i := 0; i < int(args.ChildCount()); i++ {
				arg := args.Child(i)
				switch arg.Type() {
				case "array":
					for j := 0; j < int(arg.ChildCount()); j++ {
						if dep := arg.Child(j); dep.Type() == "string" {
							if s := extractStringContent(dep, src); s != "" {
								refs = append(refs, parser.RawReference{
									ToName:        s,
									ReferenceType: "imports",
									Line:          int(dep.StartPoint().Row) + 1,
								})
							}
						}
					}
				case "function", "function_expression", "arrow_function":
					if body := findChild(arg, "statement_block"); body != nil {
						for j := 0; j < int(body.ChildCount()); j++ {
							syms, rfs := p.extractTopLevel(body.Child(j), src, scope)
							symbols = append(symbols, syms...)
							refs = append(refs, rfs...)
						}
					}
				}
			}
		}
		return symbols, refs
	}

	// IIFE: (function(...) { body })(...)
	if body := iifeBody(node); body != nil {
		for i := 0; i < int(body.ChildCount()); i++ {
			syms, rfs := p.extractTopLevel(body.Child(i), src, scope)
			symbols = append(symbols, syms...)
			refs = append(refs, rfs...)
		}
		return symbols, refs
	}

	// Mixin/factory calls that attach an object of members to a named target:
	//   $.widget("ui.form", {...})          (jQuery UI)
	//   dnn.extend(dnn.dom.positioning, {...})  (MS Ajax / DNN)
	//   Object.assign(MyNamespace, {...})
	symbols = append(symbols, p.extractMixinCall(node, src, scope)...)

	return symbols, refs
}

// extractMixinCall handles widget/extend/assign-style calls whose effect is to
// define members on a named object. The target is the first string or
// member-expression argument; the members come from the first object literal
// that follows it.
func (p *Parser) extractMixinCall(node *sitter.Node, src []byte, scope string) []parser.Symbol {
	callee := node.ChildByFieldName("function")
	if callee == nil {
		return nil
	}

	// Method name is the last identifier of the callee.
	calleeText := callee.Content(src)
	methodName := calleeText
	if idx := strings.LastIndex(calleeText, "."); idx >= 0 {
		methodName = calleeText[idx+1:]
	}
	switch methodName {
	case "widget", "extend", "assign", "registerClass", "mix", "mixin":
	default:
		return nil
	}

	args := findChild(node, "arguments")
	if args == nil {
		return nil
	}

	// Locate the target (first string or member_expression arg) and the member
	// object (first object literal after the target).
	target := ""
	targetLine := 0
	var memberObj *sitter.Node
	for i := 0; i < int(args.ChildCount()); i++ {
		arg := args.Child(i)
		switch arg.Type() {
		case "string":
			if target == "" {
				target = extractStringContent(arg, src)
				targetLine = int(arg.StartPoint().Row) + 1
			}
		case "member_expression", "identifier":
			if target == "" {
				target = arg.Content(src)
				targetLine = int(arg.StartPoint().Row) + 1
			}
		case "object":
			if target != "" && memberObj == nil {
				memberObj = arg
			}
		}
	}
	if target == "" || memberObj == nil || strings.Contains(target, "[") {
		return nil
	}

	qname := qualify(scope, target)
	name := target
	if idx := strings.LastIndex(target, "."); idx >= 0 {
		name = target[idx+1:]
	}

	symbols := []parser.Symbol{{
		Name:          name,
		QualifiedName: qname,
		Kind:          "module",
		Language:      p.lang,
		StartLine:     targetLine,
		EndLine:       int(node.EndPoint().Row) + 1,
	}}
	symbols = append(symbols, p.extractObjectMembers(memberObj, src, qname)...)
	return symbols
}

// iifeBody returns the statement_block of an immediately-invoked function
// expression call node, or nil if the node is not an IIFE.
func iifeBody(call *sitter.Node) *sitter.Node {
	if call.Type() != "call_expression" {
		return nil
	}
	callee := call.ChildByFieldName("function")
	if callee == nil {
		return nil
	}
	if callee.Type() == "parenthesized_expression" {
		for i := 0; i < int(callee.ChildCount()); i++ {
			inner := callee.Child(i)
			switch inner.Type() {
			case "function", "function_expression", "arrow_function":
				return findChild(inner, "statement_block")
			}
		}
	}
	return nil
}

// extractObjectMembers emits function-valued properties of an object literal as
// member symbols: { foo() {} }, { foo: function() {} }, { foo: () => {} }.
func (p *Parser) extractObjectMembers(obj *sitter.Node, src []byte, parentQName string) []parser.Symbol {
	var symbols []parser.Symbol
	for i := 0; i < int(obj.ChildCount()); i++ {
		child := obj.Child(i)
		switch child.Type() {
		case "pair":
			key := child.ChildByFieldName("key")
			value := child.ChildByFieldName("value")
			if key == nil || value == nil {
				continue
			}
			switch value.Type() {
			case "arrow_function", "function", "function_expression":
				name := strings.Trim(key.Content(src), `"'`)
				sig := ""
				if params := findChild(value, "formal_parameters"); params != nil {
					sig = params.Content(src)
				}
				symbols = append(symbols, parser.Symbol{
					Name:          name,
					QualifiedName: parentQName + "." + name,
					Kind:          "method",
					Language:      p.lang,
					StartLine:     int(child.StartPoint().Row) + 1,
					EndLine:       int(child.EndPoint().Row) + 1,
					Signature:     sig,
				})
			}
		case "method_definition":
			// Shorthand: { loadPage(id) { ... } }
			sym, _ := p.extractMethodDef(child, src, parentQName)
			if sym.Name != "" {
				symbols = append(symbols, sym)
			}
		}
	}
	return symbols
}

// --- Function declarations ---

func (p *Parser) extractFunctionDecl(node *sitter.Node, src []byte, scope string) (parser.Symbol, []parser.RawReference) {
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

	qname := qualify(scope, name)
	return parser.Symbol{
		Name:          name,
		QualifiedName: qname,
		Kind:          "function",
		Language:      p.lang,
		StartLine:     int(node.StartPoint().Row) + 1,
		EndLine:       int(node.EndPoint().Row) + 1,
		Signature:     sig,
	}, nil
}

// --- Class declarations ---

func (p *Parser) extractClassDecl(node *sitter.Node, src []byte, scope string) ([]parser.Symbol, []parser.RawReference) {
	var symbols []parser.Symbol
	var refs []parser.RawReference

	name := ""
	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		if (child.Type() == "identifier" || child.Type() == "type_identifier") && name == "" {
			name = child.Content(src)
			break
		}
	}
	if name == "" {
		return nil, nil
	}

	qname := qualify(scope, name)
	symbols = append(symbols, parser.Symbol{
		Name:          name,
		QualifiedName: qname,
		Kind:          "class",
		Language:      p.lang,
		StartLine:     int(node.StartPoint().Row) + 1,
		EndLine:       int(node.EndPoint().Row) + 1,
	})

	// Heritage clauses: extends / implements
	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		if child.Type() == "class_heritage" {
			rfs := p.extractHeritage(child, src, qname)
			refs = append(refs, rfs...)
		}
	}

	// Class body members inherit the class's (scoped) qualified name so that
	// members of same-named classes in different modules don't collide.
	body := findChild(node, "class_body")
	if body != nil {
		memberSyms, memberRefs := p.extractClassMembers(body, src, qname)
		symbols = append(symbols, memberSyms...)
		refs = append(refs, memberRefs...)
	}

	return symbols, refs
}

func (p *Parser) extractHeritage(node *sitter.Node, src []byte, fromQName string) []parser.RawReference {
	var refs []parser.RawReference
	line := int(node.StartPoint().Row) + 1

	// Check direct children of class_heritage.
	// JS: class_heritage → extends + identifier (no extends_clause wrapper)
	// TS: class_heritage → extends_clause + implements_clause
	hasExtClause := false
	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		if child.Type() == "extends_clause" || child.Type() == "implements_clause" {
			hasExtClause = true
			break
		}
	}

	if !hasExtClause {
		// JS pattern: class_heritage direct children are "extends" keyword + identifier
		for i := 0; i < int(node.ChildCount()); i++ {
			child := node.Child(i)
			if child.Type() == "identifier" || child.Type() == "member_expression" {
				refs = append(refs, parser.RawReference{
					FromSymbol:    fromQName,
					ToName:        child.Content(src),
					ReferenceType: "inherits",
					Line:          line,
				})
			}
		}
		return refs
	}

	// TS pattern: walk for extends_clause / implements_clause
	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		switch child.Type() {
		case "extends_clause":
			for j := 0; j < int(child.ChildCount()); j++ {
				gc := child.Child(j)
				if gc.Type() == "identifier" || gc.Type() == "member_expression" {
					refs = append(refs, parser.RawReference{
						FromSymbol:    fromQName,
						ToName:        gc.Content(src),
						ReferenceType: "inherits",
						Line:          line,
					})
				}
			}
		case "implements_clause":
			for j := 0; j < int(child.ChildCount()); j++ {
				gc := child.Child(j)
				switch gc.Type() {
				case "type_identifier", "identifier", "generic_type":
					typeName := gc.Content(src)
					if gc.Type() == "generic_type" {
						for k := 0; k < int(gc.ChildCount()); k++ {
							ggc := gc.Child(k)
							if ggc.Type() == "type_identifier" || ggc.Type() == "identifier" {
								typeName = ggc.Content(src)
								break
							}
						}
					}
					refs = append(refs, parser.RawReference{
						FromSymbol:    fromQName,
						ToName:        typeName,
						ReferenceType: "implements",
						Line:          line,
					})
				}
			}
		}
	}

	return refs
}

func (p *Parser) extractClassMembers(body *sitter.Node, src []byte, className string) ([]parser.Symbol, []parser.RawReference) {
	var symbols []parser.Symbol
	var refs []parser.RawReference

	for i := 0; i < int(body.ChildCount()); i++ {
		child := body.Child(i)
		switch child.Type() {
		case "method_definition":
			sym, rfs := p.extractMethodDef(child, src, className)
			if sym.Name != "" {
				symbols = append(symbols, sym)
			}
			refs = append(refs, rfs...)

		case "public_field_definition", "field_definition":
			name := p.extractPropertyName(child, src)
			if name != "" {
				symbols = append(symbols, parser.Symbol{
					Name:          name,
					QualifiedName: className + "." + name,
					Kind:          "property",
					Language:      p.lang,
					StartLine:     int(child.StartPoint().Row) + 1,
					EndLine:       int(child.EndPoint().Row) + 1,
				})
			}
		}
	}

	return symbols, refs
}

func (p *Parser) extractMethodDef(node *sitter.Node, src []byte, className string) (parser.Symbol, []parser.RawReference) {
	name := ""
	sig := ""
	kind := "method"
	var refs []parser.RawReference

	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		switch child.Type() {
		case "property_identifier":
			name = child.Content(src)
		case "formal_parameters":
			sig = child.Content(src)
		case "get", "set":
			kind = "property"
		}
	}

	// Check for constructor
	if name == "constructor" {
		kind = "method"
	}

	// Check for decorators
	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		if child.Type() == "decorator" {
			decoratorName := extractDecoratorName(child, src)
			if decoratorName != "" {
				refs = append(refs, parser.RawReference{
					FromSymbol:    className + "." + name,
					ToName:        decoratorName,
					ReferenceType: "references",
					Line:          int(child.StartPoint().Row) + 1,
				})
			}
		}
	}

	if name == "" {
		return parser.Symbol{}, refs
	}

	qname := className + "." + name
	return parser.Symbol{
		Name:          name,
		QualifiedName: qname,
		Kind:          kind,
		Language:      p.lang,
		StartLine:     int(node.StartPoint().Row) + 1,
		EndLine:       int(node.EndPoint().Row) + 1,
		Signature:     sig,
	}, refs
}

func (p *Parser) extractPropertyName(node *sitter.Node, src []byte) string {
	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		if child.Type() == "property_identifier" || child.Type() == "identifier" {
			return child.Content(src)
		}
	}
	return ""
}

// --- Variable/const declarations (arrow functions, exported vars) ---

func (p *Parser) extractVarDecl(node *sitter.Node, src []byte, scope string) ([]parser.Symbol, []parser.RawReference) {
	var symbols []parser.Symbol
	var refs []parser.RawReference

	isConst := node.ChildCount() > 0 && node.Child(0).Type() == "const"

	walkChildren(node, func(child *sitter.Node) {
		if child.Type() != "variable_declarator" {
			return
		}

		name := ""
		if nameNode := child.ChildByFieldName("name"); nameNode != nil && nameNode.Type() == "identifier" {
			name = nameNode.Content(src)
		}
		if name == "" {
			return // destructuring or computed — no single symbol to define
		}

		// Check for require() calls
		reqRef := p.extractRequireFromDeclarator(child, src)
		if reqRef != nil {
			refs = append(refs, *reqRef)
			return // const x = require('y') is an import binding, not a definition
		}

		value := child.ChildByFieldName("value")
		startLine := int(node.StartPoint().Row) + 1
		endLine := int(node.EndPoint().Row) + 1

		valueType := ""
		if value != nil {
			valueType = value.Type()
		}

		switch valueType {
		case "arrow_function", "function", "function_expression":
			sig := ""
			if params := findChild(value, "formal_parameters"); params != nil {
				sig = params.Content(src)
			}
			symbols = append(symbols, parser.Symbol{
				Name:          name,
				QualifiedName: qualify(scope, name),
				Kind:          "function",
				Language:      p.lang,
				StartLine:     startLine,
				EndLine:       endLine,
				Signature:     sig,
			})

		case "object":
			// Object literal: a constant map (action types) or a behaviour module
			// (Redux actions). Emit the container and its function members.
			qname := qualify(scope, name)
			symbols = append(symbols, parser.Symbol{
				Name:          name,
				QualifiedName: qname,
				Kind:          "constant",
				Language:      p.lang,
				StartLine:     startLine,
				EndLine:       endLine,
			})
			symbols = append(symbols, p.extractObjectMembers(value, src, qname)...)

		default:
			// Any other initializer (call results like combineReducers(...),
			// literals, JSX, class expressions). Top-level declarations are the
			// module's surface — record them so the file is represented.
			kind := "variable"
			if isConst {
				kind = "constant"
			}
			symbols = append(symbols, parser.Symbol{
				Name:          name,
				QualifiedName: qualify(scope, name),
				Kind:          kind,
				Language:      p.lang,
				StartLine:     startLine,
				EndLine:       endLine,
			})
		}
	})

	return symbols, refs
}

// --- Export statements ---

func (p *Parser) extractExportStatement(node *sitter.Node, src []byte, scope string) ([]parser.Symbol, []parser.RawReference) {
	var symbols []parser.Symbol
	var refs []parser.RawReference

	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		switch child.Type() {
		case "function_declaration":
			sym, rfs := p.extractFunctionDecl(child, src, scope)
			symbols = append(symbols, sym)
			refs = append(refs, rfs...)

		case "class_declaration":
			syms, rfs := p.extractClassDecl(child, src, scope)
			symbols = append(symbols, syms...)
			refs = append(refs, rfs...)

		case "lexical_declaration", "variable_declaration":
			syms, rfs := p.extractVarDecl(child, src, scope)
			symbols = append(symbols, syms...)
			refs = append(refs, rfs...)

		case "interface_declaration":
			sym, rfs := p.extractInterfaceDecl(child, src, scope)
			symbols = append(symbols, sym)
			refs = append(refs, rfs...)

		case "type_alias_declaration":
			sym := p.extractTypeAlias(child, src, scope)
			symbols = append(symbols, sym)

		case "enum_declaration":
			sym := p.extractEnumDecl(child, src, scope)
			symbols = append(symbols, sym)

		case "string", "string_fragment":
			// export { foo } from './bar'  — the source string
			source := extractStringContent(child, src)
			if source != "" {
				refs = append(refs, parser.RawReference{
					ToName:        source,
					ReferenceType: "imports",
					Line:          int(child.StartPoint().Row) + 1,
				})
			}
		}
	}

	// Check for re-export: export { x } from 'module'
	// The "from" source is a string node that may be a direct child
	source := findChild(node, "string")
	if source != nil {
		s := extractStringContent(source, src)
		if s != "" {
			// Avoid duplicate if we already added it above
			found := false
			for _, r := range refs {
				if r.ToName == s && r.ReferenceType == "imports" {
					found = true
					break
				}
			}
			if !found {
				refs = append(refs, parser.RawReference{
					ToName:        s,
					ReferenceType: "imports",
					Line:          int(source.StartPoint().Row) + 1,
				})
			}
		}
	}

	// Handle default export of identifier (export default App)
	// handled as expression export — no symbol created, that's normal

	return symbols, refs
}

// --- Import statements ---

func (p *Parser) extractImportStatement(node *sitter.Node, src []byte) []parser.RawReference {
	var refs []parser.RawReference

	// Find the source string: import ... from 'source'
	source := findChild(node, "string")
	if source != nil {
		s := extractStringContent(source, src)
		if s != "" {
			refs = append(refs, parser.RawReference{
				ToName:        s,
				ReferenceType: "imports",
				Line:          int(node.StartPoint().Row) + 1,
			})
		}
	}

	return refs
}

// --- Require calls ---

func (p *Parser) extractRequireFromDeclarator(node *sitter.Node, src []byte) *parser.RawReference {
	var ref *parser.RawReference
	walkTree(node, func(n *sitter.Node) {
		if ref != nil {
			return
		}
		if n.Type() == "call_expression" {
			fn := findChild(n, "identifier")
			if fn != nil && fn.Content(src) == "require" {
				args := findChild(n, "arguments")
				if args != nil {
					str := findChild(args, "string")
					if str != nil {
						s := extractStringContent(str, src)
						if s != "" {
							ref = &parser.RawReference{
								ToName:        s,
								ReferenceType: "imports",
								Line:          int(n.StartPoint().Row) + 1,
							}
						}
					}
				}
			}
		}
	})
	return ref
}

func (p *Parser) extractRequireFromExpression(node *sitter.Node, src []byte) []parser.RawReference {
	var refs []parser.RawReference
	walkTree(node, func(n *sitter.Node) {
		if n.Type() == "call_expression" {
			fn := findChild(n, "identifier")
			if fn != nil && fn.Content(src) == "require" {
				args := findChild(n, "arguments")
				if args != nil {
					str := findChild(args, "string")
					if str != nil {
						s := extractStringContent(str, src)
						if s != "" {
							refs = append(refs, parser.RawReference{
								ToName:        s,
								ReferenceType: "imports",
								Line:          int(n.StartPoint().Row) + 1,
							})
						}
					}
				}
			}
		}
	})
	return refs
}

// --- TypeScript: Interface ---

func (p *Parser) extractInterfaceDecl(node *sitter.Node, src []byte, scope string) (parser.Symbol, []parser.RawReference) {
	name := ""
	var refs []parser.RawReference

	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		switch child.Type() {
		case "type_identifier", "identifier":
			if name == "" {
				name = child.Content(src)
			}
		case "extends_type_clause":
			// interface Foo extends Bar, Baz
			for j := 0; j < int(child.ChildCount()); j++ {
				gc := child.Child(j)
				if gc.Type() == "type_identifier" || gc.Type() == "identifier" || gc.Type() == "generic_type" {
					refs = append(refs, parser.RawReference{
						FromSymbol:    qualify(scope, name),
						ToName:        gc.Content(src),
						ReferenceType: "inherits",
						Line:          int(gc.StartPoint().Row) + 1,
					})
				}
			}
		}
	}

	qname := qualify(scope, name)
	return parser.Symbol{
		Name:          name,
		QualifiedName: qname,
		Kind:          "interface",
		Language:      p.lang,
		StartLine:     int(node.StartPoint().Row) + 1,
		EndLine:       int(node.EndPoint().Row) + 1,
	}, refs
}

// --- TypeScript: Type alias ---

func (p *Parser) extractTypeAlias(node *sitter.Node, src []byte, scope string) parser.Symbol {
	name := ""
	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		if (child.Type() == "type_identifier" || child.Type() == "identifier") && name == "" {
			name = child.Content(src)
		}
	}

	return parser.Symbol{
		Name:          name,
		QualifiedName: qualify(scope, name),
		Kind:          "type",
		Language:      p.lang,
		StartLine:     int(node.StartPoint().Row) + 1,
		EndLine:       int(node.EndPoint().Row) + 1,
	}
}

// --- TypeScript: Enum ---

func (p *Parser) extractEnumDecl(node *sitter.Node, src []byte, scope string) parser.Symbol {
	name := ""
	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		if child.Type() == "identifier" && name == "" {
			name = child.Content(src)
		}
	}

	return parser.Symbol{
		Name:          name,
		QualifiedName: qualify(scope, name),
		Kind:          "enum",
		Language:      p.lang,
		StartLine:     int(node.StartPoint().Row) + 1,
		EndLine:       int(node.EndPoint().Row) + 1,
	}
}

// --- Database/ORM reference detection ---

// extractDatabaseRefs walks the AST for ORM and SQL patterns:
// TypeORM/Sequelize decorators, sequelize.define, raw SQL via pool.query,
// Knex query builder, Prisma model access.
func (p *Parser) extractDatabaseRefs(root *sitter.Node, src []byte, symbols []parser.Symbol) []parser.RawReference {
	var refs []parser.RawReference

	// Build symbol line ranges for FromSymbol resolution
	type symRange struct {
		qname     string
		startLine int
		endLine   int
	}
	var ranges []symRange
	for _, s := range symbols {
		if s.Kind == "class" || s.Kind == "function" || s.Kind == "method" {
			ranges = append(ranges, symRange{s.QualifiedName, s.StartLine, s.EndLine})
		}
	}
	findEnclosing := func(line int) string {
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

	walkTree(root, func(node *sitter.Node) {
		switch node.Type() {
		case "decorator":
			// @Entity("users") or @Entity({name: "users"}) on class
			ref := p.extractEntityDecorator(node, src)
			if ref != nil {
				refs = append(refs, *ref)
			}

		case "call_expression":
			line := int(node.StartPoint().Row) + 1
			from := findEnclosing(line)

			// Check for various call patterns
			fn := findChild(node, "member_expression")
			if fn != nil {
				r := p.extractMemberCallDBRef(fn, node, src, line, from)
				refs = append(refs, r...)
			} else {
				// knex('tablename') — direct call
				fnIdent := findChild(node, "identifier")
				if fnIdent != nil && fnIdent.Content(src) == "knex" {
					args := findChild(node, "arguments")
					if args != nil {
						tableName := extractFirstString(args, src)
						if tableName != "" {
							refs = append(refs, parser.RawReference{
								FromSymbol:    from,
								ToName:        tableName,
								ReferenceType: "uses_table",
								Confidence:    0.9,
								Line:          line,
							})
						}
					}
				}
			}
		}
	})

	return refs
}

// extractEntityDecorator handles @Entity("tableName") and @Table("tableName") decorators.
func (p *Parser) extractEntityDecorator(node *sitter.Node, src []byte) *parser.RawReference {
	// Decorator child is either identifier or call_expression
	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		if child.Type() == "call_expression" {
			fn := findChild(child, "identifier")
			if fn == nil {
				continue
			}
			name := fn.Content(src)
			if name != "Entity" && name != "Table" {
				continue
			}
			args := findChild(child, "arguments")
			if args == nil {
				continue
			}
			tableName := extractFirstString(args, src)
			if tableName == "" {
				// Try object arg: @Entity({name: "users"})
				tableName = extractObjectStringProp(args, src, "name")
			}
			if tableName == "" {
				continue
			}
			// Find the class this decorator is on
			parent := node.Parent()
			className := ""
			if parent != nil {
				for j := 0; j < int(parent.ChildCount()); j++ {
					gc := parent.Child(j)
					if gc.Type() == "identifier" || gc.Type() == "type_identifier" {
						className = gc.Content(src)
						break
					}
				}
			}
			return &parser.RawReference{
				FromSymbol:    className,
				ToName:        tableName,
				ReferenceType: "uses_table",
				Confidence:    0.95,
				Line:          int(node.StartPoint().Row) + 1,
			}
		}
	}
	return nil
}

// extractMemberCallDBRef handles member expression call patterns:
// pool.query("SQL"), sequelize.define("table", ...), knex.raw("SQL"),
// prisma.user.findMany(), connection.execute("SQL")
func (p *Parser) extractMemberCallDBRef(memberExpr, callNode *sitter.Node, src []byte, line int, from string) []parser.RawReference {
	var refs []parser.RawReference

	// Get the full member expression text for analysis
	memberText := memberExpr.Content(src)

	// Extract the method name (last identifier in the member expression)
	methodName := ""
	for i := int(memberExpr.ChildCount()) - 1; i >= 0; i-- {
		child := memberExpr.Child(i)
		if child.Type() == "property_identifier" || child.Type() == "identifier" {
			methodName = child.Content(src)
			break
		}
	}

	// Extract the root object name (walk down nested member_expressions)
	objectName := extractRootIdentifier(memberExpr, src)

	args := findChild(callNode, "arguments")

	switch {
	// sequelize.define('tableName', { ... })
	case methodName == "define" && args != nil:
		tableName := extractFirstString(args, src)
		if tableName != "" {
			refs = append(refs, parser.RawReference{
				FromSymbol:    from,
				ToName:        tableName,
				ReferenceType: "uses_table",
				Confidence:    0.95,
				Line:          line,
			})
		}

	// pool.query("SQL"), connection.query("SQL"), client.query("SQL"),
	// conn.execute("SQL"), connection.execute("SQL")
	case (methodName == "query" || methodName == "execute") && args != nil:
		sqlStr := extractFirstString(args, src)
		if sqlStr != "" && sqlutil.LooksLikeSQL(sqlStr) {
			tableRefs := sqlutil.ExtractTableRefs(sqlStr, line, from, "")
			for i := range tableRefs {
				tableRefs[i].Confidence = 0.9
			}
			refs = append(refs, tableRefs...)
		}

	// knex.raw("SQL"), knex.schema.raw("SQL")
	case methodName == "raw" && args != nil:
		sqlStr := extractFirstString(args, src)
		if sqlStr != "" && sqlutil.LooksLikeSQL(sqlStr) {
			tableRefs := sqlutil.ExtractTableRefs(sqlStr, line, from, "")
			for i := range tableRefs {
				tableRefs[i].Confidence = 0.85
			}
			refs = append(refs, tableRefs...)
		}

	// conn.prepareStatement("SQL"), conn.prepareCall("{call proc}")
	case (methodName == "prepareStatement" || methodName == "prepareCall") && args != nil:
		sqlStr := extractFirstString(args, src)
		if sqlStr != "" && sqlutil.LooksLikeSQL(sqlStr) {
			tableRefs := sqlutil.ExtractTableRefs(sqlStr, line, from, "")
			for i := range tableRefs {
				tableRefs[i].Confidence = 0.9
			}
			refs = append(refs, tableRefs...)
		}

	// Prisma: prisma.user.findMany(), prisma.order.create(), etc.
	case isPrismaMethod(methodName) && strings.Contains(memberText, "."):
		// Extract the model name from prisma.modelName.method()
		modelName := extractPrismaModel(memberExpr, src)
		if modelName != "" && objectName == "prisma" {
			refs = append(refs, parser.RawReference{
				FromSymbol:    from,
				ToName:        modelName,
				ReferenceType: "uses_table",
				Confidence:    0.8,
				Line:          line,
			})
		}
	}

	// knex('table') as part of chain: knex('users').select('*')
	// Check if the object part is itself a call to knex
	if objectName == "" && memberExpr.ChildCount() > 0 {
		firstChild := memberExpr.Child(0)
		if firstChild.Type() == "call_expression" {
			knexFn := findChild(firstChild, "identifier")
			if knexFn != nil && knexFn.Content(src) == "knex" {
				knexArgs := findChild(firstChild, "arguments")
				if knexArgs != nil {
					tableName := extractFirstString(knexArgs, src)
					if tableName != "" {
						refs = append(refs, parser.RawReference{
							FromSymbol:    from,
							ToName:        tableName,
							ReferenceType: "uses_table",
							Confidence:    0.9,
							Line:          line,
						})
					}
				}
			}
		}
	}

	return refs
}

func isPrismaMethod(method string) bool {
	prisma := map[string]bool{
		"findMany": true, "findFirst": true, "findUnique": true,
		"findFirstOrThrow": true, "findUniqueOrThrow": true,
		"create": true, "createMany": true,
		"update": true, "updateMany": true,
		"upsert": true,
		"delete": true, "deleteMany": true,
		"count": true, "aggregate": true, "groupBy": true,
	}
	return prisma[method]
}

// extractPrismaModel extracts the model name from prisma.modelName.method()
func extractPrismaModel(memberExpr *sitter.Node, src []byte) string {
	// Structure: member_expression → member_expression → identifier("prisma") + property_identifier("user")
	// The outer has property_identifier("findMany")
	inner := findChild(memberExpr, "member_expression")
	if inner == nil {
		return ""
	}
	// The model name is the property_identifier of the inner member expression
	for i := 0; i < int(inner.ChildCount()); i++ {
		child := inner.Child(i)
		if child.Type() == "property_identifier" {
			return child.Content(src)
		}
	}
	return ""
}

// extractRootIdentifier walks down nested member_expressions to find the root identifier.
// e.g., for prisma.user.findUnique → returns "prisma"
func extractRootIdentifier(node *sitter.Node, src []byte) string {
	current := node
	for {
		inner := findChild(current, "member_expression")
		if inner != nil {
			current = inner
			continue
		}
		ident := findChild(current, "identifier")
		if ident != nil {
			return ident.Content(src)
		}
		return ""
	}
}

// extractFirstString returns the first string literal from an arguments node.
func extractFirstString(args *sitter.Node, src []byte) string {
	for i := 0; i < int(args.ChildCount()); i++ {
		child := args.Child(i)
		if child.Type() == "string" || child.Type() == "template_string" {
			return extractStringContent(child, src)
		}
	}
	return ""
}

// extractObjectStringProp extracts a string property value from an object literal argument.
// e.g., extractObjectStringProp(args, src, "name") from @Entity({name: "users"})
func extractObjectStringProp(args *sitter.Node, src []byte, prop string) string {
	for i := 0; i < int(args.ChildCount()); i++ {
		child := args.Child(i)
		if child.Type() == "object" {
			for j := 0; j < int(child.ChildCount()); j++ {
				pair := child.Child(j)
				if pair.Type() == "pair" {
					key := findChild(pair, "property_identifier")
					if key == nil {
						key = findChild(pair, "identifier")
					}
					if key != nil && key.Content(src) == prop {
						// Look for string, template_string, or binary_expression in the value
						for k := 0; k < int(pair.ChildCount()); k++ {
							val := pair.Child(k)
							if val.Type() == "string" {
								return extractStringContent(val, src)
							}
							if val.Type() == "template_string" {
								return extractTemplateStringContent(val, src)
							}
							if val.Type() == "binary_expression" {
								if s := extractStringPrefixFromBinaryExpr(val, src); s != "" {
									return s + "{*}"
								}
							}
						}
					}
				}
			}
		}
	}
	return ""
}

// --- Decorators (TS) ---

func extractDecoratorName(node *sitter.Node, src []byte) string {
	// @Decorator or @Decorator()
	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		switch child.Type() {
		case "identifier":
			return child.Content(src)
		case "call_expression":
			fn := findChild(child, "identifier")
			if fn != nil {
				return fn.Content(src)
			}
		}
	}
	return ""
}

// --- Helpers ---

// qualify prefixes a bare top-level name with the module scope. A name that is
// already dotted (e.g. a legacy global namespace `dnn.dom.positioning`, a
// prototype target `Foo.bar`, or a widget key `ui.form`) is treated as globally
// addressable and returned unchanged — module-scoping it would both be wrong
// (these names are shared by design) and break cross-file resolution.
func qualify(scope, name string) string {
	if scope == "" || strings.Contains(name, ".") {
		return name
	}
	return scope + "." + name
}

// moduleScopeFromPath derives a stable, file-unique scope from a source path by
// dropping the extension and normalising separators. The path is relative to the
// repository root and stable across re-indexes, so symbol IDs remain stable.
func moduleScopeFromPath(path string) string {
	if path == "" {
		return ""
	}
	p := strings.ReplaceAll(path, "\\", "/")
	if idx := strings.LastIndexByte(p, '.'); idx >= 0 {
		if slash := strings.LastIndexByte(p, '/'); idx > slash {
			p = p[:idx]
		}
	}
	return p
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

func walkChildren(node *sitter.Node, fn func(*sitter.Node)) {
	for i := 0; i < int(node.ChildCount()); i++ {
		fn(node.Child(i))
	}
}

func extractStringContent(node *sitter.Node, src []byte) string {
	text := node.Content(src)
	// Strip quotes: "foo" or 'foo' or `foo`
	if len(text) >= 2 {
		return strings.Trim(text, `"'`+"`")
	}
	return ""
}

// extractAPICallRefs detects outbound HTTP API calls and emits "calls_api" references.
//
// Patterns detected:
//   - fetch("/api/users")
//   - fetch(`/api/users/${id}`)
//   - axios.get("/api/users"), axios.post("/api/orders"), etc.
//   - axios({ method: "get", url: "/api/users" })
//   - http.get("/api/users"), https.get("/api/users")
//   - this.$http.get("/api/users"), this.http.get("/api/users") (Angular/Vue)
//
// The URL is normalised before storage: template expressions like `${id}` are
// replaced with the route-parameter placeholder `{*}` so the cross-language
// resolver can match them against backend `{id}` or `:id` patterns.
func (p *Parser) extractAPICallRefs(root *sitter.Node, src []byte, symbols []parser.Symbol) []parser.RawReference {
	var refs []parser.RawReference

	// Build symbol ranges for FromSymbol resolution (same helper used in extractDatabaseRefs)
	type symRange struct {
		qname     string
		startLine int
		endLine   int
	}
	var ranges []symRange
	for _, s := range symbols {
		if s.Kind == "class" || s.Kind == "function" || s.Kind == "method" {
			ranges = append(ranges, symRange{s.QualifiedName, s.StartLine, s.EndLine})
		}
	}
	findEnclosing := func(line int) string {
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

	walkTree(root, func(node *sitter.Node) {
		if node.Type() != "call_expression" {
			return
		}

		line := int(node.StartPoint().Row) + 1

		// Pattern A: fetch("url") — bare identifier call
		fnIdent := findChild(node, "identifier")
		if fnIdent != nil && fnIdent.Content(src) == "fetch" {
			args := findChild(node, "arguments")
			if args != nil {
				url := extractAPIURLArg(args, src)
				if url != "" && looksLikeAPIPath(url) {
					refs = append(refs, parser.RawReference{
						FromSymbol:    findEnclosing(line),
						ToName:        normalizeAPIPath(url),
						ReferenceType: "calls_api",
						Confidence:    0.9,
						Line:          line,
					})
				}
			}
			return
		}

		// Pattern B: member expression calls — axios.get(...), http.get(...), this.$http.post(...)
		memberExpr := findChild(node, "member_expression")
		if memberExpr == nil {
			return
		}

		methodName := ""
		for i := int(memberExpr.ChildCount()) - 1; i >= 0; i-- {
			child := memberExpr.Child(i)
			if child.Type() == "property_identifier" || child.Type() == "identifier" {
				methodName = child.Content(src)
				break
			}
		}

		rootObj := extractRootIdentifier(memberExpr, src)
		isHTTPClient := rootObj == "axios" || rootObj == "http" || rootObj == "https" ||
			rootObj == "request" || rootObj == "got" || rootObj == "superagent" ||
			rootObj == "ky" || rootObj == "wretch" || rootObj == "api" ||
			rootObj == "$" || rootObj == "sf" || rootObj == "jQuery"

		httpVerbs := map[string]string{
			"get":     "GET",
			"post":    "POST",
			"put":     "PUT",
			"patch":   "PATCH",
			"delete":  "DELETE",
			"head":    "HEAD",
			"options": "OPTIONS",
			"request": "", // generic
			"ajax":    "", // generic jQuery
		}

		verb, isVerb := httpVerbs[strings.ToLower(methodName)]

		switch {
		// axios.get("/api/users"), http.get("/api/users"), etc.
		case isHTTPClient && isVerb:
			args := findChild(node, "arguments")
			if args == nil {
				return
			}

			// axios({ method: "post", url: "/api/..." }) — config object form
			if strings.ToLower(methodName) == "request" || strings.ToLower(methodName) == "ajax" || rootObj == "axios" || rootObj == "$" || rootObj == "jQuery" {
				if urlFromObj := extractObjectStringProp(args, src, "url"); urlFromObj != "" && looksLikeAPIPath(urlFromObj) {
					verbFromObj := strings.ToUpper(extractObjectStringProp(args, src, "method"))
					if verbFromObj == "" {
						verbFromObj = verb
					}
					route := verbFromObj + " " + normalizeAPIPath(urlFromObj)
					refs = append(refs, parser.RawReference{
						FromSymbol:    findEnclosing(line),
						ToName:        strings.TrimSpace(route),
						ReferenceType: "calls_api",
						Confidence:    0.9,
						Line:          line,
					})
					return
				}
			}

			url := extractAPIURLArg(args, src)
			if url == "" || !looksLikeAPIPath(url) {
				return
			}
			route := strings.TrimSpace(verb + " " + normalizeAPIPath(url))
			refs = append(refs, parser.RawReference{
				FromSymbol:    findEnclosing(line),
				ToName:        route,
				ReferenceType: "calls_api",
				Confidence:    0.9,
				Line:          line,
			})
		}
	})

	return refs
}

// extractAPIURLArg extracts the first URL-like string or template string from an arguments node.
// It also handles common concatenation patterns like "/api/users/" + id.
func extractAPIURLArg(args *sitter.Node, src []byte) string {
	for i := 0; i < int(args.ChildCount()); i++ {
		child := args.Child(i)
		switch child.Type() {
		case "string":
			return extractStringContent(child, src)
		case "template_string":
			// Preserve the raw template for normalisation, replacing ${...} later.
			return extractTemplateStringContent(child, src)
		case "binary_expression":
			// "/api/users/" + id  →  extract just the string prefix
			if url := extractStringPrefixFromBinaryExpr(child, src); url != "" {
				return url + "{*}"
			}
		}
	}
	return ""
}

// extractStringPrefixFromBinaryExpr returns the leading string literal from a
// binary_expression (concatenation), e.g. "/api/users/" from "/api/users/" + id.
func extractStringPrefixFromBinaryExpr(node *sitter.Node, src []byte) string {
	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		if child.Type() == "string" {
			s := extractStringContent(child, src)
			if s != "" {
				return s
			}
		}
		// Recurse in case of nested concatenation: ("api/" + base) + id
		if child.Type() == "binary_expression" {
			if s := extractStringPrefixFromBinaryExpr(child, src); s != "" {
				return s
			}
		}
	}
	return ""
}

// extractTemplateStringContent returns the text content of a template_string node,
// keeping `${}` expressions as-is so normalizeAPIPath can convert them.
func extractTemplateStringContent(node *sitter.Node, src []byte) string {
	text := node.Content(src)
	// Strip surrounding backticks.
	return strings.Trim(text, "`")
}

// looksLikeAPIPath returns true when the string is plausibly an internal API path.
// We require it to start with "/" and contain at least one path segment, or start
// with "/api" which is the most common convention.
func looksLikeAPIPath(s string) bool {
	if s == "" || s == "/" {
		return false
	}
	// Reject fully-qualified external URLs (http:// https://)
	if strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") {
		// Still consider internal base-URL patterns like "http://localhost/api/..."
		if !strings.Contains(s, "/api/") && !strings.Contains(s, "/v1/") && !strings.Contains(s, "/v2/") {
			return false
		}
		return true
	}
	// If it's passed to fetch/axios/ajax, it's an API path even if relative.
	return true
}

// normalizeAPIPath converts a raw URL string (possibly a template literal or
// fully-qualified URL) into a canonical path used for cross-language matching.
//
// Transformations applied:
//   - Strip scheme + host from fully-qualified URLs
//   - Replace template expressions ${...} with {*}
//   - Replace Express-style :param with {*}
//   - Remove trailing slashes
func normalizeAPIPath(raw string) string {
	s := raw

	// Strip scheme+host from fully-qualified URLs
	if idx := strings.Index(s, "://"); idx >= 0 {
		rest := s[idx+3:]
		if slashIdx := strings.IndexByte(rest, '/'); slashIdx >= 0 {
			s = rest[slashIdx:]
		}
	}

	// Strip query string and fragment
	if idx := strings.IndexByte(s, '?'); idx >= 0 {
		s = s[:idx]
	}
	if idx := strings.IndexByte(s, '#'); idx >= 0 {
		s = s[:idx]
	}

	// Replace JS template expressions ${...} with {*}
	for {
		start := strings.Index(s, "${")
		if start < 0 {
			break
		}
		end := strings.IndexByte(s[start:], '}')
		if end < 0 {
			break
		}
		s = s[:start] + "{*}" + s[start+end+1:]
	}

	// Replace Express-style :param segments with {*}
	parts := strings.Split(s, "/")
	for i, part := range parts {
		if strings.HasPrefix(part, ":") && len(part) > 1 {
			parts[i] = "{*}"
		}
	}
	s = strings.Join(parts, "/")

	// Remove trailing slash
	s = strings.TrimRight(s, "/")

	return s
}
