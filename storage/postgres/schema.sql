-- The grafsol schema.
--
-- CreateSchema runs this file as it stands, after replacing {{dimensions}}
-- with the embedding length it was given. Every statement is idempotent, so
-- running the file again changes nothing.

-- pgvector supplies the vector type, cosine distance and the HNSW index.
-- Version 0.8 or later is needed.
create extension if not exists vector;


-- VERTICES: one row per document or topic.
--
-- attrs holds every field of the vertex except its ID and its embedding, as
-- JSON, so that adding a field to a vertex type needs no migration. The
-- embedding has a column of its own because that is what pgvector indexes.
--
-- collate "C" makes Postgres compare IDs byte by byte. That is the order Go
-- uses for strings, so pages of results come back in the same order from
-- every storage backend, and it is also the cheapest comparison there is.
create table if not exists vertices (
    id        text collate "C" primary key,  -- 'doc:github:acme/api#42', 'topic:billing'
    kind      text  not null,                -- 'doc' or 'topic'
    attrs     jsonb not null,
    embedding vector({{dimensions}})         -- null until the vertex has been embedded
);

-- Similarity search over documents. The index covers documents only, so that
-- a search for documents is not diluted by the vectors of topics.
create index if not exists vertices_doc_embedding
    on vertices using hnsw (embedding vector_cosine_ops)
    where kind = 'doc';


-- THE EMBEDDING SPACE: which model, set up which way, made the embeddings in
-- vertices. Embeddings can be compared only if one such space holds all of
-- them, so the graph records the first that is claimed for it and holds to
-- it. The table has one row at most, written once and never changed; the
-- primary key, on a column that can only be true, is what keeps it to one.
create table if not exists embedding_space (
    only_row boolean primary key default true check (only_row),
    space    text    not null check (space <> '')   -- the embedder's name for it
);


-- EDGES: one row per edge.
--
-- An edge is stored once and found from either end through an index: the
-- primary key serves "edges leaving src", and edges_in serves "edges
-- arriving at dst". There is no second copy of an edge to keep in step.
--
-- There is deliberately no foreign key to vertices: an edge may point at a
-- document that has not been ingested yet.
create table if not exists edges (
    src     text collate "C" not null,
    kind    text collate "C" not null,       -- 'links_to', 'child_of', ...
    dst     text collate "C" not null,
    summary text not null default '',        -- why the edge exists, in a sentence
    primary key (src, kind, dst)
);

create index if not exists edges_in on edges (dst, kind, src);


-- THE LEDGER: what the sync engine remembers, kept apart from the graph.

-- The ledger's clock: a single row. Tick adds one to it, which the sync
-- engine does each time a scan of a source begins; Record reads it. The
-- primary key, on a column that can only be true, is what keeps the table
-- to one row.
create table if not exists ledger_clock (
    only_row boolean primary key default true check (only_row),
    tick     bigint  not null
);

insert into ledger_clock (tick) values (1) on conflict do nothing;

-- One row per document the sync engine has ever applied or removed.
create table if not exists ledger_marks (
    id          text collate "C" primary key,
    source      text    not null,            -- 'github', 'jira', ...
    version     bigint  not null,            -- the bits of a uint64; compared in Go, never in SQL
    fingerprint text    not null,            -- hash of the state that was applied
    deleted     boolean not null,            -- a tombstone: the document was removed
    seen        bigint  not null             -- what ledger_clock read when the row was last recorded
);

-- Serves Unseen: "live documents of a source not recorded since the clock
-- read N".
create index if not exists ledger_marks_unseen
    on ledger_marks (source, seen, id)
    where not deleted;

-- Named, opaque positions: how far each feed has been read.
create table if not exists ledger_checkpoints (
    name       text  primary key,
    checkpoint bytea not null
);
