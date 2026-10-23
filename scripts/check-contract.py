#!/usr/bin/env python3
"""Development check; pip install openapi-spec-validator==0.7.2."""
import json
from pathlib import Path
from openapi_spec_validator import validate
from jsonschema import Draft202012Validator

spec = json.loads((Path(__file__).resolve().parent.parent / "docs/openapi.json").read_text())
validate(spec)
schema = dict(spec["components"]["schemas"]["Request"], components=spec["components"])
validator = Draft202012Validator(schema)
example = {"operation": "metadata", "input": {"size_bytes": 3, "sha256": "a" * 64},
           "parameters": {}, "output_format": "json"}
validator.validate(example)
invalid = dict(example, parameters={"command": "anything"})
assert not validator.is_valid(invalid), "Unknown parameters must be rejected"
print("OpenAPI 3.1 and request examples validated.")
