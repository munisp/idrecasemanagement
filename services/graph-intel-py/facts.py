"""facts.py -- the program's figures, for questions that are not about one
case or one party.

The dispute graph answers "what connects A and B" and "what is related to
case X". Most questions people ask a dispute program are not of that kind:
how many are open, who files the most, what is late, why were disputes found
ineligible, how much money is in dispute. Those are counts and sums over the
tenant's own records, so they are computed here from Postgres -- the source
of truth -- and handed to the answerer as evidence lines, the same way graph
paths are. Nothing is estimated: a figure that cannot be computed is left
out, and the answerer is told to say so.

Every query stands alone. A tenant without a column or table one of them
needs (a federal tenant has no eligibility reviews) loses that one line.
"""
from __future__ import annotations

import os
import re
import threading
import time

import psycopg

DSN = os.environ.get("DATABASE_URL", "postgres://idre:idre@localhost:5432/idre")
_TTL = float(os.environ.get("FACTS_TTL_SECONDS", "45"))
_cache: dict[str, tuple[float, dict]] = {}
_lock = threading.Lock()

# The categories, in the order they are shown, with the words that send a
# question to each. A question matching none gets the overview.
CATEGORIES: list[tuple[str, str]] = [
    ("caseload", r"how many|total|status|stage|open|closed|summary|summar|overview|backlog|caseload|waiting|opened|new|"
                 r"current|all (the )?(cases|disputes)|in the (system|program|platform)|details? of|list (of )?(the )?(cases|disputes)"),
    ("parties", r"who|filer|filed|files?\b|filing|provider|plan|payer|organi[sz]ation|most often|top"),
    ("services", r"service|line\b|radiology|ambulance|anesthesia|lab\b|emergency|\ber\b|imaging|specialt"),
    ("deadlines", r"deadline|late|overdue|breach|sla|older|aging|ageing|days|limit|due\b|missed|escalat|"
                  r"deliverable|how long|turnaround|take\b"),
    ("eligibility", r"eligib|ineligib|threshold|authorization|\baor\b|borderline|second look|reason"),
    ("money", r"amount|dollar|\$|value|largest|biggest|million|average|disputed|money|fee|worth|award"),
    ("outcomes", r"outcome|decided|decision|award|prevail|won\b|win\b|result|determin"),
]


def _schema(tenant: str) -> str:
    return "tenant_" + "".join(c for c in tenant.lower() if c.isalnum() or c == "_")


def _money(cents) -> str:
    return f"${(int(cents or 0)) / 100:,.2f}"


_SERVICE_NAMES = {"ER": "ER (emergency)", "LAB": "LAB (laboratory)", "AIR_AMBULANCE": "AIR_AMBULANCE (air ambulance)",
                  "RADIOLOGY": "RADIOLOGY (imaging)"}


def _svc(name):
    return _SERVICE_NAMES.get(name, name)


def _pairs(rows, unit: str = "") -> str:
    return ", ".join(f"{name or '(not recorded)'}: {n}{unit}" for name, n in rows)


def gather(tenant: str) -> dict[str, list[str]]:
    """{category: [fact lines]} for a tenant, cached briefly."""
    now = time.time()
    with _lock:
        hit = _cache.get(tenant)
        if hit and now - hit[0] < _TTL:
            return hit[1]
    out = _compute(tenant)
    with _lock:
        _cache[tenant] = (now, out)
    return out


