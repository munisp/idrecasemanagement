#!/usr/bin/env python3
"""Build the NSA/IDRE instruction dataset (JSONL) for QLoRA fine-tuning.

Sources, in priority order:
  1. eCFR — 45 CFR Part 149 pulled live from the official API (no scraping,
     no drift: the API is the current Code of Federal Regulations).
  2. corpus/ — drop additional authority texts here as .txt (2026 final rule
     preamble extracts, CMS IDR guidance, your IDRE SOPs, determination
     letter exemplars). Each file is chunked and templated the same way.

Output rows (JSONL, one per line):
  {"instruction": ..., "input": ..., "output": ...}

Three row types, deliberately:
  RECALL   — section-grounded Q&A ("What does 45 CFR 149.510 require?")
             teaches terminology + procedure in the platform's voice.
  CITE     — citation-fidelity pairs: the answer MUST name the section;
             this is the muscle memory that prevents invented citations.
  REFUSAL  — questions whose answers are NOT in the source, answered with
             the platform's exact refusal phrase. Fine-tuning WITHOUT
             refusal pairs is what produces a fluent hallucinator; with
             them, the model learns the boundary of its own knowledge.

Usage:
  python3 build_dataset.py [--date 2026-01-01] [--corpus corpus/] [-o dataset.jsonl]
"""
import argparse
import json
import re
import sys
import urllib.request
import xml.etree.ElementTree as ET
from pathlib import Path

ECFR_URL = ("https://www.ecfr.gov/api/versioner/v1/full/{date}/title-45.xml"
            "?part=149&subtitle=B&chapter=A&subchapter=B")
REFUSAL = ("That is not in the record or authority available to me. "
           "I don't speculate on NSA/IDR requirements — check the cited "
           "regulation or ask the case coordinator.")


def fetch_part149(date: str) -> str:
    url = ECFR_URL.format(date=date)
    req = urllib.request.Request(url, headers={"User-Agent": "idre-copilot-training/1.0"})
    with urllib.request.urlopen(req, timeout=120) as r:
        return r.read().decode("utf-8")


def sections_from_ecfr(xml_text: str) -> list[tuple[str, str]]:
    """(section number, clean text) pairs from eCFR XML. Sections are DIV8
    elements headed N='149.x'. Defensive: any DIV8 whose HEAD mentions a
    section number is taken."""
    out = []
    root = ET.fromstring(xml_text)
    for div in root.iter("DIV8"):
        sec_no = div.attrib.get("N", "")
        text = re.sub(r"\s+", " ", " ".join(div.itertext())).strip()
        if sec_no and len(text) > 120:
            out.append((sec_no, text))
    return out


def condense(text: str, max_sent: int = 3) -> str:
    """Extractive condense — first sentences carry the operative rule in CFR
    drafting style. Never paraphrase: training on paraphrase teaches the
    model to paraphrase; training on statute text teaches it to quote."""
    sents = re.split(r"(?<=[.!?])\s+", text)
    return " ".join(sents[:max_sent]).strip()


def rows_from_section(sec: str, text: str) -> list[dict]:
    head, _, _ = text.partition(" - ")
    title = head if " - " in text else f"Section {sec}"
    cite = f"45 CFR {sec}"
    rows = [
        {"instruction": f"What does {cite} ({title}) require?",
         "input": "",
         "output": f"{condense(text)} ({cite}.)"},
        {"instruction": f"Which regulation governs {title.lower()}, and what is its operative requirement?",
         "input": "",
         "output": f"{cite}. {condense(text, 2)}"},
    ]
    return rows


REFUSAL_QUESTIONS = [
    "What will the arbitrated out-of-network rate be for case IDR-2026-0004?",
    "Is Dr. Smith's billing pattern fraudulent?",
    "What is the QPA for CPT 99285 in Dallas in 2027?",
    "Will the payer win this dispute?",
    "What did the parties say in their confidential settlement talks?",
    "Give me the phone number of the arbitrator assigned to my case.",
    "What is the filing deadline under 45 CFR 149.999?",
    "How should I structure offers to guarantee I win?",
]


def refusal_rows() -> list[dict]:
    return [{"instruction": q, "input": "", "output": REFUSAL} for q in REFUSAL_QUESTIONS]


def rows_from_corpus_file(path: Path) -> list[dict]:
    text = path.read_text(encoding="utf-8", errors="replace")
    chunks = [c.strip() for c in re.split(r"\n\s*\n", text) if len(c.strip()) > 300]
    rows = []
    for i, ch in enumerate(chunks[:200]):  # cap per file: dominance skews the mix
        rows.append({"instruction": f"Per {path.stem}, what does this passage establish? (chunk {i+1})",
                     "input": ch[:3000],
                     "output": condense(ch)})
    return rows


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--date", default="2026-01-01", help="eCFR point-in-time date")
    ap.add_argument("--corpus", default="corpus", help="dir of extra authority .txt files")
    ap.add_argument("-o", "--out", default="dataset.jsonl")
    a = ap.parse_args()

    rows: list[dict] = []
    try:
        secs = sections_from_ecfr(fetch_part149(a.date))
        for sec, text in secs:
            rows.extend(rows_from_section(sec, text))
        print(f"ecfr: {len(secs)} sections -> {len(rows)} rows", file=sys.stderr)
    except Exception as e:  # offline is fine if corpus/ is populated
        print(f"ecfr fetch failed ({e}); continuing with corpus only", file=sys.stderr)

    corpus = Path(a.corpus)
    if corpus.is_dir():
        for f in sorted(corpus.glob("*.txt")):
            n0 = len(rows)
            rows.extend(rows_from_corpus_file(f))
            print(f"corpus {f.name}: +{len(rows)-n0} rows", file=sys.stderr)

    rows.extend(refusal_rows())
    n = 0
    with open(a.out, "w", encoding="utf-8") as fh:
        for r in rows:
            fh.write(json.dumps(r, ensure_ascii=False) + "\n")
            n += 1
    print(f"wrote {n} rows -> {a.out}  (refusal rows: {len(refusal_rows())})", file=sys.stderr)


if __name__ == "__main__":
    main()
