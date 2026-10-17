package opensearch

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/opensearch-project/opensearch-go/v5/opensearchapi"
)

// Every search body is built from opensearch-go's typed query DSL, so values
// are always encoded as JSON. These helpers shorten its generated names.

type query = opensearchapi.CommonQueryDSLQueryContainer

// term matches a field's exact value.
func term[V string | int](field string, value V) query {
	var v opensearchapi.FieldValue
	switch x := any(value).(type) {
	case string:
		v = opensearchapi.NewFieldValueFromString(x)
	case int:
		v = opensearchapi.NewFieldValueFromFloat64(float64(x))
	}
	return query{Term: map[string]opensearchapi.CommonQueryDSLTermQuery{field: opensearchapi.NewCommonQueryDSLTermQueryFromFieldValue(v)}}
}

// oneOf matches a field holding any of the values. The generated terms query
// cannot name its field, so this is a bool of term queries.
func oneOf[V string | int](field string, values []V) query {
	qs := make([]query, len(values))
	for i, v := range values {
		qs[i] = term(field, v)
	}
	return anyOf(qs...)
}

// anyOf matches documents at least one query matches, scored by their sum.
func anyOf(qs ...query) query {
	should := opensearchapi.NewCommonQueryDSLBoolQueryShouldFromArray(qs)
	return query{Bool: &opensearchapi.CommonQueryDSLBoolQuery{Should: &should, MinimumShouldMatch: new(opensearchapi.NewMinimumShouldMatchFromInt(1))}}
}

// allOf matches documents every query matches, without scoring.
func allOf(qs ...query) query {
	filter := opensearchapi.NewCommonQueryDSLBoolQueryFilterFromArray(qs)
	return query{Bool: &opensearchapi.CommonQueryDSLBoolQuery{Filter: &filter}}
}

// not matches documents the query does not.
func not(q query) query {
	mustNot := opensearchapi.NewCommonQueryDSLBoolQueryMustNotFromQueryContainer(q)
	return query{Bool: &opensearchapi.CommonQueryDSLBoolQuery{MustNot: &mustNot}}
}

// below matches a numeric field's values under n.
func below(field string, n int64) query {
	r := opensearchapi.CommonQueryDSLNumberRangeQuery{Lt: new(float64(n))}
	return query{Range: map[string]opensearchapi.CommonQueryDSLRangeQuery{field: opensearchapi.NewCommonQueryDSLRangeQueryFromNumberRangeQuery(r)}}
}

// exists matches documents with a value in the field.
func exists(field string) query {
	return query{Exists: &opensearchapi.CommonQueryDSLExistsQuery{Field: field}}
}

// prefixMatch matches documents with every word of text in one field, the
// last word as a prefix; boost weighs a field's score.
func prefixMatch(text, field string, boost float32) query {
	q := opensearchapi.CommonQueryDSLMatchBoolPrefixQueryQuery{Query: text, Operator: new("and")}
	if boost != 1 {
		q.Boost = &boost
	}
	return query{MatchBoolPrefix: map[string]opensearchapi.CommonQueryDSLMatchBoolPrefixQuery{field: opensearchapi.NewCommonQueryDSLMatchBoolPrefixQueryFromQuery(q)}}
}

// sortBy orders by each field, ascending, or descending when it starts with -.
func sortBy(fields ...string) *opensearchapi.Sort {
	by := make([]opensearchapi.SortCombinations, len(fields))
	for i, f := range fields {
		order := "asc"
		if name, desc := strings.CutPrefix(f, "-"); desc {
			f, order = name, "desc"
		}
		by[i] = opensearchapi.NewSortCombinationsFromStringMap(map[string]string{f: order})
	}
	return new(opensearchapi.NewSortFromArray(by))
}

// search runs a search and decodes each hit's source; fields, when given,
// are the only ones fetched.
func search[T any](ctx context.Context, c *Client, index string, body opensearchapi.SearchBody, fields ...string) ([]T, error) {
	req := &opensearchapi.SearchReq{Indices: []string{index}, Body: &body}
	if len(fields) > 0 {
		req.Params = &opensearchapi.SearchParams{SourceIncludes: fields}
	}
	resp, err := c.api.Search(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("search %s: %w", index, err)
	}
	out := make([]T, len(resp.Hits.Hits))
	for i, h := range resp.Hits.Hits {
		if err := json.Unmarshal(h.Source, &out[i]); err != nil {
			return nil, fmt.Errorf("search %s: %w", index, err)
		}
	}
	return out, nil
}
