#!/usr/bin/env python3
"""Check Pi startup with the bundled extension, without a model request."""

import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile


extension = Path(sys.argv[1]) if len(sys.argv) > 1 else Path(__file__).with_name("pi-extension")
with tempfile.TemporaryDirectory(prefix="fletcher-pi-test-") as directory:
    home = Path(directory)
    agent = home / ".pi" / "agent"
    # Exercise the same auto-discovery path as the base image.
    shutil.copytree(extension, agent / "extensions" / "fletcher")
    (agent / "models.json").write_text(json.dumps({
        "providers": {
            "fletcher-test": {
                "baseUrl": "http://127.0.0.1:1/v1",
                "api": "openai-completions",
                "apiKey": "test-placeholder",
                "models": [{"id": "test"}],
            }
        }
    }))
    # A command runs after startup diagnostics, without calling a model.
    # --list-models exits before reporting extension errors in Pi 0.84.2.
    (agent / "extensions" / "smoke.ts").write_text('''
export default function (pi) {
  pi.registerCommand("fletcher-smoke-test", {
    description: "Check startup without calling a model",
    handler: async () => { console.log("fletcher pi startup ok"); },
  });
}
''')
    result = subprocess.run(
        [
            "pi", "--print", "--no-session", "--no-skills",
            "--no-prompt-templates", "--no-context-files",
            "--provider", "fletcher-test", "--model", "test",
            "/fletcher-smoke-test",
        ],
        cwd=home,
        env={
            "PATH": os.environ["PATH"],
            "HOME": str(home),
            "PI_CODING_AGENT_DIR": str(agent),
            "PI_OFFLINE": "1",
            "NO_COLOR": "1",
        },
        input="",
        capture_output=True,
        text=True,
        timeout=30,
    )
    output = result.stdout + result.stderr
    if (result.returncode != 0
            or "Failed to load extension" in output
            or "fletcher pi startup ok" not in output):
        sys.stderr.write(output)
        sys.exit(f"pi extension startup check did not pass (exit {result.returncode})")
    print("pi extension startup check passed")
