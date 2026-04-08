package parser

import (
	"regexp"
	"strings"
)

// backtickRe matches backtick-quoted identifiers (MySQL style).
var backtickRe = regexp.MustCompile("`\\w+`")

// DetectDialect determines whether a SQL file is T-SQL, PostgreSQL, or MySQL based on
// content analysis and file extension hints. The extension (e.g. ".sql",
// ".sqldataprovider") is used as a tiebreaker when content alone is ambiguous.
func DetectDialect(content []byte, ext string) string {
	text := strings.ToUpper(string(content))

	tsqlScore := 0
	pgsqlScore := 0
	mysqlScore := 0

	// T-SQL indicators
	if strings.Contains(text, "\nGO\n") || strings.Contains(text, "\nGO\r\n") || strings.HasSuffix(text, "\nGO") {
		tsqlScore += 10 // GO batch separator is definitive
	}
	for _, kw := range []string{"DECLARE @", "SET @", "NVARCHAR", "VARCHAR(MAX)", "BIT", "IDENTITY(",
		"EXEC ", "EXECUTE ", "SP_", "NOCOUNT", "BEGIN TRY", "BEGIN CATCH",
		"@@ROWCOUNT", "@@ERROR", "@@IDENTITY", "GETDATE()", "ISNULL(",
		"CHARINDEX(", "TOP ", "WITH (NOLOCK)", "CROSS APPLY", "OUTER APPLY"} {
		if strings.Contains(text, kw) {
			tsqlScore += 2
		}
	}

	// PostgreSQL indicators
	for _, kw := range []string{"$$", "LANGUAGE PLPGSQL", "LANGUAGE SQL", "RETURNS SETOF",
		"RETURNS TABLE", "CREATE EXTENSION", "CREATE SCHEMA", "SERIAL", "BIGSERIAL",
		"BOOLEAN", "TEXT NOT NULL", "TIMESTAMPTZ", "UUID", "JSONB",
		"::TEXT", "::INTEGER", "::UUID", "ILIKE", "SIMILAR TO",
		"CREATE OR REPLACE FUNCTION", "RAISE NOTICE", "RAISE EXCEPTION",
		"PERFORM ", "IMMUTABLE", "STABLE", "VOLATILE"} {
		if strings.Contains(text, kw) {
			pgsqlScore += 2
		}
	}

	// MySQL indicators
	if strings.Contains(text, "\nDELIMITER ") || strings.HasPrefix(text, "DELIMITER ") {
		mysqlScore += 10 // DELIMITER is definitive for MySQL
	}
	for _, kw := range []string{"AUTO_INCREMENT", "ENGINE=", "TINYINT", "MEDIUMINT",
		"UNSIGNED", "ENUM(", "SET(", "SHOW TABLES", "SHOW COLUMNS",
		"DEFINER=", "DEFAULT CHARSET", "COLLATE UTF8", "ON DUPLICATE KEY",
		"IFNULL(", "MEDIUMTEXT", "LONGTEXT", "TINYBLOB", "MEDIUMBLOB", "LONGBLOB"} {
		if strings.Contains(text, kw) {
			mysqlScore += 2
		}
	}
	// Backtick-quoted identifiers are MySQL-specific
	if backtickRe.Match(content) {
		mysqlScore += 2
	}

	// Three-way max
	if tsqlScore >= pgsqlScore && tsqlScore >= mysqlScore {
		if tsqlScore > 0 {
			return "tsql"
		}
	}
	if pgsqlScore >= tsqlScore && pgsqlScore >= mysqlScore {
		if pgsqlScore > 0 {
			return "pgsql"
		}
	}
	if mysqlScore > tsqlScore && mysqlScore > pgsqlScore {
		return "mysql"
	}

	// Tie: use file extension as a hint. Non-.sql extensions registered with
	// the SQL router (e.g. ".sqldataprovider") are more likely T-SQL since
	// PostgreSQL ecosystems rarely use custom SQL extensions.
	if ext != "" && ext != ".sql" {
		return "tsql"
	}

	// Both scores zero or tied with a plain .sql file: default to tsql.
	// PostgreSQL files almost always contain strong markers ($$, LANGUAGE
	// PLPGSQL, ::casts), so a file with no markers is more likely T-SQL.
	return "tsql"
}
