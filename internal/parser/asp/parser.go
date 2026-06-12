package asp

import (
	"regexp"
	"strings"

	"github.com/maraichr/lattice/internal/parser"
)

// Parser implements a parser for ASP Classic (VBScript) files.
type Parser struct{}

func New() *Parser {
	return &Parser{}
}

func (p *Parser) Languages() []string {
	return []string{"asp", "aspx"}
}

func (p *Parser) Parse(input parser.FileInput) (*parser.ParseResult, error) {
	content := string(input.Content)

	var symbols []parser.Symbol
	var refs []parser.RawReference

	// Extract ASP.NET directives from <%@ ... %> blocks
	dirRefs := extractDirectives(content)
	refs = append(refs, dirRefs...)

	// Data-bound control commands in markup, e.g. <asp:SqlDataSource SelectCommand="...">
	refs = append(refs, extractDataSourceCommands(content)...)

	// Extract code regions: <% ... %> and <script runat="server"> blocks
	regions := extractScriptRegions(content)
	regions = append(regions, extractServerScriptRegions(content)...)

	// .ashx WebHandler files: everything after the directive is raw C#/VB.NET code
	var fileScopeSyms []parser.Symbol
	if whRegion, whSym, ok := extractWebHandler(content); ok {
		if whSym != nil {
			fileScopeSyms = append(fileScopeSyms, *whSym)
			symbols = append(symbols, *whSym)
		}
		regions = append(regions, whRegion)
	}

	for _, region := range regions {
		// Parse VBScript constructs
		syms, rfs := parseVBScript(region.code, region.startLine)
		symbols = append(symbols, syms...)
		refs = append(refs, rfs...)

		// Candidates for enclosing-scope attribution: this region's symbols plus
		// file-spanning ones (e.g. the WebHandler class).
		candidates := make([]parser.Symbol, 0, len(syms)+len(fileScopeSyms))
		candidates = append(candidates, syms...)
		candidates = append(candidates, fileScopeSyms...)

		// Extract embedded SQL from ADO patterns; set FromSymbol to enclosing function/sub for cross-language resolution
		sqlFragments := ExtractSQL(region.code)
		for _, frag := range sqlFragments {
			line := region.startLine + frag.Line - 1
			sqlRefs := extractSQLRefs(frag.SQL, line)
			fromSymbol := enclosingSymbol(line, candidates)
			for i := range sqlRefs {
				sqlRefs[i].FromSymbol = fromSymbol
				sqlRefs[i].ToQualified = "dbo." + sqlRefs[i].ToName
				if sqlRefs[i].Confidence == 0 {
					sqlRefs[i].Confidence = frag.Confidence
				}
			}
			refs = append(refs, sqlRefs...)
		}

		// Stored procedures invoked by name, e.g. cmd.CommandText = "GetUsers"
		for _, pc := range ExtractProcCalls(region.code) {
			line := region.startLine + pc.Line - 1
			name := strings.TrimPrefix(pc.Name, "dbo.")
			refs = append(refs, parser.RawReference{
				FromSymbol:    enclosingSymbol(line, candidates),
				ToName:        name,
				ToQualified:   "dbo." + name,
				ReferenceType: "calls",
				Line:          line,
			})
		}
	}

	// Parse include directives
	includes := parseIncludes(content)
	refs = append(refs, includes...)

	return &parser.ParseResult{
		Symbols:    symbols,
		References: refs,
	}, nil
}

type scriptRegion struct {
	code      string
	startLine int
}

func extractScriptRegions(content string) []scriptRegion {
	var regions []scriptRegion

	// Match <% ... %> blocks (non-greedy)
	re := regexp.MustCompile(`(?s)<%([^=].*?)%>`)
	matches := re.FindAllStringSubmatchIndex(content, -1)

	for _, loc := range matches {
		code := content[loc[2]:loc[3]]
		startLine := strings.Count(content[:loc[2]], "\n") + 1
		regions = append(regions, scriptRegion{code: code, startLine: startLine})
	}

	return regions
}

// serverScriptRe matches <script runat="server"> blocks used by both classic
// ASP and ASP.NET WebForms pages for server-side code (VBScript, VB.NET, or C#).
var serverScriptRe = regexp.MustCompile(`(?is)<script\b[^>]*\brunat\s*=\s*"?server"?[^>]*>(.*?)</script>`)

