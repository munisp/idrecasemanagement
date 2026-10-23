FROM python:3.12-slim
RUN apt-get update && apt-get install -y --no-install-recommends libreoffice-writer fonts-dejavu fonts-liberation \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt
COPY main.py otel_setup.py builtin_letters.py office_letters.py docxfill.py ./
COPY assets ./assets
CMD ["python", "main.py"]
