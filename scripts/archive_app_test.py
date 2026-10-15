import os
import pathlib
import stat
import tempfile
import unittest
import zipfile

import archive_app


class ArchiveContractTest(unittest.TestCase):
    def setUp(self):
        root = os.environ.get("TMPDIR")
        self.assertTrue(root, "tests require explicit TMPDIR")
        self.temporary = tempfile.TemporaryDirectory(prefix="qso-archive-test-", dir=root)
        self.addCleanup(self.temporary.cleanup)
        self.root = pathlib.Path(self.temporary.name)
        self.app = self.root / "LimitBreak.app"
        executable = self.app / "Contents/MacOS/LimitBreak"
        executable.parent.mkdir(parents=True)
        executable.write_bytes(b"isolated-archive-contract")
        executable.chmod(0o755)
        # A non-ASCII resource name must still carry the UTF-8 flag.
        resource = self.app / "Contents/Resources/额度.txt"
        resource.parent.mkdir(parents=True)
        resource.write_bytes(b"resource")

    def test_name_utf8_permissions_and_no_overwrite(self):
        output = self.root / "package.zip"
        archive_app.archive(self.app, output)
        original = output.read_bytes()
        with zipfile.ZipFile(output) as package:
            self.assertIsNone(package.testzip())
            self.assertTrue(all(info.filename.startswith("LimitBreak.app/") for info in package.infolist()))
            self.assertTrue(package.getinfo("LimitBreak.app/Contents/Resources/额度.txt").flag_bits & 0x800)
            executable = package.getinfo("LimitBreak.app/Contents/MacOS/LimitBreak")
            self.assertTrue(stat.S_IMODE(executable.external_attr >> 16) & 0o111)
        with self.assertRaises(ValueError):
            archive_app.archive(self.app, output)
        self.assertEqual(output.read_bytes(), original)

    def test_reject_link_and_wrong_name(self):
        (self.app / "linked").symlink_to(self.app / "Contents")
        with self.assertRaises(ValueError):
            archive_app.archive(self.app, self.root / "bad.zip")
        wrong = self.root / "versioned.app"
        wrong.mkdir()
        with self.assertRaises(ValueError):
            archive_app.archive(wrong, self.root / "bad-name.zip")
        legacy = self.root / "额度守护.app"
        legacy.mkdir()
        with self.assertRaises(ValueError):
            archive_app.archive(legacy, self.root / "legacy-name.zip")


if __name__ == "__main__":
    unittest.main()
