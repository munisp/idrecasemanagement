# Author: Patrick Munis at newwave
"""Appointment of representative (AOR) letters (2026-11-25, change log #6).

A filer may upload, with its filing, a letter on its letterhead that names who may act for it -- e.g. "I, <name>,
<title> at <company>, certify that the following individuals can act on behalf of <company> regarding Florida
Claim Disputes: <names>", signed. This reads who authorizes it (name, title, company) and the people named, for the
Reviewer's AOR check; nothing is applied to the case on its own.
"""
from __future__ import annotations

import re

TYPE_SIGNALS = {
    "aor_letter": {
        "strong": ["can act on behalf of", "to act on behalf of", "appointment of representative", "hereby appointed to act",
                   "authorized representative", "regarding florida claim disputes", "certify that the following individuals"],
        "weak": ["on behalf of", "representative", "authorize", "authorise", "certify", "claim disputes"],
    },
}

_WHO = re.compile(r"(?s)\bI,\s*([A-Z][\w.'\- ]{1,60}?),\s*([^,\n]{2,80}?)\s+(?:at|of|for)(?:\s+|(?=[A-Z]))([^,\n]{2,80}?),\s*(?:[Cc]ertify|[Hh]ereby|[Aa]uthori[sz]e|[Aa]ppoint)")  # "Officer atHaloMD": "at" glued to the name
_LIST_START = re.compile(r"(?is)(?:act on behalf of|appointed to act|authori[sz]ed to act)[^:\n]{0,160}:")
_NAME = re.compile(r"^[A-Z][a-zA-Z.'\-]+(?:\s+[A-Z][a-zA-Z.'\-]+){1,2}$")  # two or three words


def extract(doc_type: str, text: str) -> tuple[dict, list[str]]:
    """(fields, issues) for an AOR letter; ({}, []) for any other type."""
    if doc_type != "aor_letter":
        return {}, []
    t = text or ""
    out: dict = {"letter_type": "aor_letter"}
    issues: list[str] = []
    m = _WHO.search(t)
    if m:
        out["authorized_by"] = m.group(1).strip()
        out["authorized_by_title"] = m.group(2).strip()
        out["company"] = m.group(3).strip()
    names: list[str] = []
    s = _LIST_START.search(t)
    if s:
        for line in t[s.end():].splitlines():
            line = re.sub(r"^[\s\-*•·\d.)]+", "", line).strip()
            if not line:
                continue
            found = _names_in(line, out.get("authorized_by", ""))
            if found:
                names.extend(found)
                continue
            if names:  # the list has ended (the signature block)
                break
    if names:
        out["representatives"] = names[:50]
    if not m:
        issues.append("AOR letter: who authorizes it (name, title, company) was not found — check the letter")
    if not names:
        issues.append("AOR letter: no representatives were read from it — check the names on the letter")
    return out, issues


_CAP = re.compile(r"^[A-Z][a-zA-Z.'\-]+$")


def _names_in(line: str, signer: str) -> list[str]:
    """The people on one line of the list: one name, names separated by commas or semicolons, or -- where a reader
    ran the list's lines together -- a run of capitalised words, taken two at a time ("Casey Filer Kim Lowe")."""
    parts = [x.strip() for x in re.split(r"[;,]", line) if x.strip()]
    if len(parts) > 1 and all(_NAME.match(x) for x in parts):
        return [x for x in parts if x != signer]
    if _NAME.match(line):
        return [line] if line != signer else []
    words = line.split()
    if len(words) >= 4 and len(words) % 2 == 0 and all(_CAP.match(w) for w in words):
        pairs = [" ".join(words[i:i + 2]) for i in range(0, len(words), 2)]
        return [x for x in pairs if x != signer]
    return []
