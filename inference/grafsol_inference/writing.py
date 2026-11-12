"""The language model that writes summaries, loaded with the transformers library.

This is the one Writer that is a real model. Everything about how a summary
is put together is in summarizing.py; what is here is only how to ask this
kind of model for a piece of text.
"""

from collections.abc import Sequence
from typing import Any

import torch

from grafsol_inference.hardware import Device
from grafsol_inference.summarizing import Written


class TransformersWriter:
    """A chat model run by transformers. It is a summarizing.Writer."""

    def __init__(self, tokenizer: Any, model: Any, device: Device) -> None:
        self._tokenizer = tokenizer
        self._model = model
        self._device = device

        # The tokens at which the model's reply is complete.
        ends = model.generation_config.eos_token_id
        self._ends = set(ends if isinstance(ends, list) else [ends])

        # Every string the tokenizer treats as a single marker rather than as
        # text, longest first so that none is caught inside another.
        self._markers = sorted(
            {*tokenizer.all_special_tokens, *tokenizer.get_added_vocab()}, key=len, reverse=True
        )

    @classmethod
    def load(cls, path: str, device: Device) -> "TransformersWriter":
        """Loads the model stored at path onto the device, and proves that it works there."""
        from transformers import AutoModelForCausalLM, AutoTokenizer

        tokenizer = AutoTokenizer.from_pretrained(path, local_files_only=True)
        model: Any = AutoModelForCausalLM.from_pretrained(
            path, dtype=device.dtype, local_files_only=True
        )
        model.to(device.name).eval()

        writer = cls(tokenizer, model, device)
        writer.prove()
        return writer

    def defuse(self, text: str) -> str:
        """Rewrites each of the model's markers found in text so that it reads as plain text.

        "<|im_start|>" becomes "[im_start]". A reader can still see what was
        there; the model no longer takes it for the start of a turn.
        """
        for marker in self._markers:
            if marker in text:
                text = text.replace(marker, "[" + marker.strip("<|>") + "]")
        return text

    def token_spans(self, text: str) -> Sequence[tuple[int, int]]:
        """Where each token of text starts and ends, in characters."""
        encoded = self._tokenizer(
            text, add_special_tokens=False, return_offsets_mapping=True, truncation=False
        )
        spans: Sequence[tuple[int, int]] = encoded["offset_mapping"]
        return spans

    def write(self, instructions: str, material: str, max_tokens: int) -> Written:
        """The model's reply to the material under the instructions.

        The model always takes its likeliest next word, so the same request
        gets the same reply.
        """
        prompt = self._prompt(instructions, material).to(self._device.name)

        with torch.inference_mode():
            output = self._model.generate(
                **prompt,
                max_new_tokens=max_tokens,
                do_sample=False,
                # The model ships with settings for picking words at random.
                # They are switched off above; clearing them says so quietly.
                temperature=None,
                top_p=None,
                top_k=None,
            )

        reply = output[0, prompt["input_ids"].shape[1] :]
        finished = reply.numel() > 0 and int(reply[-1]) in self._ends
        text: str = self._tokenizer.decode(reply, skip_special_tokens=True)
        return Written(text, finished)

    def _prompt(self, instructions: str, material: str) -> Any:
        """The request as the model reads it: tokens, in the model's own chat layout."""
        return self._tokenizer.apply_chat_template(
            [
                {"role": "system", "content": instructions},
                {"role": "user", "content": material},
            ],
            add_generation_prompt=True,
            # Belt and braces with defuse: any marker left in the text is
            # read as ordinary characters, never as the marker.
            sanitize_control_tokens=True,
            return_tensors="pt",
            return_dict=True,
        )

    def prove(self) -> None:
        """Asks the model for one word and raises RuntimeError if none comes back.

        Run once when the model is loaded, it catches a device on which the
        model computes nonsense before a document is lost to it.
        """
        written = self.write("Reply with the one word: ready", "Are you there?", max_tokens=8)
        if not written.finished or not written.text.strip():
            raise RuntimeError(
                f"the language model does not answer on {self._device.name}: "
                f"asked for one word, it wrote {written.text!r}"
            )
