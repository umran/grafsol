"""Which device the models run on."""

from dataclasses import dataclass

import torch


@dataclass(frozen=True)
class Device:
    """Where a model runs."""

    name: str
    """"cuda" for an NVIDIA GPU, "mps" for a Mac's GPU, or "cpu"."""

    dtype: torch.dtype
    """The number format the language model is loaded in on this device.

    On a GPU that is a half-size format: it halves the memory the model
    takes and the time it needs, and costs a language model nothing that
    shows. On a CPU half-size arithmetic is slow, so the full size is used.

    The embedding model does not use this. Its vectors are stored for good
    and compared with vectors made later, perhaps on another machine, so it
    always runs at full size.
    """


def best(preference: str = "") -> Device:
    """The best device this machine has, or the one asked for by name.

    It raises RuntimeError if the device asked for is not there.
    """
    available = {
        "cuda": torch.cuda.is_available(),
        "mps": torch.backends.mps.is_available(),
        "cpu": True,
    }
    if preference and not available[preference]:
        raise RuntimeError(f"GRAFSOL_DEVICE is {preference}, and this machine has no such device")

    name = preference or next(name for name, there in available.items() if there)
    dtype = {"cuda": torch.bfloat16, "mps": torch.float16, "cpu": torch.float32}[name]
    return Device(name, dtype)
