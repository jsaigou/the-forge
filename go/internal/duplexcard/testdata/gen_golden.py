# Writes golden_cards.json: the Haru reference's own render_card (haru/prompt.py, Apache-2.0, pinned commit 7fe6b76)
# over a fixed set of synthetic cards in every language of card_templates.json. No real user data.
# Reproduce: PYTHONPATH=<Haru>/haru python3 gen_golden.py > golden_cards.json   (needs torch and transformers importable)
import json, sys
from haru.prompt import CARD_FIELDS, card_templates, render_card

T = card_templates()
templates = T["templates"]
cases = []
for lang in [e["code"] for e in T["languages"]]:
    tpl = templates[lang]
    genders = list(tpl["gender"]) + ["nonbinary", ""]            # two known, one unknown, one empty
    levels = list(tpl["backchannel"]) + ["max", ""]
    base = {"persona": "A calm, professional guide.\nSpeaks plainly.", "name": "Haru", "other_name": "Sam",
            "relationship": "colleagues", "situation": "A quick check-in before a meeting."}
    variants = [
        ("full", {**base, "gender": genders[0], "backchannel": levels[1]}),
        ("full_other_options", {**base, "gender": genders[1], "backchannel": levels[0]}),
        ("persona_only", {"persona": "Only a persona."}),
        ("empty_card", {}),
        ("whitespace_and_none", {"persona": "  padded  ", "name": None, "gender": "  ", "other_name": "\t"}),
        ("unknown_options", {**base, "gender": "nonbinary", "backchannel": "max"}),
        ("unicode_multiline", {"persona": "Line one ☃ \U0001F600\n\nLine three: ü 日本語", "name": "Élodie"}),
        ("extra_keys_ignored", {"persona": "x", "language": lang, "unrelated": "ignored", "situation": "y"}),
    ]
    for name, card in variants:
        cases.append({"language": lang, "case": name, "card": card, "text": render_card(card, tpl)})
json.dump({"reference": "higurashi-ai/Haru 7fe6b76 haru/prompt.py render_card", "fields": list(CARD_FIELDS), "cases": cases},
          sys.stdout, ensure_ascii=False, indent=1, sort_keys=False)
print()
