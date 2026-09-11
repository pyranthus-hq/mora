import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

SCRIPT = Path(__file__).with_name("dogfood-smoke.py")

class SmokeTests(unittest.TestCase):
    def fixture(self, root, fail=False):
        vault=root/"vault";vault.mkdir();(vault/"private.md").write_text("private source text")
        binary=root/"candidate"
        binary.write_text("#!"+sys.executable+"\n" + (
            'import sys; print("PRIVATE raw error",file=sys.stderr);sys.exit(1)\n' if fail else
            'import os,json,pathlib\np=pathlib.Path(os.environ["MORA_VAULT"])\nassert p.name=="snapshot"\nassert os.environ["DO_NOT_TRACK"]=="1"\n(p/"candidate-write").write_text("isolated")\nprint(json.dumps({"memories":[{"text":"PRIVATE","evidence":{"readable":"PRIVATE"},"participation":{},"automated":None}]}))\n'))
        binary.chmod(0o755)
        return vault,binary
    def run_script(self,vault,binary,optin=True):
        args=[sys.executable,str(SCRIPT),"--binary",str(binary),"--vault",str(vault)]
        if optin:args.append("--allow-vault-read")
        return subprocess.run(args,capture_output=True,text=True)
    def test_counts_and_isolation(self):
        with tempfile.TemporaryDirectory() as tmp:
            vault,binary=self.fixture(Path(tmp));r=self.run_script(vault,binary)
            self.assertEqual(r.returncode,0,r.stderr);self.assertNotIn("PRIVATE",r.stdout+r.stderr)
            rows=[json.loads(line) for line in r.stdout.splitlines()]
            self.assertEqual(len(rows),8)
            self.assertTrue(all(row["rows"]==1 and row["with_participation"]==1 and row["no_basis"]==1 for row in rows))
            self.assertFalse((vault/"candidate-write").exists())
    def test_requires_optin_and_sanitizes_failure(self):
        with tempfile.TemporaryDirectory() as tmp:
            vault,binary=self.fixture(Path(tmp),fail=True)
            self.assertNotEqual(self.run_script(vault,binary,False).returncode,0)
            r=self.run_script(vault,binary);self.assertNotEqual(r.returncode,0)
            self.assertNotIn("PRIVATE",r.stdout+r.stderr);self.assertEqual(r.stdout,"")
    def test_symlink_refused(self):
        with tempfile.TemporaryDirectory() as tmp:
            vault,binary=self.fixture(Path(tmp));(vault/"link").symlink_to(vault/"private.md")
            r=self.run_script(vault,binary);self.assertNotEqual(r.returncode,0);self.assertEqual(r.stdout,"")

if __name__=="__main__":unittest.main()
