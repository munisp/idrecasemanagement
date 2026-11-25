"""Lakehouse de-identification on a full sync: a tenant's names go to deid-py
in batches under its request limit, each distinct value once, and every
scrubbed value lands back on the field it came from."""
import sys, types
from unittest import mock

import pytest

for name in ("graphdb", "gnn", "lakehouse_io", "epr_kgqa"):
    sys.modules.setdefault(name, types.ModuleType(name))
sys.modules["epr_kgqa"].OLLAMA_MODEL = getattr(sys.modules["epr_kgqa"], "OLLAMA_MODEL", "test")
sys.modules["gnn"].EPOCHS = getattr(sys.modules["gnn"], "EPOCHS", 1)  # read at import, for a default

import main  # noqa: E402


def test_batches_stay_under_the_limit():
    vals = [f"Clinic {i:05d}" for i in range(1000)]  # 12 chars + newline each
    batches = main._deid_batches(vals, 130)
    assert [v for b in batches for v in b] == vals
    assert all(len("\n".join(b)) <= 130 for b in batches)
    assert len(batches) == 100


def test_a_value_longer_than_the_limit_goes_alone():
    batches = main._deid_batches(["x" * 50, "a", "b"], 10)
    assert batches == [["x" * 50], ["a", "b"]]


class _Resp:
    def __init__(self, body):
        self.body = body

    def raise_for_status(self):
        pass

    def json(self):
        return self.body


def _fake_deid(sent):
    def post(url, json=None, **kw):
        if url.endswith("/v1/deidentify"):
            sent.append(json["text"])
            lines = json["text"].split("\n")
            return _Resp({"text": "\n".join(l.replace("Dr Smith", "[NAME]") for l in lines),
                          "audit": {"counts": {"NAME": sum("Dr Smith" in l for l in lines)},
                                    "detectors": ["lexicon"], "policy": "safe-harbor-18/v1"}})
        return _Resp({})
    return post


def test_each_value_scrubbed_once_and_put_back(monkeypatch):
    monkeypatch.setattr(main, "DEID_URL", "http://deid")
    monkeypatch.setattr(main, "DEID_BATCH_CHARS", 40)
    rows = [{"id": str(i), "provider_name": "Dr Smith Clinic" if i % 2 else "Bay Hospital",
             "payer_name": "Sunshine Health", "service_line": "ER\nvisit"} for i in range(500)]
    sent = []
    with mock.patch("httpx.post", side_effect=_fake_deid(sent)):
        out = main._deid_rows("fl", rows)
    joined = "\n".join(sent)
    assert joined.count("Dr Smith Clinic") == 1 and joined.count("Sunshine Health") == 1
    assert all(len(t) <= 40 for t in sent)
    assert out[1]["provider_name"] == "[NAME] Clinic" and out[0]["provider_name"] == "Bay Hospital"
    assert out[0]["service_line"] == "ER visit"  # its newline flattened, so the lines stay paired
    assert rows[1]["provider_name"] == "Dr Smith Clinic"  # the input is not changed


def test_a_short_answer_fails_the_sync(monkeypatch):
    monkeypatch.setattr(main, "DEID_URL", "http://deid")
    bad = lambda url, json=None, **kw: _Resp({"text": "only one line", "audit": {}})  # noqa: E731
    with mock.patch("httpx.post", side_effect=bad), pytest.raises(RuntimeError):
        main._deid_rows("fl", [{"provider_name": "A"}, {"provider_name": "B"}])


class _FakeRedis:
    """Enough of redis for the sync lock: one store shared, as two replicas share FalkorDB."""
    store: dict = {}

    def set(self, k, v, nx=False, ex=None):
        if nx and k in self.store:
            return None
        self.store[k] = v.encode() if isinstance(v, str) else v
        return True

    def get(self, k):
        return self.store.get(k)

    def delete(self, k):
        self.store.pop(k, None)

    def exists(self, k):
        return int(k in self.store)

    def expire(self, k, ttl):
        return k in self.store


def test_a_sync_running_on_one_replica_is_seen_from_the_other(monkeypatch):
    _FakeRedis.store = {}
    monkeypatch.setattr(main, "_redis", lambda: _FakeRedis())
    started = []
    monkeypatch.setattr(main.threading, "Thread", lambda target, args, daemon: types.SimpleNamespace(start=lambda: started.append(args)))
    first = main.sync_from_db("fl")
    assert first["started"] and started == [("fl",)]
    second = main.sync_from_db("fl")  # e.g. the other replica: it sees the lock
    assert second.get("queued") and len(started) == 1
    assert main.sync_status("fl")["running"] is True


def test_the_queued_sync_runs_once_more_then_frees_the_lock(monkeypatch):
    _FakeRedis.store = {}
    monkeypatch.setattr(main, "_redis", lambda: _FakeRedis())
    runs = []
    def full(tenant):
        runs.append(tenant)
        if len(runs) == 1:  # someone asks again while the first runs
            main.sync_from_db(tenant)
        return {"tenant": tenant, "cases": 3}
    monkeypatch.setattr(main, "_full_sync", full)
    out = main.sync_from_db("fl", wait=True)
    assert runs == ["fl", "fl"] and out["ok"] and out["cases"] == 3
    st = main.sync_status("fl")
    assert st["running"] is False and st["last"]["ok"] is True


def test_a_failed_sync_is_reported_not_raised(monkeypatch):
    _FakeRedis.store = {}
    monkeypatch.setattr(main, "_redis", lambda: _FakeRedis())
    monkeypatch.setattr(main, "_full_sync", lambda t: (_ for _ in ()).throw(RuntimeError("deid 422")))
    out = main.sync_from_db("fl", wait=True)
    assert out["ok"] is False and "deid 422" in out["error"]
    assert main.sync_status("fl")["running"] is False
