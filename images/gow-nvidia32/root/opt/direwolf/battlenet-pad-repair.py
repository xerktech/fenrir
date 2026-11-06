#!/usr/bin/env python3
"""battlenet-pad-repair.py <system.reg>: drops a half-installed Wolf pad from a
Wine prefix so Wine installs it afresh (XERK-2697, XERK-2709).

Seen once on talos04: the pad's XInput device key,
Enum\\HID\\VID_045E&PID_02EA&XI_00\\<id>, had only its "Device Parameters"
subkey and no "Driver". Wine never started it, every XInput slot read 1167,
and WoW Forever (XInput only) saw no pad. Deleting the pad's Enum and
DeviceClasses keys fixed it: Wine re-installs them when the pad is next seen.

The file is Wine's own text format: a header, then one block per key, each
starting with a "[key path] <time>" line. Value lines never start with "["
(strings with '"' or '@', hex continuations with spaces), so a block runs to
the next line starting with "[". Run only while no wineserver uses the
prefix: Wine rewrites the whole file from memory.

Exit 0 whether or not anything was repaired; non-zero only on an error, after
which the file is unchanged.
"""

import os
import re
import sys
import tempfile

# Wolf's virtual pad: an X-Box One pad, 045e:02ea. Paths as stored, relative
# to HKLM, with each separator doubled; the control set is either the link
# name or the set Wine links it to.
_CONTROL_SET = r"System\\\\(?:CurrentControlSet|ControlSet\d{3})\\\\"
_PAD = r"VID_045E&PID_02EA"
# Any key of the pad's devices on the three buses Wine uses for it.
_PAD_ENUM = re.compile(
    _CONTROL_SET + r"Enum\\\\(?:HID|WINEXINPUT|WINEBUS)\\\\" + _PAD + r"(?:[&\\]|$)", re.I
)
# Its device interfaces: DeviceClasses\{guid}\##?#<bus>#VID_045E&PID_02EA...
_PAD_CLASS = re.compile(
    _CONTROL_SET + r"Control\\\\DeviceClasses\\\\\{[^\\]*\}\\\\##\?#(?:HID|WINEXINPUT|WINEBUS)#"
    + _PAD + r"(?:[&#]|$)",
    re.I,
)
# The XInput device instance itself (not a subkey): group 1 is its path.
_XINPUT_KEY = re.compile(
    "(" + _CONTROL_SET + r"Enum\\\\HID\\\\" + _PAD + r"&XI_[0-9A-F]{2}\\\\[^\\]+)", re.I
)
_HEADER = re.compile(r"^\[(.*)\](?: \d+)?\s*$")
_DRIVER = re.compile(r'^"Driver"=', re.I)


def blocks(text):
    """Splits the file into its header and (key path, block text) pairs."""
    lines = text.splitlines(keepends=True)
    head, out, key, cur = [], [], None, None
    for line in lines:
        m = _HEADER.match(line.rstrip("\r\n")) if line.startswith("[") else None
        if m:
            if key is not None:
                out.append((key, "".join(cur)))
            key, cur = m.group(1), [line]
        elif key is None:
            head.append(line)
        else:
            cur.append(line)
    if key is not None:
        out.append((key, "".join(cur)))
    return "".join(head), out


def broken_instances(keys):
    """XInput pad instances whose key has no Driver value, or no block at all
    while a subkey does."""
    seen, healthy = {}, set()
    for key, block in keys:
        m = _XINPUT_KEY.match(key)
        if not m:
            continue
        inst = m.group(1).lower()
        seen.setdefault(inst, m.group(1).replace("\\\\", "\\"))
        if key.lower() == inst and any(_DRIVER.match(l) for l in block.splitlines()):
            healthy.add(inst)
    return sorted(name for inst, name in seen.items() if inst not in healthy)


def main(path):
    # Replace a linked file's target, not the link: the link is what Wine opens.
    path = os.path.realpath(path)
    try:
        with open(path, encoding="latin-1", newline="") as f:
            text = f.read()
    except FileNotFoundError:
        return 0
    if not text.startswith("WINE REGISTRY Version 2"):
        print(f"{path}: not a Wine registry file; left alone", file=sys.stderr)
        return 0
    head, keys = blocks(text)
    broken = broken_instances(keys)
    if not broken:
        return 0
    kept = [(k, b) for k, b in keys if not (_PAD_ENUM.match(k) or _PAD_CLASS.match(k))]
    print(
        f"{path}: pad device {', '.join(broken)} is half-installed (no Driver); "
        f"removed {len(keys) - len(kept)} pad key(s) for Wine to reinstall",
        file=sys.stderr,
    )
    # A copy of the original beside it, then an atomic replace that keeps
    # the file's mode: a torn system.reg loses the whole prefix's settings.
    st = os.stat(path)
    out = head + "".join(b for _, b in kept)
    for dest, body in ((path + ".direwolf-pad-repair.bak", text), (path, out)):
        fd, tmp = tempfile.mkstemp(dir=os.path.dirname(path) or ".", prefix=".system.reg.")
        try:
            with os.fdopen(fd, "w", encoding="latin-1", newline="") as f:
                f.write(body)
                f.flush()
                os.fsync(f.fileno())
            os.chmod(tmp, st.st_mode & 0o7777)
            # Run as root (by hand), keep the user's ownership: Wine refuses
            # a registry it cannot read.
            if os.geteuid() == 0:
                os.chown(tmp, st.st_uid, st.st_gid)
            os.replace(tmp, dest)
        except BaseException:
            try:
                os.unlink(tmp)
            except FileNotFoundError:
                pass
            raise
    return 0


if __name__ == "__main__":
    if len(sys.argv) != 2:
        print("usage: battlenet-pad-repair.py <system.reg>", file=sys.stderr)
        sys.exit(2)
    try:
        sys.exit(main(sys.argv[1]))
    except OSError as e:
        print(f"battlenet-pad-repair: {e}", file=sys.stderr)
        sys.exit(1)
