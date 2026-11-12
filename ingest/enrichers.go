package ingest

import (
	"context"
	"fmt"
	"strings"

	"github.com/umran/grafsol/model"
	"github.com/umran/grafsol/source"
	"github.com/umran/grafsol/storage"
)

// Summarize returns an Enricher that writes the summary of each changed
// document with s, from the document's title and text.
//
// Two kinds of document are left as they are: one whose source supplied a
// summary of its own, and one whose text says nothing that its title does
// not, where there is nothing to summarise beyond the title the graph
// already keeps. That covers a document with no text at all, and one whose
// text is only its title again: connectors put the title at the head of the
// text, so that is what a pull request with no description looks like.
func Summarize(s model.Summarizer) Enricher {
	return EnricherFunc(func(ctx context.Context, c *source.Change) error {
		if c.Document.Summary != "" || beyondTitle(c.Document.Title, c.Content) == "" {
			return nil
		}
		summary, err := s.Summarize(ctx, c.Document.Title, c.Content)
		if err != nil {
			return err
		}
		c.Document.Summary = summary
		return nil
	})
}

// beyondTitle returns what a document's text says once its title, if the
// text begins with it, is set aside.
func beyondTitle(title, content string) string {
	content = strings.TrimSpace(content)
	return strings.TrimSpace(strings.TrimPrefix(content, strings.TrimSpace(title)))
}

// Embed returns an Enricher that embeds each changed document, and the
// topics the change describes, with e.
//
// What is embedded is what the graph keeps and what a search will later
// read: a document's title and summary, a topic's name and summary. List
// Embed after Summarize, so that the summary exists by the time it is
// embedded. The document and its topics go to the model in one call.
//
// Embed writes the name of its embedder's embedding space on the change,
// which is how the Syncer knows what the graph is being given. If the change
// already carries embeddings from another space, Embed adds none and fails
// the change: one change cannot hold embeddings from two.
func Embed(e model.Embedder) Enricher {
	return EnricherFunc(func(ctx context.Context, c *source.Change) error {
		// Each text is paired with the place its vector belongs.
		var texts []string
		var places []*[]float32
		add := func(place *[]float32, title, summary string) {
			text := strings.TrimSpace(title + "\n\n" + summary)
			if len(*place) == 0 && text != "" {
				texts = append(texts, text)
				places = append(places, place)
			}
		}
		add(&c.Document.Embedding, c.Document.Title, c.Document.Summary)
		for i := range c.Topics {
			add(&c.Topics[i].Embedding, c.Topics[i].Name, c.Topics[i].Summary)
		}
		if len(texts) == 0 {
			return nil
		}

		space, err := e.Space(ctx)
		if err != nil {
			return err
		}
		if c.EmbeddingSpace != "" && c.EmbeddingSpace != space {
			return fmt.Errorf("%w: the change already carries embeddings in %q, and the embedder's are in %q", storage.ErrOtherSpace, c.EmbeddingSpace, space)
		}

		vectors, err := e.EmbedDocuments(ctx, texts)
		if err != nil {
			return err
		}
		if len(vectors) != len(texts) {
			return fmt.Errorf("ingest: the embedder returned %d vectors for %d texts", len(vectors), len(texts))
		}
		for i, vector := range vectors {
			*places[i] = vector
		}
		c.EmbeddingSpace = space
		return nil
	})
}
