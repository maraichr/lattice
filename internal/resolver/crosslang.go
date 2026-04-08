package resolver

import (
	"log/slog"
	"strings"

	"github.com/google/uuid"

	"github.com/maraichr/lattice/internal/parser"
)

// BridgeRule defines how to resolve references between different languages.
type BridgeRule struct {
	SourceLanguage string // e.g., "delphi", "asp", "java"
	TargetLanguage string // e.g., "tsql", "pgsql"
	MatchStrategy  string // exact, case_insensitive, schema_qualified, strip_prefix
}

// BridgeMatch represents a successful cross-language resolution with confidence.
type BridgeMatch struct {
	TargetID   uuid.UUID
	Confidence float64 // exact=1.0, schema_qualified=0.95, case_insensitive=0.85, strip_prefix=0.75, orm_convention=0.7
	Strategy   string
	Bridge     string // e.g., "csharp→tsql"
}

// CrossLangResolver resolves references across language boundaries.
type CrossLangResolver struct {
	rules  []BridgeRule
	logger *slog.Logger
}

// NewCrossLangResolver creates a new cross-language resolver.
func NewCrossLangResolver(logger *slog.Logger) *CrossLangResolver {
	c := &CrossLangResolver{logger: logger}
	c.RegisterDefaultRules()
	return c
}

// RegisterDefaultRules sets up the default cross-language bridge rules.
func (c *CrossLangResolver) RegisterDefaultRules() {
	c.rules = []BridgeRule{
		// App → SQL: Delphi/ASP/Java/C# referencing SQL objects
		{SourceLanguage: "delphi", TargetLanguage: "tsql", MatchStrategy: "schema_qualified"},
		{SourceLanguage: "asp", TargetLanguage: "tsql", MatchStrategy: "case_insensitive"},
		{SourceLanguage: "java", TargetLanguage: "pgsql", MatchStrategy: "case_insensitive"},
		{SourceLanguage: "java", TargetLanguage: "tsql", MatchStrategy: "case_insensitive"},
		{SourceLanguage: "csharp", TargetLanguage: "tsql", MatchStrategy: "schema_qualified"},
		{SourceLanguage: "csharp", TargetLanguage: "tsql", MatchStrategy: "case_insensitive"},
		{SourceLanguage: "javascript", TargetLanguage: "tsql", MatchStrategy: "case_insensitive"},
		{SourceLanguage: "typescript", TargetLanguage: "tsql", MatchStrategy: "case_insensitive"},

		// JS/TS → PostgreSQL (common with Node.js stacks)
		{SourceLanguage: "javascript", TargetLanguage: "pgsql", MatchStrategy: "case_insensitive"},
		{SourceLanguage: "typescript", TargetLanguage: "pgsql", MatchStrategy: "case_insensitive"},

		// C# → PostgreSQL
		{SourceLanguage: "csharp", TargetLanguage: "pgsql", MatchStrategy: "schema_qualified"},

		// ORM convention matching (pluralize/singularize)
		{SourceLanguage: "csharp", TargetLanguage: "tsql", MatchStrategy: "orm_convention"},
		{SourceLanguage: "java", TargetLanguage: "pgsql", MatchStrategy: "orm_convention"},
		{SourceLanguage: "java", TargetLanguage: "tsql", MatchStrategy: "orm_convention"},

		// Delphi T-prefix: strip T from class names when matching SQL objects
		{SourceLanguage: "delphi", TargetLanguage: "tsql", MatchStrategy: "strip_prefix"},

		// Frontend → Backend API route matching (calls_api references)
		// JS/TS clients calling C# ASP.NET or Java Spring endpoints
		{SourceLanguage: "javascript", TargetLanguage: "csharp", MatchStrategy: "api_route_match"},
		{SourceLanguage: "typescript", TargetLanguage: "csharp", MatchStrategy: "api_route_match"},
		{SourceLanguage: "javascript", TargetLanguage: "java", MatchStrategy: "api_route_match"},
		{SourceLanguage: "typescript", TargetLanguage: "java", MatchStrategy: "api_route_match"},
	}
}

// SymbolLookup is the interface CrossLangResolver uses to look up symbols.
// Implemented by partialSymbolTable (production) and SymbolTable (tests).
type SymbolLookup interface {
	// ByFQNMap returns the full qualified-name-to-ID map for cross-language iteration.
	ByFQNMap() map[string]uuid.UUID
	// LangOf returns the language for a qualified name, or "" if unknown.
	LangOf(fqn string) string
	// EndpointsBySignature returns a map from endpoint Signature (e.g. "GET /api/orders/{id}")
	// to symbol ID. Used by the api_route_match bridge strategy.
	EndpointsBySignature() map[string]uuid.UUID
}

