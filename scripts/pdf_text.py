#!/usr/bin/env python3
"""Print a PDF's extracted text — the ATS's view of the document."""
import sys
from pypdf import PdfReader
print("\n".join(p.extract_text() or "" for p in PdfReader(sys.argv[1]).pages))
