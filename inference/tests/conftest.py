"""Shared by all the tests: when the real models are loaded, and how."""

import os
from collections.abc import Iterator

import pytest

from grafsol_inference import hardware
from grafsol_inference.embedding import Embedder
from grafsol_inference.settings import (
    EMBEDDING_MODEL,
    EMBEDDING_REVISION,
    SUMMARY_MODEL,
    SUMMARY_REVISION,
    EmbeddingSettings,
)
from grafsol_inference.writing import TransformersWriter


def pytest_collection_modifyitems(items: list[pytest.Item]) -> None:
    """Skips the tests that load the real models unless they were asked for."""
    if os.environ.get("GRAFSOL_TEST_MODELS") == "1":
        return
    skip = pytest.mark.skip(reason="loads the real models; set GRAFSOL_TEST_MODELS=1 to run it")
    for item in items:
        if "models" in item.keywords:
            item.add_marker(skip)


def downloaded(model: str, revision: str) -> str:
    """Where a model's files are on this machine. It never downloads them."""
    from huggingface_hub import snapshot_download

    return snapshot_download(model, revision=revision, local_files_only=True)


@pytest.fixture(scope="session")
def embedder() -> Embedder:
    """The real embedding model, loaded once for all the tests that use it."""
    path = downloaded(EMBEDDING_MODEL, EMBEDDING_REVISION)
    return Embedder.load(path, EmbeddingSettings(), hardware.best())


@pytest.fixture(scope="session")
def writer() -> Iterator[TransformersWriter]:
    """The real language model, loaded once for all the tests that use it."""
    yield TransformersWriter.load(downloaded(SUMMARY_MODEL, SUMMARY_REVISION), hardware.best())
