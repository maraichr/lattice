package parser

// SQLRouter routes .sql files to the appropriate dialect parser based on FileInput.Language.
type SQLRouter struct {
	tsql  Parser
	pgsql Parser
	mysql Parser
}

func NewSQLRouter(tsql, pgsql, mysql Parser) *SQLRouter {
	return &SQLRouter{tsql: tsql, pgsql: pgsql, mysql: mysql}
}

func (r *SQLRouter) Parse(input FileInput) (*ParseResult, error) {
	switch input.Language {
	case "mysql":
		return r.mysql.Parse(input)
	case "pgsql":
		return r.pgsql.Parse(input)
	default:
		return r.tsql.Parse(input)
	}
}

func (r *SQLRouter) Languages() []string {
	return []string{"tsql", "pgsql", "mysql", "sql"}
}
