package postgres

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/umran/grafsol/graph"
	"github.com/umran/grafsol/storage"
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

// saidBody is what goes into the body column of the said table: everything
// in a storage.Said except the document's ID, which is the row's key. Like
// the attrs of a vertex it is a format this package owns, spelled out here
// so that renaming a field of a graph type cannot change what is on disk.
//
// Embeddings are kept if they are given, though the sync engine gives none:
// the graph's own tables are where a vertex's embedding lives.
type saidBody struct {
	Document   documentAttrs   `json:"document"`
	Embedding  []float32       `json:"embedding,omitempty"`
	References []referenceBody `json:"references,omitempty"`
	Labels     []labelBody     `json:"labels,omitempty"`
	Edges      []edgeBody      `json:"edges,omitempty"`
	Topics     []topicBody     `json:"topics,omitempty"`
}

type referenceBody struct {
	Target  graph.ID `json:"target,omitempty"`
	URL     string   `json:"url,omitempty"`
	Form    string   `json:"form"`
	Written string   `json:"written,omitempty"`
	Context string   `json:"context,omitempty"`
}

type labelBody struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// edgeBody is an edge that starts at the document the row is for.
type edgeBody struct {
	Kind    graph.EdgeKind `json:"kind"`
	To      graph.ID       `json:"to"`
	Summary string         `json:"summary,omitempty"`
}

type topicBody struct {
	ID        graph.ID  `json:"id"`
	Name      string    `json:"name"`
	Summary   string    `json:"summary,omitempty"`
	Embedding []float32 `json:"embedding,omitempty"`
}

// encodeSaid turns what was said of a document into the body that stores it.
func encodeSaid(said storage.Said) ([]byte, error) {
	if err := said.Validate(); err != nil {
		return nil, err
	}
	document := said.Document
	body := saidBody{
		Document: documentAttrs{
			Type:      document.Type,
			Source:    document.Source,
			URL:       document.URL,
			Title:     document.Title,
			Summary:   document.Summary,
			CreatedAt: document.CreatedAt,
			UpdatedAt: document.UpdatedAt,
		},
		Embedding: document.Embedding,
	}
	for _, reference := range said.References {
		body.References = append(body.References, referenceBody(reference))
	}
	for _, label := range said.Labels {
		body.Labels = append(body.Labels, labelBody(label))
	}
	for _, edge := range said.Edges {
		body.Edges = append(body.Edges, edgeBody{Kind: edge.Kind, To: edge.To, Summary: edge.Summary})
	}
	for _, topic := range said.Topics {
		body.Topics = append(body.Topics, topicBody(topic))
	}

	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("postgres: encoding what was said of %s: %w", document.ID, err)
	}
	return encoded, nil
}

// decodeSaid turns a stored body back into what was said of a document.
func decodeSaid(id graph.ID, encoded []byte) (storage.Said, error) {
	var body saidBody
	if err := json.Unmarshal(encoded, &body); err != nil {
		return storage.Said{}, fmt.Errorf("postgres: decoding what was said of %s: %w", id, err)
	}
	said := storage.Said{
		Document: graph.Document{
			ID:        id,
			Type:      body.Document.Type,
			Source:    body.Document.Source,
			URL:       body.Document.URL,
			Title:     body.Document.Title,
			Summary:   body.Document.Summary,
			Embedding: body.Embedding,
			CreatedAt: body.Document.CreatedAt,
			UpdatedAt: body.Document.UpdatedAt,
		},
	}
	for _, reference := range body.References {
		said.References = append(said.References, graph.Reference(reference))
	}
	for _, label := range body.Labels {
		said.Labels = append(said.Labels, graph.Label(label))
	}
	for _, edge := range body.Edges {
		said.Edges = append(said.Edges, graph.Edge{From: id, Kind: edge.Kind, To: edge.To, Summary: edge.Summary})
	}
	for _, topic := range body.Topics {
		said.Topics = append(said.Topics, graph.Topic(topic))
	}
	return said, nil
}

// toStrings converts a slice of IDs for use as a text[] parameter.
func toStrings(ids []graph.ID) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = string(id)
	}
	return out
}
