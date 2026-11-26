FROM python:3.12-slim
LABEL org.opencontainers.image.authors="Patrick Munis at newwave"
RUN apt-get update && apt-get install -y --no-install-recommends libreoffice-writer fonts-dejavu fonts-liberation \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt
COPY main.py otel_setup.py builtin_letters.py office_letters.py docxfill.py object_store.py ./
COPY assets ./assets
# Not root (2026-11-26, Trivy DS-0002): an unprivileged user with a writable HOME (LibreOffice's profile, library caches).
RUN useradd -r -u 10001 -d /tmp app
ENV HOME=/tmp
USER 10001
CMD ["python", "main.py"]
