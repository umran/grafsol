package postgres

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/umran/grafsol/graph"
	"github.com/umran/grafsol/storage"
)

// An upgrade is one step that brings a database made by an earlier version
// of grafsol up to this one. schema.sql creates whatever tables are missing;
// an upgrade does what creating a table cannot, and moves or rewrites what
// the earlier version stored.
//
// CreateSchema runs the upgrades a database has not had, in order, inside
// its one transaction, and records each in schema_upgrades. A new database
// has them too: over empty tables every one of them does nothing.
type upgrade struct {
	number int
	name   string
	run    func(ctx context.Context, tx pgx.Tx) error
}

// upgrades lists every upgrade, oldest first. New ones go at the end. One
// that has been released is never renumbered and never changed: some
// database out there has already had it as it was.
var upgrades = []upgrade{
	{number: 1, name: "keep what sources said", run: keepWhatSourcesSaid},
}

// runUpgrades runs the upgrades the database has not had yet.
func runUpgrades(ctx context.Context, tx pgx.Tx) error {
	rows, err := tx.Query(ctx, selectUpgrades)
	if err != nil {
		return err
	}
	had, err := pgx.CollectRows(rows, pgx.RowTo[int])
	if err != nil {
		return err
	}
	if newest := upgrades[len(upgrades)-1].number; len(had) > 0 && slices.Max(had) > newest {
		return fmt.Errorf("the database has had upgrade %d and this version of grafsol knows none after %d: a later version has used this database", slices.Max(had), newest)
	}

	for _, upgrade := range upgrades {
		if slices.Contains(had, upgrade.number) {
			continue
		}
		if err := upgrade.run(ctx, tx); err != nil {
			return fmt.Errorf("upgrade %d (%s): %w", upgrade.number, upgrade.name, err)
		}
		if _, err := tx.Exec(ctx, insertUpgrade, upgrade.number, upgrade.name); err != nil {
			return fmt.Errorf("upgrade %d (%s): %w", upgrade.number, upgrade.name, err)
		}
	}
	return nil
}

// taggedWith is the kind of edge that a label used to make.
const taggedWith graph.EdgeKind = "tagged_with"

// keepWhatSourcesSaid is upgrade 1.
//
// Before it, a connector decided the kind of every edge itself, and the
// graph kept only the result: a links_to edge for a mention, a child_of edge
// for a parent, a tagged_with edge for a label. Connectors now report
// references and labels, rules decide what each becomes, and what was
// reported is kept in the said table.
//
// For a graph filled before that, this step works backwards, from the edges
// each document has to the references and labels they must have come from.
// What it writes is enough for rules to be applied to every document
// straight away. It is not all that the source said: where a document
// mentioned another twice only one of the two lines was ever kept, and the
// words a reference was written in were not kept at all. The sync engine
// writes the whole of it the next time it comes across the document.
//
// Last, the edges that labels made are retyped from tagged_with to in_topic,
// the one kind that now joins a document to a topic.
func keepWhatSourcesSaid(ctx context.Context, tx pgx.Tx) error {
	const pageSize = 500
	for after := ""; ; {
		documents, err := documentsPage(ctx, tx, after, pageSize)
		if err != nil {
			return err
		}
		if len(documents) == 0 {
			break
		}
		ids := make([]string, len(documents))
		for i, document := range documents {
			ids[i] = string(document.ID)
		}

		rows, err := tx.Query(ctx, expandOut, ids)
		if err != nil {
			return err
		}
		edges, err := pgx.CollectRows(rows, scanEdge)
		if err != nil {
			return err
		}
		topics, err := topicsOf(ctx, tx, edges)
		if err != nil {
			return err
		}

		bodies := make([]string, len(documents))
		for i, document := range documents {
			var own []graph.Edge
			for _, edge := range edges {
				if edge.From == document.ID {
					own = append(own, edge)
				}
			}
			body, err := encodeSaid(saidFromEdges(document, own, topics))
			if err != nil {
				return err
			}
			bodies[i] = string(body)
		}
		if _, err := tx.Exec(ctx, insertSaidWhereMissing, ids, bodies); err != nil {
			return err
		}
		after = ids[len(ids)-1]
	}

	if _, err := tx.Exec(ctx, copyTaggedWithToInTopic); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, deleteTaggedWith)
	return err
}

