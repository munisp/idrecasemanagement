# Author: Patrick Munis at newwave
import unittest

import aor_docs as ad

# An authorization letter in the shape filers send (names changed).
LETTER = """I, Dana Reyes, Chief Operating Officer at Sunrise Billing Partners, certify that the following individuals can
act on behalf of Sunrise Billing Partners regarding Florida Claim Disputes:

Kim Lowe
Ari Patel
Jo-Ann Smith

Dana Reyes
Signer ID: 9QX2...
Dana Reyes, COO
"""


class AOR(unittest.TestCase):
    def test_reads_who_authorizes_and_who_is_named(self):
        f, issues = ad.extract("aor_letter", LETTER)
        self.assertEqual((f["authorized_by"], f["authorized_by_title"], f["company"]), ("Dana Reyes", "Chief Operating Officer", "Sunrise Billing Partners"))
        self.assertEqual(f["representatives"], ["Kim Lowe", "Ari Patel", "Jo-Ann Smith"])
        self.assertEqual(issues, [])

    def test_classifies_as_an_aor_letter(self):
        try:
            import pipeline
        except ImportError as e:
            self.skipTest(f"pipeline dependencies not installed: {e}")
        self.assertEqual(pipeline.classify_text("Halo_Letter.pdf", LETTER)[0], "aor_letter")

    def test_names_run_together_or_comma_separated(self):
        f, _ = ad.extract("aor_letter", "I, Robin Officer, COO at Acme TPA, certify that the following individuals can act on behalf of Acme TPA regarding Florida Claim Disputes:\nCasey Filer Kim Lowe\nRobin Officer, COO")
        self.assertEqual(f["representatives"], ["Casey Filer", "Kim Lowe"])
        f, _ = ad.extract("aor_letter", "I, Robin Officer, COO at Acme TPA, certify that the following individuals can act on behalf of Acme TPA:\nCasey Filer, Kim Lowe; Ari Patel\n")
        self.assertEqual(f["representatives"], ["Casey Filer", "Kim Lowe", "Ari Patel"])

    def test_other_types_untouched(self):
        self.assertEqual(ad.extract("eob", LETTER), ({}, []))


if __name__ == "__main__":
    unittest.main()