def _compute(tenant: str) -> dict[str, list[str]]:
    t = _schema(tenant)
    out: dict[str, list[str]] = {name: [] for name, _ in CATEGORIES}
    try:
        conn = psycopg.connect(DSN, autocommit=True, connect_timeout=4)
    except Exception:
        return out
    # No "%" anywhere in these statements: some run with parameters, where a
    # bare percent sign would be read as a placeholder.
    closed = "(left(upper(status),6) = 'CLOSED' OR coalesce(agency_status,'') IN ('Closed','Decided'))"

    def rows(sql: str, params: tuple = ()) -> list:
        try:
            return conn.execute(sql, params or None).fetchall()
        except Exception:
            return []

    def one(sql: str, params: tuple = ()):
        r = rows(sql, params)
        return r[0] if r else None

    def add(cat: str, line: str | None) -> None:
        if line:
            out[cat].append(line)

    with conn:
        # ---- caseload ---------------------------------------------------
        r = one(f"SELECT count(*), count(*) FILTER (WHERE {closed}), "
                f"count(*) FILTER (WHERE opened_at > now() - interval '30 days') FROM {t}.cases")
        if r and r[0]:
            add("caseload", f"There are {r[0]} disputes in total: {r[0] - r[1]} open and {r[1]} closed or decided.")
            add("caseload", f"{r[2]} disputes were opened in the last 30 days.")
        rs = rows(f"SELECT status, count(*) FROM {t}.cases GROUP BY status ORDER BY count(*) DESC")
        add("caseload", rs and "Disputes by status (the stage each is in): " + _pairs(rs) + ".")
        rs = rows(f"SELECT internal_status, count(*) FROM {t}.cases WHERE internal_status IS NOT NULL "
                  f"GROUP BY internal_status ORDER BY count(*) DESC LIMIT 12")
        add("caseload", rs and "Disputes by internal status (where the work is): " + _pairs(rs) + ".")
        rs = rows(f"SELECT agency_status, count(*) FROM {t}.cases WHERE agency_status IS NOT NULL "
                  f"GROUP BY agency_status ORDER BY count(*) DESC")
        add("caseload", rs and "Disputes by agency status: " + _pairs(rs) + ".")

        # ---- parties ----------------------------------------------------
        rs = rows(f"SELECT details->>'requester_org', count(*) FROM {t}.cases "
                  f"WHERE coalesce(details->>'requester_org','') <> '' GROUP BY 1 ORDER BY count(*) DESC LIMIT 8")
        add("parties", rs and "Organizations that filed the most disputes: " + _pairs(rs) + ".")
        rs = rows(f"SELECT CASE details->>'filing_party_type' WHEN 'HEALTH_PLAN' THEN 'health plans' "
                  f"WHEN 'PROVIDER' THEN 'providers' ELSE 'not recorded' END, count(*) FROM {t}.cases GROUP BY 1 "
                  f"ORDER BY count(*) DESC")
        add("parties", rs and "Disputes by who filed: " + _pairs(rs) + ".")
        rs = rows(f"SELECT details->>'health_plan', count(*) FROM {t}.cases "
                  f"WHERE coalesce(details->>'health_plan','') <> '' GROUP BY 1 ORDER BY count(*) DESC LIMIT 8")
        add("parties", rs and "Health plans named in the most disputes: " + _pairs(rs) + ".")
        rs = rows(f"SELECT details->>'provider_org', count(*) FROM {t}.cases "
                  f"WHERE coalesce(details->>'provider_org','') <> '' GROUP BY 1 ORDER BY count(*) DESC LIMIT 8")
        add("parties", rs and "Providers named in the most disputes: " + _pairs(rs) + ".")
        r = one(f"SELECT count(*) FILTER (WHERE coalesce(details->>'health_plan','') = ''), count(*) FROM {t}.cases")
        if r and r[1] and r[0]:
            add("parties", f"{r[0]} of {r[1]} disputes have no health plan name recorded yet, so plan rankings cover only the rest.")
        rs = rows(f"SELECT (details->>'provider_org') || ' vs ' || (details->>'health_plan'), count(*) FROM {t}.cases "
                  f"WHERE coalesce(details->>'provider_org','') <> '' AND coalesce(details->>'health_plan','') <> '' "
                  f"GROUP BY 1 ORDER BY count(*) DESC LIMIT 5")
        add("parties", rs and "Provider and plan pairs that appear together most: " + _pairs(rs) + ".")

        # ---- services ---------------------------------------------------
        rs = rows(f"SELECT service_line, count(*) FROM {t}.cases GROUP BY service_line ORDER BY count(*) DESC")
        add("services", rs and "Disputes by service line: " + _pairs([(_svc(n), c) for n, c in rs]) + ".")
        rs = rows(f"SELECT service_line, round(avg(disputed_amount_cents)) FROM {t}.cases "
                  f"WHERE service_line IS NOT NULL AND disputed_amount_cents > 0 GROUP BY service_line "
                  f"ORDER BY avg(disputed_amount_cents) DESC")
        add("money", rs and "Average disputed amount by service line: "
            + ", ".join(f"{n}: {_money(v)}" for n, v in rs) + ".")
        rs = rows(f"SELECT service_line, string_agg(case_number, ', ') FROM ("
                  f"SELECT service_line, case_number, row_number() OVER (PARTITION BY service_line ORDER BY opened_at DESC) rn "
                  f"FROM {t}.cases WHERE service_line IS NOT NULL) x WHERE rn <= 5 GROUP BY service_line")
        for name, nums in rs:
            add("services", f"Most recent {_svc(name)} disputes: {nums}.")

        # ---- deadlines --------------------------------------------------
        r = one(f"SELECT count(*) FILTER (WHERE opened_at < now() - interval '60 days'), "
                f"count(*) FILTER (WHERE opened_at < now() - interval '90 days'), count(*) "
                f"FROM {t}.cases WHERE NOT {closed}")
        if r and r[2]:
            add("deadlines", f"Of {r[2]} open disputes, {r[0]} have been open more than 60 days and {r[1]} more than 90 days.")
        rs = rows(f"SELECT case_number, (now()::date - opened_at::date) FROM {t}.cases WHERE NOT {closed} "
                  f"AND opened_at < now() - interval '60 days' ORDER BY opened_at LIMIT 8")
        add("deadlines", rs and "Oldest open disputes past 60 days: "
            + ", ".join(f"{n} ({d} days)" for n, d in rs) + ".")
        rs = rows("SELECT clock, count(*) FROM public.escalations WHERE tenant=%s GROUP BY clock ORDER BY count(*) DESC",
                  (tenant,))
        add("deadlines", rs and "Missed deadlines recorded, by type: " + _pairs(rs) + ".")
        if not rs:
            add("deadlines", "No missed deadlines have been recorded.")
        rs = rows("SELECT coalesce(timeliness, status), count(*) FROM public.deliverables WHERE tenant=%s "
                  "GROUP BY 1 ORDER BY count(*) DESC", (tenant,))
        add("deadlines", rs and "Contract deliverables by state: " + _pairs(rs) + ".")
        rs = rows("SELECT name, due_date::date FROM public.deliverables WHERE tenant=%s AND status='DUE' "
                  "ORDER BY due_date LIMIT 6", (tenant,))
        add("deadlines", rs and "Deliverables currently due: " + ", ".join(f"{n} (due {d})" for n, d in rs) + ".")
        r = one(f"SELECT round(avg(extract(epoch FROM (updated_at - opened_at)) / 86400)), count(*) FROM {t}.cases "
                f"WHERE {closed} AND updated_at > opened_at")
        if r and r[1]:
            add("deadlines", f"Closed disputes took {int(r[0])} days on average from opening to their last update ({r[1]} cases).")

        # ---- eligibility ------------------------------------------------
        latest = ("(SELECT DISTINCT ON (case_id) case_id, result, reason, borderline FROM public.eligibility_reviews "
                  "WHERE tenant=%s ORDER BY case_id, created_at DESC) e")
        rs = rows(f"SELECT result, count(*) FROM {latest} GROUP BY result ORDER BY count(*) DESC", (tenant,))
        add("eligibility", rs and "Latest eligibility decision per dispute: " + _pairs(rs) + ".")
        rs = rows(f"SELECT reason, count(*) FROM {latest} WHERE result='INELIGIBLE' GROUP BY reason "
                  f"ORDER BY count(*) DESC LIMIT 8", (tenant,))
        add("eligibility", rs and "Reasons for ineligibility: "
            + _pairs([(str(n or '').replace('_', ' '), c) for n, c in rs]) + ".")
        r = one(f"SELECT count(*) FILTER (WHERE result='HOLD_AOR'), count(*) FILTER (WHERE borderline) FROM {latest}",
                (tenant,))
        if r:
            add("eligibility", f"{r[0]} disputes are on hold for an invalid authorization of representative; "
                               f"{r[1]} decisions were borderline and sent for a second look.")
        r = one(f"SELECT count(*) FROM {t}.cases c WHERE NOT {closed} AND status NOT IN ('AWAITING_PAYMENT') "
                f"AND NOT EXISTS (SELECT 1 FROM public.eligibility_reviews e WHERE e.tenant=%s AND e.case_id::text=c.id::text)",
                (tenant,))
        if r:
            add("eligibility", f"{r[0]} open disputes past payment have no eligibility decision yet.")

        # ---- money ------------------------------------------------------
        r = one(f"SELECT count(*), sum(disputed_amount_cents), round(avg(disputed_amount_cents)), "
                f"percentile_cont(0.5) WITHIN GROUP (ORDER BY disputed_amount_cents), "
                f"count(*) FILTER (WHERE disputed_amount_cents >= 100000000) "
                f"FROM {t}.cases WHERE disputed_amount_cents > 0")
        if r and r[0]:
            add("money", f"Total amount in dispute: {_money(r[1])} across {r[0]} disputes with an amount recorded. "
                         f"Average {_money(r[2])}, median {_money(r[3])}.")
            add("money", f"{r[4]} disputes are $1,000,000 or more.")
        rs = rows(f"SELECT case_number, disputed_amount_cents FROM {t}.cases WHERE disputed_amount_cents > 0 "
                  f"ORDER BY disputed_amount_cents DESC LIMIT 5")
        add("money", rs and "Five largest disputes by amount: " + ", ".join(f"{n} ({_money(v)})" for n, v in rs) + ".")
        r = one(f"SELECT count(*) FROM {t}.cases WHERE coalesce(disputed_amount_cents,0) = 0")
        if r and r[0]:
            add("money", f"{r[0]} disputes have no amount recorded and are not in these totals.")

        # ---- outcomes ---------------------------------------------------
        rs = rows(f"SELECT agency_status, count(*) FROM {t}.cases WHERE agency_status IN ('Decided','Closed') "
                  f"GROUP BY agency_status")
        add("outcomes", rs and "Decided and closed disputes: " + _pairs(rs) + ".")
        rs = rows(f"SELECT details->>'case_outcome', count(*) FROM {t}.cases "
                  f"WHERE coalesce(details->>'case_outcome','') <> '' GROUP BY 1 ORDER BY count(*) DESC")
        add("outcomes", rs and "Recorded outcomes: " + _pairs(rs) + ".")
        r = one(f"SELECT count(*), sum((details->>'final_amount_awarded_cents')::bigint) FROM {t}.cases "
                f"WHERE details->>'final_amount_awarded_cents' ~ '^[0-9]+$'")
        if r and r[0]:
            add("money", f"Final amount awarded is recorded on {r[0]} disputes, totalling {_money(r[1])}.")
        else:
            add("money", "No final award amounts have been recorded yet.")
        rs = rows(f"SELECT status_reason, count(*) FROM {t}.cases WHERE {closed} AND coalesce(status_reason,'') <> '' "
                  f"GROUP BY status_reason ORDER BY count(*) DESC LIMIT 6")
        add("outcomes", rs and "Why disputes closed: " + _pairs(rs) + ".")
    return out


