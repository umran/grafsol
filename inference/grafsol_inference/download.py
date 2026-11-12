"""Fetches the two models from Hugging Face.

    make models

This is the only code in the project that uses the network. The services
themselves are started with HF_HUB_OFFLINE=1 and read the files this leaves
on disk, in Hugging Face's usual cache (~/.cache/huggingface, unless HF_HOME
or BENTOML_HF_CACHE_DIR says otherwise). Run again, it downloads nothing that
is already there.
"""

from pathlib import Path

from grafsol_inference import services


def main() -> None:
    for weights in (services.EMBEDDING_WEIGHTS, services.SUMMARY_WEIGHTS):
        print(f"{weights.model_id} at revision {weights.revision[:12]}", flush=True)
        # The very call a service makes when it starts, so what is fetched
        # here is exactly what a service will look for.
        path = Path(weights.resolve())
        size = sum(file.stat().st_size for file in path.rglob("*") if file.is_file())
        print(f"  {size / 1e9:.2f} GB at {path}", flush=True)


if __name__ == "__main__":
    main()
