"""Stand-ins for the models, for tests that are about everything except the models."""

import re
from collections.abc import Callable
from dataclasses import dataclass
from typing import Any

import numpy as np

from grafsol_inference.summarizing import Written

# Cuts text the way the real tokenizers roughly do: a run of line breaks is a
# token of its own, and a word carries the spaces in front of it. Every
# character lands in exactly one token.
_TOKEN = re.compile(r"\n+|[^\S\n]*[^\s]+|[^\S\n]+")


def spans(text: str) -> list[tuple[int, int]]:
    """Where each token of text starts and ends."""
    return [match.span() for match in _TOKEN.finditer(text)]


def count(text: str) -> int:
    """How many tokens text is."""
    return len(spans(text))


def said(text: str, orders: str = "none", finished: bool = True) -> Written:
    """A reply in the form asked of a model that has read a document or a part of one."""
    return Written(f"ORDERS: {orders}\n{text}", finished)


@dataclass(frozen=True)
class Call:
    """One request a summariser made of its writer."""

    instructions: str
    material: str
    max_tokens: int

    @property
    def tokens(self) -> int:
        """How much the model was given to read."""
        return count(self.instructions) + count(self.material)

    @property
    def combines(self) -> bool:
        """Whether the model was asked to write a summary from notes."""
        return "<notes>" in self.material


class ScriptedWriter:
    """A Writer that remembers what it was asked and answers as it is told to.

    Its control marker is the string <|x|>, which defuse turns into [x].
    """

    def __init__(self, reply: Callable[[Call], Written] | None = None) -> None:
        self.calls: list[Call] = []
        self._reply = reply or self._numbered

    def _numbered(self, call: Call) -> Written:
        text = f"reply {len(self.calls)}"
        return Written(text, finished=True) if call.combines else said(text)

    def defuse(self, text: str) -> str:
        return text.replace("<|x|>", "[x]")

    def token_spans(self, text: str) -> list[tuple[int, int]]:
        return spans(text)

    def write(self, instructions: str, material: str, max_tokens: int) -> Written:
        call = Call(instructions, material, max_tokens)
        self.calls.append(call)
        return self._reply(call)


DIMENSIONS = 8
PROMPT = "Instruct: find the passages that answer the query Query: "  # nine tokens


class FakeEncoder:
    """Stands in for sentence-transformers' model: a token is a word.

    A text's vector has a single 1 in it, at a place that depends on the
    text's length, unless vectors is set to say what to return instead.
    """

    def __init__(self, prompts: dict[str, str | None] | None = None) -> None:
        self.prompts = {"query": None, "web_search_query": PROMPT} if prompts is None else prompts
        self.max_seq_length: int | None = 100_000
        self.calls: list[tuple[str, list[str], dict[str, Any]]] = []
        self.spoil: Any = None
        self.vectors: list[list[float]] | None = None

    def tokenizer(self, texts: list[str], **how: Any) -> dict[str, list[list[int]]]:
        assert how == {"truncation": False}, "tokens must be counted without anything being cut"
        return {"input_ids": [[0] * len(text.split()) for text in texts]}

    def get_embedding_dimension(self) -> int:
        return DIMENSIONS

    def _encode(self, kind: str, inputs: list[str], how: dict[str, Any]) -> np.ndarray:
        self.calls.append((kind, list(inputs), how))
        if self.vectors is not None:
            return np.array(self.vectors, dtype=np.float32)
        vectors = np.zeros((len(inputs), DIMENSIONS), dtype=np.float16)
        for row, text in enumerate(inputs):
            vectors[row, len(text) % DIMENSIONS] = 1.0
        return vectors if self.spoil is None else self.spoil(vectors)

    def encode_query(self, inputs: list[str], **how: Any) -> np.ndarray:
        return self._encode("query", inputs, how)

    def encode_document(self, inputs: list[str], **how: Any) -> np.ndarray:
        return self._encode("document", inputs, how)