# What each category is called when a caller is told it is not theirs to ask.
LABELS = {"caseload": "caseload", "parties": "who files (Reports)", "services": "services",
          "deadlines": "deadlines and turnaround (Ops and Reports)", "eligibility": "eligibility totals (Reports)",
          "money": "amounts and awards (Financials)", "outcomes": "outcomes (Reports)"}


def matching(question: str) -> list[str]:
    """The categories a question is about."""
    q = (question or "").lower()
    return [name for name, words in CATEGORIES if re.search(words, q)]


def select(question: str, facts: dict[str, list[str]], limit: int = 40) -> list[str]:
    """The fact lines that bear on a question: its categories' lines, or a
    line or two from every category when it matches none."""
    q = (question or "").lower()
    picked = [name for name, words in CATEGORIES if re.search(words, q) and facts.get(name)]
    if not picked:
        return [line for name, _ in CATEGORIES for line in facts.get(name, [])[:2]][:limit]
    # "How many ..." is in every count question, so caseload usually matches
    # beside a narrower category. Nothing is dropped for that: the answerer
    # ranks these lines against the question, and "waiting on the provider"
    # is a caseload line even though the question says "provider".
    narrow = [n for n in picked if n != "caseload"]
    order = narrow + (["caseload"] if "caseload" in picked else [])
    return [line for name in order for line in facts[name]][:limit]


