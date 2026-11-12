-- export.sql writes graph.json: a frozen copy of the graph in a Postgres
-- database, for the test set in this folder. It prints one JSON document that
-- holds every document, every topic and every edge, without embeddings (the
-- test set keeps its vectors in vectors.json).
--
--   psql "$GRAFSOL_POSTGRES_URL" -At -f eval/export.sql > eval/graph.json
--
-- Nothing is left out and nothing is cut short: there is no limit anywhere
-- below. Rows come out in the byte order of their IDs, the order the stores
-- themselves use, one to a line, so that two exports can be compared with
-- diff. Run `grafsol-eval check` afterwards: it reads the file back and says
-- how many of each it holds.
select
    E'{\n  "documents": [\n    ' ||
    coalesce((
        select string_agg(json_build_object(
                   'id',         id,
                   'type',       attrs->>'type',
                   'source',     attrs->>'source',
                   'url',        attrs->>'url',
                   'title',      attrs->>'title',
                   'summary',    attrs->>'summary',
                   'created_at', attrs->>'created_at',
                   'updated_at', attrs->>'updated_at'
               )::text, E',\n    ' order by id)
          from vertices
         where kind = 'doc'
    ), '') ||
    E'\n  ],\n  "topics": [\n    ' ||
    coalesce((
        select string_agg(json_build_object(
                   'id',      id,
                   'name',    attrs->>'name',
                   'summary', attrs->>'summary'
               )::text, E',\n    ' order by id)
          from vertices
         where kind = 'topic'
    ), '') ||
    E'\n  ],\n  "edges": [\n    ' ||
    coalesce((
        select string_agg(json_build_object(
                   'from',    src,
                   'kind',    kind,
                   'to',      dst,
                   'summary', summary
               )::text, E',\n    ' order by src, kind, dst)
          from edges
    ), '') ||
    E'\n  ]\n}';