// SymbolTable is a simple symbol lookup structure used in tests and legacy code.
// Production code uses partialSymbolTable populated from targeted DB queries.
type SymbolTable struct {
	ByFQN         map[string]uuid.UUID
	ByShortName   map[string][]uuid.UUID
	ByLang        map[string]string
	BySignature   map[string]uuid.UUID // endpoint Signature → ID
}

func newSymbolTable() *SymbolTable {
	return &SymbolTable{
		ByFQN:       make(map[string]uuid.UUID),
		ByShortName: make(map[string][]uuid.UUID),
		ByLang:      make(map[string]string),
		BySignature: make(map[string]uuid.UUID),
	}
}

// ByFQNMap implements SymbolLookup for SymbolTable.
func (t *SymbolTable) ByFQNMap() map[string]uuid.UUID { return t.ByFQN }

// LangOf implements SymbolLookup for SymbolTable.
func (t *SymbolTable) LangOf(fqn string) string { return t.ByLang[fqn] }

// EndpointsBySignature implements SymbolLookup for SymbolTable.
func (t *SymbolTable) EndpointsBySignature() map[string]uuid.UUID { return t.BySignature }

// shortNameCandidates implements symbolIndex for SymbolTable.
func (t *SymbolTable) shortNameCandidates(name string) []uuid.UUID { return t.ByShortName[name] }

// Resolve attempts to resolve a reference using cross-language bridge rules.
// Returns a BridgeMatch with confidence and strategy information.
func (c *CrossLangResolver) Resolve(ref parser.RawReference, sourceLang string, table SymbolLookup) (BridgeMatch, bool) {
	targetName := ref.ToName
	targetQualified := ref.ToQualified
	if targetQualified == "" {
		targetQualified = targetName
	}

	byFQN := table.ByFQNMap()

	for _, rule := range c.rules {
		if !matchesLanguage(sourceLang, rule.SourceLanguage) {
			continue
		}

		bridge := rule.SourceLanguage + "→" + rule.TargetLanguage

		switch rule.MatchStrategy {
		case "exact":
			if id, ok := byFQN[targetQualified]; ok {
				return BridgeMatch{TargetID: id, Confidence: 1.0, Strategy: "exact", Bridge: bridge}, true
			}

		case "case_insensitive":
			lower := strings.ToLower(targetName)
			for fqn, id := range byFQN {
				if strings.ToLower(shortNameOf(fqn)) == lower {
					lang := table.LangOf(fqn)
					if lang == "" || matchesLanguage(lang, rule.TargetLanguage) {
						return BridgeMatch{TargetID: id, Confidence: 0.85, Strategy: "case_insensitive", Bridge: bridge}, true
					}
				}
			}

		case "schema_qualified":
			candidates := []string{targetQualified, "dbo." + targetName, targetName}
			for _, candidate := range candidates {
				lower := strings.ToLower(candidate)
				for fqn, id := range byFQN {
					if strings.ToLower(fqn) == lower {
						return BridgeMatch{TargetID: id, Confidence: 0.95, Strategy: "schema_qualified", Bridge: bridge}, true
					}
				}
			}

		case "strip_prefix":
			stripped := targetName
			if strings.HasPrefix(stripped, "T") && len(stripped) > 1 {
				stripped = stripped[1:]
			}
			lower := strings.ToLower(stripped)
			for fqn, id := range byFQN {
				if strings.ToLower(shortNameOf(fqn)) == lower {
					return BridgeMatch{TargetID: id, Confidence: 0.75, Strategy: "strip_prefix", Bridge: bridge}, true
				}
			}

		case "orm_convention":
			variants := ormNameVariants(targetName)
			for _, variant := range variants {
				lower := strings.ToLower(variant)
				for fqn, id := range byFQN {
					if strings.ToLower(shortNameOf(fqn)) == lower {
						lang := table.LangOf(fqn)
						if lang == "" || matchesLanguage(lang, rule.TargetLanguage) {
							return BridgeMatch{TargetID: id, Confidence: 0.7, Strategy: "orm_convention", Bridge: bridge}, true
						}
					}
				}
			}

		case "api_route_match":
			// Only handle calls_api references (emitted by JS/TS parser).
			if ref.ReferenceType != "calls_api" {
				continue
			}

			// The frontend ToName is already normalized (e.g. "GET /api/orders/{*}"
			// or "/api/orders/{*}" when no verb is present). We compare it against
			// endpoint symbols stored by their Signature field.
			endpointSigs := table.EndpointsBySignature()
			normalizedRef := normalizeRouteForMatch(targetName)

			for sig, id := range endpointSigs {
				lang := table.LangOf(sig)
				if lang != "" && !matchesLanguage(lang, rule.TargetLanguage) {
					continue
				}
				normalizedSig := normalizeRouteForMatch(sig)
				if routeMatches(normalizedRef, normalizedSig) {
					return BridgeMatch{
						TargetID:   id,
						Confidence: 0.9,
						Strategy:   "api_route_match",
						Bridge:     bridge,
					}, true
				}
			}
		}
	}

	return BridgeMatch{}, false
}

