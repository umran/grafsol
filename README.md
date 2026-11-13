# grafsol

grafsol is a knowledge graph of documents: issues, pull requests, tickets and
wiki pages from the systems a team works in, joined by simple typed edges,
kept in step with those systems, and searched to answer questions.

A question is answered by a best-first search. It starts at the documents
nearest the question and follows the most promising edges, asking a judge at
each step what the leads in front of it are worth and whether the evidence
gathered so far is enough. What comes back is not an answer but the material
for one: the documents that state it, with the path of links that led to each.
AI agents ask through an MCP server.

It is a Go module, with a small Python project beside it for the two models it
runs on your own machine.

> [!NOTE]
> This is early work. It runs end to end on one machine, GitHub is the only
> connector so far, and the search's thresholds have not been tuned yet. See
> [What is not here yet](#what-is-not-here-yet).

**Contents**

- [How it fits together](#how-it-fits-together)
- [The graph](#the-graph)
- [Keeping the graph in step](#keeping-the-graph-in-step)
- [One question, one search](#one-question-one-search)
- [An agent asks over MCP](#an-agent-asks-over-mcp)
- [Measuring the judge](#measuring-the-judge)
- [Running it](#running-it)
- [The module](#the-module)
- [What is not here yet](#what-is-not-here-yet)

## How it fits together

Two paths meet at the graph. One writes it: the sync engine takes changes from
the systems that own the documents and makes the graph reflect them. The other
reads it: a search, started by an agent's question, gathers evidence from it.

```mermaid
flowchart TB
    subgraph all[" "]
        direction LR
        agent(["AI agent"]):::outside
        sources["Source systems<br/>GitHub today"]:::outside
        local["Local models<br/>summaries,<br/>embeddings"]:::local
        mcp["MCP server<br/>grafsol-mcp"]:::code
        syncer["Sync engine<br/>ingest.Syncer"]:::code
        engine["Search engine<br/>retrieval.Engine"]:::code
        store[("The graph<br/>Postgres,<br/>pgvector")]:::data
        jev("Jev<br/>the judge"):::paid

        agent -- "questions" --> mcp
        mcp -- "searches" --> engine
        engine -- "reads" --> store
        sources -- "changes" --> syncer
        syncer -- "writes" --> store
        local <-.-> engine
        local <-.-> syncer
        jev <-. "judges" .-> engine
    end

    %% GitHub lays its zoom buttons over the bottom right corner, which this layout leaves empty.

    classDef outside fill:#e2e8f0,stroke:#64748b,color:#0f172a
    classDef code fill:#dbeafe,stroke:#3b82f6,color:#0f172a
    classDef data fill:#dcfce7,stroke:#22c55e,color:#0f172a
    classDef local fill:#fef3c7,stroke:#f59e0b,color:#0f172a
    classDef paid fill:#fae8ff,stroke:#c026d3,color:#0f172a
    style all fill:none,stroke:none
```

The dotted lines are calls to models. The sync engine has each document
summarised and embedded, and the search engine has each question embedded and
asks Jev to judge.

The colours mean the same in every diagram below: grey is outside grafsol, blue
is grafsol's code, green is what is stored, amber is a model on your machine,
and purple is Jev.

Three models are involved, and only one of them is paid for:

| Model | Where it runs | What it does |
| --- | --- | --- |
| `Qwen/Qwen3-4B-Instruct-2507` | your machine | writes the summary of each document |
| `microsoft/harrier-oss-v1-0.6b` | your machine | embeds documents and questions, as 1,024 numbers each |
| Jev, a System One model from [TypeSafe](https://docs.typesafe.ai) | TypeSafe's API | judges the search: rates leads and weighs the evidence |

Where your documents go:

- **The sources** are read through their own APIs.
- **TypeSafe** is sent the question and its context, and the titles,
  summaries, types, URLs and link notes of the vertices being judged. It is
  never sent a document's text: the graph does not keep any.
- **The agent that asks** receives the evidence: titles, summaries, URLs and
  the links between them.

Summaries and embeddings are made on your machine. The model services run
offline once the models are downloaded, and the Makefile that starts them
switches off the usage reporting of BentoML and Hugging Face.

## The graph

A vertex is a document or a topic. An edge runs from one vertex to another and
has a kind. This made-up example shows all four:

```mermaid
flowchart TB
    subgraph all[" "]
        direction LR
        pr["Pull request 9<br/>Cache prices"]:::doc
        sub["Issue 12<br/>Add a load test"]:::doc
        issue["Issue 7<br/>Slow checkout"]:::doc
        checkout(["Topic<br/>checkout"]):::topic
        perf(["Topic<br/>performance"]):::topic

        pr -- "links_to<br/>fixes the slow query in" --> issue
        sub -- "child_of<br/>sub-issue of" --> issue
        issue -- "in_topic" --> checkout
        issue -- "tagged_with" --> perf
        pr -- "tagged_with" --> perf
    end

    %% An empty row at the bottom: GitHub lays its zoom buttons over that corner.
    all ~~~ corner["&nbsp;<br/>&nbsp;<br/>&nbsp;"]

    classDef doc fill:#dcfce7,stroke:#22c55e,color:#0f172a
    classDef topic fill:#ccfbf1,stroke:#14b8a6,color:#0f172a
    style all fill:none,stroke:none
    style corner fill:none,stroke:none
```

| Edge | Runs | Means |
| --- | --- | --- |
| `links_to` | document to document | the first references the second |
| `child_of` | document to document | the first sits under the second |
| `in_topic` | document to topic | the document belongs to the topic |
| `tagged_with` | document to topic | the document carries the topic as a tag |

What each thing carries:

- **A document** has a type, a URL, a title, a **summary** and an
  **embedding**. The graph keeps what is needed to find a document and to
  judge its relevance, not its full text: the URL points back to the original.
  Its ID says where it came from, as in `doc:github:jackc/puddle#18`.
- **A topic** has a name, a summary and an embedding. Topics are hubs: they
  join documents that never mention each other.
- **An edge** may have a note. For a link, it is the text around the link.

The summaries and notes are what a search reads to decide where to go next.

Embeddings can be compared only if one model, set up one way, made all of
them. So a graph holds to one embedding space and records which: the sync
engine will not store an embedding from another, and the search engine will
not search with one.

## Keeping the graph in step

Package [`ingest`](ingest). A source reports every change the same way: the
complete state of one document at one moment, or word that the document is
gone. It never reports "the title changed". That one decision is what lets the
graph converge on the truth when changes are lost, repeated or arrive out of
order: applying the newest change makes the document right, whatever came
before.

Systems differ in how they let changes be observed, so a connector implements
whichever of three capabilities its system has, and the sync engine has a way
in for each:

```mermaid
flowchart LR
    subgraph system["A source system offers"]
        log["a change log<br/>source.Subscriber"]:::outside
        push["webhooks<br/>source.Receiver"]:::outside
        listing["a full listing<br/>source.Scanner"]:::outside
    end

    subgraph syncer["ingest.Syncer"]
        follow["Follow<br/>resumes from<br/>a checkpoint"]:::code
        deliver["Deliver<br/>checks it is<br/>authentic"]:::code
        reconcile["Reconcile<br/>fills, and<br/>later repairs"]:::code
        apply["Apply<br/>one change<br/>at a time"]:::code
    end

    store[("The graph")]:::data
    ledger[("The ledger")]:::data

    log --> follow --> apply
    push --> deliver --> apply
    listing --> reconcile --> apply
    apply --> store
    apply <--> ledger

    classDef outside fill:#e2e8f0,stroke:#64748b,color:#0f172a
    classDef code fill:#dbeafe,stroke:#3b82f6,color:#0f172a
    classDef data fill:#dcfce7,stroke:#22c55e,color:#0f172a
```

A change feed, by subscription or by push, is the preferred way to stay
current: it is prompt and cheap. A scan is the safety net under it. It fills
the graph the first time, and run periodically it repairs whatever the feed
missed.

All three ways in end in `Apply`, which turns one change into graph writes.
First the URLs found in the document become edges: every source is asked
whether a URL is one of its own, so a link can cross from one system into
another. Then the ledger says whether the change is news:

```mermaid
flowchart TB
    subgraph all[" "]
        direction LR
        links["Resolve links<br/>URLs become<br/>edges"]:::code
        news["Ask the ledger:<br/>is it news?"]:::data
        old(["Nothing<br/>is written"]):::outside
        enrich["Summarise,<br/>then embed"]:::local
        lock["Lock it, ask<br/>the ledger<br/>again"]:::code
        write["Write edges,<br/>topics and the<br/>document; the<br/>ledger last"]:::data
        remove["Remove it,<br/>leave a<br/>tombstone"]:::data

        links --> news
        news -- "old news" --> old
        news -- "new" --> enrich --> lock
        news -- "gone" --> lock
        lock -- "new" --> write
        lock -- "gone" --> remove
    end

    %% An empty row at the bottom: GitHub lays its zoom buttons over that corner.
    all ~~~ corner["&nbsp;<br/>&nbsp;<br/>&nbsp;"]

    classDef outside fill:#e2e8f0,stroke:#64748b,color:#0f172a
    classDef code fill:#dbeafe,stroke:#3b82f6,color:#0f172a
    classDef data fill:#dcfce7,stroke:#22c55e,color:#0f172a
    classDef local fill:#fef3c7,stroke:#f59e0b,color:#0f172a
    style all fill:none,stroke:none
    style corner fill:none,stroke:none
```

The models run before the lock is taken, because they can take seconds. The
ledger is written last, so a process that dies part-way leaves a change that
simply counts as not yet applied.

Changes may be lost, repeated, reordered or handled by several workers at
once. Each of those has its own remedy:

| What goes wrong | Why the graph still ends up right |
| --- | --- |
| A change is delivered twice | The ledger keeps a fingerprint of what was applied, so a repeat does nothing. |
| Changes arrive out of order | Every change carries a version, and one older than what the ledger has seen is ignored. |
| An edit arrives after the removal | A removal leaves a tombstone in the ledger, and an edit no newer than it is ignored. |
| Two workers take one document | The ledger's lock lets one go at a time. |
| A change is lost | A `Reconcile` pass re-reads the source and applies what differs. |
| A removal is lost | `Reconcile` removes documents the source no longer lists, but only after two passes in a row have missed them: a listing that changes while it is read can skip a document. |
| The process dies mid-change | The ledger is written last, so the change is applied again from the top. |

Once a source stops changing and two `Reconcile` passes have run to completion,
the graph holds exactly that source's documents and their edges.

There is no ready-made sync service yet. `ingest.Syncer` is the engine, and
your program decides how to drive it:

```go
syncer := ingest.NewSyncer(ingest.Config{
    Store:     store,
    Ledger:    ledger,
    Sources:   []source.Source{repo},
    Enrichers: []ingest.Enricher{ingest.Summarize(summarizer), ingest.Embed(embedder)},
})

report, err := syncer.Reconcile(ctx, repo)                 // fill the graph, or repair it
http.Handle("POST /webhooks/github", syncer.Handler(repo)) // stay current
```

## One question, one search

Package [`retrieval`](retrieval); the loop is in
[`retrieval/search.go`](retrieval/search.go). A search starts from the 8
documents nearest the question, and from any anchors the caller names. Then it
goes round a loop, one call to the judge each time, until one of three things
ends it.

```mermaid
flowchart TB
    subgraph all[" "]
        direction LR
        seed["Embed the<br/>question, start<br/>at the 8 nearest"]:::code
        judge("Ask the judge<br/>rate new leads,<br/>weigh evidence"):::paid
        follow["Follow the 3<br/>best leads at<br/>0.25 or more"]:::code
        sufficient(["sufficient"]):::outside
        exhausted(["exhausted"]):::outside
        stopped(["stopped"]):::outside

        seed --> judge
        judge -- "enough" --> sufficient
        judge -- "not yet" --> follow
        follow -- "new leads, and<br/>evidence at 0.75" --> judge
        follow -- "none left" --> exhausted
        follow -- "a limit" --> stopped
    end

    %% An empty row at the bottom: GitHub lays its zoom buttons over that corner.
    all ~~~ corner["&nbsp;<br/>&nbsp;<br/>&nbsp;"]

    classDef outside fill:#e2e8f0,stroke:#64748b,color:#0f172a
    classDef code fill:#dbeafe,stroke:#3b82f6,color:#0f172a
    classDef paid fill:#fae8ff,stroke:#c026d3,color:#0f172a
    style all fill:none,stroke:none
    style corner fill:none,stroke:none
```

A lead is a vertex the search could visit next, and how it would get there. The
judge is shown the vertex's title, summary, type and URL and, for a lead at the
end of an edge, how it is joined and the note on the link. Every step makes at
most one call to the judge, and that call asks two kinds of question:

| Asked of Jev | Comes back as | The search uses it to |
| --- | --- | --- |
| For each new lead: how much does this candidate help to answer the question? It has nothing to do with it, or it is on the subject but states nothing that answers it, or it states a fact that answers it. | a rating from 0 to 1 | follow a lead rated 0.25 or more, and count a followed document as evidence at 0.75 or more |
| For the evidence so far: does it state every fact needed to answer the question? | a likelihood from 0 to 1 | stop at 0.7 or more |

A search ends in one of three ways, and says which:

| Verdict | Means |
| --- | --- |
| `sufficient` | The judge found that the evidence states everything the answer needs. |
| `exhausted` | The search ran out of leads worth following first. The evidence is the best the graph has, and may answer only part of the question. |
| `stopped` | A limit ended the search early: 12 steps by default, or a time limit. The findings say how much was known of and not reached. |

Every number above is a default in `retrieval.Config`, and none of them has
been tuned yet. [Measuring the judge](#measuring-the-judge) is how they will
be.

## An agent asks over MCP

[`cmd/grafsol-mcp`](cmd/grafsol-mcp) serves one tool, `gather`. An agent whose
task has several parts asks all its questions in one call. They are embedded
together and searched side by side:

```mermaid
%%{init: {"sequence": {"width": 110, "actorMargin": 30, "mirrorActors": false}}}%%
sequenceDiagram
    autonumber
    participant A as AI agent
    participant M as grafsol-mcp
    participant E as Search engine
    participant V as Embeddings
    participant G as The graph
    participant J as Jev

    A->>M: gather(questions)
    M->>E: GatherAll
    E->>G: which embedding space?
    E->>V: embed the questions
    V-->>E: a vector each

    par One search per question, up to 8 at a time
        loop Until sufficient, exhausted or stopped
            E->>G: nearest documents, neighbours
            E->>J: rate the leads, weigh the evidence
            J-->>E: ratings, sufficiency
        end
    end

    E-->>M: a finding each
    M-->>A: verdicts, evidence
```

The searches share nothing but the graph, and one that fails does not disturb
the others. Each may run for 30 seconds by default (`-timeout`); a search that
runs out of time returns what it has, with the verdict `stopped`.

| Argument | What it is for |
| --- | --- |
| `questions` | The questions, in natural language. Each should ask one thing. |
| `context` | What the questions are for: the task, and the system, team or period they concern. It helps to tell relevant documents from merely similar ones. |
| `anchors` | IDs of documents or topics to start every search from, as given in the evidence of an earlier call. |
| `trace` | Also return every step of each search: which leads were judged, how each was rated, which were followed. |

This is a real result, shortened, from a graph of the issues and pull requests
of [jackc/puddle](https://github.com/jackc/puddle). The second document was
not among the starting points: the search reached it by following the link
from the first, and says so in `path`.

```json
{
  "findings": [
    {
      "question": "What problem was the pull request that uses atomic operations to avoid locking written to solve?",
      "verdict": "sufficient",
      "sufficient": 0.86,
      "evidence": [
        {
          "id": "doc:github:jackc/puddle#18",
          "type": "pull_request",
          "title": "Use atomic operations to avoid locking as much as possible",
          "url": "https://github.com/jackc/puddle/pull/18",
          "summary": "Used atomic operations to reduce locking in a code change related to GitHub issue https://github.com/jackc/puddle/issues/17. ...",
          "potential": 0.83,
          "path": []
        },
        {
          "id": "doc:github:jackc/puddle#17",
          "type": "issue",
          "title": "The function Acquire does not return immediately，when ctx Done()",
          "url": "https://github.com/jackc/puddle/issues/17",
          "summary": "The Acquire function in pool.go does not return immediately when ctx.Done() is called due to a lock attempt before returning. ...",
          "potential": 0.955,
          "path": [
            {
              "from": "doc:github:jackc/puddle#18",
              "kind": "links_to",
              "to": "doc:github:jackc/puddle#17",
              "summary": "For this issue：https://github.com/jackc/puddle/issues/17"
            }
          ]
        }
      ],
      "left": { "unjudged": 0, "unfollowed": 0, "unread": 0 }
    }
  ]
}
```

The text in a result is quoted from source documents, or summarised from them.
The tool says so to the agent, and tells it to treat that text as data and not
as instructions.

## Measuring the judge

The judge's wording and the three thresholds decide how good a search is. Jev's
answers cost money, and they differ a little from one asking to the next, so
neither can be tuned by trying a question or two by hand.

[`/eval`](eval) is a test set to tune them against: a frozen copy of a graph
of 77 documents (the issues and pull requests of two public repositories,
`jackc/puddle` and `tidwall/btree`) and 46 questions about it. Each question
lists the facts that answer it and, for each fact, the exact words in the
graph that state it. Thirteen questions are held back, to confirm that a change
made on the strength of the others was not fitted to them.

```mermaid
flowchart TB
    subgraph all[" "]
        direction LR
        set[("The test set<br/>a frozen graph<br/>and questions<br/>with answers")]:::data
        record["record -live<br/>the only step<br/>that costs"]:::code
        jev("Jev"):::paid
        tape[("The tape<br/>every answer,<br/>filed under<br/>its question")]:::data
        report["report<br/>replays the<br/>tape, free"]:::code

        set --> record
        record <-- "what the tape lacks" --> jev
        record -- "answers" --> tape
        set --> report
        tape --> report
    end

    %% An empty row at the bottom: GitHub lays its zoom buttons over that corner.
    all ~~~ corner["&nbsp;<br/>&nbsp;<br/>&nbsp;"]

    classDef code fill:#dbeafe,stroke:#3b82f6,color:#0f172a
    classDef data fill:#dcfce7,stroke:#22c55e,color:#0f172a
    classDef paid fill:#fae8ff,stroke:#c026d3,color:#0f172a
    style all fill:none,stroke:none
    style corner fill:none,stroke:none
```

Anything asked again in the same words is answered from the tape. So money
buys only new words: a new wording, a new question, a new model. Trying another
threshold, or a change to the search itself, replays the tape. That is free and
gives the same answers every time.

```bash
go run ./cmd/grafsol-eval check                      # load the set; check every label and quote
go run ./cmd/grafsol-eval record -model jev-1.14.0   # say what the tape lacks, and what it would cost
go run ./cmd/grafsol-eval record -model jev-1.14.0 -live -budget 12000000
go run ./cmd/grafsol-eval report -model jev-1.14.0   # replay the tape and report
```

Only `record -live` sends anything to TypeSafe. It needs `TYPESAFE_API_KEY`, a
named model version and a budget in input tokens that it will not go past.

The first recording has not been made yet, so the repository holds no tape and
there are no results to show.

## Running it

You need Go 1.25 or newer, Docker, [uv](https://docs.astral.sh/uv/) for the
Python project, and a TypeSafe API key for Jev. The two models take about 9 GB
of disk. So far all of this has been run on one Apple-silicon Mac.

**1. Start the database**, Postgres with pgvector:

```bash
docker compose up -d --wait
export GRAFSOL_POSTGRES_URL=postgres://grafsol:grafsol@localhost:54329/grafsol
```

**2. Start the two model services.** The first command downloads the models,
once:

```bash
(cd inference && make models)
(cd inference && make embeddings)   # in a terminal of its own, at http://127.0.0.1:3010
(cd inference && make summaries)    # in another, at http://127.0.0.1:3011
```

**3. Fill a graph and ask it something.** The example copies the issues and
pull requests of one or more GitHub repositories into the graph, summarising
and embedding each on the way:

```bash
export TYPESAFE_API_KEY=...   # only needed with -question
export GITHUB_TOKEN=...       # optional for public repositories
go run ./examples/github -repository owner/name -question "Why was X changed?"
go run ./examples/github -repository owner/api,owner/web   # several, into one graph
```

A summary takes a local model several seconds, so try a small repository first.
Later runs only touch what has changed.

> [!WARNING]
> Name every repository on every run. The example files them all under one
> source, and the sync engine takes a document that two runs in a row did not
> list to be gone: leave a repository out twice and its documents are removed
> from the graph. To keep two sets of repositories apart, give each set a
> database of its own.

**4. Put the graph in front of an agent.** Build the server and name it in your
MCP client's configuration. Most clients take this form:

```bash
go build -o grafsol-mcp ./cmd/grafsol-mcp
```

```json
{
  "mcpServers": {
    "grafsol": {
      "command": "/path/to/grafsol-mcp",
      "env": {
        "GRAFSOL_POSTGRES_URL": "postgres://grafsol:grafsol@localhost:54329/grafsol",
        "GRAFSOL_EMBEDDINGS_URL": "http://127.0.0.1:3010",
        "TYPESAFE_API_KEY": "..."
      }
    }
  }
}
```

The server talks over standard input and output by default. With
`-http 127.0.0.1:8080` it serves the same tool over HTTP. It does no
authentication of its own, so bind it only where trusted callers can reach it.

**Tests**

```bash
go test ./...                       # needs nothing running
GRAFSOL_TEST_POSTGRES_URL=$GRAFSOL_POSTGRES_URL go test ./...   # also the Postgres tests, in schemas of their own
(cd inference && make test check)   # the Python project
```

## The module

Imports only ever point upward in this list, so each package can be read
knowing only the ones above it. [`doc.go`](doc.go) says more.

| Package | What it is |
| --- | --- |
| [`graph`](graph) | The vocabulary: documents, topics, edges and their IDs. Pure data. |
| [`storage`](storage) | What it takes to keep a graph: a `Store`, a `DocumentSearcher`, a `NeighborSearcher` and a `Ledger`. `storage/memory` is for tests and small graphs, `storage/postgres` for real ones, and both pass the suites in `storage/storagetest`. |
| [`traverse`](traverse) | Walks the graph breadth-first, over any store. |
| [`source`](source) | What a connector to an external system provides. `source/github` is the first. |
| [`model`](model) | What the module needs from language models: an `Embedder` and a `Summarizer`. `model/inference` asks the two local services. |
| [`ingest`](ingest) | Keeps the graph in step with its sources. |
| [`typesafe`](typesafe) | A client for Jev. |
| [`retrieval`](retrieval) | The search. `retrieval/jev` is the judge built on Jev. |
| [`internal/evaluation`](internal/evaluation) | Measures the search and its judge against the test set. |

Three programs put the packages to work:
[`cmd/grafsol-mcp`](cmd/grafsol-mcp) serves a graph to agents,
[`cmd/grafsol-eval`](cmd/grafsol-eval) records and reports on the test set, and
[`examples/github`](examples/github) fills a graph from a GitHub repository.

Two folders are not Go. [`/inference`](inference) is the Python project that
holds the two model services, built on BentoML. [`/eval`](eval) is the test
set.

## What is not here yet

- **More connectors.** GitLab, Jira and Notion are not written. The GitHub
  connector reads issues and pull requests: their titles and bodies, not their
  comments.
- **A sync service.** The engine is here, and the example runs one pass of it.
  A long-running program that follows feeds, receives webhooks and reconciles
  on a schedule is yours to write for now.
- **Document text.** The graph keeps summaries, not the text they were made
  from. An agent that needs an exact detail opens the document's URL.
- **Tuned thresholds.** The search's thresholds and the judge's wording are
  first guesses.
- **Re-embedding.** A graph cannot yet be moved to another embedding model.
- **Other machines.** Nothing has been run on Linux or on an NVIDIA GPU.

One limit is worth knowing whatever else changes: summaries are written by a
small model. It can get a detail wrong, and a document written to mislead it
can succeed. Treat a summary as a pointer to its document, not as a
replacement for it.

## Licence

[MIT](LICENSE)
