# Python 3.11: Apache Flink (apache-flink) has no Python 3.12 support yet; on 3.12 its install fails.
FROM python:3.11-slim
LABEL org.opencontainers.image.authors="Patrick Munis at newwave"
WORKDIR /app
COPY requirements.txt requirements-flink.txt ./
RUN pip install --no-cache-dir -r requirements.txt
# The Flink streaming job's image only (it pins an older pyarrow): docker build --build-arg FLINK=1
ARG FLINK=0
RUN if [ "$FLINK" = "1" ]; then pip install --no-cache-dir -r requirements-flink.txt; fi
COPY . .
USER 10001
# Entrypoint is selected per job, e.g.:
#   spark-submit spark_gold.py tx 2026-09
#   flink run -py flink_stream.py   (an image built with --build-arg FLINK=1)
#   python datafusion_service.py | ray_jobs.py | sedona_geo.py
CMD ["python", "datafusion_service.py"]
