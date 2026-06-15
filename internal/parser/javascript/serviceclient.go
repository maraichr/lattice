package javascript

import (
	"strings"

	sitter "github.com/smacker/go-tree-sitter"

	"github.com/maraichr/lattice/internal/parser"
)

// Service-client API-route reconstruction.
//
// Many codebases don't call the network with a literal path. Instead they
// configure a client object with one or more base-path segments and then call a
// verb method with only the trailing path:
//
//	const api = axios.create({ baseURL: "/api/users" });
//	api.get(`/${id}`);                       // → /api/users/{*}
//
//	// DNN ServicesFramework
//	getServiceFramework(controller) {
//	    const sf = util.utilities.sf;
//	    sf.moduleRoot = "PersonaBar";
//	    sf.controller = controller;          // controller bound at the call site
//	    return sf;
//	}
//	const sf = this.getServiceFramework("SiteSettings");
//	sf.get("GetPortalSettings?id=1");        // → PersonaBar/SiteSettings/GetPortalSettings
//
// In both cases the literal argument to the verb call is only the last segment of
// the real route. This pass reconstructs the full path by tracking base-path
// configuration — whether set directly, via axios.create, or returned from an
// in-file factory — and prepending it. DNN is one instance of the pattern, not a
// special case: the trigger is the set of base-path property names below.

// baseProps are property names that hold a base-path segment of an API client.
// A value assigned to one of these (or passed to axios.create as baseURL) is
// treated as a leading segment of the route.
var baseProps = map[string]bool{
	"baseurl":     true,
	"basepath":    true,
	"root":        true,
	"rootpath":    true,
	"serviceroot": true,
	"moduleroot":  true,
	"controller":  true,
	"prefix":      true,
	"apiprefix":   true,
	"resource":    true,
}

// urlBuilderFuncs are helper functions that build a request URL from a base
// reference and an action, e.g. umbRequestHelper.getApiUrl("contentApiBaseUrl",
// "GetById", {...}). The base reference is usually a config key whose name is
// derived from the controller (contentApiBaseUrl → content); combined with the
// action and the enclosing HTTP verb this reconstructs a matchable route. This is
// a distinct dialect from the configured-client pattern and common in AngularJS.
var urlBuilderFuncs = map[string]bool{
	"getapiurl":     true,
	"getapibaseurl": true,
	"buildapiurl":   true,
	"geturl":        true,
	"buildurl":      true,
	"resolveurl":    true,
	"createurl":     true,
	"apiurl":        true,
}

// baseKeySuffixes are trailing tokens stripped from a base-URL config key to
// recover the resource/controller segment (contentApiBaseUrl → content). Ordered
// longest-first so the most specific suffix wins.
var baseKeySuffixes = []string{"ApiBaseUrl", "ApiUrl", "BaseUrl", "Url"}

// segSpec is one base segment of a factory's configured route: either a string
// literal, or a reference to the factory's Nth parameter (resolved at the call
// site).
type segSpec struct {
	literal  string
	paramIdx int // -1 when literal
}

// factoryInfo describes an in-file function that returns a base-configured client.
type factoryInfo struct {
	params []string
	segs   []segSpec
}