// documentsPage returns the next documents after an ID, without their
// embeddings.
func documentsPage(ctx context.Context, tx pgx.Tx, after string, limit int) ([]*graph.Document, error) {
	rows, err := tx.Query(ctx, documentsAfter, after, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (*graph.Document, error) {
		var stored vertexRow
		if err := row.Scan(&stored.id, &stored.kind, &stored.attrs); err != nil {
			return nil, err
		}
		vertex, err := decodeVertex(stored)
		if err != nil {
			return nil, err
		}
		return vertex.(*graph.Document), nil
	})
}

// topicsOf returns the stored topics that the edges point at, by ID.
func topicsOf(ctx context.Context, tx pgx.Tx, edges []graph.Edge) (map[graph.ID]*graph.Topic, error) {
	var ids []string
	for _, edge := range edges {
		if edge.To.Kind() == graph.KindTopic {
			ids = append(ids, string(edge.To))
		}
	}
	topics := make(map[graph.ID]*graph.Topic, len(ids))
	if len(ids) == 0 {
		return topics, nil
	}
	rows, err := tx.Query(ctx, selectVertices, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var stored vertexRow
		if err := rows.Scan(&stored.id, &stored.kind, &stored.attrs, &stored.embedding); err != nil {
			return nil, err
		}
		vertex, err := decodeVertex(stored)
		if err != nil {
			return nil, err
		}
		if topic, isTopic := vertex.(*graph.Topic); isTopic {
			topics[topic.ID] = topic
		}
	}
	return topics, rows.Err()
}

// saidFromEdges works out what a document's source must have said of it,
// from the edges that an earlier version of grafsol made of what it said.
// topics holds the stored topics those edges point at.
func saidFromEdges(document *graph.Document, edges []graph.Edge, topics map[graph.ID]*graph.Topic) storage.Said {
	said := storage.Said{Document: *document}
	slices.SortFunc(edges, func(a, b graph.Edge) int {
		return cmp.Or(cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.To, b.To))
	})
	for _, edge := range edges {
		switch edge.Kind {
		case graph.LinksTo:
			said.References = append(said.References, graph.Reference{Target: edge.To, Form: graph.FormMention, Context: edge.Summary})
			continue
		case graph.ChildOf:
			said.References = append(said.References, graph.Reference{Target: edge.To, Form: graph.FormParent, Context: edge.Summary})
			continue
		case taggedWith:
			if label, found := labelFor(edge.To, topics[edge.To]); found {
				said.Labels = append(said.Labels, label)
				continue
			}
			// No label would lead back to this topic, so the edge is kept as
			// one the source stated outright, under the kind it has now.
			edge.Kind = graph.InTopic
		}

		// An edge of any other kind was stated outright by its source.
		if slices.ContainsFunc(said.Edges, func(kept graph.Edge) bool { return kept.Triple() == edge.Triple() }) {
			continue
		}
		said.Edges = append(said.Edges, edge)
		if topic, stored := topics[edge.To]; stored {
			said.Topics = append(said.Topics, graph.Topic{ID: topic.ID, Name: topic.Name, Summary: topic.Summary})
		}
	}
	return said
}

// labelFor returns the label that a tagged_with edge to a topic came from:
// the name the topic was created with, which is the label's, or failing
// that the topic's key. Neither will do if it would not name this topic
// again, as a topic that was put in the graph by hand may not.
func labelFor(id graph.ID, topic *graph.Topic) (graph.Label, bool) {
	names := []string{id.Key()}
	description := ""
	if topic != nil {
		names = []string{topic.Name, id.Key()}
		description = topic.Summary
	}
	for _, name := range names {
		if name != "" && graph.TopicID(name) == id {
			return graph.Label{Name: name, Description: description}, true
		}
	}
	return graph.Label{}, false
}
