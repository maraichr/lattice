package resolver

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"

	"github.com/maraichr/lattice/internal/parser"
	"github.com/maraichr/lattice/internal/store"
	"github.com/maraichr/lattice/internal/store/postgres"
)

const (
	// symbolPageSize controls how many symbols are loaded per DB page during resolution.
	// Smaller values reduce memory pressure; larger values reduce round-trips.
	symbolPageSize int64 = 10_000
)

// Engine performs cross-file symbol resolution within a project.
type Engine struct {
	store     *store.Store
	crossLang *CrossLangResolver
	logger    *slog.Logger
}

func NewEngine(s *store.Store, logger *slog.Logger) *Engine {
	return &Engine{
		store:     s,
		crossLang: NewCrossLangResolver(logger),
		logger:    logger,
	}
}

// ResolveProject performs cross-file symbol resolution for all unresolved references
// in a project by reading raw references from the staging table (persisted during
// the parse/persist phase) and resolving them against the project's symbol table.
//
// This is the primary entry point used by the distributed pipeline. All parse workers
// have already persisted symbols and raw references before this runs. The resolver:
//  1. Loads raw references from the staging table for this index run.
//  2. Builds a partial symbol table from the referenced target names.
//  3. For each raw reference, resolves source and target, creating SymbolEdge records.
//  4. Cleans up the staging table after resolution.
//
// Returns the number of new edges created.
func (e *Engine) ResolveProject(ctx context.Context, projectID, indexRunID uuid.UUID) (int, error) {
	// Load raw references persisted by parse workers.
	rawRefs, err := e.store.ListRawReferencesByIndexRun(ctx, indexRunID)
	if err != nil {
		return 0, fmt.Errorf("load raw references: %w", err)
	}
	if len(rawRefs) == 0 {
		e.logger.Info("no raw references to resolve")
		return 0, nil
	}

	// Collect target names from raw references.
	var fqnTargets []string
	var shortTargets []string
	fileIDSet := make(map[uuid.UUID]struct{})
	for _, rr := range rawRefs {
		if rr.ToQualified != "" {
			fqnTargets = append(fqnTargets, rr.ToQualified)
		}
		if rr.ToName != "" {
			shortTargets = append(shortTargets, rr.ToName)
		}
		fileIDSet[rr.FileID] = struct{}{}
	}
	fqnTargets = deduplicateStrings(fqnTargets)
	shortTargets = deduplicateStrings(shortTargets)

	// Build the partial symbol table via targeted SQL lookups.
	table, err := e.buildPartialTable(ctx, projectID, fqnTargets, shortTargets)
	if err != nil {
		return 0, fmt.Errorf("build partial symbol table: %w", err)
	}

	// Load all endpoint symbols so the api_route_match strategy can compare signatures.
	if e.crossLang != nil {
		endpoints, epErr := e.store.ListEndpointSymbolsByProject(ctx, projectID)
		if epErr != nil {
			e.logger.Warn("failed to load endpoint symbols for route matching", slog.String("err", epErr.Error()))
		} else {
			for _, ep := range endpoints {
				if ep.Signature != "" {
					table.BySignature[ep.Signature] = ep.ID
					table.ByLang[ep.Signature] = ep.Language
				}
			}
		}
	}

	// Load local symbols for all files that have raw references.
	fileIDs := make([]uuid.UUID, 0, len(fileIDSet))
	for fid := range fileIDSet {
		fileIDs = append(fileIDs, fid)
	}
	localSymbols, err := e.loadFileSymbols(ctx, fileIDs)
	if err != nil {
		return 0, fmt.Errorf("load file symbols: %w", err)
	}

	// Load file languages.
	files, err := e.store.ListFilesByProject(ctx, projectID)
	if err != nil {
		return 0, fmt.Errorf("load files: %w", err)
	}
	fileByID := make(map[uuid.UUID]string, len(files))
	for _, f := range files {
		fileByID[f.ID] = f.Language
	}

	// Resolve each raw reference.
	created := 0
	for _, rr := range rawRefs {
		localScope := localSymbols[rr.FileID]
		fileLang := rr.Language
		if fileLang == "" {
			fileLang = fileByID[rr.FileID]
		}

		ref := parser.RawReference{
			FromSymbol:    rr.FromSymbol,
			ToName:        rr.ToName,
			ToQualified:   rr.ToQualified,
			ReferenceType: rr.ReferenceType,
			Confidence:    rr.Confidence,
		}
		if rr.Line != nil {
			ref.Line = int(*rr.Line)
		}

		// Resolve source symbol.
		sourceID, ok := localScope[ref.FromSymbol]
		if !ok {
			sourceID, ok = table.ByFQN[ref.FromSymbol]
		}
		if !ok && ref.FromSymbol == "" && ref.ToName != "" && ref.ReferenceType == "uses_table" {
			sourceID = inferSourceFromFileSymbols(rr.FileID, localScope)
		}
		if sourceID == uuid.Nil {
			continue
		}

		result := resolveTarget(ref, localScope, table, e.crossLang, fileLang)
		if !result.Resolved || sourceID == result.TargetID {
			continue
		}

		confidence := result.Confidence
		if ref.Confidence > 0 && confidence > 0 {
			confidence = ref.Confidence * confidence
		} else if ref.Confidence > 0 {
			confidence = ref.Confidence
		}

		if result.CrossLang {
			meta := map[string]interface{}{
				"confidence":     confidence,
				"match_strategy": result.Strategy,
				"bridge":         result.Bridge,
			}
			metaJSON, _ := json.Marshal(meta)
			_, err := e.store.CreateSymbolEdgeWithMetadata(ctx, postgres.CreateSymbolEdgeWithMetadataParams{
				ProjectID: projectID,
				SourceID:  sourceID,
				TargetID:  result.TargetID,
				EdgeType:  ref.ReferenceType,
				Metadata:  metaJSON,
			})
			if err != nil {
				continue
			}
		} else {
			_, err := e.store.CreateSymbolEdge(ctx, postgres.CreateSymbolEdgeParams{
				ProjectID: projectID,
				SourceID:  sourceID,
				TargetID:  result.TargetID,
				EdgeType:  ref.ReferenceType,
			})
			if err != nil {
				continue
			}
		}
		created++
	}

	// Clean up staging data.
	_ = e.store.DeleteRawReferencesByIndexRun(ctx, indexRunID)

	e.logger.Info("cross-file resolution complete",
		slog.Int("edges_created", created),
		slog.Int("raw_refs", len(rawRefs)))

	return created, nil
}

