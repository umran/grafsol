package postgres

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/umran/grafsol/graph"
)

// vertexRow is one row of the vertices table.
type vertexRow struct {
	id        string
	kind      string
	attrs     []byte // JSON
	embedding []float32
}

// documentAttrs and topicAttrs are what goes into the attrs column: every
// field of the vertex except the ID and the embedding, which have columns of
// their own. They are separate from graph.Document and graph.Topic on
// purpose: what is stored on disk is a format this package owns, and it
// should not change by accident when a field of a graph type is renamed.
type documentAttrs struct {
	Type      graph.DocumentType `json:"type"`
	Source    string             `json:"source"`
	URL       string             `json:"url"`
	Title     string             `json:"title"`
	Summary   string             `json:"summary"`
	CreatedAt time.Time          `json:"created_at"`
	UpdatedAt time.Time          `json:"updated_at"`
}

type topicAttrs struct {
	Name    string `json:"name"`
	Summary string `json:"summary"`
}

// encodeVertex turns a vertex into the row that stores it.
func encodeVertex(v graph.Vertex) (vertexRow, error) {
	if err := graph.ValidateVertex(v); err != nil {
		return vertexRow{}, err
	}

	var (
		attrs     any
		embedding []float32
	)
	switch v := v.(type) {
	case *graph.Document:
		attrs = documentAttrs{
			Type:      v.Type,
			Source:    v.Source,
			URL:       v.URL,
			Title:     v.Title,
			Summary:   v.Summary,
			CreatedAt: v.CreatedAt,
			UpdatedAt: v.UpdatedAt,
		}
		embedding = v.Embedding
	case *graph.Topic:
		attrs = topicAttrs{Name: v.Name, Summary: v.Summary}
		embedding = v.Embedding
	}

	encoded, err := json.Marshal(attrs)
	if err != nil {
		return vertexRow{}, fmt.Errorf("postgres: encoding %s: %w", v.VertexID(), err)
	}
	if len(embedding) == 0 {
		// pgvector has no empty vector. A vertex without an embedding
		// stores null, which is also what keeps it out of the index.
		embedding = nil
	}
	return vertexRow{
		id:        string(v.VertexID()),
		kind:      string(v.VertexID().Kind()),
		attrs:     encoded,
		embedding: embedding,
	}, nil
}

// decodeVertex turns a stored row back into a vertex.
func decodeVertex(row vertexRow) (graph.Vertex, error) {
	id := graph.ID(row.id)
	switch graph.VertexKind(row.kind) {
	case graph.KindDocument:
		var attrs documentAttrs
		if err := json.Unmarshal(row.attrs, &attrs); err != nil {
			return nil, fmt.Errorf("postgres: decoding %s: %w", id, err)
		}
		return &graph.Document{
			ID:        id,
			Type:      attrs.Type,
			Source:    attrs.Source,
			URL:       attrs.URL,
			Title:     attrs.Title,
			Summary:   attrs.Summary,
			Embedding: row.embedding,
			CreatedAt: attrs.CreatedAt,
			UpdatedAt: attrs.UpdatedAt,
		}, nil
	case graph.KindTopic:
		var attrs topicAttrs
		if err := json.Unmarshal(row.attrs, &attrs); err != nil {
			return nil, fmt.Errorf("postgres: decoding %s: %w", id, err)
		}
		return &graph.Topic{
			ID:        id,
			Name:      attrs.Name,
			Summary:   attrs.Summary,
			Embedding: row.embedding,
		}, nil
	}
	return nil, fmt.Errorf("postgres: vertex %s has unknown kind %q", id, row.kind)
}

// toStrings converts a slice of IDs for use as a text[] parameter.
func toStrings(ids []graph.ID) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = string(id)
	}
	return out
}
