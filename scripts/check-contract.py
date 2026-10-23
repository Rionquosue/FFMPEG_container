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

def video(operation, parameters, output_format="mp4"):
    return {"operation": operation, "input": {"size_bytes": 3, "sha256": "a" * 64},
            "parameters": parameters, "output_format": output_format}

animation = {"audio_mode": "remove", "max_dimension": 640, "max_fps": 15, "quality": 28, "max_bitrate_kbps": 800}
for valid in [video("transcode", animation), video("trim", dict(animation, start_seconds=3, duration_seconds=8)),
              video("remux", {"audio_mode": "remove"}, "webm"), video("transcode", {"width": 640, "height": 360})]:
    validator.validate(valid)
for rejected, reason in [
    (video("transcode", {"audio_mode": None}), "null parameter"),
    (video("transcode", {"audio_mode": "mute"}), "unknown audio mode"),
    (video("transcode", {"max_dimension": 641}), "odd maximum dimension"),
    (video("transcode", {"max_fps": 61}), "frame rate above range"),
    (video("transcode", {"max_dimension": 640, "width": 640, "height": 360}), "max_dimension with width/height"),
    (video("transcode", {"max_bitrate_kbps": 800}, "webm"), "bitrate cap on webm"),
    (video("trim", {"start_seconds": 0, "duration_seconds": 1, "width": 640}), "width without height"),
    (video("remux", {"max_fps": 15}), "remux geometry"),
]:
    assert not validator.is_valid(rejected), f"Must reject: {reason}"
print("OpenAPI 3.1 and request examples validated.")
