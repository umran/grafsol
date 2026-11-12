package postgres

// Every SQL statement this package sends, in one place. The tables and
// indexes they rely on are defined in schema.sql.

// --- vertices ---------------------------------------------------------------

const (
	// upsertVertex writes one vertex.
	//
	// The embedding travels as an array of reals and is cast to a vector on
	// arrival; that keeps the Go side free of pgvector-specific types.
	//
	// The final WHERE skips the write when the stored row already holds
	// exactly these values, so re-applying unchanged data leaves no dead
	// row versions behind for vacuum to clean up.
	upsertVertex = `
insert into vertices (id, kind, attrs, embedding)
values ($1, $2, $3, $4::real[]::vector)
on conflict (id) do update
   set kind = excluded.kind,
       attrs = excluded.attrs,
       embedding = excluded.embedding
 where (vertices.kind, vertices.attrs, vertices.embedding)
       is distinct from (excluded.kind, excluded.attrs, excluded.embedding)`

	selectVertices = `
select id, kind, attrs, embedding::real[]
  from vertices
 where id = any($1)`

	deleteVertices = `
delete from vertices
 where id = any($1)`
)

// --- the embedding space ------------------------------------------------------

const (
	// claimSpace records the graph's embedding space if none is recorded
	// yet, and otherwise does nothing at all: it rewrites no row. It is
	// always followed by selectSpace, which shows whose claim stands.
	//
	// When two claims arrive at once, the second waits here for the first
	// to be committed, then finds the row and stands back. Its selectSpace,
	// being a statement of its own, sees the first claim's row.
	claimSpace = `
insert into embedding_space (space)
values ($1)
on conflict (only_row) do nothing`

	selectSpace = `
select space
  from embedding_space`
)

// --- edges ------------------------------------------------------------------

const (
	// upsertEdges writes a whole batch of edges in one statement. The four
	// arrays are columns of the batch: element i of each describes edge i.
	upsertEdges = `
insert into edges (src, kind, dst, summary)
select * from unnest($1::text[], $2::text[], $3::text[], $4::text[])
on conflict (src, kind, dst) do update
   set summary = excluded.summary
 where edges.summary is distinct from excluded.summary`

	deleteEdges = `
delete from edges
 using unnest($1::text[], $2::text[], $3::text[]) as doomed (src, kind, dst)
 where edges.src = doomed.src
   and edges.kind = doomed.kind
   and edges.dst = doomed.dst`
)

// Paging through the edges of one vertex.
//
// Each statement returns the rows that come after a position, in order, and
// stops at a limit. "After a position" is what makes paging cheap: Postgres
// descends the index straight to that position instead of counting past the
// rows of earlier pages. The first page passes empty strings, which sort
// before everything.
//
// The two "out" statements read the primary key (src, kind, dst); the two
// "in" statements read the edges_in index (dst, kind, src).
const (
	// $1 vertex, ($2, $3) the kind and far vertex the last page ended at, $4 limit.
	outEdges = `
select src, kind, dst, summary
  from edges
 where src = $1 and (kind, dst) > ($2, $3)
 order by kind, dst
 limit $4`

	// $1 vertex, $2 kind, $3 the far vertex the last page ended at, $4 limit.
	outEdgesOfKind = `
select src, kind, dst, summary
  from edges
 where src = $1 and kind = $2 and dst > $3
 order by dst
 limit $4`

	inEdges = `
select src, kind, dst, summary
  from edges
 where dst = $1 and (kind, src) > ($2, $3)
 order by kind, src
 limit $4`

	inEdgesOfKind = `
select src, kind, dst, summary
  from edges
 where dst = $1 and kind = $2 and src > $3
 order by src
 limit $4`
)

// Expanding a whole frontier at once: every edge that touches any of the
// vertices in $1. There is no limit and no ordering; the rows are streamed
// to the caller as Postgres produces them.
const (
	expandOut = `
select src, kind, dst, summary
  from edges
 where src = any($1)`

	expandOutOfKind = `
select src, kind, dst, summary
  from edges
 where src = any($1) and kind = $2`

	expandIn = `
select src, kind, dst, summary
  from edges
 where dst = any($1)`

	expandInOfKind = `
select src, kind, dst, summary
  from edges
 where dst = any($1) and kind = $2`
)

