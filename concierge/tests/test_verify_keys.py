"""verify.py on spec/vectors/key-manifest.json (D-042): the same results as the Go and browser verifiers."""
import json
import subprocess
import sys
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
VEC = ROOT / "spec" / "vectors"


class KeyVectorsTest(unittest.TestCase):
    def test_key_manifest(self):
        cases = json.loads((VEC / "key-manifest.json").read_text())["cases"]
        self.assertGreater(len(cases), 0)
        for c in cases:
            with self.subTest(c["name"]):
                r = subprocess.run([sys.executable, str(ROOT / "concierge" / "verify.py"), str(VEC / c["jwks"]), str(VEC / c["file"])],
                                   capture_output=True, text=True)
                if c["expect"] == "jwks_error":
                    self.assertEqual(r.returncode, 2, r.stdout)
                    continue
                verified = r.returncode == 0 and "RESULT: VERIFIED" in r.stdout
                self.assertEqual(verified, c["expect"]["verified"], r.stdout)
                self.assertEqual("signature ............ valid" in r.stdout, c["expect"]["signature"], r.stdout)

    def test_pem_still_works(self):
        r = subprocess.run([sys.executable, str(ROOT / "concierge" / "verify.py"), str(VEC / "test-public-key.pem"),
                            str(VEC / "pass" / "KST-CTA-0001.dsse.json")], capture_output=True, text=True)
        self.assertEqual(r.returncode, 0, r.stdout)

    def test_unknown_kid_in_a_jwks_is_invalid(self):
        r = subprocess.run([sys.executable, str(ROOT / "concierge" / "verify.py"), str(VEC / "keys" / "other-key-revoked.jwks.json"),
                            str(VEC / "extra" / "unknown-keyid.dsse.json")], capture_output=True, text=True)
        self.assertEqual(r.returncode, 1, r.stdout)
        self.assertIn("unknown key", r.stdout)


if __name__ == "__main__":
    unittest.main()
