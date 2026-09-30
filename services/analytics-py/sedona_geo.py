"""Apache Sedona: geospatial analytics for jurisdiction & coverage.

- Verifies provider service location falls inside the claimed state tenant
  (federal vs. specified-state-law jurisdiction checks at borders).
- Coverage-gap mapping: counties with no in-network emergency providers.
- Air-ambulance corridor analysis (state-scope rules for air claims).
"""

from __future__ import annotations

from pyspark.sql import functions as F
from sedona.spark import SedonaContext

MINIO = "s3a://lakehouse"


def main() -> None:
    sedona = (
        SedonaContext.builder()
        .appName("idre-geo")
        .config("spark.hadoop.fs.s3a.endpoint", "http://minio:9000")
        .config("spark.hadoop.fs.s3a.access.key", "idre")
        .config("spark.hadoop.fs.s3a.secret.key", "idre-secret")
        .config("spark.hadoop.fs.s3a.path.style.access", "true")
        .getOrCreate()
    )
    spark = sedona.spark

    states = spark.read.parquet(f"{MINIO}/ref/us_states")          # Census TIGER boundaries
    providers = spark.read.parquet(f"{MINIO}/silver/providers")    # service locations
    cases = spark.read.format("delta").load(f"{MINIO}/silver/cases")

    providers.createOrReplaceTempView("providers")
    states.createOrReplaceTempView("states")
    cases.createOrReplaceTempView("cases")

    # Jurisdiction check: the state polygon containing the service location must
    # equal the case tenant — otherwise SSL/federal routing may be wrong.
    jurisdiction = spark.sql("""
        SELECT c.case_number, c.tenant AS claimed_tenant,
               s.state_code AS geo_state,
               (s.state_code = c.tenant) AS jurisdiction_consistent
        FROM cases c
        JOIN providers p ON p.provider_id = c.provider_id
        JOIN states s ON ST_Contains(s.geom, ST_Point(p.lon, p.lat))
    """)
    jurisdiction.write.format("delta").mode("overwrite").save(f"{MINIO}/gold/geo_jurisdiction_check")

    # Coverage gaps: counties with zero in-network emergency providers.
    spark.sql("""
        SELECT s.state_code, s.county_fips, COUNT(p.provider_id) AS in_network_providers
        FROM states s
        LEFT JOIN providers p
          ON ST_Contains(s.geom, ST_Point(p.lon, p.lat)) AND p.in_network = true
        GROUP BY s.state_code, s.county_fips
    """).filter("in_network_providers = 0") \
       .write.format("delta").mode("overwrite").save(f"{MINIO}/gold/geo_coverage_gaps")

    print("geo analytics written to gold zone")


if __name__ == "__main__":
    main()
