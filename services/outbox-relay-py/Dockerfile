FROM python:3.12-slim
LABEL org.opencontainers.image.authors="Patrick Munis at newwave"
WORKDIR /app
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt
COPY relay.py otel_setup.py ./
# Not root (2026-11-26, Trivy DS-0002): an unprivileged user with a writable HOME (LibreOffice's profile, library caches).
RUN useradd -r -u 10001 -d /tmp app
ENV HOME=/tmp
USER 10001
CMD ["python", "relay.py"]
