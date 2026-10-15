import importlib.util
from pathlib import Path
import tempfile
import unittest

spec = importlib.util.spec_from_file_location("qso_installer", Path(__file__).with_name("install_hooks.py"))
installer = importlib.util.module_from_spec(spec)
spec.loader.exec_module(installer)


class HookMergeTests(unittest.TestCase):
    def fragment(self):
        return {"hooks": {"StopFailure": [{"hooks": [{"type": "command", "command": "qso hook"}]}]}}

    def test_preserves_unrelated_settings_and_deduplicates(self):
        existing = {"permissions": {"deny": ["Bash(rm *)"]}, "hooks": {"StopFailure": [{"hooks": [{"type": "command", "command": "existing"}]}]}}
        merged = installer.merge(existing, self.fragment())
        self.assertEqual(merged["permissions"], existing["permissions"])
        self.assertEqual(len(merged["hooks"]["StopFailure"]), 2)
        self.assertEqual(installer.merge(merged, self.fragment()), merged)
        self.assertEqual(len(existing["hooks"]["StopFailure"]), 1)

    def test_uninstall_only_own_hook(self):
        existing = {"hooks": {"StopFailure": [{"hooks": [{"type": "command", "command": "existing"}, {"type": "command", "command": "qso hook"}]}]}}
        result = installer.merge(existing, self.fragment(), True)
        self.assertEqual(result["hooks"]["StopFailure"][0]["hooks"], [{"type": "command", "command": "existing"}])

    def test_managed_policy_is_never_weakened(self):
        for policy in ("disableAllHooks", "allowManagedHooksOnly"):
            with self.assertRaises(ValueError):
                installer.merge({policy: True}, self.fragment())

    def test_concurrent_changes_rejected(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "settings.json"
            path.write_bytes(b'{"other":true}')
            with self.assertRaises(ValueError):
                installer.atomic_replace(path, b"{}", b"{}")
            self.assertEqual(path.read_bytes(), b'{"other":true}')

    def test_atomic_backup(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "settings.json"
            old = b'{"permissions":{}}'
            path.write_bytes(old)
            installer.atomic_replace(path, b'{"hooks":{}}', old)
            self.assertEqual(path.read_bytes(), b'{"hooks":{}}')
            backups = list(Path(directory).glob("*.qso-backup-*"))
            self.assertEqual(len(backups), 1)
            self.assertEqual(backups[0].read_bytes(), old)
            self.assertEqual(path.stat().st_mode & 0o777, 0o600)


if __name__ == "__main__":
    unittest.main()
