"""Measures what this machine can take, to set the two token limits by.

    make calibrate

How long a text a model can be given is a property of the machine more than
of the model: the time and memory a model needs grow with the length of what
it reads. This loads both models as the services do and reads longer and
longer texts, printing the time and memory each took. Nothing is changed.

Read the tables it prints, then set:

    GRAFSOL_SUMMARY_WINDOW_TOKENS   to the longest document length whose time
                                    and memory you can live with, and at which
                                    the fact from the document's end was kept
    GRAFSOL_EMBEDDING_MAX_TOKENS    to the longest text length likewise

Run it again after changing machine, model, or the version of a library.
"""

import resource
import sys
import time

import numpy as np
import torch

from grafsol_inference import hardware, services
from grafsol_inference.embedding import Embedder
from grafsol_inference.errors import CannotProcess
from grafsol_inference.hardware import Device
from grafsol_inference.settings import EmbeddingSettings, SummarySettings
from grafsol_inference.summarizing import Summarizer
from grafsol_inference.writing import TransformersWriter

EMBEDDING_LENGTHS = (64, 256, 1024, 4096, 8192, 16384)
DOCUMENT_LENGTHS = (500, 2000, 4000, 6000, 8000, 12000, 16000)

OPENING = (
    "After release 2024.03.12 the checkout endpoint's p95 latency went from 180 ms to 410 ms. "
    "Bisecting points at #812, which removed the product cache because it served stale prices "
    "after the pricing migration. Every checkout now makes three extra calls to the catalogue "
    "service."
)
FILLER = (
    "The retry budget for the catalogue client stays at two attempts with a 50 ms backoff.",
    "Load tests on staging replayed the traffic of 4 March at 1,200 requests per second.",
    "The dashboards moved to the new metrics cluster, so older figures are not comparable.",
    "The on-call rota for the payments team changes on the first Monday of each month.",
    "A separate thread tracks the flaky integration test in the refunds suite.",
)
ENDING = (
    "Final decision, recorded 19 March: the fix shipped in #845 and p95 fell back to 185 ms. "
    "The code name of the rollout was BLUE HERON."
)
ENDING_FACTS = ("#845", "BLUE HERON", "185")


def memory(device: Device) -> str:
    """How much memory this process has taken on the device, as text."""
    if device.name == "mps":
        return f"{torch.mps.driver_allocated_memory() / 1e9:5.1f} GB"
    if device.name == "cuda":
        return f"{torch.cuda.max_memory_allocated() / 1e9:5.1f} GB"
    # Peak for the whole process; in bytes on macOS, in kilobytes on Linux.
    peak = resource.getrusage(resource.RUSAGE_SELF).ru_maxrss
    return f"{peak / (1e9 if sys.platform == 'darwin' else 1e6):5.1f} GB"


def document(writer: TransformersWriter, tokens: int) -> str:
    """A made-up document about that many tokens long, with a fact at its very end."""
    paragraphs = [OPENING]
    text = OPENING
    while len(writer.token_spans(text)) < tokens:
        number = len(paragraphs)
        comment = f"Comment {number} ({number % 27 + 1} March). {FILLER[number % len(FILLER)]}"
        paragraphs.append(comment)
        text = "\n\n".join([*paragraphs, ENDING])
    return text


def embeddings(device: Device) -> None:
    print(f"\nEmbedding model: {services.SETTINGS.embedding.model}")
    started = time.monotonic()
    settings = EmbeddingSettings(max_tokens=max(EMBEDDING_LENGTHS), max_texts=1024)
    embedder = Embedder.load(services.EMBEDDING_WEIGHTS.resolve(), settings, device)
    print(f"  loaded in {time.monotonic() - started:.1f} s; memory {memory(device)}")

    short = [
        f"Issue {n}: checkout latency doubled after the cache was removed." for n in range(256)
    ]
    started = time.monotonic()
    embedder.embed_documents(short)
    took = time.monotonic() - started
    print(f"  256 short texts in {took:.2f} s ({256 / took:.0f} a second)")

    print("\n  tokens in one text     time    memory")
    for length in EMBEDDING_LENGTHS:
        text = "latency " * (length - 8)
        try:
            started = time.monotonic()
            vector = embedder.embed_documents([text])[0]
            took = time.monotonic() - started
            sound = "" if np.isfinite(vector).all() else "  NOT FINITE"
            print(f"  {length:>19,} {took:>6.2f} s  {memory(device)}{sound}")
        except Exception as error:
            print(f"  {length:>19,}  failed: {type(error).__name__}: {str(error)[:120]}")
            break


def summaries(device: Device) -> None:
    print(f"\nSummarising model: {services.SETTINGS.summary.model}")
    started = time.monotonic()
    writer = TransformersWriter.load(services.SUMMARY_WEIGHTS.resolve(), device)
    print(f"  loaded in {time.monotonic() - started:.1f} s; memory {memory(device)}")

    print("\n  tokens read at once     time    memory   fact from the end")
    for length in DOCUMENT_LENGTHS:
        text = document(writer, length)
        # A window with room for this document whole, so that it is read in one go.
        summarizer = Summarizer(writer, SummarySettings(window_tokens=length + 1500))
        try:
            started = time.monotonic()
            summary = summarizer.summarize("Checkout latency doubled", text)
            took = time.monotonic() - started
        except CannotProcess as refusal:
            print(f"  {length:>19,}  refused: {refusal.reason}: {refusal.message[:100]}")
            continue
        except Exception as error:
            print(f"  {length:>19,}  failed: {type(error).__name__}: {str(error)[:120]}")
            break
        kept = sum(fact in summary.text for fact in ENDING_FACTS)
        whole = "read whole" if summary.parts == 1 else f"read in {summary.parts} parts"
        print(
            f"  {length:>19,} {took:>6.1f} s  {memory(device)}   "
            f"kept {kept} of {len(ENDING_FACTS)} ({whole})"
        )


def main() -> None:
    device = hardware.best(services.SETTINGS.hardware.device)
    print(f"Device: {device.name}, language model numbers in {device.dtype}")
    embeddings(device)
    summaries(device)


if __name__ == "__main__":
    main()
