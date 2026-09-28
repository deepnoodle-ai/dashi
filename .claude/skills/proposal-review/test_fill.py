#!/usr/bin/env python3
"""Regression tests for fill.py. Run: python3 test_fill.py"""

import json
import re
import unittest
from pathlib import Path

import fill

HOSTILE = '</script><script>window.pwned = 1</script><!-- &   >'


def sample(text=HOSTILE):
    return {
        "id": "t", "title": "Budgets " + text, "source": text,
        "context": [{"label": text, "text": text}],
        "questions": [{
            "topic": text, "question": text,
            "facts": [{"label": text, "items": [text]}],
            "options": [{"label": text, "desc": text}, {"label": "B"}],
        }],
    }


class FillTest(unittest.TestCase):
    def setUp(self):
        self.template = fill.TEMPLATE.read_text(encoding="utf-8")

    def test_hostile_text_cannot_close_or_open_script(self):
        out = fill.fill(self.template, sample())
        low = self.template.lower()
        self.assertEqual(out.lower().count("</script"), low.count("</script"))
        self.assertEqual(out.lower().count("<script"), low.count("<script"))
        self.assertEqual(out.count("<!--"), self.template.count("<!--"))
        block = re.search(
            r'id="review-data">(.*?)</script>', out, re.S).group(1)
        for raw in ("<", ">", "&", "\u2028", "\u2029"):
            self.assertNotIn(raw, block)

    def test_data_round_trips_exactly(self):
        data = sample()
        out = fill.fill(self.template, data)
        block = re.search(
            r'<script type="application/json" id="review-data">(.*?)</script>',
            out, re.S)
        self.assertIsNotNone(block)
        self.assertEqual(json.loads(block.group(1)), data)

    def test_title_is_html_escaped(self):
        out = fill.fill(self.template, sample())
        title = re.search(r"<title>(.*?)</title>", out, re.S).group(1)
        self.assertNotIn("<", title)
        self.assertIn("&lt;/script&gt;", title)

    def test_placeholders_in_text_are_not_expanded(self):
        data = sample("__DATA__ __TITLE__")
        out = fill.fill(self.template, data)
        title = re.search(r"<title>(.*?)</title>", out, re.S).group(1)
        self.assertEqual(title, "Budgets __DATA__ __TITLE__")
        block = re.search(
            r'id="review-data">(.*?)</script>', out, re.S).group(1)
        self.assertEqual(json.loads(block), data)


if __name__ == "__main__":
    unittest.main()