func extractServerScriptRegions(content string) []scriptRegion {
	var regions []scriptRegion
	for _, loc := range serverScriptRe.FindAllStringSubmatchIndex(content, -1) {
		code := content[loc[2]:loc[3]]
		startLine := strings.Count(content[:loc[2]], "\n") + 1
		regions = append(regions, scriptRegion{code: code, startLine: startLine})
	}
	return regions
}

// webHandlerRe matches the <%@ WebHandler %> directive that starts .ashx files.
var webHandlerRe = regexp.MustCompile(`(?i)<%@\s*WebHandler\b[^%]*%>`)

// extractWebHandler treats the body of an .ashx file (everything after the
// <%@ WebHandler %> directive) as a code region, and emits a class symbol for
// the handler so embedded SQL references have an enclosing scope.
func extractWebHandler(content string) (scriptRegion, *parser.Symbol, bool) {
	loc := webHandlerRe.FindStringIndex(content)
	if loc == nil {
		return scriptRegion{}, nil, false
	}

	directive := content[loc[0]:loc[1]]
	code := content[loc[1]:]
	startLine := strings.Count(content[:loc[1]], "\n") + 1

	var sym *parser.Symbol
	if cls := extractAttrValue(directive, "Class"); cls != "" {
		name := cls
		if idx := strings.LastIndex(cls, "."); idx >= 0 {
			name = cls[idx+1:]
		}
		sym = &parser.Symbol{
			Name:          name,
			QualifiedName: cls,
			Kind:          "class",
			Language:      "asp",
			StartLine:     1,
			EndLine:       strings.Count(content, "\n") + 1,
		}
	}

	return scriptRegion{code: code, startLine: startLine}, sym, true
}

// dataSourceCmdRe matches SQL command attributes on data-bound WebForms
// controls, e.g. <asp:SqlDataSource SelectCommand="SELECT ..." UpdateCommand="...">.
var dataSourceCmdRe = regexp.MustCompile(`(?i)\b(?:Select|Insert|Update|Delete)Command\s*=\s*"([^"]+)"`)

func extractDataSourceCommands(content string) []parser.RawReference {
	var refs []parser.RawReference
	for _, loc := range dataSourceCmdRe.FindAllStringSubmatchIndex(content, -1) {
		val := content[loc[2]:loc[3]]
		line := strings.Count(content[:loc[0]], "\n") + 1
		if looksLikeSQL(val) {
			sqlRefs := extractSQLRefs(val, line)
			for i := range sqlRefs {
				sqlRefs[i].ToQualified = "dbo." + sqlRefs[i].ToName
				if sqlRefs[i].Confidence == 0 {
					sqlRefs[i].Confidence = 0.9
				}
			}
			refs = append(refs, sqlRefs...)
		} else if procNameRe.MatchString(val) {
			// SelectCommandType="StoredProcedure" form: the command is a proc name
			name := strings.TrimPrefix(val, "dbo.")
			refs = append(refs, parser.RawReference{
				ToName:        name,
				ToQualified:   "dbo." + name,
				ReferenceType: "calls",
				Line:          line,
			})
		}
	}
	return refs
}