// extractServiceClientAPIRefs reconstructs calls_api references for verb calls on
// configured service clients. It is additive to extractAPICallRefs; overlapping
// references (same caller + line) are reconciled in Parse, keeping the longer
// reconstructed route.
func (p *Parser) extractServiceClientAPIRefs(root *sitter.Node, src []byte, symbols []parser.Symbol) []parser.RawReference {
	factories := collectFactories(root, src)

	enclosing := enclosingResolver(symbols)

	// clientBase maps a variable name to its accumulated base-path segments,
	// populated in document order as declarations and config assignments are
	// visited before the verb calls that use them.
	clientBase := map[string][]string{}
	var refs []parser.RawReference

	walkTree(root, func(node *sitter.Node) {
		switch node.Type() {
		case "variable_declarator":
			name := ""
			if n := node.ChildByFieldName("name"); n != nil && n.Type() == "identifier" {
				name = n.Content(src)
			}
			value := node.ChildByFieldName("value")
			if name == "" || value == nil || value.Type() != "call_expression" {
				return
			}
			if segs, ok := resolveClientFromCall(value, src, factories); ok {
				clientBase[name] = segs
			}

		case "assignment_expression":
			// client.<baseProp> = value  → append a base segment to that client.
			left := node.ChildByFieldName("left")
			right := node.ChildByFieldName("right")
			if left == nil || right == nil || left.Type() != "member_expression" {
				return
			}
			obj := left.ChildByFieldName("object")
			propNode := left.ChildByFieldName("property")
			if obj == nil || propNode == nil || obj.Type() != "identifier" {
				return
			}
			if !baseProps[strings.ToLower(propNode.Content(src))] {
				return
			}
			if seg := literalSegment(right, src); seg != "" {
				v := obj.Content(src)
				clientBase[v] = append(clientBase[v], seg)
			}

		case "call_expression":
			// Verb call on a tracked client: client.get("path").
			memberExpr := node.ChildByFieldName("function")
			if memberExpr == nil || memberExpr.Type() != "member_expression" {
				return
			}
			obj := memberExpr.ChildByFieldName("object")
			if obj == nil || obj.Type() != "identifier" {
				return
			}
			base, ok := clientBase[obj.Content(src)]
			if !ok {
				return
			}
			verb, isVerb := httpVerbFromMethod(memberExpr, src)
			if !isVerb {
				return
			}
			args := findChild(node, "arguments")
			if args == nil {
				return
			}
			path := extractAPIURLArg(args, src)
			if path == "" {
				return
			}

			segs := append(append([]string{}, base...), splitSegments(normalizeAPIPath(path))...)
			route := strings.Join(segs, "/")
			if route == "" {
				return
			}
			line := int(node.StartPoint().Row) + 1
			to := strings.TrimSpace(verb + " " + route)
			refs = append(refs, parser.RawReference{
				FromSymbol:    enclosing(line),
				ToName:        to,
				ReferenceType: "calls_api",
				Confidence:    0.85,
				Line:          line,
			})
		}
	})

	return refs
}

// extractURLBuilderAPIRefs reconstructs calls_api references for the URL-builder
// dialect: an HTTP verb call whose URL argument is a helper call that assembles
// the URL from a base reference and an action, e.g.
//
//	$http.post(umbRequestHelper.getApiUrl("contentApiBaseUrl", "GetById", {id}))
//	fetch(buildUrl("usersBaseUrl", "list"))
//
// The verb comes from the enclosing call (.post/.get/…); the route is
// <resource>/<action> where <resource> is the base key with a trailing
// Url/BaseUrl/ApiBaseUrl suffix stripped (the key is conventionally named after
// the controller). The suffix route matcher then links it to the backend
// endpoint without needing to resolve the runtime base URL.
func (p *Parser) extractURLBuilderAPIRefs(root *sitter.Node, src []byte, symbols []parser.Symbol) []parser.RawReference {
	enclosing := enclosingResolver(symbols)
	var refs []parser.RawReference

	walkTree(root, func(node *sitter.Node) {
		if node.Type() != "call_expression" {
			return
		}

		// Verb from the outer call: member-expression verb method, or bare fetch().
		verb := ""
		callee := node.ChildByFieldName("function")
		if callee == nil {
			return
		}
		switch callee.Type() {
		case "member_expression":
			v, ok := httpVerbFromMethod(callee, src)
			if !ok {
				return
			}
			verb = v
		case "identifier":
			if callee.Content(src) != "fetch" {
				return
			}
		default:
			return
		}

		// First argument must be a url-builder helper call.
		args := findChild(node, "arguments")
		if args == nil {
			return
		}
		builder := firstArgCall(args)
		if builder == nil {
			return
		}
		if !urlBuilderFuncs[strings.ToLower(calleeName(builder.ChildByFieldName("function"), src))] {
			return
		}

		bArgs := stringArgs(findChild(builder, "arguments"), src)
		baseKey, action := "", ""
		for _, a := range bArgs {
			if a == "" {
				continue
			}
			if baseKey == "" {
				baseKey = a
			} else {
				action = a
				break
			}
		}
		resource := stripBaseKeySuffix(baseKey)
		if resource == "" || action == "" {
			return
		}

		segs := append(splitSegments(resource), splitSegments(normalizeAPIPath(action))...)
		if len(segs) < 2 {
			return
		}
		line := int(node.StartPoint().Row) + 1
		refs = append(refs, parser.RawReference{
			FromSymbol:    enclosing(line),
			ToName:        strings.TrimSpace(verb + " " + strings.Join(segs, "/")),
			ReferenceType: "calls_api",
			Confidence:    0.8,
			Line:          line,
		})
	})

	return refs
}