// --- similarity search ------------------------------------------------------

const (
	// nearestDocuments ranks documents by cosine distance (the <=> operator)
	// from the query vector. "kind = 'doc'" matches the condition of the
	// vertices_doc_embedding index, which is what lets Postgres use it.
	nearestDocuments = `
select id, embedding <=> $1::real[]::vector as distance
  from vertices
 where kind = 'doc' and embedding is not null
 order by distance
 limit $2`

	// setLocal changes a setting until the end of the current transaction.
	setLocal = `select set_config($1, $2, true)`

	// rankedNeighbors pages through the edges of vertex $1, from both ends,
	// each with the vertex at its far end, nearest first to the vector $2.
	//
	// The innermost query gathers the edges: those that leave the vertex
	// (direction 1, read from the primary key) and those that arrive at it
	// (direction 2, read from edges_in), without the edges that run from the
	// vertex to itself. Joining each to the vertex at its far end drops the
	// edges that point at nothing.
	//
	// The distance is 2, the farthest there is, wherever there is nothing to
	// measure: a far end without an embedding, no $2, or an embedding of
	// zeroes, for which <=> answers NaN.
	//
	// ($3, $4, $5, $6) is where the previous page ended, in the order the
	// rows are listed in.
	rankedNeighbors = `
select direction, kind, neighbor, summary, vertex_kind, attrs, distance
  from (select n.direction, n.kind, n.neighbor, n.summary, v.kind as vertex_kind, v.attrs,
               coalesce(nullif(v.embedding <=> $2::real[]::vector, 'NaN'), 2) as distance
          from (select 1 as direction, kind, dst as neighbor, summary
                  from edges
                 where src = $1 and dst <> $1
                 union all
                select 2 as direction, kind, src as neighbor, summary
                  from edges
                 where dst = $1 and src <> $1) n
          join vertices v on v.id = n.neighbor) ranked
 where (distance, kind, neighbor, direction) > ($3, $4, $5, $6)
 order by distance, kind, neighbor, direction
 limit $7`
)

// --- ledger -----------------------------------------------------------------

const (
	selectMark = `
select source, version, fingerprint, deleted, seen
  from ledger_marks
 where id = $1`

	// upsertMark records a mark and stamps it with what the ledger's clock
	// reads. A mark that is already stored exactly so, stamp included, is
	// left as it is rather than rewritten.
	upsertMark = `
insert into ledger_marks (id, source, version, fingerprint, deleted, seen)
values ($1, $2, $3, $4, $5, (select tick from ledger_clock))
on conflict (id) do update
   set source = excluded.source,
       version = excluded.version,
       fingerprint = excluded.fingerprint,
       deleted = excluded.deleted,
       seen = excluded.seen
 where (ledger_marks.source, ledger_marks.version, ledger_marks.fingerprint, ledger_marks.deleted, ledger_marks.seen)
       is distinct from (excluded.source, excluded.version, excluded.fingerprint, excluded.deleted, excluded.seen)`

	// nextTick advances the ledger's clock and returns the new reading.
	nextTick = `update ledger_clock set tick = tick + 1 returning tick`

	// unseenMarks pages, in (seen, id) order, through the live documents of
	// a source last recorded before the clock read $2. ($3, $4) is where the
	// previous page ended. It reads the ledger_marks_unseen index.
	unseenMarks = `
select id, seen
  from ledger_marks
 where source = $1 and not deleted and seen < $2 and (seen, id) > ($3, $4)
 order by seen, id
 limit $5`

	selectCheckpoint = `
select checkpoint
  from ledger_checkpoints
 where name = $1`

	upsertCheckpoint = `
insert into ledger_checkpoints (name, checkpoint)
values ($1, $2)
on conflict (name) do update
   set checkpoint = excluded.checkpoint`

	// lockKey waits for, then holds until the transaction ends, a lock named
	// by the hash of $1. Two different keys that happen to share a hash will
	// wait for each other needlessly, which is harmless.
	lockKey = `select pg_advisory_xact_lock(hashtextextended($1, 0))`
)
