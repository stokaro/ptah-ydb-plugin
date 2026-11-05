package ydbdecl

import (
	"fmt"
	"strings"
)

// pgvectorOperators are pgvector's operator classes for its float vector, with
// the metric each names on YDB. A declaration written for PostgreSQL names its
// metric through one of these, and YDB names the same metric with a setting:
// `<=>` is cosine distance, `<->` Euclidean distance, `<+>` taxicab distance
// and `<#>` the negative inner product, which orders as the inner product
// similarity does. The host's embedded engine reads them alike.
var pgvectorOperators = map[string]VectorIndex{
	"vector_cosine_ops": {Distance: "cosine"},
	"vector_l2_ops":     {Distance: "euclidean"},
	"vector_l1_ops":     {Distance: "manhattan"},
	"vector_ip_ops":     {Similarity: "inner_product"},
}

// ResolveVectorOperator folds the operator class a vector index's declaration
// names into the metric it names: vector_cosine_ops reads as distance=cosine,
// vector_l2_ops as distance=euclidean, vector_l1_ops as distance=manhattan and
// vector_ip_ops as similarity=inner_product. A class that names a metric the
// settings name too has to agree with them, and a class with no YDB
// counterpart is refused, in the words of the host's embedded engine. No class
// changes nothing.
func ResolveVectorOperator(spec *VectorIndex, operator string) error {
	operator = strings.ToLower(strings.TrimSpace(operator))
	if operator == "" {
		return nil
	}
	metric, known := pgvectorOperators[operator]
	if !known {
		return fmt.Errorf("operator class %q has no YDB counterpart: a YDB vector index names its metric with "+
			"distance (cosine, euclidean, manhattan) or similarity (inner_product, cosine), and its vectors are "+
			"float, uint8, int8 or bit", operator)
	}
	switch {
	case spec.Distance == "" && spec.Similarity == "":
		spec.Distance, spec.Similarity = metric.Distance, metric.Similarity
	case spec.Distance != metric.Distance || spec.Similarity != metric.Similarity:
		return fmt.Errorf("operator class %q names %s, and the index's settings name %s", operator, metricName(metric), metricName(*spec))
	}
	return nil
}

// metricName writes the metric a spec names as its setting.
func metricName(spec VectorIndex) string {
	if spec.Similarity != "" {
		return AttributeSimilarity + "=" + spec.Similarity
	}
	return AttributeDistance + "=" + spec.Distance
}
