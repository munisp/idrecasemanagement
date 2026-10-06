#!/usr/bin/env bash
# Provision a new state tenant end-to-end:
#   1. Postgres schema (DDL template) + state_config row
#   2. TigerBeetle ledger + core accounts
#   3. Kafka topics (idre.<state>.{cases,offers,fees,audit,voice}) + ACLs
#   4. Keycloak tenant group
# Usage: ./scripts/provision-tenant.sh <two-letter-state-code> [tb_ledger_id]
set -euo pipefail

TENANT="${1:?state code required}"
LEDGER="${2:-$((RANDOM % 50 + 150))}"
PG=${PG:-psql "$DATABASE_URL"}

$PG -v ON_ERROR_STOP=1 <<SQL
  SELECT public.provision_tenant('${TENANT}');
  INSERT INTO public.state_config (tenant, tb_ledger_id)
  VALUES ('${TENANT}', ${LEDGER}) ON CONFLICT (tenant) DO NOTHING;
SQL

# TigerBeetle core accounts (escrow / admin / idre / refund) on the tenant ledger.
tbctl --addresses "${TB_ADDRESSES:-localhost:3000}" create-accounts \
  --ledger "${LEDGER}" --code 700 \
  --account "${TENANT}:1000:escrow" --account "${TENANT}:3000:admin" \
  --account "${TENANT}:4000:idre"  --account "${TENANT}:5000:refund"

# Kafka topics with per-tenant prefix ACLs.
# Shared ops topic: Flink SLA early-warning sink produces here (tenant
# carried in the payload; produce-only grant for the flink principal).
if ! kafka-topics.sh --bootstrap-server "$KAFKA" --describe --topic idre.alerts >/dev/null 2>&1; then
  kafka-topics.sh --bootstrap-server "$KAFKA" --create --topic idre.alerts     --partitions 3 --replication-factor 3 --config min.insync.replicas=2
fi
for domain in cases offers fees audit voice documents rules; do
  kafka-topics.sh --bootstrap-server "${KAFKA_BROKERS:-localhost:9092}" \
    --create --if-not-exists --topic "idre.${TENANT}.${domain}" \
    --partitions 6 --replication-factor 3
done

# Keycloak tenant group (k6 = keycloak admin CLI or REST call).
curl -sf -X POST "${KEYCLOAK_URL}/admin/realms/idre/groups" \
  -H "Authorization: Bearer ${KC_ADMIN_TOKEN}" \
  -H 'Content-Type: application/json' \
  -d "{\"name\": \"tenant/${TENANT}\"}"

echo "tenant ${TENANT} provisioned (ledger ${LEDGER})"