def hints(tenant: str) -> dict:
    """Real names from this tenant for the portal's sample questions, so a
    suggested question is always about something that exists."""
    t = _schema(tenant)
    out = {"case_numbers": [], "filers": [], "plans": [], "service_lines": [], "pair": []}
    try:
        with psycopg.connect(DSN, autocommit=True, connect_timeout=4) as c:
            def col(sql: str) -> list[str]:
                try:
                    return [r[0] for r in c.execute(sql).fetchall() if r[0]]
                except Exception:
                    return []
            out["case_numbers"] = col(
                f"SELECT case_number FROM {t}.cases WHERE coalesce(details->>'requester_org','') <> '' "
                f"AND case_number ~ '[0-9]' ORDER BY opened_at DESC LIMIT 2") or col(
                f"SELECT case_number FROM {t}.cases ORDER BY opened_at DESC LIMIT 2")
            out["filers"] = col(
                f"SELECT details->>'requester_org' FROM {t}.cases WHERE coalesce(details->>'requester_org','') <> '' "
                f"GROUP BY 1 ORDER BY count(*) DESC LIMIT 2")
            out["plans"] = col(
                f"SELECT details->>'health_plan' FROM {t}.cases WHERE coalesce(details->>'health_plan','') <> '' "
                f"GROUP BY 1 ORDER BY count(*) DESC LIMIT 1")
            try:
                r = c.execute(
                    f"SELECT details->>'provider_org', details->>'health_plan' FROM {t}.cases "
                    f"WHERE coalesce(details->>'provider_org','') <> '' AND coalesce(details->>'health_plan','') <> '' "
                    f"GROUP BY 1, 2 ORDER BY count(*) DESC LIMIT 1").fetchone()
                out["pair"] = list(r) if r else []
            except Exception:
                pass
            out["service_lines"] = col(
                f"SELECT service_line FROM {t}.cases WHERE service_line IS NOT NULL AND service_line <> 'OTHER' "
                f"GROUP BY 1 ORDER BY count(*) DESC LIMIT 2")
    except Exception:
        pass
    return out


