"""Settings come from the environment, with defaults, and are checked."""

import pytest
from pydantic import ValidationError

from grafsol_inference import settings


def test_with_nothing_set_the_two_chosen_models_are_used_at_their_pinned_revisions(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    for name in ("MODEL", "REVISION"):
        monkeypatch.delenv(f"GRAFSOL_EMBEDDING_{name}", raising=False)
        monkeypatch.delenv(f"GRAFSOL_SUMMARY_{name}", raising=False)

    loaded = settings.load()

    assert loaded.embedding.model == "microsoft/harrier-oss-v1-0.6b"
    assert loaded.embedding.revision == "f9b9dc8d367d443f2479d27aa5d8d2850c0774ee"
    assert loaded.embedding.query_prompt == "web_search_query"
    assert loaded.summary.model == "Qwen/Qwen3-4B-Instruct-2507"
    assert loaded.summary.revision == "cdbee75f17c01a7cc42f958dc650907174af0554"
    assert loaded.hardware.device == ""


def test_values_are_read_from_the_environment(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("GRAFSOL_EMBEDDING_MAX_TOKENS", "512")
    monkeypatch.setenv("GRAFSOL_SUMMARY_WORDS", "80")
    monkeypatch.setenv("GRAFSOL_SUMMARY_WINDOW_TOKENS", "4000")
    monkeypatch.setenv("GRAFSOL_DEVICE", "cpu")

    loaded = settings.load()

    assert loaded.embedding.max_tokens == 512
    assert (loaded.summary.words, loaded.summary.window_tokens) == (80, 4000)
    assert loaded.hardware.device == "cpu"


@pytest.mark.parametrize("kind", ["EMBEDDING", "SUMMARY"])
def test_another_model_must_come_with_its_revision(
    monkeypatch: pytest.MonkeyPatch, kind: str
) -> None:
    monkeypatch.setenv(f"GRAFSOL_{kind}_MODEL", "someone/another-model")
    monkeypatch.delenv(f"GRAFSOL_{kind}_REVISION", raising=False)

    with pytest.raises(ValidationError, match=f"GRAFSOL_{kind}_REVISION must name"):
        settings.load()

    monkeypatch.setenv(f"GRAFSOL_{kind}_REVISION", "0123abc")
    loaded = settings.load()
    chosen = loaded.embedding if kind == "EMBEDDING" else loaded.summary
    assert (chosen.model, chosen.revision) == ("someone/another-model", "0123abc")


@pytest.mark.parametrize(
    ("name", "value"),
    [
        ("GRAFSOL_EMBEDDING_MAX_TOKENS", "0"),
        ("GRAFSOL_EMBEDDING_MAX_TEXTS", "-1"),
        ("GRAFSOL_SUMMARY_WORDS", "5"),
        ("GRAFSOL_SUMMARY_WINDOW_TOKENS", "many"),
        ("GRAFSOL_SUMMARY_MAX_CHARACTERS", "0"),
        ("GRAFSOL_DEVICE", "gpu"),
    ],
)
def test_a_value_that_makes_no_sense_is_refused(
    monkeypatch: pytest.MonkeyPatch, name: str, value: str
) -> None:
    monkeypatch.setenv(name, value)

    with pytest.raises(ValidationError):
        settings.load()