// Resolve performs cross-file symbol resolution given explicit parse results.
// This method is kept for backwards compatibility with tests and any callers
// that still have parse results in memory. New code should prefer ResolveProject.
func (e *Engine) Resolve(ctx context.Context, projectID uuid.UUID, parseResults []parser.FileResult) (int, error) {
	if len(parseResults) == 0 {
		return 0, nil
	}

	// ------------------------------------------------------------------
	// Step 1: Collect target names from parse results.
	// ------------------------------------------------------------------
	var fqnTargets []string
	var shortTargets []string
	for _, fr := range parseResults {
		for _, ref := range fr.References {
			if ref.ToQualified != "" {
				fqnTargets = append(fqnTargets, ref.ToQualified)
			}
			if ref.ToName != "" {
				shortTargets = append(shortTargets, ref.ToName)
			}
		}
	}
	fqnTargets = deduplicateStrings(fqnTargets)
	shortTargets = deduplicateStrings(shortTargets)

	// ------------------------------------------------------------------
	// Step 2: Targeted DB lookups.
	// ------------------------------------------------------------------
	table, err := e.buildPartialTable(ctx, projectID, fqnTargets, shortTargets)
	if err != nil {
		return 0, fmt.Errorf("build partial symbol table: %w", err)
	}

	// Populate endpoint signatures from parse results for api_route_match.
	for _, fr := range parseResults {
		for _, sym := range fr.Symbols {
			if sym.Kind == "endpoint" && sym.Signature != "" {
				if id, ok := table.ByFQN[sym.QualifiedName]; ok {
					table.BySignature[sym.Signature] = id
					table.ByLang[sym.Signature] = fr.Language
				}
			}
		}
	}

	files, err := e.store.ListFilesByProject(ctx, projectID)
	if err != nil {
		return 0, fmt.Errorf("load files: %w", err)
	}
	fileByPath := make(map[string]uuid.UUID, len(files))
	for _, f := range files {
		fileByPath[f.Path] = f.ID
	}

	fileIDs := make([]uuid.UUID, 0, len(parseResults))
	for _, fr := range parseResults {
		if fid, ok := fileByPath[fr.Path]; ok {
			fileIDs = append(fileIDs, fid)
		}
	}
	localSymbols, err := e.loadFileSymbols(ctx, fileIDs)
	if err != nil {
		return 0, fmt.Errorf("load local symbols: %w", err)
	}

	// ------------------------------------------------------------------
	// Step 3: Resolve.
	// ------------------------------------------------------------------
	created := 0

	for _, fr := range parseResults {
		fileID, ok := fileByPath[fr.Path]
		if !ok {
			continue
		}
		localScope := localSymbols[fileID]

		for _, ref := range fr.References {
			sourceID, ok := localScope[ref.FromSymbol]
			if !ok {
				sourceID, ok = table.ByFQN[ref.FromSymbol]
			}
			if !ok && ref.FromSymbol == "" && ref.ToName != "" && ref.ReferenceType == "uses_table" {
				sourceID = inferSourceFromFileSymbols(fileID, localScope)
			}
			if sourceID == uuid.Nil {
				continue
			}

			result := resolveTarget(ref, localScope, table, e.crossLang, fr.Language)
			if !result.Resolved || sourceID == result.TargetID {
				continue
			}

			confidence := result.Confidence
			if ref.Confidence > 0 && confidence > 0 {
				confidence = ref.Confidence * confidence
			} else if ref.Confidence > 0 {
				confidence = ref.Confidence
			}

			if result.CrossLang {
				meta := map[string]interface{}{
					"confidence":     confidence,
					"match_strategy": result.Strategy,
					"bridge":         result.Bridge,
				}
				metaJSON, _ := json.Marshal(meta)
				_, err := e.store.CreateSymbolEdgeWithMetadata(ctx, postgres.CreateSymbolEdgeWithMetadataParams{
					ProjectID: projectID,
					SourceID:  sourceID,
					TargetID:  result.TargetID,
					EdgeType:  ref.ReferenceType,
					Metadata:  metaJSON,
				})
				if err != nil {
					continue
				}
			} else {
				_, err := e.store.CreateSymbolEdge(ctx, postgres.CreateSymbolEdgeParams{
					ProjectID: projectID,
					SourceID:  sourceID,
					TargetID:  result.TargetID,
					EdgeType:  ref.ReferenceType,
				})
				if err != nil {
					continue
				}
			}
			created++
		}
	}

	e.logger.Info("cross-file resolution complete",
		slog.Int("edges_created", created),
		slog.Int("fqn_targets", len(fqnTargets)),
		slog.Int("short_targets", len(shortTargets)))

	return created, nil
}

