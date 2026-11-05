#!/usr/bin/env python3
"""Writes the vr session's settings into WiVRn's config.json, at every start.

Usage: wivrn-config.py <config.json> <port> <openvr-compat-path>

The user's other settings (encoder, bitrate, ...) are kept; these always win:
- "port": the session's, from its port block: never WiVRn's default.
- "publish-service": "none": no Avahi (it needs the node's system bus); the
  headset dials the node's IP and port.
- "hid-forwarding": false: it needs the uinput device, which App pods never get.
- "openvr-compat-path": the OpenVR layer the session chose.
- no "application": WiVRn would start it on every headset connect, beside the
  game Steam runs.
"""

import json
import os
import sys
import tempfile


def main(path: str, port: str, compat: str) -> None:
    config = {}
    try:
        with open(path, encoding="utf-8") as f:
            loaded = json.load(f)
        if isinstance(loaded, dict):
            config = loaded
        else:
            print(f"wivrn-config: {path} is not a JSON object; replacing it", file=sys.stderr)
    except FileNotFoundError:
        pass
    except (ValueError, UnicodeDecodeError) as e:
        print(f"wivrn-config: {path} is not valid JSON ({e}); replacing it", file=sys.stderr)

    config.pop("application", None)
    config.update({
        "port": int(port),
        "publish-service": "none",
        "hid-forwarding": False,
        "openvr-compat-path": compat,
    })
    # The spike's encoder (XERK-2659): NVENC on the session's GPU.
    config.setdefault("encoder", "nvenc")

    fd, tmp = tempfile.mkstemp(prefix=".config.json.", dir=os.path.dirname(path))
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as f:
            json.dump(config, f, indent=2)
            f.write("\n")
        os.replace(tmp, path)
    except BaseException:
        os.unlink(tmp)
        raise


if __name__ == "__main__":
    if len(sys.argv) != 4:
        sys.exit(__doc__)
    main(*sys.argv[1:])
