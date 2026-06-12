package delphi

import (
	"regexp"
	"strings"

	"github.com/maraichr/lattice/internal/parser"
	"github.com/maraichr/lattice/internal/parser/sqlutil"
)

// DFMComponent represents a component in a DFM file.
type DFMComponent struct {
	Name        string
	ClassName   string
	Line        int
	SQL         []string // SQL text from query/command components
	Procs       []string // stored procedure names (StoredProcName, ProcedureName)
	Commands    []string // non-SQL CommandText values: proc or table names per CommandType
	CommandType string   // ADO CommandType (cmdStoredProc, cmdTable, …)
}

// ParseDFM parses a Delphi DFM (text format) file.
func ParseDFM(content string, baseOffset int) ([]parser.Symbol, []parser.RawReference) {
	var symbols []parser.Symbol
	var refs []parser.RawReference

	components := extractComponents(content)

	for _, comp := range components {
		sym := parser.Symbol{
			Name:          comp.Name,
			QualifiedName: comp.ClassName + "." + comp.Name,
			Kind:          "variable", // DFM components are instance variables
			Language:      "delphi",
			StartLine:     comp.Line + baseOffset,
			EndLine:       comp.Line + baseOffset,
			Signature:     comp.ClassName,
		}
		symbols = append(symbols, sym)

		for _, sql := range comp.SQL {
			tableRefs := sqlutil.ExtractTableRefs(sql, comp.Line+baseOffset, comp.Name, "dbo")
			for j := range tableRefs {
				tableRefs[j].Confidence = 0.9
			}
			refs = append(refs, tableRefs...)
		}
		for _, proc := range comp.Procs {
			if ref, ok := procCallRef(proc, comp.Name, comp.Line+baseOffset); ok {
				refs = append(refs, ref)
			}
		}
		for _, name := range comp.Commands {
			// cmdTable/cmdTableDirect mean CommandText holds a table name.
			if strings.Contains(strings.ToLower(comp.CommandType), "table") {
				ref := parser.RawReference{
					FromSymbol:    comp.Name,
					ToName:        name,
					ReferenceType: "uses_table",
					Confidence:    0.9,
					Line:          comp.Line + baseOffset,
				}
				if strings.Contains(name, ".") {
					ref.ToQualified = name
				} else {
					ref.ToQualified = "dbo." + name
				}
				refs = append(refs, ref)
			} else if ref, ok := procCallRef(name, comp.Name, comp.Line+baseOffset); ok {
				refs = append(refs, ref)
			}
		}
	}

	return symbols, refs
}

func extractComponents(content string) []DFMComponent {
	var components []DFMComponent

	// Match: object ComponentName: TClassName. Visual form inheritance writes
	// "inherited"/"inline" instead of "object".
	objectRe := regexp.MustCompile(`(?i)^(?:object|inherited|inline)\s+(\w+):\s*(\w+)`)
	// SQL-bearing string-list properties across the BDE/ADO/IBX/FireDAC
	// component families (TQuery.SQL, TIBQuery.SelectSQL, TUpdateSQL.ModifySQL, …).
	sqlListRe := regexp.MustCompile(`(?i)^(?:SQL|SelectSQL|InsertSQL|ModifySQL|DeleteSQL|RefreshSQL|UpdateSQL|CommandText)\.Strings\s*=\s*\(`)
	// TStoredProc.StoredProcName / TADOStoredProc.ProcedureName bind the
	// component to a stored procedure by name.
	procPropRe := regexp.MustCompile(`(?i)^(?:StoredProcName|ProcedureName)\s*=\s*'([^']+)'`)
	commandTextRe := regexp.MustCompile(`(?i)^CommandText\s*=(.*)`)
	commandTypeRe := regexp.MustCompile(`(?i)^CommandType\s*=\s*(\w+)`)

	lines := strings.Split(content, "\n")

	var current *DFMComponent
	inSQLList := false
	inCommandText := false
	var sqlBuilder strings.Builder

	flushCommandText := func() {
		inCommandText = false
		text := strings.TrimSpace(sqlBuilder.String())
		if current == nil || text == "" {
			return
		}
		if sqlutil.LooksLikeSQL(text) {
			current.SQL = append(current.SQL, text)
		} else {
			current.Commands = append(current.Commands, text)
		}
	}

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)

		if inSQLList {
			value, closed, continues := dfmLineValue(trimmed)
			sqlBuilder.WriteString(value)
			if !continues {
				// A line without a trailing '+' ends a list item.
				sqlBuilder.WriteString(" ")
			}
			if closed {
				inSQLList = false
				if current != nil {
					current.SQL = append(current.SQL, sqlBuilder.String())
				}
			}
			continue
		}

		if inCommandText {
			value, _, continues := dfmLineValue(trimmed)
			sqlBuilder.WriteString(value)
			if !continues {
				flushCommandText()
			}
			continue
		}

		if m := objectRe.FindStringSubmatch(trimmed); len(m) >= 3 {
			if current != nil {
				components = append(components, *current)
			}
			current = &DFMComponent{
				Name:      m[1],
				ClassName: m[2],
				Line:      i + 1,
			}
			continue
		}

		if trimmed == "end" && current != nil {
			components = append(components, *current)
			current = nil
			continue
		}

		if current == nil {
			continue
		}

		if sqlListRe.MatchString(trimmed) {
			inSQLList = true
			sqlBuilder.Reset()
			continue
		}

		if m := procPropRe.FindStringSubmatch(trimmed); len(m) >= 2 {
			current.Procs = append(current.Procs, m[1])
			continue
		}

		if m := commandTypeRe.FindStringSubmatch(trimmed); len(m) >= 2 {
			current.CommandType = m[1]
			continue
		}

		// CommandText = 'EXEC dbo.GetUser' — possibly continued with '+'
		// across lines, or with the value starting on the next line.
		if m := commandTextRe.FindStringSubmatch(trimmed); m != nil {
			rest := strings.TrimSpace(m[1])
			sqlBuilder.Reset()
			value, _, continues := dfmLineValue(rest)
			sqlBuilder.WriteString(value)
			if continues || rest == "" {
				inCommandText = true
			} else {
				flushCommandText()
			}
		}
	}

	if current != nil {
		components = append(components, *current)
	}

	return components
}

// dfmLineValue extracts the string content of a DFM property value line.
// Handles quoted fragments with '' escapes and #nn character codes (rendered
// as spaces). closed reports a ')' outside quotes — the end of a string-list
// property, which the DFM writer puts on the last item's line. continues
// reports a trailing '+', meaning the next line concatenates onto this value
// (with no separator: the DFM writer splits long strings mid-token).
func dfmLineValue(s string) (value string, closed, continues bool) {
	var b strings.Builder
	i := 0
	for i < len(s) {
		switch s[i] {
		case '\'':
			continues = false
			i++
			for i < len(s) {
				if s[i] == '\'' {
					if i+1 < len(s) && s[i+1] == '\'' {
						b.WriteByte('\'')
						i += 2
						continue
					}
					break
				}
				b.WriteByte(s[i])
				i++
			}
			i++ // closing quote
		case '#':
			i++
			for i < len(s) && s[i] >= '0' && s[i] <= '9' {
				i++
			}
			b.WriteByte(' ')
		case ')':
			closed = true
			i++
		case '+':
			continues = true
			i++
		default:
			i++
		}
	}
	return b.String(), closed, continues
}