def case_lines(tenant: str, case_id: str) -> list[str]:
    """What the records say about one dispute, and which other disputes share
    its filer or its plan -- the questions asked about a single case."""
    t = _schema(tenant)
    out: list[str] = []
    try:
        with psycopg.connect(DSN, autocommit=True, connect_timeout=4) as c:
            r = c.execute(
                f"SELECT case_number, status, internal_status, agency_status, service_line, disputed_amount_cents, "
                f"opened_at::date, details->>'requester_org', details->>'provider_org', details->>'health_plan', "
                f"details->>'provider_type', details->>'filing_party_type' FROM {t}.cases WHERE id::text=%s", (case_id,)
            ).fetchone()
            if not r:
                return out
            num, status, internal, agency, svc, cents, opened, filer, prov, plan, ptype, ftype = r
            bits = [f"status {status}"]
            for label, v in (("internal status", internal), ("agency status", agency), ("service line", _svc(svc) if svc else None),
                             ("provider type", (ptype or "").replace("_", " ") or None), ("opened", opened)):
                if v:
                    bits.append(f"{label} {v}")
            if cents:
                bits.append(f"disputed amount {_money(cents)}")
            out.append(f"{num}: " + ", ".join(bits) + ".")
            who = [f"filed by {filer}" + (" (a health plan)" if ftype == "HEALTH_PLAN" else "") if filer else None,
                   f"provider {prov}" if prov else None, f"health plan {plan}" if plan else None]
            if any(who):
                out.append(f"{num} parties: " + "; ".join(w for w in who if w) + ".")
            e = c.execute("SELECT result, reason, created_at::date FROM public.eligibility_reviews "
                          "WHERE tenant=%s AND case_id::text=%s ORDER BY created_at DESC LIMIT 1", (tenant, case_id)).fetchone()
            out.append(f"{num} eligibility: " + (f"{e[0]}" + (f" ({str(e[1]).replace('_', ' ')})" if e[1] else "")
                                                 + f", decided {e[2]}." if e else "no decision recorded yet."))
            for label, key, val in (("filer", "requester_org", filer), ("health plan", "health_plan", plan)):
                if not val:
                    continue
                sib = c.execute(
                    f"SELECT case_number FROM {t}.cases WHERE details->>'{key}'=%s AND id::text<>%s "
                    f"ORDER BY opened_at DESC LIMIT 10", (val, case_id)).fetchall()
                n = c.execute(f"SELECT count(*) FROM {t}.cases WHERE details->>'{key}'=%s AND id::text<>%s",
                              (val, case_id)).fetchone()[0]
                out.append(f"Other disputes with the same {label} as {num} ({val}): "
                           + (", ".join(x[0] for x in sib) + (f" and {n - len(sib)} more" if n > len(sib) else "")
                              if sib else "none") + ".")
    except Exception:
        pass
    return out