func parseVBScript(code string, baseOffset int) ([]parser.Symbol, []parser.RawReference) {
	var symbols []parser.Symbol
	var refs []parser.RawReference

	lines := strings.Split(code, "\n")

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		lower := strings.ToLower(trimmed)
		lineNum := baseOffset + i

		// Function Name(params)
		if strings.HasPrefix(lower, "function ") || strings.HasPrefix(lower, "public function ") || strings.HasPrefix(lower, "private function ") {
			name, sig := parseProcDecl(trimmed)
			if name != "" {
				symbols = append(symbols, parser.Symbol{
					Name:          name,
					QualifiedName: name,
					Kind:          "function",
					Language:      "asp",
					StartLine:     lineNum,
					EndLine:       findEndLine(lines, i, "function", baseOffset),
					Signature:     sig,
				})
			}
		}

		// Sub Name(params)
		if strings.HasPrefix(lower, "sub ") || strings.HasPrefix(lower, "public sub ") || strings.HasPrefix(lower, "private sub ") {
			name, sig := parseProcDecl(trimmed)
			if name != "" {
				symbols = append(symbols, parser.Symbol{
					Name:          name,
					QualifiedName: name,
					Kind:          "procedure",
					Language:      "asp",
					StartLine:     lineNum,
					EndLine:       findEndLine(lines, i, "sub", baseOffset),
					Signature:     sig,
				})
			}
		}

		// Class Name
		if strings.HasPrefix(lower, "class ") {
			parts := strings.Fields(trimmed)
			if len(parts) >= 2 {
				name := parts[1]
				symbols = append(symbols, parser.Symbol{
					Name:          name,
					QualifiedName: name,
					Kind:          "class",
					Language:      "asp",
					StartLine:     lineNum,
					EndLine:       findEndLine(lines, i, "class", baseOffset),
				})
			}
		}

		// Const declarations
		if strings.HasPrefix(lower, "const ") || strings.HasPrefix(lower, "public const ") || strings.HasPrefix(lower, "private const ") {
			name := parseConstDecl(trimmed)
			if name != "" {
				symbols = append(symbols, parser.Symbol{
					Name:          name,
					QualifiedName: name,
					Kind:          "constant",
					Language:      "asp",
					StartLine:     lineNum,
					EndLine:       lineNum,
				})
			}
		}

		// Server.CreateObject references
		if strings.Contains(lower, "server.createobject") {
			re := regexp.MustCompile(`(?i)Server\.CreateObject\s*\(\s*"([^"]+)"\s*\)`)
			if m := re.FindStringSubmatch(trimmed); len(m) >= 2 {
				refs = append(refs, parser.RawReference{
					ToName:        m[1],
					ReferenceType: "references",
					Line:          lineNum,
				})
			}
		}
	}

	return symbols, refs
}

func parseProcDecl(line string) (name, signature string) {
	// Remove access modifier
	lower := strings.ToLower(line)
	for _, prefix := range []string{"public ", "private "} {
		if strings.HasPrefix(lower, prefix) {
			line = line[len(prefix):]
			break
		}
	}

	// Remove Function/Sub keyword
	lower = strings.ToLower(line)
	for _, prefix := range []string{"function ", "sub "} {
		if strings.HasPrefix(lower, prefix) {
			line = line[len(prefix):]
			break
		}
	}

	// Extract name and params
	if idx := strings.Index(line, "("); idx >= 0 {
		name = strings.TrimSpace(line[:idx])
		endIdx := strings.Index(line, ")")
		if endIdx > idx {
			signature = line[idx : endIdx+1]
		}
	} else {
		name = strings.TrimSpace(line)
	}

	return name, signature
}

func parseConstDecl(line string) string {
	lower := strings.ToLower(line)
	for _, prefix := range []string{"public const ", "private const ", "const "} {
		if strings.HasPrefix(lower, prefix) {
			rest := line[len(prefix):]
			if idx := strings.Index(rest, "="); idx >= 0 {
				return strings.TrimSpace(rest[:idx])
			}
			return strings.TrimSpace(rest)
		}
	}
	return ""
}

// findEndLine returns the absolute line of the matching "End <kind>" statement.
// baseOffset is the absolute line number of lines[0].
func findEndLine(lines []string, startIdx int, kind string, baseOffset int) int {
	endKW := "end " + kind
	for i := startIdx + 1; i < len(lines); i++ {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(lines[i])), endKW) {
			return baseOffset + i
		}
	}
	return baseOffset + startIdx
}

// enclosingSymbol returns the qualified name of the innermost symbol (function/sub/class) that contains the given line.
func enclosingSymbol(line int, symbols []parser.Symbol) string {
	var best *parser.Symbol
	for i := range symbols {
		s := &symbols[i]
		if s.StartLine <= line && line <= s.EndLine {
			if best == nil || (s.EndLine-s.StartLine) < (best.EndLine-best.StartLine) {
				best = s
			}
		}
	}
	if best == nil {
		return ""
	}
	return best.QualifiedName
}

func parseIncludes(content string) []parser.RawReference {
	var refs []parser.RawReference

	re := regexp.MustCompile(`<!--\s*#include\s+(file|virtual)\s*=\s*"([^"]+)"\s*-->`)
	for _, match := range re.FindAllStringSubmatch(content, -1) {
		if len(match) >= 3 {
			refs = append(refs, parser.RawReference{
				ToName:        match[2],
				ReferenceType: "imports",
			})
		}
	}

	return refs
}

