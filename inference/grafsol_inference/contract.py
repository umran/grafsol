"""What the services send back.

With the signatures of the endpoints in services.py, these are the whole wire
format: a request is a JSON object whose fields are an endpoint's parameters,
and a reply is one of the objects below.

The folder inference/contract holds an example of every request and reply.
The tests here check that the services produce exactly those, and the tests
of the Go client in model/inference check that it sends and reads exactly
those, so the two sides cannot drift apart unnoticed.
"""

from pydantic import BaseModel


class EmbedderDescription(BaseModel):
    """What the embeddings service embeds with."""

    model: str
    """The Hugging Face repository of the model."""

    revision: str
    """The commit of that repository in use."""

    space: str
    """The name of the embedding space the service's vectors are in.

    It stands for the model and everything about how it is set up. Vectors
    can be compared only if their spaces have the same name; the name means
    nothing else, and a caller only ever compares it.
    """

    dimensions: int
    """The length of every vector. A database has to be created for this length."""

    max_tokens: int
    """The longest text that is embedded; a longer one is refused."""


class Embedded(BaseModel):
    """The vectors for the texts of one request."""

    model: str
    revision: str

    space: str
    """The embedding space these vectors are in: see EmbedderDescription.space.

    Every reply names it, so that a caller can tell at once if the service
    has been restarted with another model since it last asked.
    """

    embeddings: list[list[float]]
    """One vector per text, in the order of the texts, each scaled to length 1."""


class Summarized(BaseModel):
    """The summary of one document."""

    model: str
    revision: str

    summary: str

    parts: int
    """How many pieces the document was read in; 1 if it was read whole."""


class Refusal(BaseModel):
    """Sent with status 422: this text cannot be processed, and asking again will not help.

    A caller should go on without the embedding or the summary. Any other
    failure is either the caller's mistake (400) or the service's (5xx).
    """

    reason: str
    """One of errors.Reason, such as "too_long"."""

    message: str
    """The same, for a person to read."""