// symbolCandidate holds a symbol ID along with its kind for disambiguation.
type symbolCandidate struct {
	ID       uuid.UUID
	Kind     string
	Language string
}

// partialSymbolTable is a lightweight lookup structure populated from targeted DB queries.
// Unlike the old SymbolTable it holds only symbols that are actually referenced, not
// the full project corpus.
type partialSymbolTable struct {
	ByFQN           map[string]uuid.UUID          // qualified_name → symbol ID
	ByShortName     map[string][]uuid.UUID         // short name → candidate IDs (may have multiple)
	ByShortNameFull map[string][]symbolCandidate   // short name → candidates with kind info
	ByLang          map[string]string              // qualified_name → language
	BySignature     map[string]uuid.UUID           // endpoint Signature → symbol ID (api_route_match)

	// Case-insensitive indexes, built once after population. These replace the
	// per-reference O(symbols) linear scans of ByFQN that case-insensitive,
	// schema-qualified, and cross-language fallbacks would otherwise perform — the
	// difference between O(refs) and O(refs × symbols) on large projects.
	lowerShort map[string][]symbolCandidate // lowercased short name → candidates
	lowerFQN   map[string]uuid.UUID         // lowercased qualified_name → symbol ID
}

func newPartialTable() *partialSymbolTable {
	return &partialSymbolTable{
		ByFQN:           make(map[string]uuid.UUID),
		ByShortName:     make(map[string][]uuid.UUID),
		ByShortNameFull: make(map[string][]symbolCandidate),
		ByLang:          make(map[string]string),
		BySignature:     make(map[string]uuid.UUID),
		lowerShort:      make(map[string][]symbolCandidate),
		lowerFQN:        make(map[string]uuid.UUID),
	}
}

