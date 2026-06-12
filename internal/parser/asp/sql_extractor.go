package asp

import (
	"regexp"
	"sort"
	"strings"
)

// SQLFragment represents an extracted SQL string from ASP code.
type SQLFragment struct {
	SQL        string
	Line       int
	Confidence float64
}

// ProcCall represents a stored procedure invoked by name
// (e.g. cmd.CommandText = "GetUsers" with CommandType adCmdStoredProc).
type ProcCall struct {
	Name string
	Line int
}

var adoExecPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\.Execute\s*\(\s*"([^"]+)"\s*\)`),
	regexp.MustCompile(`(?i)\.Execute\s*\(\s*(.+?)\s*\)`),
	regexp.MustCompile(`(?i)\.Open\s+"([^"]+)"`),
	regexp.MustCompile(`(?i)\.Open\s+(.+?)[\s,]`),
	regexp.MustCompile(`(?i)\.CommandText\s*=\s*"([^"]+)"`),
	// ADO.NET patterns for C#/VB.NET code in <script runat="server"> blocks and .ashx handlers
	regexp.MustCompile(`(?i)new\s+(?:Sql|OleDb|Odbc)Command\s*\(\s*@?"([^"]+)"`),
	regexp.MustCompile(`(?i)new\s+(?:Sql|OleDb|Odbc)DataAdapter\s*\(\s*@?"([^"]+)"`),
}

// procCallPatterns capture strings that name a stored procedure rather than inline SQL.
var procCallPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\.CommandText\s*=\s*"([^"]+)"`),
	regexp.MustCompile(`(?i)new\s+(?:Sql|OleDb|Odbc)Command\s*\(\s*@?"([^"]+)"`),
}

// procNameRe matches a plain (optionally schema-qualified, optionally bracketed) identifier.
var procNameRe = regexp.MustCompile(`^\[?\w+\]?(?:\.\[?\w+\]?)?$`)

// ExtractSQL finds SQL strings from ASP/VBScript code regions.
func ExtractSQL(code string) []SQLFragment {
	var fragments []SQLFragment

	lines := strings.Split(code, "\n")

	// Look for ADO execution patterns
	for i, line := range lines {
		for _, pat := range adoExecPatterns {
			matches := pat.FindStringSubmatch(line)
			if len(matches) >= 2 {
				sql := cleanSQL(matches[1])
				if looksLikeSQL(sql) {
					fragments = append(fragments, SQLFragment{
						SQL:        sql,
						Line:       i + 1,
						Confidence: 0.9,
					})
				}
			}
		}
	}

	// Look for SQL built up in string variables (single assignments, multi-line
	// concatenation, and self-append patterns)
	fragments = append(fragments, extractConcatenatedSQL(lines)...)

	return fragments
}

// ExtractProcCalls finds stored procedures invoked by name, e.g.
// cmd.CommandText = "GetUsersByStatus" (typically with CommandType adCmdStoredProc)
// or new SqlCommand("GetUsersByStatus", conn).
func ExtractProcCalls(code string) []ProcCall {
	var calls []ProcCall
	lines := strings.Split(code, "\n")
	for i, line := range lines {
		for _, pat := range procCallPatterns {
			m := pat.FindStringSubmatch(line)
			if len(m) < 2 {
				continue
			}
			name := cleanSQL(m[1])
			if name == "" || looksLikeSQL(name) || !procNameRe.MatchString(name) {
				continue
			}
			calls = append(calls, ProcCall{Name: name, Line: i + 1})
		}
	}
	return calls
}

// sqlVarState accumulates SQL string fragments assigned to one variable.
type sqlVarState struct {
	parts     []string
	startLine int
	conf      float64
}

var (
	// varAppendRe matches "v = v & <expr>" (VBScript) or "v = v + <expr>" (C#/VB.NET)
	varAppendRe = regexp.MustCompile(`(?i)^(\w+)\s*=\s*(\w+)\s*[&+]\s*(.+)$`)
	// varOpEqRe matches "v &= <expr>" (VB.NET) or "v += <expr>" (C#)
	varOpEqRe = regexp.MustCompile(`(?i)^(\w+)\s*[&+]=\s*(.+)$`)
	// varAssignRe matches "v = <expr>" with an optional declaration keyword
	varAssignRe = regexp.MustCompile(`(?i)^(?:dim\s+|string\s+|var\s+)?(\w+)(?:\s+as\s+string)?\s*=\s*(.+)$`)
	// stringLitRe matches double-quoted string literals
	stringLitRe = regexp.MustCompile(`"([^"]*)"`)
)

