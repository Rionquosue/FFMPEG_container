#!/usr/bin/env python3
"""Create a consumer key file without printing keys or overwriting existing keys."""
import json
import os
from pathlib import Path
import secrets

directory = Path(__file__).resolve().parent.parent / "secrets"
directory.mkdir(mode=0o700, exist_ok=True)
path = directory / "api_keys.json"
fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
with os.fdopen(fd, "w") as f:
    json.dump({"consumers": [{"id": "rails", "keys": [secrets.token_urlsafe(32)]}]}, f)
    f.write("\n")
print("Created secrets/api_keys.json. Make it readable by container UID 10001 before starting.")
