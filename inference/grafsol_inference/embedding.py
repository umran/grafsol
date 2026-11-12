"""Embedder turns texts into vectors, and decides which texts it will take.

The model is run by the sentence-transformers library. What this file adds is
the care around it:

  - A question and a passage are embedded differently. Models of this kind
    prefix a question with an instruction, and the library applies one only
    if it is named. Embedder names it, and refuses to start with a model that
    does not have it.
  - The library shortens a text that is too long for it without saying so.
    Embedder counts first and refuses such a text, so nothing is ever cut.
  - A vector that is not sound is never returned.
"""

from collections.abc import Callable, Mapping, Sequence
from typing import Any, Protocol, cast

import numpy as np
import numpy.typing as npt

from grafsol_inference.errors import BadRequest, CannotProcess, Reason
from grafsol_inference.hardware import Device
from grafsol_inference.settings import EmbeddingSettings

Vectors = npt.NDArray[np.float32]
"""One row per text: each row a vector, scaled to length 1."""


class Encoder(Protocol):
    """What Embedder needs of the library's model (a SentenceTransformer)."""

    prompts: dict[str, str | None]
    """The model's own prompts, by name."""

    max_seq_length: int | None
    """The length, in tokens, that the library cuts a longer text down to."""

    def tokenizer(self, texts: list[str], **how: Any) -> Mapping[str, Any]: ...
    def get_embedding_dimension(self) -> int | None: ...
    def encode_query(self, inputs: list[str], **how: Any) -> Any: ...
    def encode_document(self, inputs: list[str], **how: Any) -> Any: ...


class Embedder:
    """Embeds texts with one model."""

    def __init__(self, model: Encoder, settings: EmbeddingSettings) -> None:
        """It raises ValueError if the model lacks a prompt the settings name."""
        self._model = model
        self._settings = settings
        self._query_prompt = self._prompt(settings.query_prompt)
        self._document_prompt = self._prompt(settings.document_prompt)

        # Texts over the limit are refused before the library sees them. Its
        # own cut-off is set to the same length so that it can never cut one
        # that was let through.
        model.max_seq_length = settings.max_tokens

        dimensions = model.get_embedding_dimension()
        if not dimensions:
            raise ValueError("the embedding model does not say how long its vectors are")
        self.dimensions: int = dimensions
        """The length of every vector this embedder returns."""

        self.max_tokens: int = settings.max_tokens
        """The longest text this embedder takes."""

        self.space: str = (
            f"{settings.model}@{settings.revision}; "
            f"query prompt {settings.query_prompt or 'none'}; "
            f"document prompt {settings.document_prompt or 'none'}; "
            f"{dimensions} dimensions"
        )
        """The name of the embedding space this embedder's vectors are in.

        Vectors can be compared only if one model, set up one way, made all
        of them. The name is made of everything here that, if it changed,
        would make new vectors incomparable with old ones. Callers never take
        it apart: they only check whether two names are the same, and refuse
        to mix vectors when they are not.

        So whatever is added to this class that changes the vectors it makes
        must be added to the name as well.
        """

    @classmethod
    def load(cls, path: str, settings: EmbeddingSettings, device: Device) -> "Embedder":
        """Loads the model stored at path onto the device, and proves that it works there."""
        import torch
        from sentence_transformers import SentenceTransformer

        model = SentenceTransformer(
            path,
            device=device.name,
            # Full-size numbers on every device: see hardware.Device.dtype.
            model_kwargs={"dtype": torch.float32},
            local_files_only=True,
        )
        embedder = cls(cast(Encoder, model), settings)
        embedder.prove()
        return embedder

    def embed_documents(self, texts: Sequence[str]) -> Vectors:
        """Embeds texts that will be stored and searched for.

        It raises CannotProcess if a text is too long to embed, and
        BadRequest if there are no texts, too many, or an empty one.
        """
        return self._embed(
            texts,
            self._settings.document_prompt,
            self._document_prompt,
            self._model.encode_document,
        )

    def embed_queries(self, texts: Sequence[str]) -> Vectors:
        """Embeds texts that will be searched with, such as questions.

        It raises what embed_documents raises.
        """
        return self._embed(
            texts, self._settings.query_prompt, self._query_prompt, self._model.encode_query
        )

    def prove(self) -> None:
        """Embeds one sentence both ways and raises RuntimeError if the result is not sound.

        Run once when the model is loaded, it catches a device that computes
        nonsense and a query prompt that is not being applied.
        """
        sentence = "The checkout cache was restored in March."
        document = self.embed_documents([sentence])[0]
        query = self.embed_queries([sentence])[0]

        for vector in (document, query):
            length = float(np.linalg.norm(vector))
            if abs(length - 1) > 1e-3:
                raise RuntimeError(f"the embedding model returned a vector of length {length:.4f}")
        if self._query_prompt and float(document @ query) > 0.9999:
            raise RuntimeError(
                "the embedding model gave a sentence the same vector as a query and as a "
                "document, so its query prompt is not being applied"
            )

    def _embed(
        self,
        texts: Sequence[str],
        prompt_name: str,
        prompt: str,
        encode: Callable[..., Any],
    ) -> Vectors:
        texts = list(texts)
        self._check(texts, prompt)

        vectors = np.asarray(
            encode(
                texts,
                prompt_name=prompt_name or None,
                batch_size=self._settings.batch,
                normalize_embeddings=True,
                convert_to_numpy=True,
                show_progress_bar=False,
            ),
            dtype=np.float32,
        )

        if vectors.shape != (len(texts), self.dimensions):
            rows = vectors.shape[0] if vectors.ndim >= 1 else 0
            columns = vectors.shape[1] if vectors.ndim == 2 else 0
            raise RuntimeError(
                f"the embedding model returned {rows} vectors of {columns} numbers for "
                f"{len(texts)} texts, and a vector has {self.dimensions}"
            )
        if not np.isfinite(vectors).all():
            raise RuntimeError("the embedding model returned a vector that is not finite")
        return vectors

    def _check(self, texts: list[str], prompt: str) -> None:
        """Refuses a request the model should not be given."""
        if not texts:
            raise BadRequest("there are no texts to embed")
        if len(texts) > self._settings.max_texts:
            raise BadRequest(
                f"{len(texts)} texts were sent, and one request may carry at most "
                f"{self._settings.max_texts}"
            )
        for number, text in enumerate(texts, start=1):
            if not text.strip():
                raise BadRequest(f"text {number} of {len(texts)} is empty")

        # Counted as the model will read them: with the prompt in front, and
        # with nothing cut.
        tokenized = self._model.tokenizer([prompt + text for text in texts], truncation=False)
        for number, tokens in enumerate(tokenized["input_ids"], start=1):
            if len(tokens) > self._settings.max_tokens:
                raise CannotProcess(
                    Reason.TOO_LONG,
                    f"text {number} of {len(texts)} is {len(tokens):,} tokens long, and the "
                    f"most that is embedded is {self._settings.max_tokens:,}",
                )

    def _prompt(self, name: str) -> str:
        """The text of the model's prompt with that name; nothing for no name."""
        if not name:
            return ""
        text = self._model.prompts.get(name)
        if not text:
            have = sorted(name for name, text in self._model.prompts.items() if text)
            raise ValueError(
                f"the embedding model has no prompt named {name!r}; "
                f"the prompts it has are: {', '.join(have) or 'none'}"
            )
        return text
