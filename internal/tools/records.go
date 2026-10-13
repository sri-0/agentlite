package tools

import (
	_ "embed"
	"encoding/json"
	"slices"
	"strings"

	"agentlite/pkg/citations"
	"agentlite/pkg/toolx"
)

// The demo record system: a few records with the identifiers, data markings
// and control sets a real system would attach to each row.
//
//go:embed records.json
var recordsJSON []byte

type record struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	Summary    string `json:"summary"`
	Owner      string `json:"owner"`
	Updated    string `json:"updated"`
	Marking    string `json:"marking"`
	ControlSet string `json:"control_set"`
}

type searchRecordsArgs struct {
	Query string `json:"query" jsonschema:"words to look for in record titles and summaries"`
}

// recordHit is what the model sees of a record. The record's id, marking and
// control set are not in it: they go into the source, untouched by the model.
type recordHit struct {
	Source  int    `json:"source"`
	Title   string `json:"title"`
	Summary string `json:"summary"`
	Owner   string `json:"owner"`
	Updated string `json:"updated"`
}

const recordsMax = 5

// searchRecords is the sources demo, like write_note is the approval demo: it
// searches the sample records and records every hit as a source, with the
// row's own identifiers and markings as the source's metadata.
func searchRecords(conn toolx.Connector) (toolx.Tool, error) {
	var records []record
	if err := json.Unmarshal(recordsJSON, &records); err != nil {
		return toolx.Tool{}, err
	}
	return toolx.New(toolx.Spec{
		Name:        "search_records",
		Description: "Search the organisation's records (sample data). Each hit has a source number to cite.",
	}, func(tc toolx.Context, a searchRecordsArgs) (map[string]any, error) {
		hits := []recordHit{}
		for _, r := range rank(records, a.Query) {
			n, err := tc.AddSource(citations.Source{
				System: conn.ID, Key: r.ID, URL: "https://records.example.org/r/" + r.ID, Title: r.Title,
				Metadata: map[string]any{"record_id": r.ID, "marking": r.Marking, "control_set": r.ControlSet, "owner": r.Owner, "updated": r.Updated},
			})
			if err != nil {
				return nil, err
			}
			hits = append(hits, recordHit{Source: n, Title: r.Title, Summary: r.Summary, Owner: r.Owner, Updated: r.Updated})
		}
		return map[string]any{"records": hits}, nil
	})
}

// rank returns the records matching most query words, best first.
func rank(records []record, query string) []record {
	words := strings.Fields(strings.ToLower(query))
	score := func(r record) int {
		text := strings.ToLower(r.Title + " " + r.Summary)
		n := 0
		for _, w := range words {
			if strings.Contains(text, w) {
				n++
			}
		}
		return n
	}
	var out []record
	for _, r := range records {
		if score(r) > 0 {
			out = append(out, r)
		}
	}
	slices.SortStableFunc(out, func(a, b record) int { return score(b) - score(a) })
	return out[:min(len(out), recordsMax)]
}