// extractConcatenatedSQL tracks SQL built up in string variables across lines.
// It handles the patterns typical of classic ASP and WebForms code-behind:
//
//	sql = "SELECT * "                  ' single assignment
//	sql = sql & "FROM Users "          ' self-append
//	sql = "SELECT * " & _              ' VBScript line continuation
//	      "FROM Users"
//	string sql = "SELECT * " +         // C# concatenation
//	    "FROM Users";
//	sql += " WHERE Active = 1";        // C# append
func extractConcatenatedSQL(lines []string) []SQLFragment {
	var fragments []SQLFragment
	buffers := map[string]*sqlVarState{}

	flush := func(key string) {
		buf := buffers[key]
		if buf == nil {
			return
		}
		delete(buffers, key)
		sql := strings.Join(buf.parts, " ")
		if looksLikeSQL(sql) {
			fragments = append(fragments, SQLFragment{SQL: sql, Line: buf.startLine, Confidence: buf.conf})
		}
	}

	continuing := "" // variable whose assignment continues onto the next line

	for i, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "'") || strings.HasPrefix(line, "//") {
			continuing = ""
			continue
		}

		if continuing != "" {
			if buf := buffers[continuing]; buf != nil {
				buf.parts = append(buf.parts, stringLiterals(line)...)
			}
			if !hasLineContinuation(line) {
				continuing = ""
			}
			continue
		}

		// v = v & "..." / v = v + "..."
		if m := varAppendRe.FindStringSubmatch(line); m != nil && strings.EqualFold(m[1], m[2]) {
			key := strings.ToLower(m[1])
			if buf := buffers[key]; buf != nil {
				buf.parts = append(buf.parts, stringLiterals(m[3])...)
				if hasLineContinuation(line) {
					continuing = key
				}
			}
			continue
		}

		// v &= "..." / v += "..."
		if m := varOpEqRe.FindStringSubmatch(line); m != nil {
			key := strings.ToLower(m[1])
			if buf := buffers[key]; buf != nil {
				buf.parts = append(buf.parts, stringLiterals(m[2])...)
				if hasLineContinuation(line) {
					continuing = key
				}
			}
			continue
		}

		// v = "..."
		if m := varAssignRe.FindStringSubmatch(line); m != nil {
			key := strings.ToLower(m[1])
			rhs := m[2]
			// A previous SQL buffer for this variable ends here either way.
			flush(key)

			// Only treat plain string expressions as SQL sources; values built
			// from method calls are handled by the ADO execution patterns.
			quote := strings.IndexByte(rhs, '"')
			if quote < 0 || strings.ContainsAny(rhs[:quote], ".(") {
				continue
			}
			parts := stringLiterals(rhs)
			if len(parts) == 0 || !looksLikeSQL(parts[0]) {
				continue
			}
			conf := 0.6
			if sqlishVarName(key) {
				conf = 0.8
			}
			buffers[key] = &sqlVarState{parts: parts, startLine: i + 1, conf: conf}
			if hasLineContinuation(line) {
				continuing = key
			}
			continue
		}
	}

	// Flush remaining buffers in source order
	remaining := make([]string, 0, len(buffers))
	for key := range buffers {
		remaining = append(remaining, key)
	}
	sort.Slice(remaining, func(a, b int) bool {
		return buffers[remaining[a]].startLine < buffers[remaining[b]].startLine
	})
	for _, key := range remaining {
		flush(key)
	}

	return fragments
}

// hasLineContinuation reports whether a statement continues on the next line:
// VBScript "& _" continuations or C# expressions ending in a concat operator.
func hasLineContinuation(line string) bool {
	return strings.HasSuffix(line, "_") || strings.HasSuffix(line, "&") || strings.HasSuffix(line, "+")
}

// sqlishVarName reports whether a variable name suggests it holds SQL.
func sqlishVarName(name string) bool {
	for _, hint := range []string{"sql", "query", "qry", "cmd"} {
		if strings.Contains(name, hint) {
			return true
		}
	}
	return false
}

// stringLiterals extracts the contents of all double-quoted strings in an expression.
func stringLiterals(expr string) []string {
	var parts []string
	for _, m := range stringLitRe.FindAllStringSubmatch(expr, -1) {
		if m[1] != "" {
			parts = append(parts, m[1])
		}
	}
	return parts
}

func cleanSQL(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, `"`)
	s = strings.ReplaceAll(s, `""`, `"`)
	return s
}

func looksLikeSQL(s string) bool {
	upper := strings.ToUpper(strings.TrimSpace(s))
	sqlKeywords := []string{"SELECT", "INSERT", "UPDATE", "DELETE", "CREATE", "ALTER", "DROP", "EXEC", "EXECUTE"}
	for _, kw := range sqlKeywords {
		if strings.HasPrefix(upper, kw) {
			return true
		}
	}
	return false
}
