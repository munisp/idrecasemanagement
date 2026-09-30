FROM python:3.12-slim
WORKDIR /app
# Paddle system deps
RUN apt-get update && apt-get install -y --no-install-recommends \
    libgl1 libglib2.0-0 && rm -rf /var/lib/apt/lists/*
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt pypdfium2
COPY . .
USER 10001
# GPU variant: use paddlepaddle-gpu and an nvidia runtime; pipeline is identical.
CMD ["python", "main.py"]
