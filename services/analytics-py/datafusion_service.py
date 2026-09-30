"""DataFusion ad-hoc query service: sub-second SQL over the gold zone without
spinning up Spark. Tenant-scoped, read-only. Run: python datafusion_service.py"""

from __future__ import annotations

import json
import re
from http.server import BaseHTTPRequestHandler, HTTPServer

import datafusion

MINIO = "s3://lakehouse/gold"
ALLOWED = re.compile(r"^\s*select\b", re.IGNORECASE)  # read-only gate


def make_ctx(tenant: str) -> datafusion.SessionContext:
    ctx = datafusion.SessionContext()
    ctx.register_object_store("s3://", datafusion.object_store.AmazonS3(
        bucket_name="lakehouse", endpoint_url="http://minio:9000",
        access_key_id="idre", secret_access_key="idre-secret", allow_http=True,
    ), url_prefix="s3://")
    # Gold marts registered as Parquet/Delta snapshots (tenant partition pruned)
    ctx.register_parquet("cms_monthly_report", f"{MINIO}/cms_monthly_report/tenant={tenant}/")
    return ctx


class Handler(BaseHTTPRequestHandler):
    def do_POST(self):
        if self.path != "/analytics/query":
            self.send_error(404)
            return
        tenant = self.headers.get("X-Tenant-Code", "")
        if not re.fullmatch(r"[a-z]{2}", tenant):
            self.send_error(403, "tenant required")
            return
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        sql = body.get("sql", "")
        if not ALLOWED.match(sql) or ";" in sql:
            self.send_error(403, "read-only SELECT only")
            return
        ctx = make_ctx(tenant)
        try:
            df = ctx.sql(sql)
            rows = df.collect()
            out = rows[0].to_pydict() if rows else {}
        except Exception as exc:  # noqa: BLE001
            self.send_error(400, str(exc))
            return
        payload = json.dumps(out).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(payload)


if __name__ == "__main__":
    HTTPServer(("0.0.0.0", 8092), Handler).serve_forever()
