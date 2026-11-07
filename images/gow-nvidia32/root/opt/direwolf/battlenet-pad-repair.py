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
# A Driver value as stored: its class key's name under Control\Class, with
# each separator doubled, as in key paths.
_DRIVER_VALUE = re.compile(r'^"Driver"="([^"]*)"\s*$', re.I)


def drivers(block):
    """The Driver values a key's block names, lowercased."""
    out = set()
    for line in block.splitlines():
        m = _DRIVER_VALUE.match(line)
        if m:
            out.add(m.group(1).lower())
    return out


# HIDClass, the class of the pad's HID device: its half-installed instance
# names no driver key, yet Wine made one ({guid}\\NNNN, InfPath only).
_HID_CLASS = "{745a17a0-74d3-11d0-b6fe-00a0c90f57da}"
_CLASS_KEY = re.compile(_CONTROL_SET + r"Control\\\\Class\\\\((\{[^\\]*\})\\\\[^\\]+)", re.I)


def orphan_class_key(key, classes, named):
    """Whether key is a driver key (or a subkey of one) under Control\\Class
    in one of classes that no Driver value in named points at."""
    m = _CLASS_KEY.match(key)
    return bool(m) and m.group(2).lower() in classes and m.group(1).lower() not in named


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


def lone_id_namespace():
    """Whether this runs in a user namespace that maps one id (unshare -r):
    its root can override no other user's files. The host's (identity) map
    and an App pod's (65536 ids, hostUsers: false) can. An unreadable map
    counts as neither. BATTLENET_PAD_REPAIR_UID_MAP names another map, for tests."""
    try:
        with open(os.environ.get("BATTLENET_PAD_REPAIR_UID_MAP", "/proc/self/uid_map")) as f:
            lines = [line.split() for line in f.read().splitlines() if line.strip()]
    except OSError:
        return False
    return len(lines) == 1 and len(lines[0]) == 3 and lines[0][2] == "1"


def main(path):
    # Never as a root that can override other users' files: the host's, or an
    # App pod's (kubectl exec). Run by hand over a user's prefix, a link they
    # planted, `..` past one, or a directory they swap mid-run led an
    # owner-checking version into another user's prefix, and an allowed root
    # re-owned the files to root (QA, XERK-2709). Run it as the prefix's
    # owner, as launch-battlenet does.
    if os.geteuid() == 0 and not lone_id_namespace():
        print(f"{path}: not run as root; run it as the prefix's owner (runuser -u <user> -- ...)", file=sys.stderr)
        return 1
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
    pad = [(k, b) for k, b in keys if _PAD_ENUM.match(k) or _PAD_CLASS.match(k)]
    kept = [(k, b) for k, b in keys if not (_PAD_ENUM.match(k) or _PAD_CLASS.match(k))]
    # Wine makes a new driver key under Control\Class for each device it
    # reinstalls, so the removed devices' old ones would be left behind: in
    # their classes (and HIDClass, as the broken instance names none), every
    # driver key no kept device names. A device installs its key and names
    # it together; one named by nothing is such a leftover.
    classes = {d.split("\\", 1)[0] for _, b in pad for d in drivers(b)} | {_HID_CLASS}
    named = set().union(*(drivers(b) for _, b in kept))
    kept = [(k, b) for k, b in kept if not orphan_class_key(k, classes, named)]
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
