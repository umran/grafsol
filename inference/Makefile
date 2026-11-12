# The few commands this project needs. Run them from this folder.
#
#   make models        install the packages and download the two models (once)
#   make embeddings    run the embeddings service, at http://127.0.0.1:3010
#   make summaries     run the summaries service,  at http://127.0.0.1:3011
#   make calibrate     measure what this machine can take, to set the token limits by
#   make test          the fast tests, which load no model
#   make test-models   the tests that load the real models (minutes)
#   make check         formatting, lint and types
#
# Only `make models` uses the network. Everything that loads a model runs with
# HF_HUB_OFFLINE=1, so it starts from the files already on disk and contacts
# nobody.
#
# BentoML reports usage to its maker unless told not to, and it reads that
# switch once, when a process starts. So it is set here, for every command,
# rather than in the Python code. Hugging Face's telemetry is switched off the
# same way.

export BENTOML_DO_NOT_TRACK := True
export HF_HUB_DISABLE_TELEMETRY := 1

# The tokenizer library forks worker threads that a server has no use for.
export TOKENIZERS_PARALLELISM := false

RUN := uv run --no-sync
# With nothing to download there is no progress to show.
OFFLINE := HF_HUB_OFFLINE=1 HF_HUB_DISABLE_PROGRESS_BARS=1

.PHONY: models embeddings summaries calibrate test test-models check

models:
	uv sync
	$(RUN) python -m grafsol_inference.download

embeddings:
	$(OFFLINE) $(RUN) bentoml serve grafsol_inference.services:Embeddings

summaries:
	$(OFFLINE) $(RUN) bentoml serve grafsol_inference.services:Summaries

calibrate:
	$(OFFLINE) $(RUN) python -m grafsol_inference.calibrate

test:
	$(RUN) pytest

test-models:
	$(OFFLINE) GRAFSOL_TEST_MODELS=1 $(RUN) pytest tests/test_models.py

check:
	$(RUN) ruff format --check .
	$(RUN) ruff check .
	$(RUN) mypy
