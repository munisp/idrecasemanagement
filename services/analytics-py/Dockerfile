# Python 3.11: Apache Flink (apache-flink) has no Python 3.12 support yet; on 3.12 its install fails.
FROM python:3.11-slim
LABEL org.opencontainers.image.authors="Patrick Munis at newwave"
WORKDIR /app
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt
COPY . .
USER 10001
# Entrypoint is selected per job, e.g.:
#   spark-submit spark_gold.py tx 2026-09
#   flink run -py flink_stream.py
#   python datafusion_service.py | ray_jobs.py | sedona_geo.py
CMD ["python", "datafusion_service.py"]