// firstArgCall returns the first argument of an arguments node if it is a call
// expression (skipping punctuation), else nil.
func firstArgCall(args *sitter.Node) *sitter.Node {
	for i := 0; i < int(args.ChildCount()); i++ {
		child := args.Child(i)
		switch child.Type() {
		case "(", ")", ",", "comment":
			continue
		case "call_expression":
			return child
		default:
			return nil
		}
	}
	return nil
}

// stripBaseKeySuffix removes a trailing Url/BaseUrl/ApiBaseUrl token from a base
// key to recover the resource segment (contentApiBaseUrl → content). Returns ""
// if the key is empty or is nothing but the suffix.
func stripBaseKeySuffix(key string) string {
	for _, suf := range baseKeySuffixes {
		if len(key) > len(suf) && strings.EqualFold(key[len(key)-len(suf):], suf) {
			return key[:len(key)-len(suf)]
		}
	}
	return key
}

// collectFactories finds in-file functions/methods that configure a client with
// base-path properties, returning a map keyed by the function name.
func collectFactories(root *sitter.Node, src []byte) map[string]*factoryInfo {
	factories := map[string]*factoryInfo{}

	walkTree(root, func(node *sitter.Node) {
		var nameNode, paramsNode, body *sitter.Node
		switch node.Type() {
		case "function_declaration", "method_definition":
			nameNode = firstChildOfType(node, "identifier", "property_identifier")
			paramsNode = findChild(node, "formal_parameters")
			body = findChild(node, "statement_block")
		default:
			return
		}
		if nameNode == nil || body == nil {
			return
		}
		name := nameNode.Content(src)

		params := paramNames(paramsNode, src)
		segs := collectBaseSegments(body, src, params)
		if len(segs) > 0 {
			factories[name] = &factoryInfo{params: params, segs: segs}
		}
	})

	return factories
}

// collectBaseSegments scans a function body for `obj.<baseProp> = value`
// assignments and returns them, in source order, as literal or parameter-bound
// segments.
func collectBaseSegments(body *sitter.Node, src []byte, params []string) []segSpec {
	var segs []segSpec
	walkTree(body, func(n *sitter.Node) {
		if n.Type() != "assignment_expression" {
			return
		}
		left := n.ChildByFieldName("left")
		right := n.ChildByFieldName("right")
		if left == nil || right == nil || left.Type() != "member_expression" {
			return
		}
		prop := left.ChildByFieldName("property")
		if prop == nil || !baseProps[strings.ToLower(prop.Content(src))] {
			return
		}
		if right.Type() == "string" || right.Type() == "template_string" {
			if s := literalSegment(right, src); s != "" {
				segs = append(segs, segSpec{literal: s, paramIdx: -1})
			}
			return
		}
		if right.Type() == "identifier" {
			if idx := indexOf(params, right.Content(src)); idx >= 0 {
				segs = append(segs, segSpec{paramIdx: idx})
			}
		}
	})
	return segs
}

// resolveClientFromCall returns the base segments for `factory(args)` /
// `axios.create({baseURL})` call expressions.
func resolveClientFromCall(call *sitter.Node, src []byte, factories map[string]*factoryInfo) ([]string, bool) {
	callee := call.ChildByFieldName("function")
	if callee == nil {
		return nil, false
	}

	// axios.create({ baseURL: "..." })
	if callee.Type() == "member_expression" {
		if prop := callee.ChildByFieldName("property"); prop != nil && prop.Content(src) == "create" {
			if args := findChild(call, "arguments"); args != nil {
				if b := extractObjectStringProp(args, src, "baseURL"); b != "" {
					return splitSegments(normalizeAPIPath(b)), true
				}
			}
		}
	}

	// Named factory call: factory(args) / obj.factory(args) / this.factory(args).
	fnName := calleeName(callee, src)
	fi, ok := factories[fnName]
	if !ok {
		return nil, false
	}
	callArgs := stringArgs(findChild(call, "arguments"), src)

	var out []string
	for _, seg := range fi.segs {
		var val string
		if seg.paramIdx < 0 {
			val = seg.literal
		} else if seg.paramIdx < len(callArgs) && callArgs[seg.paramIdx] != "" {
			val = callArgs[seg.paramIdx]
		} else {
			val = "{*}" // parameter not bound to a literal at this call site
		}
		out = append(out, splitSegments(val)...)
	}
	return out, true
}

