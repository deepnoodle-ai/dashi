#!/usr/bin/env python3
"""Fill the proposal-review template with review data, safely.

Usage: fill.py DATA_JSON OUT_HTML

The title goes into <title> HTML-escaped. The data goes into a
<script type="application/json"> block, so it is serialized with <, >, &,
U+2028, and U+2029 written as JSON unicode escapes; a literal "</script>" in a
proposal can then never close the block or inject markup. JSON.parse restores
the original text, and the page escapes it again when rendering.
"""

import html
import json
import sys
from pathlib import Path

TEMPLATE = Path(__file__).with_name("template.html")


def fail(msg):
    sys.exit(f"fill.py: {msg}")


def check(data):
    if not isinstance(data, dict):
        fail("data must be a JSON object")
    if not isinstance(data.get("title"), str) or not data["title"].strip():
        fail("data.title must be a non-empty string")
    questions = data.get("questions")
    if not isinstance(questions, list) or not questions:
        fail("data.questions must be a non-empty list")
    for n, q in enumerate(questions, 1):
        if not isinstance(q, dict) or not isinstance(q.get("question"), str):
            fail(f"question {n} needs a question string")
        opts = q.get("options")
        if not isinstance(opts, list) or len(opts) < 2:
            fail(f"question {n} needs at least two options")
        for o in opts:
            if not isinstance(o, dict) or not isinstance(o.get("label"), str):
                fail(f"question {n} has an option without a label")
        if sum(1 for o in opts if o.get("recommended")) > 1:
            fail(f"question {n} marks more than one option recommended")


def script_safe_json(data):
    text = json.dumps(data, ensure_ascii=False, indent=2)
    for ch, esc in (("&", "\\u0026"), ("<", "\\u003c"), (">", "\\u003e"),
                    (" ", "\\u2028"), (" ", "\\u2029")):
        text = text.replace(ch, esc)
    return text


def fill(template, data):
    # Split on the original template so text inside the title can never be
    # mistaken for the data placeholder, or the other way round.
    head, sep, rest = template.partition("__TITLE__")
    if not sep:
        fail("template has no __TITLE__ placeholder")
    middle, sep, tail = rest.partition("__DATA__")
    if not sep:
        fail("template has no __DATA__ placeholder")
    title = html.escape(data["title"], quote=True)
    return head + title + middle + script_safe_json(data) + tail


def main():
    if len(sys.argv) != 3:
        fail("usage: fill.py DATA_JSON OUT_HTML")
    try:
        data = json.loads(Path(sys.argv[1]).read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as e:
        fail(f"cannot read data: {e}")
    check(data)
    out = fill(TEMPLATE.read_text(encoding="utf-8"), data)
    Path(sys.argv[2]).write_text(out, encoding="utf-8")
    print(sys.argv[2])


if __name__ == "__main__":
    main()
