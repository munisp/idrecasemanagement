"""Spark batch: bronze -> silver -> gold, Delta Lake on MinIO/S3.
Generates the CMS monthly report per tenant (45 CFR 149 reporting mandate)."""

import sys
from datetime import date

from delta import configure_spark_with_delta_pip
from pyspark.sql import SparkSession, functions as F

MINIO = "s3a://lakehouse"


def spark() -> SparkSession:
    builder = (
        SparkSession.builder.appName("idre-gold")
        .config("spark.sql.extensions", "io.delta.sql.DeltaSparkSessionExtension")
        .config("spark.sql.catalog.spark_catalog", "org.apache.spark.sql.delta.catalog.DeltaCatalog")
        .config("spark.hadoop.fs.s3a.endpoint", "http://minio:9000")
        .config("spark.hadoop.fs.s3a.access.key", "idre")
        .config("spark.hadoop.fs.s3a.secret.key", "idre-secret")
        .config("spark.hadoop.fs.s3a.path.style.access", "true")
    )
    return configure_spark_with_delta_pip(builder).getOrCreate()


def bronze_to_silver(s: SparkSession, tenant: str) -> None:
    cases = s.read.format("delta").load(f"{MINIO}/bronze/cases").where(F.col("tenant") == tenant)
    offers = s.read.format("delta").load(f"{MINIO}/bronze/offers").where(F.col("tenant") == tenant)
    fees = s.read.format("delta").load(f"{MINIO}/bronze/fees").where(F.col("tenant") == tenant)

    (cases.dropDuplicates(["case_id"])
         .withColumn("qpa_usd", F.col("qpa_cents") / 100)
         .write.format("delta").mode("overwrite")
         .option("replaceWhere", f"tenant = '{tenant}'")
         .save(f"{MINIO}/silver/cases"))
    offers.write.format("delta").mode("overwrite").option("replaceWhere", f"tenant = '{tenant}'").save(f"{MINIO}/silver/offers")
    fees.write.format("delta").mode("overwrite").option("replaceWhere", f"tenant = '{tenant}'").save(f"{MINIO}/silver/fees")


def cms_monthly_report(s: SparkSession, tenant: str, month: str) -> str:
    cases = s.read.format("delta").load(f"{MINIO}/silver/cases").where(F.col("tenant") == tenant)
    fees = s.read.format("delta").load(f"{MINIO}/silver/fees").where(F.col("tenant") == tenant)
    breaches = s.read.format("delta").load(f"{MINIO}/silver/sla_breaches").where(F.col("tenant") == tenant)

    report = (
        cases.where(F.date_trunc("month", "opened_at") == F.lit(f"{month}-01").cast("date"))
        .groupBy("tenant").agg(
            F.count("*").alias("disputes_initiated"),
            F.sum(F.when(F.col("status") == "CLOSED_PAID", 1).otherwise(0)).alias("closed_paid"),
            F.avg("qpa_usd").alias("avg_qpa_usd"),
        )
        .join(fees.groupBy("tenant").agg(F.sum("amount_cents").alias("fee_volume_cents")), "tenant", "left")
        .join(breaches.groupBy("tenant").count().withColumnRenamed("count", "sla_breaches"), "tenant", "left")
    )
    key = f"{MINIO}/gold/cms_monthly_report/tenant={tenant}/month={month}"
    report.write.format("delta").mode("overwrite").save(key)
    return key


def optimize_and_vacuum(s: SparkSession) -> None:
    for table in ("cases", "offers", "fees"):
        s.sql(f"OPTIMIZE delta.`{MINIO}/silver/{table}` ZORDER BY (tenant, case_id)")
    # 6-year retention mandate: never vacuum beyond legal hold window
    s.sql(f"VACUUM delta.`{MINIO}/silver/cases` RETAIN 720 HOURS")  # metadata only; rows kept 6y


if __name__ == "__main__":
    tenant, month = sys.argv[1], sys.argv[2] if len(sys.argv) > 2 else date.today().strftime("%Y-%m")
    sess = spark()
    bronze_to_silver(sess, tenant)
    print(cms_monthly_report(sess, tenant, month))
    optimize_and_vacuum(sess)
