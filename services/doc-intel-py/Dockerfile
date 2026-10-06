FROM python:3.12-slim
WORKDIR /app
# Paddle system deps + tesseract-ocr (pytesseract is just a wrapper around
# this binary -- installing the pip package alone imports fine but every
# actual OCR call fails at runtime with "tesseract is not installed or it's
# not in your PATH")
RUN apt-get update && apt-get install -y --no-install-recommends \
    libgl1 libglib2.0-0 tesseract-ocr && rm -rf /var/lib/apt/lists/*
COPY requirements.txt .
# Split into several layers + a CPU-only torch wheel: a single layer bundling
# docling+paddleocr+opencv+torch reliably stalls mid-push on this registry
# (confirmed repeatedly: the same blob digest 520s every retry). torch's
# default PyPI wheel also drags in ~3.5GB of nvidia-* CUDA/cuDNN/triton
# packages irrelevant to this CPU-only cluster.
# torch and torchvision install together, from the SAME cpu wheel index, in
# one command: docling-ibm-models (pulled in by the next line) also depends
# on torchvision, and if it isn't already satisfied, pip resolves it from
# the default PyPI index instead of this one -- a torchvision build whose
# compiled ops (torchvision::nms, etc.) are version-matched to a DIFFERENT
# torch than the one actually installed. Confirmed live: that mismatch
# surfaced as a RuntimeError several layers deep inside transformers'
# AutoImageProcessor import (docling's layout model needs it), which
# transformers' own lazy-import wrapper flattened into a generic
# "Could not import module" with no indication it was a torch/torchvision
# ABI mismatch at all.
RUN pip install --no-cache-dir torch torchvision --index-url https://download.pytorch.org/whl/cpu
RUN pip install --no-cache-dir paddlepaddle paddleocr pypdfium2
# paddleocr pulls in paddlex transitively but without its [ocr] extra, so
# PP-StructureV3 (the layout/seal-detection stage) can't construct --
# PaddleX's own pipeline-creation code raises a generic "dependency error,
# refer to the installation documentation" with no hint of which extra is
# missing. Confirmed live: every document's layout stage failed this way,
# for every document, forever, until this was added.
RUN pip install --no-cache-dir "paddlex[ocr]==3.7.2"
RUN pip install --no-cache-dir docling
RUN pip install --no-cache-dir openai confluent-kafka minio httpx "psycopg[binary]" opensearch-py pyyaml pillow
# check_processor.py (physical check OCR/MICR intake) -- requirements.txt
# has listed these since that feature was added, but this Dockerfile never
# actually reads requirements.txt (see the layer-split comment above), so
# main.py's unconditional `from check_processor import extract_check` has
# been crashing every build on import since then. Nobody had rebuilt this
# image until now, so it went unnoticed.
RUN pip install --no-cache-dir pytesseract opencv-python-headless
COPY . .
# docling's default OCR backend (rapidocr) lazy-downloads its model weights
# into its own site-packages dir on first real use, not into $HOME/.cache --
# owned by root from the `pip install` above, so every analysis failed
# "Permission denied" under the non-root USER below (confirmed live: no
# document had ever been OCR'd successfully in this deployment).
RUN chmod -R a+w /usr/local/lib/python3.12/site-packages/rapidocr
# UID 10001 has no /etc/passwd entry and no HOME set, so anything that
# resolves "~" (HuggingFace Hub's model cache, used by docling's layout
# model; torch's own cache) falls back to the filesystem root, landing on
# the unwritable /.cache -- same root cause as above, different library.
# Confirmed live: this was the very next "Permission denied" to surface
# once the rapidocr one above was fixed.
ENV HOME=/tmp
USER 10001
# GPU variant: use paddlepaddle-gpu and an nvidia runtime; pipeline is identical.
CMD ["python", "main.py"]
