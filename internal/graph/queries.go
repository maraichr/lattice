package graph

// Cypher query constants for Neo4j operations.
const (
	// CreateConstraintSymbolID ensures Symbol(id) is unique and indexed (required for fast MERGE/MATCH).
	CreateConstraintSymbolID = `CREATE CONSTRAINT symbol_id IF NOT EXISTS FOR (s:Symbol) REQUIRE s.id IS UNIQUE`
	// CreateConstraintFileID ensures File(id) is unique and indexed (required for fast MERGE/MATCH).
	CreateConstraintFileID = `CREATE CONSTRAINT file_id IF NOT EXISTS FOR (f:File) REQUIRE f.id IS UNIQUE`

	// UpsertSymbolNode merges a symbol node by its ID and sets all properties.
	UpsertSymbolNode = `
UNWIND $symbols AS sym
MERGE (s:Symbol {id: sym.id})
SET s.name = sym.name,
    s.qualifiedName = sym.qualifiedName,
    s.kind = sym.kind,
    s.language = sym.language,
    s.projectId = sym.projectId,
    s.fileId = sym.fileId,
    s.startLine = sym.startLine,
    s.endLine = sym.endLine
`

	// UpsertEdge merges a relationship between source and target symbols.
	UpsertEdge = `
UNWIND $edges AS edge
MATCH (src:Symbol {id: edge.sourceId})
MATCH (tgt:Symbol {id: edge.targetId})
MERGE (src)-[r:DEPENDS_ON {edgeType: edge.edgeType}]->(tgt)
SET r.projectId = edge.projectId
`

	// UpsertFileNode merges a file node by its ID.
	UpsertFileNode = `
UNWIND $files AS f
MERGE (file:File {id: f.id})
SET file.path = f.path,
    file.language = f.language,
    file.projectId = f.projectId,
    file.sourceId = f.sourceId
`

	// LinkSymbolToFile creates DEFINED_IN relationships between symbols and files.
	LinkSymbolToFile = `
UNWIND $symbols AS sym
MATCH (s:Symbol {id: sym.id})
MATCH (f:File {id: sym.fileId})
MERGE (s)-[:DEFINED_IN]->(f)
`

	// DeleteProjectNodes removes all nodes and relationships for a project.
	DeleteProjectNodes = `
MATCH (n {projectId: $projectId})
DETACH DELETE n
`

	// DeleteSymbolNodesByID removes symbol nodes (and their relationships) by ID.
	DeleteSymbolNodesByID = `
UNWIND $ids AS id
MATCH (s:Symbol {id: id})
DETACH DELETE s
`

	// DeleteFileNodesByID removes file nodes (and their relationships) by ID.
	DeleteFileNodesByID = `
UNWIND $ids AS id
MATCH (f:File {id: id})
DETACH DELETE f
`

	// LineageUpstream finds all upstream dependencies of a symbol.
	LineageUpstream = `
MATCH path = (upstream)-[:DEPENDS_ON*1..%d]->(target:Symbol {id: $symbolId})
RETURN path
`

	// LineageDownstream finds all downstream dependents of a symbol.
	LineageDownstream = `
MATCH path = (source:Symbol {id: $symbolId})-[:DEPENDS_ON*1..%d]->(downstream)
RETURN path
`

	// LineageBoth finds both upstream and downstream connections.
	LineageBoth = `
MATCH path = (upstream)-[:DEPENDS_ON*1..%d]->(target:Symbol {id: $symbolId})
RETURN path
UNION
MATCH path = (source:Symbol {id: $symbolId})-[:DEPENDS_ON*1..%d]->(downstream)
RETURN path
`

	// UpsertColumnEdge creates COLUMN_FLOW relationships with metadata.
	UpsertColumnEdge = `
UNWIND $edges AS edge
MATCH (src:Symbol {id: edge.sourceId})
MATCH (tgt:Symbol {id: edge.targetId})
MERGE (src)-[r:COLUMN_FLOW {derivationType: edge.derivationType}]->(tgt)
SET r.projectId = edge.projectId,
    r.expression = edge.expression
`

	// ColumnLineageUpstream finds upstream column flows.
	ColumnLineageUpstream = `
MATCH path = (up)-[:COLUMN_FLOW*1..%d]->(target:Symbol {id: $symbolId})
RETURN path
`

	// ColumnLineageDownstream finds downstream column flows.
	ColumnLineageDownstream = `
MATCH path = (src:Symbol {id: $symbolId})-[:COLUMN_FLOW*1..%d]->(down)
RETURN path
`

	// ColumnLineageBoth finds both upstream and downstream column flows.
	ColumnLineageBoth = `
MATCH path = (up)-[:COLUMN_FLOW*1..%d]->(target:Symbol {id: $symbolId})
RETURN path
UNION
MATCH path = (src:Symbol {id: $symbolId})-[:COLUMN_FLOW*1..%d]->(down)
RETURN path
`
)
