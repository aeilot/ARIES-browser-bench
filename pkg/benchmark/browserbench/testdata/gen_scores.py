"""Regenerate assistantbench_scores.json from the upstream Python scorer.

The cases pair synthetic gold answers, shaped like AssistantBench's (numbers
with units, plain strings, newline lists and JSON objects), with predictions
that exercise every branch of question_scorer, including the ones on which it
raises. They contain no dataset answers.

Usage: python gen_scores.py <dir> > assistantbench_scores.json, where <dir>
contains a package named "abeval" holding BrowserGym's
browsergym/assistantbench/src/browsergym/assistantbench/evaluation at
ServiceNow/BrowserGym@f0e4275d706f31aa341e58e49a5effbb52a392a8, with numpy
and scipy installed.
"""

import json
import math
import sys
import warnings

sys.path.insert(0, sys.argv[1])
from abeval.evaluator import question_scorer  # noqa: E402

warnings.simplefilter("ignore")

GOLDS = [
    "14.2",
    "1010000",
    "$55",
    "1148 sqft",
    "45",
    "0",
    "-3",
    "12 square kilometers",
    "Shanghai villa",
    "Glass Onion: A Knives Out Mystery",
    "Adrenalinpark Köln",
    "Shiran Nawi, Yoni Osherov, Daniel Lereya",
    "The Root Sports & Fitness Center",
    "For Pete's Sake",
    "https://ftp.example.org/pub/release-101/gff3/file.gff3.gz",
    "Fidelity® Emerging Markets Index Fund (FPADX)",
    "VGT",
    "CrossFit East River\nAvea Pilates",
    "Trout lake trail\nArtist Point\nFountain Paint Pot\nLone Star Geyser",
    "Wanda Austin\nRonald D. Sugar\nSue Wagner",
    '{"sender": "USPS", "price (usd)": "41.75"}',
    '{"sender": "DHL", "price (usd)": "55-70"}\n{"sender": "Fedex", "price (usd)": "62-95"}\n{"sender": "USPS", "price (usd)": "73.4-78.15"}',
    "{'sender': 'UPS', 'price (usd)': '12'}",
    '{"name": "a", "count": 3, "open": true}',
    "true",
    '"quoted"',
    "an apple a day",
    "1-2-3",
    "\n  Lone value  \n",
]

GENERIC = [
    "",
    " ",
    "\n",
    "45",
    "45.0",
    "4.5e1",
    "1_000",
    "nan",
    "NaN",
    "Infinity",
    "-Infinity",
    "inf",
    "true",
    "false",
    "null",
    "[]",
    "{}",
    "[45]",
    '["45"]',
    "[45.5]",
    "[true]",
    '["", true]',
    '["", ""]',
    "[NaN]",
    "[[1]]",
    '["a", "b"]',
    '[1, "a"]',
    '{"a": 1}',
    '{"sender": "USPS"}',
    '"VGT"',
    "VGT",
    "the vgt",
    "The answer is VGT.",
    "$1,010,000",
    "1,010,000",
    "1010000",
    "1.01 million",
    "999000",
    "14.20",
    "14",
    "-3",
    "0",
    "0.0",
    "1e400",
    "1148",
    "1148 sqft",
    "12",
    "Glass Onion",
    "glass onion a knives out mystery",
    "Köln",
    "Adrenalinpark Koln",
    "CrossFit East River",
    "CrossFit East River\nAvea Pilates",
    '["CrossFit East River", "Avea Pilates"]',
    '["Avea Pilates", "CrossFit East River", "Blink Fitness"]',
    "Avea Pilates, CrossFit East River",
    '{"sender": "USPS", "price (usd)": "41.75"}',
    '{"sender": "USPS", "price (usd)": 41.75}',
    "{'sender': 'USPS', 'price (usd)': '41.75'}",
    '{"sender": "DHL", "price (usd)": "55-70"}\n{"sender": "Fedex", "price (usd)": "62-95"}',
    '[{"sender": "DHL", "price (usd)": "55-70"}, {"sender": "USPS", "price (usd)": "73.4-78.15"}]',
    '[{"sender": "DHL"}, "x"]',
    '{"sender": null}',
    '{"sender": {"x": 1}}',
    '{"name": "a", "count": "3", "open": true}',
    '{"name": "A", "count": 3.0, "open": "True"}',
    "an apple",
    "1-2-3",
    "1 2 3",
    "For Pete’s Sake",
    "Fidelity Emerging Markets Index Fund FPADX",
    "https://ftp.example.org/pub/release-101/gff3/file.gff3.gz",
    "Wanda Austin\nSue Wagner",
    "45\n46",
    "quoted",
    "Lone value",
    "\t45\t",
    "١٢",
    "²",
    "2²",
]


def case(prediction, gold):
    try:
        accuracy, _ = question_scorer(prediction, gold)
    except Exception as error:  # the Go port reports these as errors
        return {"prediction": prediction, "gold": gold, "raises": type(error).__name__}
    accuracy = float(accuracy)
    if not math.isfinite(accuracy):
        return {"prediction": prediction, "gold": gold, "nonfinite": repr(accuracy)}
    return {"prediction": prediction, "gold": gold, "score": accuracy}


cases = []
for gold in GOLDS:
    predictions = list(dict.fromkeys(GENERIC + [gold, gold.upper(), gold.lower(), gold + "!", "  " + gold + "  "]))
    for prediction in predictions:
        cases.append(case(prediction, gold))
json.dump(cases, sys.stdout, ensure_ascii=False, indent=0)
sys.stdout.write("\n")
