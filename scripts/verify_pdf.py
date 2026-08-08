#!/usr/bin/env python3
"""ATS verification for a rendered resume.

Runs against EVERY PDF text engine installed, not one. ATS vendors do not agree
on an extractor, and a text layer that is clean in one can be rubble in another
— so a single-engine pass is not evidence the resume parses.

Four levels of check, because they catch different failures:

* must_contain (lenient) — whole sentences and bullets, compared with ALL
  whitespace stripped, since an extractor legitimately re-wraps lines.

* must_contain_exact (strict) — single tokens containing no spaces of their own
  (AWS, TypeScript, PostgreSQL, an email address). These must appear verbatim.
  The lenient check cannot catch a kern split here: "A WS" and "AWS" both
  collapse to "aws" once whitespace is stripped, so a PDF where an ATS could
  never keyword-match "AWS" was passing. Found 2026-08-05 on a real render.

* word separation (strict) — multi-word phrases must survive with their spaces
  INTACT. The lenient check is blind here for the same reason it was blind to
  kern splits: it strips the very whitespace under test, so a page extracting as
  "Backend-focusedengineerwith1year" passed clean. xdvipdfmx writes no space
  glyphs at all — word gaps are positional, and an extractor rebuilds them by
  comparing each gap to a tolerance. Justification was squeezing gaps to 2.86pt,
  under pdfplumber's 3.0pt default, and every keyword on the page died in that
  engine while pypdf saw nothing wrong. Found 2026-08-07.

* hazards — ligatures, soft hyphens and line-break hyphenation, which turn a
  searchable word into one an ATS cannot match.
"""
import json
import re
import sys
import unicodedata

# typographic chars LaTeX may emit -> ASCII equivalents an ATS would expect
FOLD = str.maketrans({"’": "'", "‘": "'", "“": '"', "”": '"',
                      "–": "-", "—": "-", "­": ""})

LIGATURES = {"ﬀ": "ff", "ﬁ": "fi", "ﬂ": "fl", "ﬃ": "ffi", "ﬄ": "ffl"}


def fold(s: str) -> str:
    """NFKC (splits ligatures) + typographic punctuation to ASCII."""
    return unicodedata.normalize("NFKC", s).translate(FOLD)


def norm(s: str) -> str:
    return "".join(fold(s).lower().split())


def flat(s: str) -> str:
    """Fold, lowercase, collapse whitespace runs — but KEEP word boundaries."""
    return re.sub(r"\s+", " ", fold(s).lower()).strip()


def extract_all(path):
    """Every engine we can import. Missing libraries are skipped, not fatal."""
    out = {}

    try:
        from pypdf import PdfReader
        r = PdfReader(path)
        out["pypdf"] = ("\n".join(p.extract_text() or "" for p in r.pages),
                        len(r.pages))
    except Exception as e:
        print(f"  ! pypdf unavailable ({e}) — this one is required", file=sys.stderr)

    try:
        import pdfminer.high_level as pm
        out["pdfminer"] = (pm.extract_text(path), None)
    except Exception:
        pass

    try:
        import pypdfium2 as pdfium
        doc = pdfium.PdfDocument(path)
        out["pdfium"] = ("\n".join(doc[i].get_textpage().get_text_range()
                                   for i in range(len(doc))), len(doc))
    except Exception:
        pass

    try:
        import pdfplumber
        with pdfplumber.open(path) as pdf:
            # DEFAULT tolerances on purpose: this is the engine that caught the
            # squeezed-word-space bug, and only because it was not tuned to pass.
            out["pdfplumber"] = ("\n".join(p.extract_text() or ""
                                           for p in pdf.pages), len(pdf.pages))
    except Exception:
        pass

    return out


def main() -> int:
    pdf_path, expected_path = sys.argv[1], sys.argv[2]
    with open(expected_path) as f:
        expected = json.load(f)

    engines = extract_all(pdf_path)
    if "pypdf" not in engines:
        print("FAIL\n  - pypdf could not read the PDF")
        return 1

    strings = [s for s in expected.get("must_contain", []) if s.strip()]
    tokens = expected.get("must_contain_exact", [])
    # phrases whose internal spacing is the thing under test
    phrases = [s for s in strings if len(s.split()) >= 3]

    failures = []

    for eng, (raw_text, pages) in sorted(engines.items()):
        if pages is not None and pages > expected["max_pages"]:
            failures.append(f"[{eng}] page count {pages} > {expected['max_pages']}")

        raw = fold(raw_text)
        squashed = norm(raw)
        raw_lower = raw.lower()
        spaced = flat(raw)

        for s in strings:
            if norm(s) not in squashed:
                failures.append(f"[{eng}] not found in extracted text: {s[:70]!r}")

        for tok in tokens:
            if tok.lower() not in raw_lower:
                loose = re.sub(r"\s+", r"\\s*", re.escape(tok))
                m = re.search(loose, raw, re.I)
                got = f" (extracted as {m.group(0)!r})" if m else ""
                failures.append(
                    f"[{eng}] token split or mangled — an ATS keyword search for "
                    f"{tok!r} would MISS this resume{got}")

        # word separation: the phrase must appear WITH its spaces
        for s in phrases:
            if flat(s) not in spaced:
                failures.append(
                    f"[{eng}] word spacing lost — {' '.join(s.split()[:5])!r}... "
                    f"does not extract with its spaces intact; an ATS splitting "
                    f"words this way matches no keyword on the page")
                break  # one report per engine is enough; the cause is global

        # hazards that survive folding are reported from the unfolded text
        for lig, plain in LIGATURES.items():
            if lig in raw_text:
                bad = [w for w in raw_text.split() if lig in w][:3]
                failures.append(f"[{eng}] ligature U+{ord(lig):04X} ({plain}) in {bad}")
        if "­" in raw_text:
            failures.append(f"[{eng}] soft hyphen U+00AD in extracted text")
        for m in re.finditer(r"([A-Za-z]{2,})-\n([a-z]{2,})", raw_text):
            failures.append(
                f"[{eng}] hyphenated across a line break: "
                f"{m.group(1)}-{m.group(2)} — searched as one word it is missed")

    if failures:
        print("FAIL")
        for msg in dict.fromkeys(failures):
            print(f"  - {msg}")
        return 1

    print(f"PASS: {len(engines)} engine(s) [{', '.join(sorted(engines))}], "
          f"{len(strings)} strings + {len(tokens)} exact tokens + "
          f"{len(phrases)} spacing checks")
    return 0


if __name__ == "__main__":
    sys.exit(main())
