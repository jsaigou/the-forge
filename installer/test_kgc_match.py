#!/usr/bin/env python3
"""The matcher's registry path must name a file that exists (it once pointed one directory up, so
every install reported "could not run" and no hardware profile was ever matched)."""
import importlib.util
import json
import os
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
spec = importlib.util.spec_from_file_location("kgc_match", os.path.join(HERE, "kgc-match.py"))
kgc_match = importlib.util.module_from_spec(spec)
spec.loader.exec_module(kgc_match)


class Registry(unittest.TestCase):
    def test_registry_path_exists_and_lists_the_strix_halo_profile(self):
        self.assertTrue(os.path.isfile(kgc_match.REGISTRY), kgc_match.REGISTRY)
        with open(kgc_match.REGISTRY) as f:
            ids = [p["id"] for p in json.load(f)["profiles"]]
        self.assertIn("strix-halo-gfx1151", ids)
        for pid in ids:
            self.assertTrue(os.path.isfile(os.path.join(HERE, "kgc", "profiles", pid + ".json")), pid)


class Gfx(unittest.TestCase):
    def test_target_version_to_name(self):
        self.assertEqual(kgc_match.gfx_from_target_version("110501"), "gfx1151")
        self.assertEqual(kgc_match.gfx_from_target_version("110000"), "gfx1100")
        self.assertEqual(kgc_match.gfx_from_target_version("0"), "")
        self.assertEqual(kgc_match.gfx_from_target_version("junk"), "")


if __name__ == "__main__":
    unittest.main()