// buildLowerIndexes populates the case-insensitive lookup maps from the exact
// maps. Called once after buildPartialTable finishes loading symbols.
func (t *partialSymbolTable) buildLowerIndexes() {
	for fqn, id := range t.ByFQN {
		t.lowerFQN[strings.ToLower(fqn)] = id
	}
	for short, cands := range t.ByShortNameFull {
		l := strings.ToLower(short)
		t.lowerShort[l] = append(t.lowerShort[l], cands...)
	}
}

// lowerShortCandidates and lowerFQNID implement the fastLookup interface.
func (t *partialSymbolTable) lowerShortCandidates(lower string) []symbolCandidate {
	return t.lowerShort[lower]
}

func (t *partialSymbolTable) lowerFQNID(lower string) (uuid.UUID, bool) {
	id, ok := t.lowerFQN[lower]
	return id, ok
}

// fastLookup is implemented by symbol tables that maintain case-insensitive
// indexes. Resolution helpers use it when available and fall back to a linear
// scan otherwise (e.g. the small SymbolTable used in tests).
type fastLookup interface {
	lowerShortCandidates(lower string) []symbolCandidate
	lowerFQNID(lower string) (uuid.UUID, bool)
}

// lookupShortNameCI returns the first symbol whose short name equals name
// (case-insensitively) and whose language is compatible with targetLang (empty
// targetLang matches any). It uses the fast index when present, else scans.
func lookupShortNameCI(table SymbolLookup, name, targetLang string) (uuid.UUID, bool) {
	lower := strings.ToLower(name)
	if fast, ok := table.(fastLookup); ok {
		for _, c := range fast.lowerShortCandidates(lower) {
			if targetLang == "" || c.Language == "" || matchesLanguage(c.Language, targetLang) {
				return c.ID, true
			}
		}
		return uuid.Nil, false
	}
	for fqn, id := range table.ByFQNMap() {
		if strings.ToLower(shortNameOf(fqn)) == lower {
			if targetLang == "" || matchesLanguage(table.LangOf(fqn), targetLang) {
				return id, true
			}
		}
	}
	return uuid.Nil, false
}

// lookupFQNCI returns the symbol whose qualified name equals fqn
// (case-insensitively). Uses the fast index when present, else scans.
func lookupFQNCI(table SymbolLookup, fqn string) (uuid.UUID, bool) {
	lower := strings.ToLower(fqn)
	if fast, ok := table.(fastLookup); ok {
		return fast.lowerFQNID(lower)
	}
	for cand, id := range table.ByFQNMap() {
		if strings.ToLower(cand) == lower {
			return id, true
		}
	}
	return uuid.Nil, false
}

// buildPartialTable executes targeted batch SQL queries to load only symbols that
// are referenced by the parse results.
func (e *Engine) buildPartialTable(ctx context.Context, projectID uuid.UUID, fqns, shortNames []string) (*partialSymbolTable, error) {
	table := newPartialTable()

	// Batch FQN lookup.
	if len(fqns) > 0 {
		rows, err := e.store.ListSymbolsByQualifiedNames(ctx, projectID, fqns)
		if err != nil {
			return nil, fmt.Errorf("list by fqn: %w", err)
		}
		for _, sym := range rows {
			table.ByFQN[sym.QualifiedName] = sym.ID
			short := shortNameOf(sym.QualifiedName)
			table.ByShortName[short] = appendUnique(table.ByShortName[short], sym.ID)
			table.ByShortNameFull[short] = appendUniqueCand(table.ByShortNameFull[short], symbolCandidate{ID: sym.ID, Kind: sym.Kind, Language: sym.Language})
			table.ByLang[sym.QualifiedName] = sym.Language
		}
	}

	// Batch short-name lookup for names not already resolved by FQN.
	unresolvedShort := make([]string, 0, len(shortNames))
	for _, name := range shortNames {
		if _, ok := table.ByShortName[name]; !ok {
			unresolvedShort = append(unresolvedShort, name)
		}
	}
	if len(unresolvedShort) > 0 {
		rows, err := e.store.ListSymbolsByNames(ctx, postgres.ListSymbolsByNamesParams{
			ProjectID: projectID,
			Column2:   unresolvedShort,
		})
		if err != nil {
			return nil, fmt.Errorf("list by short name: %w", err)
		}
		for _, sym := range rows {
			if _, ok := table.ByFQN[sym.QualifiedName]; !ok {
				table.ByFQN[sym.QualifiedName] = sym.ID
			}
			short := shortNameOf(sym.QualifiedName)
			table.ByShortName[short] = appendUnique(table.ByShortName[short], sym.ID)
			table.ByShortNameFull[short] = appendUniqueCand(table.ByShortNameFull[short], symbolCandidate{ID: sym.ID, Kind: sym.Kind, Language: sym.Language})
			table.ByLang[sym.QualifiedName] = sym.Language
		}
	}

	table.buildLowerIndexes()
	return table, nil
}

