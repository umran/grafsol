"""Which device the models run on."""

import pytest
import torch

from grafsol_inference import hardware


def machine(monkeypatch: pytest.MonkeyPatch, cuda: bool, mps: bool) -> None:
    monkeypatch.setattr(torch.cuda, "is_available", lambda: cuda)
    monkeypatch.setattr(torch.backends.mps, "is_available", lambda: mps)


def test_an_nvidia_gpu_is_preferred(monkeypatch: pytest.MonkeyPatch) -> None:
    machine(monkeypatch, cuda=True, mps=True)

    assert hardware.best() == hardware.Device("cuda", torch.bfloat16)


def test_the_macs_gpu_comes_next(monkeypatch: pytest.MonkeyPatch) -> None:
    machine(monkeypatch, cuda=False, mps=True)

    assert hardware.best() == hardware.Device("mps", torch.float16)


def test_the_cpu_is_the_last_resort_and_gets_full_size_numbers(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    machine(monkeypatch, cuda=False, mps=False)

    assert hardware.best() == hardware.Device("cpu", torch.float32)


def test_a_device_can_be_asked_for_by_name(monkeypatch: pytest.MonkeyPatch) -> None:
    machine(monkeypatch, cuda=True, mps=True)

    assert hardware.best("cpu") == hardware.Device("cpu", torch.float32)
    assert hardware.best("mps") == hardware.Device("mps", torch.float16)


def test_asking_for_a_device_the_machine_lacks_is_an_error(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    machine(monkeypatch, cuda=False, mps=True)

    with pytest.raises(RuntimeError, match="GRAFSOL_DEVICE is cuda"):
        hardware.best("cuda")