func extractSQLRefs(sql string, line int) []parser.RawReference {
	var refs []parser.RawReference

	upper := strings.ToUpper(sql)

	// Extract table names from FROM/JOIN/INTO/UPDATE clauses
	tablePatterns := []*regexp.Regexp{
		regexp.MustCompile(`(?i)\bFROM\s+(\[?\w+\]?\.?\[?\w+\]?)`),
		regexp.MustCompile(`(?i)\bJOIN\s+(\[?\w+\]?\.?\[?\w+\]?)`),
		regexp.MustCompile(`(?i)\bINTO\s+(\[?\w+\]?\.?\[?\w+\]?)`),
		regexp.MustCompile(`(?i)\bUPDATE\s+(\[?\w+\]?\.?\[?\w+\]?)`),
	}

	for _, pat := range tablePatterns {
		for _, m := range pat.FindAllStringSubmatch(sql, -1) {
			if len(m) >= 2 {
				tableName := strings.Trim(m[1], "[]")
				if !isSQLKeyword(tableName) {
					refType := "reads_from"
					if strings.Contains(upper, "INSERT") || strings.Contains(upper, "UPDATE") || strings.Contains(upper, "DELETE") {
						refType = "writes_to"
					}
					refs = append(refs, parser.RawReference{
						ToName:        tableName,
						ReferenceType: refType,
						Line:          line,
					})
				}
			}
		}
	}

	// Extract EXEC calls
	execPat := regexp.MustCompile(`(?i)\bEXEC(?:UTE)?\s+(\[?\w+\]?\.?\[?\w+\]?)`)
	for _, m := range execPat.FindAllStringSubmatch(sql, -1) {
		if len(m) >= 2 {
			refs = append(refs, parser.RawReference{
				ToName:        strings.Trim(m[1], "[]"),
				ReferenceType: "calls",
				Line:          line,
			})
		}
	}

	return refs
}

func isSQLKeyword(s string) bool {
	kw := map[string]bool{
		"SELECT": true, "FROM": true, "WHERE": true, "SET": true,
		"VALUES": true, "INTO": true, "TABLE": true, "AS": true,
	}
	return kw[strings.ToUpper(s)]
}

// extractDirectives parses ASP.NET <%@ ... %> directive blocks.
func extractDirectives(content string) []parser.RawReference {
	var refs []parser.RawReference

	re := regexp.MustCompile(`(?i)<%@\s*(Page|Control|Master|Register|Import)\s+([^%]+?)%>`)
	for _, match := range re.FindAllStringSubmatch(content, -1) {
		if len(match) < 3 {
			continue
		}
		directive := strings.ToLower(match[1])
		attrs := match[2]
		line := strings.Count(content[:strings.Index(content, match[0])], "\n") + 1

		switch directive {
		case "page", "control", "master":
			// CodeBehind="Foo.aspx.cs" or CodeFile="Foo.aspx.cs"
			if cb := extractAttrValue(attrs, "CodeBehind"); cb != "" {
				refs = append(refs, parser.RawReference{
					ToName:        cb,
					ReferenceType: "imports",
					Line:          line,
				})
			}
			if cf := extractAttrValue(attrs, "CodeFile"); cf != "" {
				refs = append(refs, parser.RawReference{
					ToName:        cf,
					ReferenceType: "imports",
					Line:          line,
				})
			}
			// Inherits="MyApp.UsersPage"
			if inh := extractAttrValue(attrs, "Inherits"); inh != "" {
				refs = append(refs, parser.RawReference{
					ToName:        inh,
					ReferenceType: "inherits",
					Line:          line,
				})
			}

		case "import":
			// Namespace="System.Data"
			if ns := extractAttrValue(attrs, "Namespace"); ns != "" {
				refs = append(refs, parser.RawReference{
					ToName:        ns,
					ReferenceType: "imports",
					Line:          line,
				})
			}

		case "register":
			// Assembly="..." Namespace="..."
			if ns := extractAttrValue(attrs, "Namespace"); ns != "" {
				refs = append(refs, parser.RawReference{
					ToName:        ns,
					ReferenceType: "imports",
					Line:          line,
				})
			}
			if src := extractAttrValue(attrs, "Src"); src != "" {
				refs = append(refs, parser.RawReference{
					ToName:        src,
					ReferenceType: "imports",
					Line:          line,
				})
			}
		}
	}

	return refs
}

func extractAttrValue(attrs, name string) string {
	re := regexp.MustCompile(`(?i)` + name + `\s*=\s*"([^"]*)"`)
	if m := re.FindStringSubmatch(attrs); len(m) >= 2 {
		return m[1]
	}
	return ""
}
