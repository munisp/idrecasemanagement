# GeoLibre — geospatial visualization over the lakehouse

[GeoLibre](https://github.com/opengeos/GeoLibre) (opengeos, MIT) is the map
workspace for the platform's geospatial audit outputs. It is cloud-native:
it opens **GeoParquet and PMTiles directly from S3/HTTPS** with DuckDB-WASM
Spatial in the browser — no GIS server to run or secure, and data never leaves
the deployment.

## What it shows

Two gold-zone layers produced by `services/analytics-py/sedona_geo.py`
(Apache Sedona on Spark):

| Layer | Question it answers |
|---|---|
| `gold-geo/geo_jurisdiction_check.parquet` | Does the service location actually fall inside the state tenant that claims the case? (federal vs. specified-state-law routing audit) |
| `gold-geo/geo_coverage_gaps.parquet` | Which counties have zero in-network emergency providers? (network-adequacy context for QPA credibility) |

## Run it

1. `docker compose up minio spark` then run the Sedona job — it writes both
   Delta tables (gold zone) and GeoParquet mirrors (`gold-geo/` bucket prefix).
2. Open GeoLibre (self-host the web build, or the desktop/mobile app) and
   **Add Data → Vector Layer → GeoParquet from URL**:
   `https://<minio-endpoint>/lakehouse/gold-geo/geo_jurisdiction_check.parquet`
3. Save the project; the portal's **Reports → Geospatial audit** link opens it
   (configure the saved-project URL in `portal/js/config.js` as `geoMapUrl`).

Because GeoLibre also runs as native iOS/Android apps, field auditors get the
same map on mobile — consistent with the platform's PWA + Capacitor strategy.