// --- small helpers ---

// httpVerbFromMethod returns the HTTP verb for a member expression whose method
// name is an HTTP verb (get/post/put/patch/delete/head/options).
func httpVerbFromMethod(memberExpr *sitter.Node, src []byte) (string, bool) {
	prop := memberExpr.ChildByFieldName("property")
	if prop == nil {
		return "", false
	}
	verbs := map[string]string{
		"get": "GET", "post": "POST", "put": "PUT", "patch": "PATCH",
		"delete": "DELETE", "head": "HEAD", "options": "OPTIONS",
	}
	v, ok := verbs[strings.ToLower(prop.Content(src))]
	return v, ok
}

// literalSegment returns the string content of a string/template literal node, or
// "" for anything else.
func literalSegment(node *sitter.Node, src []byte) string {
	switch node.Type() {
	case "string":
		return extractStringContent(node, src)
	case "template_string":
		return extractTemplateStringContent(node, src)
	}
	return ""
}

// calleeName returns the final identifier of a call's callee: "f" for f(),
// "create" for axios.create(), "getServiceFramework" for this.getServiceFramework().
func calleeName(callee *sitter.Node, src []byte) string {
	switch callee.Type() {
	case "identifier":
		return callee.Content(src)
	case "member_expression":
		if prop := callee.ChildByFieldName("property"); prop != nil {
			return prop.Content(src)
		}
	}
	return ""
}

// stringArgs returns the string-literal arguments of a call in order; a
// non-literal argument yields "" at its position so parameter indices line up.
func stringArgs(args *sitter.Node, src []byte) []string {
	if args == nil {
		return nil
	}
	var out []string
	for i := 0; i < int(args.ChildCount()); i++ {
		child := args.Child(i)
		switch child.Type() {
		case "(", ")", ",", "comment":
			continue
		case "string", "template_string":
			out = append(out, literalSegment(child, src))
		default:
			out = append(out, "")
		}
	}
	return out
}

// paramNames returns the identifier names of a formal_parameters node in order.
func paramNames(params *sitter.Node, src []byte) []string {
	if params == nil {
		return nil
	}
	var out []string
	for i := 0; i < int(params.ChildCount()); i++ {
		child := params.Child(i)
		switch child.Type() {
		case "identifier", "required_parameter", "optional_parameter":
			if child.Type() == "identifier" {
				out = append(out, child.Content(src))
			} else if id := findChild(child, "identifier"); id != nil {
				out = append(out, id.Content(src))
			}
		}
	}
	return out
}

// splitSegments splits a path on "/" into non-empty, query-stripped segments.
func splitSegments(path string) []string {
	if idx := strings.IndexByte(path, '?'); idx >= 0 {
		path = path[:idx]
	}
	var out []string
	for _, s := range strings.Split(path, "/") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func firstChildOfType(node *sitter.Node, types ...string) *sitter.Node {
	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		for _, t := range types {
			if child.Type() == t {
				return child
			}
		}
	}
	return nil
}

func indexOf(ss []string, target string) int {
	for i, s := range ss {
		if s == target {
			return i
		}
	}
	return -1
}

// enclosingResolver returns a function mapping a source line to the smallest
// enclosing class/function/method symbol's qualified name. Mirrors the
// findEnclosing helper used by the other API passes.
func enclosingResolver(symbols []parser.Symbol) func(line int) string {
	type symRange struct {
		qname              string
		startLine, endLine int
	}
	var ranges []symRange
	for _, s := range symbols {
		switch s.Kind {
		case "class", "function", "method":
			ranges = append(ranges, symRange{s.QualifiedName, s.StartLine, s.EndLine})
		}
	}
	return func(line int) string {
		best := ""
		bestSpan := 1<<31 - 1
		for _, r := range ranges {
			if line >= r.startLine && line <= r.endLine {
				if span := r.endLine - r.startLine; span < bestSpan {
					bestSpan = span
					best = r.qname
				}
			}
		}
		return best
	}
}