// loadFileSymbols loads symbols for a specific set of file IDs, grouped by file ID.
// This is used to build the local scope for source-side resolution.
func (e *Engine) loadFileSymbols(ctx context.Context, fileIDs []uuid.UUID) (map[uuid.UUID]map[string]uuid.UUID, error) {
	result := make(map[uuid.UUID]map[string]uuid.UUID, len(fileIDs))
	if len(fileIDs) == 0 {
		return result, nil
	}

	symbols, err := e.store.ListSymbolsByFileIDs(ctx, fileIDs)
	if err != nil {
		return nil, fmt.Errorf("list symbols by file IDs: %w", err)
	}

	for _, sym := range symbols {
		if result[sym.FileID] == nil {
			result[sym.FileID] = make(map[string]uuid.UUID)
		}
		result[sym.FileID][sym.QualifiedName] = sym.ID
		result[sym.FileID][sym.Name] = sym.ID
	}
	return result, nil
}

// resolveResult holds the outcome of target resolution.
type resolveResult struct {
	TargetID   uuid.UUID
	Confidence float64
	Strategy   string
	Bridge     string
	CrossLang  bool
	Resolved   bool
}

// symbolIndex is the internal interface used by resolveTarget. Both *partialSymbolTable
// (production) and *SymbolTable (tests) implement it.
type symbolIndex interface {
	SymbolLookup
	shortNameCandidates(name string) []uuid.UUID
}

// resolveTarget attempts to find the target symbol for a reference.
// Resolution order: qualified name → file-local scope → project-wide short name → case-insensitive → cross-language.
func resolveTarget(ref parser.RawReference, localScope map[string]uuid.UUID, table symbolIndex, crossLang *CrossLangResolver, sourceLang string) resolveResult {
	byFQN := table.ByFQNMap()

	// 1. Try fully qualified name.
	if ref.ToQualified != "" {
		if id, ok := byFQN[ref.ToQualified]; ok {
			return resolveResult{TargetID: id, Confidence: 1.0, Resolved: true}
		}
	}

	// 2. Try the target name in local scope.
	if id, ok := localScope[ref.ToName]; ok {
		return resolveResult{TargetID: id, Confidence: 1.0, Resolved: true}
	}
	if ref.ToQualified != "" {
		if id, ok := localScope[ref.ToQualified]; ok {
			return resolveResult{TargetID: id, Confidence: 1.0, Resolved: true}
		}
	}

	// 3. Try project-wide by short name.
	candidates := table.shortNameCandidates(ref.ToName)
	if len(candidates) == 1 {
		return resolveResult{TargetID: candidates[0], Confidence: 1.0, Resolved: true}
	}
	// When multiple candidates exist, use reference type to prefer the right kind.
	if len(candidates) > 1 {
		if preferred := disambiguateByKind(ref, table); preferred != uuid.Nil {
			return resolveResult{TargetID: preferred, Confidence: 0.9, Resolved: true}
		}
	}

	// 4. Try case-insensitive short-name match (SQL is often case-insensitive).
	if id, ok := lookupShortNameCI(table, ref.ToName, ""); ok {
		return resolveResult{TargetID: id, Confidence: 1.0, Resolved: true}
	}

	// 5. Try cross-language resolution.
	if crossLang != nil && sourceLang != "" {
		if match, ok := crossLang.Resolve(ref, sourceLang, table); ok {
			return resolveResult{
				TargetID:   match.TargetID,
				Confidence: match.Confidence,
				Strategy:   match.Strategy,
				Bridge:     match.Bridge,
				CrossLang:  true,
				Resolved:   true,
			}
		}
	}

	return resolveResult{}
}

