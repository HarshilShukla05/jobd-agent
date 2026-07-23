#!/usr/bin/env python3
"""ATS verification: assert page count and that every expected string survives
PDF text extraction. Comparison is lowercase with ALL whitespace stripped, so
extractor spacing quirks (e.g. "W AL") don't cause false failures — if even
this loose match fails, an ATS parser has no chance."""
import json
import sys
import unicodedata

from pypdf import PdfReader

# typographic chars LaTeX may emit -> ASCII equivalents an ATS would expect
FOLD = str.maketrans({"’": "'", "‘": "'", "“": '"', "”": '"',
                      "–": "-", "—": "-", "­": ""})


def norm(s: str) -> str:
    s = unicodedata.normalize("NFKC", s).translate(FOLD)  # NFKC splits ligatures
    return "".join(s.lower().split())


def main() -> int:
    pdf_path, expected_path = sys.argv[1], sys.argv[2]
    with open(expected_path) as f:
        expected = json.load(f)

    reader = PdfReader(pdf_path)
    failures = []

    if len(reader.pages) > expected["max_pages"]:
        failures.append(f"page count {len(reader.pages)} > {expected['max_pages']}")

    text = norm("".join(page.extract_text() or "" for page in reader.pages))
    for s in expected["must_contain"]:
        if norm(s) not in text:
            failures.append(f"bullet not found in extracted text: {s!r}")

    if failures:
        print("FAIL")
        for msg in failures:
            print(f"  - {msg}")
        return 1
    print(f"PASS: {len(reader.pages)} page(s), {len(expected['must_contain'])} strings verified")
    return 0


if __name__ == "__main__":
    sys.exit(main())
