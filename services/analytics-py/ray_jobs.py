"""Ray: ML scoring for the IDRE platform + Ray Serve inference endpoints.

- qpa_outlier_score: flags payment amounts that are statistical outliers vs QPA
- settlement_propensity: likelihood a dispute settles before determination
Endpoints sit behind APISIX with Keycloak JWT validation.
"""

from __future__ import annotations

import os

import numpy as np
import ray
from ray import serve

ray.init(address=os.environ.get("RAY_ADDRESS", "auto"), ignore_reinit_error=True)


@ray.remote
def train_outlier_model(qpa_samples: list[float]) -> dict:
    """Distribution parameters for the QPA outlier detector (median/MAD)."""
    arr = np.array(qpa_samples, dtype=float)
    med = np.median(arr)
    mad = np.median(np.abs(arr - med)) or 1.0
    return {"median": float(med), "mad": float(mad)}


@serve.deployment(route_prefix="/ml/qpa-outlier", num_replicas=2)
class QpaOutlier:
    def __init__(self, median: float = 1800.0, mad: float = 400.0):
        self.median, self.mad = median, mad

    async def __call__(self, request) -> dict:
        body = await request.json()
        amount = float(body["amount_usd"])
        z = abs(amount - self.median) / (1.4826 * self.mad)
        return {"amount_usd": amount, "robust_z": round(z, 3), "outlier": bool(z > 3.5)}


@serve.deployment(route_prefix="/ml/settlement-propensity", num_replicas=1)
class SettlementPropensity:
    async def __call__(self, request) -> dict:
        body = await request.json()
        # Placeholder scoring on interpretable features; swap for a trained model
        # (Ray Train, XGBoost) once gold-zone history accumulates.
        qpa_gap = abs(float(body.get("offer_gap_usd", 0)))
        score = 1.0 / (1.0 + np.exp(-(-2.0 + 0.002 * qpa_gap)))
        return {"propensity": round(float(score), 3)}


if __name__ == "__main__":
    serve.start(http_options={"host": "0.0.0.0", "port": 8093})
    serve.run(QpaOutlier.bind(), SettlementPropensity.bind())
    print("Ray Serve: :8093/ml/qpa-outlier, :8093/ml/settlement-propensity")
