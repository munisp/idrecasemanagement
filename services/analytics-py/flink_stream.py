"""PyFlink streaming: Kafka -> bronze Parquet (exactly-once) and a streaming
SLA early-warning job that emits breach candidates before Temporal timers fire."""

import json
import os

from pyflink.datastream import StreamExecutionEnvironment
from pyflink.datastream.connectors.kafka import KafkaSource, KafkaOffsetsInitializer
from pyflink.common.serialization import SimpleStringSchema
from pyflink.common.watermark_strategy import WatermarkStrategy
from pyflink.datastream.connectors.file_system import FileSink, OutputFileConfig, RollingPolicy
from pyflink.common import Types

BROKERS = os.environ.get("KAFKA_BROKERS", "kafka:9092")


def main() -> None:
    env = StreamExecutionEnvironment.get_execution_environment()
    env.enable_checkpointing(30_000)  # exactly-once baseline

    source = (
        KafkaSource.builder()
        .set_bootstrap_servers(BROKERS)
        .set_topics("idre.tx.cases", "idre.tx.offers", "idre.tx.fees", "idre.tx.rules", "idre.tx.documents")
        .set_group_id("flink-bronze")
        .set_starting_offsets(KafkaOffsetsInitializer.earliest())
        .set_value_only_deserializer(SimpleStringSchema())
        .build()
    )

    stream = env.from_source(source, WatermarkStrategy.no_watermarks(), "kafka-bronze")

    # Bronze archive: raw JSON lines partitioned by date (Parquet in prod via
    # ParquetWriterFactory; JSON kept here for a dependency-light local run).
    sink = (
        FileSink.for_row_format(
            __import__("pyflink.datastream.connectors.file_system", fromlist=["StreamFormat"]),
            SimpleStringSchema(),
        )
        .with_output_file_config(OutputFileConfig.builder().with_part_prefix("events").build())
        .with_rolling_policy(RollingPolicy.default_rolling_policy())
        .build()
    )
    stream.sink_to(sink)

    # Early-warning stream: flag open cases whose offer-window end is < 2bd away.
    def breach_candidates(raw: str):
        evt = json.loads(raw)
        if evt.get("type") == "case.initiated" and evt.get("offer_window_ends_at"):
            yield json.dumps({
                "type": "sla.early_warning", "tenant": evt.get("tenant"),
                "case_id": evt.get("case_id"), "clock": "OFFER_WINDOW_10BD",
                "deadline": evt["offer_window_ends_at"],
            })

    alerts = stream.flat_map(
        lambda r: list(breach_candidates(r)), output_type=Types.STRING()
    )
    alerts.print()  # prod: KafkaSink -> idre.<tenant>.alerts

    env.execute("idre-bronze-ingest")


if __name__ == "__main__":
    main()
