"""grafsol's own model services.

Two services, each a BentoML service in its own process:

  - Embeddings turns texts into vectors, so that documents can be found by
    meaning.
  - Summaries writes the short description of a document that the graph keeps
    in place of the document's text.

Both run open models from Hugging Face on this machine. Nothing here calls a
paid API. Start reading at services.py.
"""
