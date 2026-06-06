package ingestion

import (
	"context"
	"crypto/sha256"
	"fmt"

	"github.com/google/uuid"

	"github.com/maraichr/lattice/internal/parser"
	"github.com/maraichr/lattice/internal/store"
	"github.com/maraichr/lattice/internal/store/postgres"
)

// PersistResults writes parsed file results to PostgreSQL, including column references.
// indexRunID is used to namespace column references for later retrieval by the lineage stage.
// Returns counts of files, symbols, and intra-file edges persisted.
func PersistResults(ctx context.Context, s *store.Store, indexRunID uuid.UUID, results []parser.FileResult) (files, symbols, edges int, err error) {
	for _, fr := range results {
		hash := fmt.Sprintf("%x", sha256.Sum256([]byte(fr.Path)))
		if fr.Hash != "" {
			hash = fr.Hash
		}

		dbFile, err := s.UpsertFile(ctx, postgres.UpsertFileParams{
			ProjectID: fr.ProjectID,
			SourceID:  fr.SourceID,
			Path:      fr.Path,
			Language:  fr.Language,
			SizeBytes: fr.SizeBytes,
			Hash:      hash,
		})
		if err != nil {
			return files, symbols, edges, fmt.Errorf("upsert file %s: %w", fr.Path, err)
		}
		files++

		// Upsert symbols for this file. We deliberately avoid delete-then-insert:
		// CreateSymbol upserts on (project_id, qualified_name, kind), so symbols that
		// survive a re-index keep their IDs. That preserves cross-file edges and
		// embeddings, which are FK'd to symbols ON DELETE CASCADE and would otherwise
		// be silently dropped on every run. Symbols that no longer exist are reconciled
		// away afterwards.
		symbolIDs := make(map[string]uuid.UUID)
		keepIDs := make([]uuid.UUID, 0, len(fr.Symbols))

		for _, sym := range fr.Symbols {
			created, err := createSymbol(ctx, s, fr.ProjectID, dbFile.ID, sym)
			if err != nil {
				return files, symbols, edges, fmt.Errorf("create symbol %s: %w", sym.QualifiedName, err)
			}
			symbolIDs[sym.QualifiedName] = created.ID
			keepIDs = append(keepIDs, created.ID)
			symbols++

			for _, child := range sym.Children {
				childCreated, err := createSymbol(ctx, s, fr.ProjectID, dbFile.ID, child)
				if err != nil {
					return files, symbols, edges, fmt.Errorf("create child symbol %s: %w", child.QualifiedName, err)
				}
				symbolIDs[child.QualifiedName] = childCreated.ID
				keepIDs = append(keepIDs, childCreated.ID)
				symbols++
			}
		}

		// Reconcile: drop symbols that previously belonged to this file but are no
		// longer produced, and stage their Neo4j nodes for deletion so Postgres and
		// Neo4j stay consistent. An empty keepIDs set deletes all of the file's symbols.
		removed, err := s.DeleteSymbolsByFileExcept(ctx, postgres.DeleteSymbolsByFileExceptParams{
			FileID:  dbFile.ID,
			KeepIds: keepIDs,
		})
		if err != nil {
			return files, symbols, edges, fmt.Errorf("reconcile symbols for %s: %w", fr.Path, err)
		}
		for _, id := range removed {
			_ = s.RecordGraphDeletion(ctx, postgres.RecordGraphDeletionParams{
				IndexRunID: indexRunID,
				ProjectID:  fr.ProjectID,
				NodeID:     id,
				NodeType:   "symbol",
			})
		}

		// Insert intra-file edges (best-effort: cross-file edges resolved later).
		for _, ref := range fr.References {
			sourceID, ok := symbolIDs[ref.FromSymbol]
			if !ok {
				continue
			}
			targetID, ok := symbolIDs[ref.ToQualified]
			if !ok {
				targetID, ok = symbolIDs[ref.ToName]
				if !ok {
					// Not intra-file — persist as raw reference for cross-file resolution.
					var line *int32
					if ref.Line > 0 {
						l := int32(ref.Line)
						line = &l
					}
					_ = s.InsertRawReference(ctx, postgres.InsertRawReferenceParams{
						ProjectID:     fr.ProjectID,
						IndexRunID:    indexRunID,
						FileID:        dbFile.ID,
						FromSymbol:    ref.FromSymbol,
						ToName:        ref.ToName,
						ToQualified:   ref.ToQualified,
						ReferenceType: ref.ReferenceType,
						Confidence:    ref.Confidence,
						Line:          line,
						Language:      fr.Language,
					})
					continue
				}
			}

			_, err := s.CreateSymbolEdge(ctx, postgres.CreateSymbolEdgeParams{
				ProjectID: fr.ProjectID,
				SourceID:  sourceID,
				TargetID:  targetID,
				EdgeType:  ref.ReferenceType,
			})
			if err != nil {
				continue
			}
			edges++
		}

		// Persist column references to DB so the lineage stage can process them
		// after all parse chunks complete, without keeping them in memory.
		for _, colRef := range fr.ColumnReferences {
			var expr, ctxStr *string
			if colRef.Expression != "" {
				expr = &colRef.Expression
			}
			if colRef.Context != "" {
				ctxStr = &colRef.Context
			}
			var line *int32
			if colRef.Line > 0 {
				l := int32(colRef.Line)
				line = &l
			}
			_ = s.InsertColumnReference(ctx, postgres.InsertColumnReferenceParams{
				ProjectID:      fr.ProjectID,
				IndexRunID:     indexRunID,
				SourceColumn:   colRef.SourceColumn,
				TargetColumn:   colRef.TargetColumn,
				DerivationType: colRef.DerivationType,
				Expression:     expr,
				Context:        ctxStr,
				Line:           line,
			})
		}
	}

	return files, symbols, edges, nil
}

func createSymbol(ctx context.Context, s *store.Store, projectID, fileID uuid.UUID, sym parser.Symbol) (postgres.Symbol, error) {
	var startCol, endCol *int32
	if sym.StartCol > 0 {
		v := int32(sym.StartCol)
		startCol = &v
	}
	if sym.EndCol > 0 {
		v := int32(sym.EndCol)
		endCol = &v
	}
	var sig, doc *string
	if sym.Signature != "" {
		sig = &sym.Signature
	}
	if sym.DocComment != "" {
		doc = &sym.DocComment
	}

	return s.CreateSymbol(ctx, postgres.CreateSymbolParams{
		ProjectID:     projectID,
		FileID:        fileID,
		Name:          sym.Name,
		QualifiedName: sym.QualifiedName,
		Kind:          sym.Kind,
		Language:      sym.Language,
		StartLine:     int32(sym.StartLine),
		EndLine:       int32(sym.EndLine),
		StartCol:      startCol,
		EndCol:        endCol,
		Signature:     sig,
		DocComment:    doc,
	})
}
