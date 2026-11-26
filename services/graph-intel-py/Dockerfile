FROM python:3.12-slim
LABEL org.opencontainers.image.authors="Patrick Munis at newwave"
WORKDIR /app
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt
COPY . .
ENV PORT=8082
EXPOSE 8082
# Not root (2026-11-26, Trivy DS-0002): an unprivileged user with a writable HOME (LibreOffice's profile, library caches).
RUN useradd -r -u 10001 -d /tmp app && mkdir -p /app/lakehouse && chown 10001 /app/lakehouse  # gnn weights + lakehouse files (LAKEHOUSE_DIR=./lakehouse)
ENV HOME=/tmp
USER 10001
CMD ["python", "main.py"]
