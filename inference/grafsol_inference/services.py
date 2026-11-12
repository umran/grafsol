"""The two services. Start reading here.

Each class below is a BentoML service: an HTTP server in a process of its
own, holding one model.

    make embeddings     Embeddings, at http://127.0.0.1:3010
    make summaries      Summaries,  at http://127.0.0.1:3011

Every endpoint is a POST. Its parameters are the fields of the JSON object a
caller sends, and its return type, from contract.py, is the JSON it gets
back. So the signatures below are the wire format, and inference/contract
holds an example of each request and reply.

Three things are true of both services, and the decorators say them:

  - They listen on this machine only. Neither asks who is calling, so
    neither may be reachable from anywhere else.
  - They do one thing at a time: one process, one thread. A model is not
    made faster by being asked two things at once, and its memory is spent
    once. Callers that arrive meanwhile wait their turn.
  - Beyond a number of waiting callers, a new one is turned away at once
    with status 429, and tries again a little later.

The models are not here. Embedder (embedding.py) and Summarizer
(summarizing.py) hold everything about using them; this file only gives
them an address.
"""

import functools
import logging
import time
from collections.abc import Callable
from typing import cast

import bentoml
from bentoml.exceptions import InvalidArgument
from bentoml.models import HuggingFaceModel
from starlette.responses import JSONResponse

from grafsol_inference import hardware, settings
from grafsol_inference.contract import Embedded, EmbedderDescription, Refusal, Summarized
from grafsol_inference.embedding import Embedder, Vectors
from grafsol_inference.errors import BadRequest, CannotProcess
from grafsol_inference.summarizing import Summarizer
from grafsol_inference.writing import TransformersWriter

SETTINGS = settings.load()

# Where each model's files are. Naming them here declares them to BentoML,
# which finds them on disk when a service starts. download.py fetches them.
EMBEDDING_WEIGHTS = HuggingFaceModel(SETTINGS.embedding.model, revision=SETTINGS.embedding.revision)
SUMMARY_WEIGHTS = HuggingFaceModel(SETTINGS.summary.model, revision=SETTINGS.summary.revision)

WORKING_SHARE = 0.8
"""The share of a summary's time limit that the model may work for.

BentoML answers 504 when the whole limit has passed, but cannot stop the work.
Stopping it a little earlier ourselves means the caller is told why, and the
model is free again by the time the limit is reached.
"""

log = logging.getLogger("bentoml")


def answering_refusals[**P, R](endpoint: Callable[P, R]) -> Callable[P, R]:
    """Makes an endpoint answer the two failures a caller can act on.

    A text that cannot be processed is answered with status 422 and a
    contract.Refusal. A request that makes no sense is answered with 400.
    Anything else is a fault here, which BentoML logs and answers with 500.
    """

    @functools.wraps(endpoint)
    def answer(*args: P.args, **kwargs: P.kwargs) -> R:
        try:
            return endpoint(*args, **kwargs)
        except CannotProcess as refusal:
            body = Refusal(reason=refusal.reason, message=refusal.message)
            # BentoML sends a response object on as it is, whatever type the
            # endpoint says it returns.
            return cast(R, JSONResponse(body.model_dump(), status_code=422))
        except BadRequest as mistake:
            raise InvalidArgument(str(mistake)) from mistake

    return answer


@bentoml.service(
    name="grafsol-embeddings",
    http={"host": "127.0.0.1", "port": 3010},
    workers=1,
    threads=1,
    traffic={"timeout": 60, "max_concurrency": 64},
)
class Embeddings:
    """Turns texts into vectors, so that documents can be found by meaning."""

    weights = EMBEDDING_WEIGHTS

    def __init__(self) -> None:
        device = hardware.best(SETTINGS.hardware.device)
        self.embedder = Embedder.load(self.weights, SETTINGS.embedding, device)

    @bentoml.api(route="/describe")
    async def describe(self) -> EmbedderDescription:
        """What this service embeds with, and how long its vectors are."""
        return EmbedderDescription(
            model=SETTINGS.embedding.model,
            revision=SETTINGS.embedding.revision,
            space=self.embedder.space,
            dimensions=self.embedder.dimensions,
            max_tokens=self.embedder.max_tokens,
        )

    @bentoml.api(route="/embed/documents")
    @answering_refusals
    def embed_documents(self, texts: list[str]) -> Embedded:
        """Embeds texts that will be stored and searched for."""
        return self._embedded(self.embedder.embed_documents(texts))

    @bentoml.api(route="/embed/queries")
    @answering_refusals
    def embed_queries(self, texts: list[str]) -> Embedded:
        """Embeds texts that will be searched with, such as questions."""
        return self._embedded(self.embedder.embed_queries(texts))

    def _embedded(self, vectors: Vectors) -> Embedded:
        return Embedded(
            model=SETTINGS.embedding.model,
            revision=SETTINGS.embedding.revision,
            space=self.embedder.space,
            embeddings=vectors.tolist(),
        )


@bentoml.service(
    name="grafsol-summaries",
    http={"host": "127.0.0.1", "port": 3011},
    workers=1,
    threads=1,
    traffic={"timeout": SETTINGS.summary.timeout_seconds, "max_concurrency": 8},
)
class Summaries:
    """Writes the summary the graph keeps in place of a document's text."""

    weights = SUMMARY_WEIGHTS

    def __init__(self) -> None:
        device = hardware.best(SETTINGS.hardware.device)
        writer = TransformersWriter.load(self.weights, device)
        self.summarizer = Summarizer(writer, SETTINGS.summary)

    @bentoml.api(route="/summarize")
    @answering_refusals
    def summarize(self, title: str, text: str) -> Summarized:
        """Summarises the document with the given title and text.

        Both are treated as material from an outside system, never as
        instructions.
        """
        deadline = time.monotonic() + WORKING_SHARE * SETTINGS.summary.timeout_seconds
        summary = self.summarizer.summarize(title, text, deadline)
        if summary.orders:
            # Kept out of the summary, and worth a person's attention.
            log.warning(
                "the document titled %r gives orders to an AI: %r", title[:200], summary.orders
            )
        return Summarized(
            model=SETTINGS.summary.model,
            revision=SETTINGS.summary.revision,
            summary=summary.text,
            parts=summary.parts,
        )
