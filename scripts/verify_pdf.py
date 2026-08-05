#!/usr/bin/env python3
"""ATS verification for a rendered resume.

Two levels of check, because they catch different failures:

* must_contain (lenient) — whole sentences and bullets. Compared with ALL
  whitespace stripped, since an extractor legitimately re-wraps lines.

* must_contain_exact (strict) — single tokens that contain no spaces of their
  own (AWS, TypeScript, PostgreSQL, an email address). These must appear
  verbatim in the raw extracted text. The lenient check cannot catch a kern
  split here: "A WS" and "AWS" both collapse to "aws" once whitespace is
  stripped, so a PDF where an ATS could never keyword-match "AWS" was passing
  verification. Found 2026-08-05 on a real render.
"""
import json
import re
import sys
import unicodedata

from pypdf import PdfReader

# typographic chars LaTeX may emit -> ASCII equivalents an ATS would expect
FOLD = str.maketrans({"’": "'", "‘": "'", "“": '"', "”": '"',
                      "–": "-", "—": "-", "­": ""})


def fold(s: str) -> str:
    """NFKC (splits ligatures) + typographic punctuation to ASCII."""
    return unicodedata.normalize("NFKC", s).translate(FOLD)


def norm(s: str) -> str:
    return "".join(fold(s).lower().split())


def main() -> int:
    pdf_path, expected_path = sys.argv[1], sys.argv[2]
    with open(expected_path) as f:
        expected = json.load(f)

    reader = PdfReader(pdf_path)
    failures = []

    if len(reader.pages) > expected["max_pages"]:
        failures.append(f"page count {len(reader.pages)} > {expected['max_pages']}")

    raw = fold("\n".join(page.extract_text() or "" for page in reader.pages))
    squashed = norm(raw)
    raw_lower = raw.lower()

    for s in expected.get("must_contain", []):
        if norm(s) not in squashed:
            failures.append(f"not found in extracted text: {s!r}")

    for tok in expected.get("must_contain_exact", []):
        if tok.lower() not in raw_lower:
            # show how it actually came out, so the cause is obvious
            loose = re.sub(r"\s+", r"\\s*", re.escape(tok))
            m = re.search(loose, raw, re.I)
            got = f" (extracted as {m.group(0)!r})" if m else ""
            failures.append(
                f"token split or mangled — an ATS keyword search for {tok!r} "
                f"would MISS this resume{got}")

    if failures:
        print("FAIL")
        for msg in failures:
            print(f"  - {msg}")
        return 1
    print(f"PASS: {len(reader.pages)} page(s), "
          f"{len(expected.get('must_contain', []))} strings + "
          f"{len(expected.get('must_contain_exact', []))} exact tokens verified")
    return 0


if __name__ == "__main__":
    sys.exit(main())
