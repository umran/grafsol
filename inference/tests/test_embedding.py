"""What the embedder will take, and what it does with it. No model is loaded here."""

import numpy as np
import pytest

from grafsol_inference.embedding import Embedder
from grafsol_inference.errors import BadRequest, CannotProcess, Reason
from grafsol_inference.settings import EmbeddingSettings
from tests.fakes import DIMENSIONS, PROMPT, FakeEncoder


def settings(max_tokens: int = 50, max_texts: int = 4, batch: int = 2) -> EmbeddingSettings:
    return EmbeddingSettings(max_tokens=max_tokens, max_texts=max_texts, batch=batch)


def test_documents_are_embedded_bare_and_queries_under_the_models_search_prompt() -> None:
    model = FakeEncoder()
    embedder = Embedder(model, settings())

    embedder.embed_documents(["a passage"])
    embedder.embed_queries(["a question"])

    (kind_d, texts_d, how_d), (kind_q, texts_q, how_q) = model.calls
    assert (kind_d, texts_d, how_d["prompt_name"]) == ("document", ["a passage"], None)
    assert (kind_q, texts_q, how_q["prompt_name"]) == ("query", ["a question"], "web_search_query")


def test_vectors_come_back_one_per_text_in_order_as_ordinary_floats() -> None:
    embedder = Embedder(FakeEncoder(), settings())

    vectors = embedder.embed_documents(["a", "bbb", "cc"])

    assert vectors.shape == (3, DIMENSIONS)
    assert vectors.dtype == np.float32
    assert [int(np.argmax(v)) for v in vectors] == [1, 3, 2]
    assert embedder.dimensions == DIMENSIONS


def test_the_model_is_told_to_scale_vectors_to_length_one_and_given_the_batch_size() -> None:
    model = FakeEncoder()

    Embedder(model, settings(batch=2)).embed_documents(["a", "b", "c"])

    how = model.calls[0][2]
    assert how["normalize_embeddings"] is True
    assert how["batch_size"] == 2


def test_the_librarys_own_cut_off_is_set_to_ours_so_that_it_never_cuts_first() -> None:
    model = FakeEncoder()

    Embedder(model, settings(max_tokens=50))

    assert model.max_seq_length == 50


def test_a_text_longer_than_is_embedded_is_refused_and_nothing_is_embedded() -> None:
    model = FakeEncoder()
    embedder = Embedder(model, settings(max_tokens=50))

    with pytest.raises(CannotProcess) as refusal:
        embedder.embed_documents(["short", "word " * 51, "short"])

    assert refusal.value.reason is Reason.TOO_LONG
    assert "text 2 of 3 is 51 tokens long" in refusal.value.message
    assert "50" in refusal.value.message
    assert model.calls == []


def test_a_text_of_exactly_the_most_is_embedded() -> None:
    embedder = Embedder(FakeEncoder(), settings(max_tokens=50))

    assert embedder.embed_documents(["word " * 50]).shape == (1, DIMENSIONS)


def test_a_querys_prompt_counts_towards_its_length() -> None:
    embedder = Embedder(FakeEncoder(), settings(max_tokens=50))
    text = "word " * 45  # fits as a document; with the nine-token prompt it does not as a query

    assert embedder.embed_documents([text]).shape == (1, DIMENSIONS)
    with pytest.raises(CannotProcess) as refusal:
        embedder.embed_queries([text])

    assert "54 tokens" in refusal.value.message


@pytest.mark.parametrize("texts", [["fine", ""], ["fine", "  \n "]])
def test_an_empty_text_is_the_callers_mistake(texts: list[str]) -> None:
    model = FakeEncoder()

    with pytest.raises(BadRequest, match="text 2 of 2 is empty"):
        Embedder(model, settings()).embed_documents(texts)

    assert model.calls == []


def test_no_texts_and_too_many_texts_are_the_callers_mistake() -> None:
    embedder = Embedder(FakeEncoder(), settings(max_texts=4))

    with pytest.raises(BadRequest, match="no texts"):
        embedder.embed_queries([])
    with pytest.raises(BadRequest, match=r"5 texts.*at most 4"):
        embedder.embed_queries(["a"] * 5)


def test_a_model_without_the_named_query_prompt_is_refused_at_the_start() -> None:
    model = FakeEncoder(prompts={"query": None, "sts_query": "Instruct: similar Query: "})

    with pytest.raises(ValueError, match=r"no prompt named 'web_search_query'.*sts_query"):
        Embedder(model, settings())