// shortNameOf extracts the short name from a qualified name.
// e.g., "dbo.Customers" → "Customers", "schema.proc" → "proc"
func shortNameOf(qualifiedName string) string {
	parts := strings.Split(qualifiedName, ".")
	return parts[len(parts)-1]
}

// inferSourceFromFileSymbols returns one symbol ID from the file's local scope when
// refs have no FromSymbol (e.g. C# uses_table via [Table("X")]).
func inferSourceFromFileSymbols(fileID uuid.UUID, localScope map[string]uuid.UUID) uuid.UUID {
	for _, id := range localScope {
		return id
	}
	return uuid.Nil
}

// deduplicateStrings returns a deduplicated copy of ss preserving order.
func deduplicateStrings(ss []string) []string {
	seen := make(map[string]struct{}, len(ss))
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		if _, ok := seen[s]; !ok {
			seen[s] = struct{}{}
			out = append(out, s)
		}
	}
	return out
}

// appendUnique appends id to ids only if not already present.
func appendUnique(ids []uuid.UUID, id uuid.UUID) []uuid.UUID {
	for _, existing := range ids {
		if existing == id {
			return ids
		}
	}
	return append(ids, id)
}

// appendUniqueCand appends a candidate only if its ID is not already present.
func appendUniqueCand(cands []symbolCandidate, c symbolCandidate) []symbolCandidate {
	for _, existing := range cands {
		if existing.ID == c.ID {
			return cands
		}
	}
	return append(cands, c)
}

// disambiguateByKind tries to pick the best candidate when multiple short-name
// matches exist, using the reference type to infer the expected symbol kind.
func disambiguateByKind(ref parser.RawReference, table symbolIndex) uuid.UUID {
	pt, ok := table.(interface {
		candidatesFull(name string) []symbolCandidate
	})
	if !ok {
		return uuid.Nil
	}

	cands := pt.candidatesFull(ref.ToName)
	if len(cands) <= 1 {
		return uuid.Nil
	}

	preferredKinds := refTypeToPreferredKinds(ref.ReferenceType)
	if len(preferredKinds) == 0 {
		return uuid.Nil
	}

	var filtered []symbolCandidate
	for _, c := range cands {
		for _, k := range preferredKinds {
			if c.Kind == k {
				filtered = append(filtered, c)
				break
			}
		}
	}

	if len(filtered) == 1 {
		return filtered[0].ID
	}
	return uuid.Nil
}

// refTypeToPreferredKinds maps reference types to the symbol kinds they typically target.
func refTypeToPreferredKinds(refType string) []string {
	switch refType {
	case "inherits":
		return []string{"class"}
	case "implements":
		return []string{"interface"}
	case "uses_table", "reads_from", "writes_to":
		return []string{"table", "view"}
	case "calls":
		return []string{"procedure", "function", "method"}
	case "references":
		return []string{"class", "interface", "enum", "type"}
	case "joins":
		return []string{"table", "view"}
	default:
		return nil
	}
}

// ByFQNMap implements SymbolLookup for partialSymbolTable.
func (t *partialSymbolTable) ByFQNMap() map[string]uuid.UUID { return t.ByFQN }

// LangOf implements SymbolLookup for partialSymbolTable.
func (t *partialSymbolTable) LangOf(fqn string) string { return t.ByLang[fqn] }

// EndpointsBySignature implements SymbolLookup for partialSymbolTable.
func (t *partialSymbolTable) EndpointsBySignature() map[string]uuid.UUID { return t.BySignature }

// shortNameCandidates implements symbolIndex for partialSymbolTable.
func (t *partialSymbolTable) shortNameCandidates(name string) []uuid.UUID {
	return t.ByShortName[name]
}

// candidatesFull returns candidates with kind info for disambiguation.
func (t *partialSymbolTable) candidatesFull(name string) []symbolCandidate {
	return t.ByShortNameFull[name]
}
