"""Everything about the services that can be configured.

Each value comes from an environment variable or else from the default
written here, and is checked once, when a service starts. Nothing else in the
package reads the environment.
"""

from dataclasses import dataclass
from typing import Any, Literal

from pydantic import Field, model_validator
from pydantic_settings import BaseSettings, SettingsConfigDict

# The models, each at one exact published version. A model's files can be
# replaced under the same name; a revision never changes. For embeddings this
# matters twice over: vectors made by two versions of a model cannot be
# compared.
EMBEDDING_MODEL = "microsoft/harrier-oss-v1-0.6b"
EMBEDDING_REVISION = "f9b9dc8d367d443f2479d27aa5d8d2850c0774ee"
SUMMARY_MODEL = "Qwen/Qwen3-4B-Instruct-2507"
SUMMARY_REVISION = "cdbee75f17c01a7cc42f958dc650907174af0554"


def _pin(values: Any, model: str, revision: str, prefix: str) -> Any:
    """Gives the default model its revision, and insists on one for any other."""
    if not isinstance(values, dict) or values.get("revision"):
        return values
    if values.get("model", model) != model:
        raise ValueError(
            f"{prefix}MODEL names a model other than {model}, so {prefix}REVISION must name "
            "the exact revision of it to use (a commit of its Hugging Face repository)"
        )
    return {**values, "revision": revision}


class EmbeddingSettings(BaseSettings):
    """How texts are embedded. The variables begin GRAFSOL_EMBEDDING_."""

    model_config = SettingsConfigDict(env_prefix="GRAFSOL_EMBEDDING_", frozen=True)

    model: str = EMBEDDING_MODEL
    """The Hugging Face repository of the embedding model."""

    revision: str = ""
    """The commit of that repository to use. Required for any model but the default."""

    query_prompt: str = "web_search_query"
    """The name of the model's own prompt for texts that are searched with.

    Models of this kind embed a question differently from a passage: the
    question is prefixed with an instruction. The instruction's text ships
    with the model, under a name; this is the name.
    """

    document_prompt: str = ""
    """The name of the model's prompt for texts that are searched for, if it has one."""

    max_tokens: int = Field(default=8192, gt=0)
    """The longest text that is embedded. A longer one is refused, never shortened."""

    max_texts: int = Field(default=256, gt=0)
    """The most texts accepted in one request."""

    batch: int = Field(default=32, gt=0)
    """How many texts the model is given at once."""

    @model_validator(mode="before")
    @classmethod
    def _pinned(cls, values: Any) -> Any:
        return _pin(values, EMBEDDING_MODEL, EMBEDDING_REVISION, "GRAFSOL_EMBEDDING_")


class SummarySettings(BaseSettings):
    """How summaries are written. The variables begin GRAFSOL_SUMMARY_."""

    model_config = SettingsConfigDict(env_prefix="GRAFSOL_SUMMARY_", frozen=True)

    model: str = SUMMARY_MODEL
    """The Hugging Face repository of the model that writes."""

    revision: str = ""
    """The commit of that repository to use. Required for any model but the default."""

    words: int = Field(default=120, ge=20, le=1000)
    """About how long a summary may be."""

    window_tokens: int = Field(default=8192, gt=0)
    """The most the model is given to read at once: its instructions and the text together.

    A document that does not fit is read in parts. The limit is this machine's
    more than the model's: the memory and time a model takes grow steeply
    with the length of what it reads.
    """

    max_characters: int = Field(default=200_000, gt=0)
    """The most of one document that is read. A summary of a longer one says so."""

    timeout_seconds: int = Field(default=900, ge=30)
    """The longest one summary may take before the service gives up on it."""

    @model_validator(mode="before")
    @classmethod
    def _pinned(cls, values: Any) -> Any:
        return _pin(values, SUMMARY_MODEL, SUMMARY_REVISION, "GRAFSOL_SUMMARY_")


class HardwareSettings(BaseSettings):
    """Where the models run. The one variable is GRAFSOL_DEVICE."""

    model_config = SettingsConfigDict(env_prefix="GRAFSOL_", frozen=True)

    device: Literal["", "cuda", "mps", "cpu"] = ""
    """The device to run on; empty means the best one this machine has."""


@dataclass(frozen=True)
class Settings:
    """All the settings of both services."""

    embedding: EmbeddingSettings
    summary: SummarySettings
    hardware: HardwareSettings


def load() -> Settings:
    """Reads the settings from the environment. It raises if one is not valid."""
    return Settings(
        embedding=EmbeddingSettings(),
        summary=SummarySettings(),
        hardware=HardwareSettings(),
    )
