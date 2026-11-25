"""Filling the office's own Word letters (assets/office) without changing how they look.

A template is one of the office's DOCX letters with each blank turned into a {field} (tools/make_office_templates.py):
the letterhead, headers, footers, fonts, spacing and tables are the office's, untouched. Filling it:

  {field}            replaced in place, in the run that holds it, so the run keeps its font, size and bold. A field
                     whose value is several paragraphs, alone in its paragraph, becomes that many paragraphs in the
                     paragraph's own format (a ("b", text) item is one in bold).
  {?name} ... {/name} a section kept only when the field `name` is true: from the paragraph holding {?name} up to the
                     paragraph holding {/name} (that one stays). A whole table inside goes with it.
  {#name}            in a table row: the row is repeated for each item of the list `name`, its cells' {r.key}
                     taken from the item; with no items the row goes.

A paragraph made only of fields (an address line) is dropped when they all come out empty, so a contact with no
second address line leaves no blank line. Body, tables, headers and footers are all filled.
"""
from __future__ import annotations

import copy
import io
import re

from docx import Document
from docx.oxml.ns import qn

FIELD = re.compile(r"\{([a-z0-9_.]+)\}")
MARK = re.compile(r"\{([?/#])([a-z0-9_]+)\}")
ONLY_FIELDS = re.compile(r"^[\s ]*(\{[a-z0-9_.]+\}[\s ]*)+$")


# ---- runs and text --------------------------------------------------------------------------------------------

def parts(doc):
    """The document body and every header and footer part."""
    out = [doc.element.body]
    for rel in doc.part.rels.values():
        if rel.reltype.endswith("/header") or rel.reltype.endswith("/footer"):
            out.append(rel.target_part.element)
    return out


def paragraphs(doc):
    for root in parts(doc):
        yield from list(root.iter(qn("w:p")))


def runs(p):
    """The paragraph's text runs in order, those inside hyperlinks and smart tags included."""
    return [r for r in p.iter(qn("w:r")) if r.find(qn("w:t")) is not None]


def rtext(r) -> str:
    return "".join(t.text or "" for t in r.findall(qn("w:t")))


def set_rtext(r, text: str) -> None:
    ts = r.findall(qn("w:t"))
    ts[0].text = text
    ts[0].set("{http://www.w3.org/XML/1998/namespace}space", "preserve")
    for t in ts[1:]:
        t.text = ""


def ptext(p) -> str:
    return "".join(rtext(r) for r in runs(p))


def unhighlight(r) -> None:
    rpr = r.find(qn("w:rPr"))
    if rpr is not None:
        for h in rpr.findall(qn("w:highlight")):
            rpr.remove(h)


def replace_in(p, old: str, new: str, start: int = 0, clear_highlight: bool = True) -> int:
    """Replace the first `old` at or after `start` in the paragraph's text, across runs if it spans them: the new
    text goes in the run where `old` began (keeping that run's look), the rest of `old` is taken out of the runs
    after it. Returns where the paragraph's text continues, or -1 when `old` is not there."""
    rs = runs(p)
    texts = [rtext(r) for r in rs]
    full = "".join(texts)
    k = full.find(old, start)
    if k < 0:
        return -1
    end, pos, placed = k + len(old), 0, False
    for r, t in zip(rs, texts):
        a0, b0 = pos, pos + len(t)
        pos = b0
        if b0 <= k or a0 >= end or (not t and a0 != k):
            continue
        a, b = max(k, a0) - a0, min(end, b0) - a0
        set_rtext(r, t[:a] + ("" if placed else new) + t[b:])
        if clear_highlight:
            unhighlight(r)
        placed = True
    return k + len(new)


def unwrap_hyperlinks_with_fields(p) -> None:
    """A field that ended up inside a hyperlink (a placeholder email address the office had linked) is plain text:
    the link pointed at the placeholder, not the address that replaces it."""
    for h in list(p.iter(qn("w:hyperlink"))):
        if FIELD.search("".join(t.text or "" for t in h.iter(qn("w:t")))):
            parent = h.getparent()
            i = parent.index(h)
            for child in list(h):
                parent.insert(i, child)
                i += 1
            parent.remove(h)


# ---- filling --------------------------------------------------------------------------------------------------

def _truthy(v) -> bool:
    return bool(v) and v not in ("0", "false", "False")