def test_a_model_without_the_named_document_prompt_is_refused_at_the_start() -> None:
    with pytest.raises(ValueError, match="no prompt named 'passage'"):
        Embedder(FakeEncoder(), EmbeddingSettings(document_prompt="passage"))


def test_a_named_document_prompt_is_used_and_counted() -> None:
    model = FakeEncoder(prompts={"web_search_query": PROMPT, "passage": "passage: "})
    embedder = Embedder(model, EmbeddingSettings(document_prompt="passage", max_tokens=50))

    embedder.embed_documents(["a passage"])
    with pytest.raises(CannotProcess):
        embedder.embed_documents(["word " * 50])

    assert model.calls[0][2]["prompt_name"] == "passage"


def test_a_vector_that_is_not_made_of_finite_numbers_is_never_returned() -> None:
    model = FakeEncoder()
    embedder = Embedder(model, settings())

    def poison(vectors: np.ndarray) -> np.ndarray:
        vectors[0, 0] = np.nan
        return vectors

    model.spoil = poison
    with pytest.raises(RuntimeError, match="not finite"):
        embedder.embed_documents(["a", "b"])


def test_a_reply_with_the_wrong_number_or_length_of_vectors_is_never_returned() -> None:
    model = FakeEncoder()
    embedder = Embedder(model, settings())

    model.spoil = lambda vectors: vectors[:1]
    with pytest.raises(RuntimeError, match="1 vectors of 8 numbers for 2 texts"):
        embedder.embed_documents(["a", "b"])

    model.spoil = lambda vectors: vectors[:, :5]
    with pytest.raises(RuntimeError, match="2 vectors of 5 numbers"):
        embedder.embed_documents(["a", "b"])


def test_the_proof_fails_a_model_whose_query_prompt_changes_nothing() -> None:
    # The fake ignores prompts, so a sentence gets one vector whichever way it is embedded.
    embedder = Embedder(FakeEncoder(), settings())

    with pytest.raises(RuntimeError, match="query prompt is not being applied"):
        embedder.prove()


def test_the_proof_fails_a_model_whose_vectors_are_not_of_length_one() -> None:
    model = FakeEncoder()
    embedder = Embedder(model, settings())

    model.spoil = lambda vectors: vectors * 3
    with pytest.raises(RuntimeError, match=r"vector of length 3\.0000"):
        embedder.prove()


# --- the embedding space -------------------------------------------------------


def test_the_space_names_the_model_its_revision_its_prompts_and_its_vector_length() -> None:
    embedder = Embedder(FakeEncoder(), settings())

    assert embedder.space == (
        "microsoft/harrier-oss-v1-0.6b@f9b9dc8d367d443f2479d27aa5d8d2850c0774ee; "
        "query prompt web_search_query; document prompt none; 8 dimensions"
    )


def test_anything_that_changes_the_vectors_changes_the_name_of_the_space() -> None:
    prompts: dict[str, str | None] = {"web_search_query": PROMPT, "other": "x: ", "passage": "p: "}
    usual = EmbeddingSettings()
    spaces = {
        "as usual": Embedder(FakeEncoder(prompts), usual).space,
        "another model": Embedder(
            FakeEncoder(prompts), EmbeddingSettings(model="acme/other", revision="abc")
        ).space,
        "another revision": Embedder(
            FakeEncoder(prompts), EmbeddingSettings(model=usual.model, revision="abc")
        ).space,
        "another query prompt": Embedder(
            FakeEncoder(prompts), EmbeddingSettings(query_prompt="other")
        ).space,
        "a document prompt": Embedder(
            FakeEncoder(prompts), EmbeddingSettings(document_prompt="passage")
        ).space,
        "another vector length": Embedder(Longer(prompts), usual).space,
    }

    assert len(set(spaces.values())) == len(spaces), spaces
    # What does not change the vectors does not change the name.
    assert (
        Embedder(FakeEncoder(prompts), EmbeddingSettings(max_tokens=99, batch=7)).space
        == (spaces["as usual"])
    )


class Longer(FakeEncoder):
    """A model whose vectors are longer than the usual stand-in's."""

    def get_embedding_dimension(self) -> int:
        return DIMENSIONS + 1
