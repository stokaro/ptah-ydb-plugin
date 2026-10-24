package embedstore

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
)

// distanceOrder is the ORDER BY a nearest-neighbour search sorts by: the Knn
// distance ascending, or the inner-product similarity descending, from a
// closed set, because a wrong function answers every query with plausible
// rows in the wrong order.
func distanceOrder(metric protocol.EmbeddingTarget_Metric, column string) (string, bool) {
	switch metric {
	case protocol.EmbeddingTarget_COSINE:
		return "Knn::CosineDistance(" + column + ", $p1)", true
	case protocol.EmbeddingTarget_L2:
		return "Knn::EuclideanDistance(" + column + ", $p1)", true
	case protocol.EmbeddingTarget_INNER_PRODUCT:
		return "Knn::InnerProductSimilarity(" + column + ", $p1) DESC", true
	default:
		return "", false
	}
}

// Search answers the keys nearest to a vector twice: through the
// generation's index, as an application's query with VIEW would, and without
// it, which is an exhaustive scan. A generation with no index, or one whose
// index is not there and ready, answers both by scan. A search YDB refuses is
// a FAILED result with the reason, not a failed call.
func (s *Store) Search(ctx context.Context, request *protocol.SearchEmbeddingsRequest) (*protocol.EmbeddingSearchResult, error) {
	spec := request.GetSpec()
	failed := func(err error) *protocol.EmbeddingSearchResult {
		return &protocol.EmbeddingSearchResult{Outcome: protocol.EmbeddingSearchResult_FAILED, Message: oneLine(err.Error())}
	}
	order, known := distanceOrder(spec.GetTarget().GetMetric(), Quote(spec.GetTarget().GetColumn()))
	if !known {
		return failed(fmt.Errorf("no YDB distance function for metric %s", spec.GetTarget().GetMetric())), nil
	}
	index := ""
	if strings.TrimSpace(spec.GetTarget().GetIndexMethod()) != "" {
		described, exists, err := s.describe(ctx, targetPath(spec))
		if err != nil {
			return nil, err
		}
		if found, present := described.Index(spec.GetNames().GetIndex()); exists && present && found.Ready {
			index = found.Name
		}
	}
	indexed, err := s.nearest(ctx, spec, order, request.GetVector(), int(request.GetDepth()), index)
	if err != nil {
		return failed(err), nil
	}
	exact, err := s.nearest(ctx, spec, order, request.GetVector(), int(request.GetDepth()), "")
	if err != nil {
		return failed(err), nil
	}
	return &protocol.EmbeddingSearchResult{Outcome: protocol.EmbeddingSearchResult_COMPLETED, Indexed: indexed, Exact: exact, IndexUsed: index != ""}, nil
}

// nearest returns the keys closest to a vector among the generation's rows,
// through index when one is named and by exhaustive scan when none is.
func (s *Store) nearest(ctx context.Context, spec *protocol.EmbeddingSpec, order string, vector []float32, depth int, index string) ([]*protocol.EmbeddingKey, error) {
	keys := make([]string, len(spec.GetSource().GetKeyFields()))
	for i, field := range spec.GetSource().GetKeyFields() {
		keys[i] = asText("", field)
	}
	view := ""
	if index != "" {
		view = " VIEW " + Quote(index)
	}
	column := Quote(spec.GetTarget().GetColumn())
	// #nosec G201 -- YQL takes no parameter for a path, a column or an index.
	// The names come from the specification through Quote; the function is
	// chosen from a closed set; the vector and the generation are parameters
	// and the depth is an integer.
	query := fmt.Sprintf("SELECT %s FROM %s%s WHERE %s IS NOT NULL AND %s = $p2 ORDER BY %s LIMIT %d", strings.Join(keys, ", "),
		Quote(targetPath(spec)), view, column, Quote(spec.GetTarget().GetColumn()+generationSuffix), order, depth)
	rows, err := s.db.QueryContext(ctx, query, named(EncodeVector(vector), spec.GetGeneration())...)
	if err != nil {
		return nil, fmt.Errorf("search %s: %s", targetPath(spec), statusName(err))
	}
	defer func() { _ = rows.Close() }()
	var found []*protocol.EmbeddingKey
	seen := make(map[string]bool)
	for rows.Next() {
		values := make([]sql.NullString, len(keys))
		destinations := make([]any, len(values))
		for i := range values {
			destinations[i] = &values[i]
		}
		if err := rows.Scan(destinations...); err != nil {
			return nil, fmt.Errorf("read a search result: %s", statusName(err))
		}
		components := make([]string, len(values))
		for i, value := range values {
			components[i] = value.String
		}
		// A key of a chunked generation holds a set, and its nearest chunk
		// answers for it.
		if seen[keyID(components)] {
			continue
		}
		seen[keyID(components)] = true
		found = append(found, &protocol.EmbeddingKey{Components: components})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the search results: %s", statusName(err))
	}
	return found, nil
}