def _repeat_rows(doc, fields: dict) -> None:
    for root in parts(doc):
        for tr in list(root.iter(qn("w:tr"))):
            txt = "".join(t.text or "" for t in tr.iter(qn("w:t")))
            m = re.search(r"\{#([a-z0-9_]+)\}", txt)
            if not m:
                continue
            items = fields.get(m.group(1)) or []
            parent = tr.getparent()
            at = parent.index(tr)
            for n, item in enumerate(items):
                row = copy.deepcopy(tr)
                for p in row.iter(qn("w:p")):
                    while replace_in(p, "{#" + m.group(1) + "}", "") >= 0:
                        pass
                    for f in set(FIELD.findall(ptext(p))):
                        if f.startswith("r."):
                            while replace_in(p, "{" + f + "}", str(item.get(f[2:], "") or "")) >= 0:
                                pass
                parent.insert(at + 1 + n, row)
            parent.remove(tr)


def _sections(doc, fields: dict) -> None:
    """Drop each {?name} ... {/name} span whose field is false, and the markers of those kept."""
    body = doc.element.body
    dropping: list[str] = []  # the sections now being dropped
    for el in list(body):
        if el.tag not in (qn("w:p"), qn("w:tbl")):
            continue
        txt = "".join(t.text or "" for t in el.iter(qn("w:t")))
        for kind, name in MARK.findall(txt):
            if kind == "/" and name in dropping:
                dropping.remove(name)
        if dropping:
            body.remove(el)
            continue
        for kind, name in MARK.findall(txt):
            if kind == "?" and not _truthy(fields.get(name)):
                dropping.append(name)
        if dropping:
            body.remove(el)
            continue
        if el.tag == qn("w:p"):
            for kind, name in MARK.findall(txt):
                if kind in "?/":
                    while replace_in(el, "{" + kind + name + "}", "") >= 0:
                        pass


def _paras_of(v) -> list:
    if isinstance(v, (list, tuple)):
        out = []
        for x in v:
            out.extend(_paras_of(x) if not (isinstance(x, tuple) and len(x) == 2 and x[0] == "b") else [x])
        return out
    return [s for s in re.split(r"\n\s*\n|\n", str(v)) if s.strip()] if v not in (None, "") else []


def _set_para_text(p, text: str, bold: bool | None = None) -> None:
    rs = runs(p)
    if not rs:
        return
    set_rtext(rs[0], text)
    for r in rs[1:]:
        set_rtext(r, "")
    if bold is not None:
        rpr = rs[0].find(qn("w:rPr"))
        if rpr is None:
            rpr = rs[0].makeelement(qn("w:rPr"), {})
            rs[0].insert(0, rpr)
        for b in rpr.findall(qn("w:b")):
            rpr.remove(b)
        if bold:
            rpr.insert(0, rpr.makeelement(qn("w:b"), {}))


def _fill_paragraph(p, fields: dict) -> None:
    txt = ptext(p)
    names = FIELD.findall(txt)
    if not names:
        return
    only = ONLY_FIELDS.match(txt) is not None
    # One field alone in its paragraph with several paragraphs to say: one paragraph each, in this one's format.
    if only and len(names) == 1:
        v = fields.get(names[0], "")
        items = _paras_of(v)
        if len(items) > 1:
            parent, at = p.getparent(), p.getparent().index(p)
            for n, item in enumerate(items):
                q = copy.deepcopy(p) if n else p
                bold, text = (True, item[1]) if isinstance(item, tuple) else (None, item)
                _set_para_text(q, text, bold)
                if n:
                    parent.insert(at + n, q)
            return
    for name in names:
        v = fields.get(name, "")
        v = " ".join(x[1] if isinstance(x, tuple) else x for x in _paras_of(v)) if isinstance(v, (list, tuple)) else str(v or "")
        replace_in(p, "{" + name + "}", v)
    # An address line whose fields were all empty leaves no blank line.
    if only and not ptext(p).replace(" ", "").strip():
        p.getparent().remove(p)


def fill(template: bytes, fields: dict) -> bytes:
    doc = Document(io.BytesIO(template))
    _repeat_rows(doc, fields)
    _sections(doc, fields)
    for p in list(paragraphs(doc)):
        if p.getparent() is not None:
            _fill_paragraph(p, fields)
    out = io.BytesIO()
    doc.save(out)
    return out.getvalue()


def fields_in(template: bytes) -> set[str]:
    """Every {field} a template asks for (its section and row markers excluded)."""
    doc = Document(io.BytesIO(template))
    return {f for p in paragraphs(doc) for f in FIELD.findall(ptext(p))}