// ormNameVariants returns naming convention variants for ORM resolution.
func ormNameVariants(name string) []string {
	variants := []string{name}

	// Pluralize
	lower := strings.ToLower(name)
	if strings.HasSuffix(lower, "y") && !strings.HasSuffix(lower, "ey") && !strings.HasSuffix(lower, "ay") && !strings.HasSuffix(lower, "oy") {
		variants = append(variants, name[:len(name)-1]+"ies")
	} else if strings.HasSuffix(lower, "s") || strings.HasSuffix(lower, "x") || strings.HasSuffix(lower, "ch") || strings.HasSuffix(lower, "sh") {
		variants = append(variants, name+"es")
	} else {
		variants = append(variants, name+"s")
	}

	// Singularize
	if strings.HasSuffix(lower, "ies") {
		variants = append(variants, name[:len(name)-3]+"y")
	} else if strings.HasSuffix(lower, "es") {
		variants = append(variants, name[:len(name)-2])
	} else if strings.HasSuffix(lower, "s") && !strings.HasSuffix(lower, "ss") {
		variants = append(variants, name[:len(name)-1])
	}

	return variants
}

func matchesLanguage(actual, pattern string) bool {
	return strings.EqualFold(actual, pattern)
}

// ---------------------------------------------------------------------------
// API route matching helpers
// ---------------------------------------------------------------------------

// normalizeRouteForMatch converts a raw route string into a canonical form for
// comparison. Both frontend references and backend endpoint signatures are
// normalised before comparison so that different parameterisation styles match.
//
// Normalisations applied:
//   - Lowercase the whole string
//   - Replace {anything} and {*} (ASP.NET / Spring) with the uniform token {p}
//   - Replace :param Express-style segments with {p}
//   - Remove trailing slashes
func normalizeRouteForMatch(route string) string {
	s := strings.ToLower(strings.TrimSpace(route))
	s = strings.TrimRight(s, "/")

	// Replace {param}, {param:constraint}, {*} with uniform token {p}
	var out strings.Builder
	i := 0
	for i < len(s) {
		if s[i] == '{' {
			end := strings.IndexByte(s[i:], '}')
			if end >= 0 {
				out.WriteString("{p}")
				i += end + 1
				continue
			}
		}
		out.WriteByte(s[i])
		i++
	}
	s = out.String()

	// Replace Express-style :param segments
	parts := strings.Split(s, "/")
	for i, part := range parts {
		if strings.HasPrefix(part, ":") && len(part) > 1 {
			parts[i] = "{p}"
		}
	}
	s = strings.Join(parts, "/")

	return s
}

// routeMatches returns true when refNorm (the frontend normalized route) matches
// sigNorm (the backend endpoint normalized signature).
//
// A match requires:
//  1. Same HTTP verb prefix (if the reference carries a verb). A reference
//     without a verb prefix (e.g. plain "/api/users/{p}") matches any verb.
//  2. Same path after normalisation.
func routeMatches(refNorm, sigNorm string) bool {
	refVerb, refPath := splitVerbPath(refNorm)
	sigVerb, sigPath := splitVerbPath(sigNorm)

	// Verb must match if the reference specifies one
	if refVerb != "" && sigVerb != "" && refVerb != sigVerb {
		return false
	}

	return refPath == sigPath
}

// splitVerbPath splits a normalised route string like "get /api/orders/{p}"
// into ("get", "/api/orders/{p}"). If there is no space the whole string is
// treated as a path.
func splitVerbPath(norm string) (verb, path string) {
	idx := strings.IndexByte(norm, ' ')
	if idx < 0 {
		return "", norm
	}
	return norm[:idx], norm[idx+1:]
}
