"""Turn Steam Input off for every installed Steam game (XERK-1953).

Steam Input builds its virtual pad on /dev/uinput, which App containers do not
get. Steam still lists Wolf's pad in the game's SDL_GAMECONTROLLER_IGNORE_DEVICES,
so Proton drops it and the game sees no controller at all. With Steam Input
"Disabled" for the game (UseSteamControllerConfig "0"), Steam leaves the pad
alone. This sets that for every app in libraryfolders.vdf, in every
userdata/*/config/localconfig.vdf, before Steam starts. It overrides a user's
own choice: Steam Input cannot work in a session either way.

Usage: steam-input-off.py STEAM_DIR. Runs as the desktop user. A file it cannot
parse is left untouched; the exit status is non-zero on any error.
"""

import glob
import os
import sys
import tempfile

KEY = '"UseSteamControllerConfig"'
MAX_DEPTH = 32
MAX_SIZE = 64 << 20


class VDFError(Exception):
    pass


def tokens(text):
    """Yields raw tokens: quoted strings (escapes kept verbatim), { and }."""
    i, n = 0, len(text)
    while i < n:
        c = text[i]
        if c in " \t\r\n":
            i += 1
        elif text.startswith("//", i):
            j = text.find("\n", i)
            i = n if j < 0 else j + 1
        elif c in "{}":
            yield c
            i += 1
        elif c == '"':
            j = i + 1
            while j < n and text[j] != '"':
                j += 2 if text[j] == "\\" else 1
            if j >= n:
                raise VDFError("unterminated string")
            yield text[i : j + 1]
            i = j + 1
        else:
            raise VDFError("unexpected %r at offset %d" % (c, i))


def parse(text):
    """Returns [(raw key, raw value or [children])]."""
    root, stack, key = [], [], None
    cur = root
    for tok in tokens(text):
        if tok == "{":
            if key is None or len(stack) >= MAX_DEPTH:
                raise VDFError("misplaced or too deep {")
            child = []
            cur.append((key, child))
            stack.append(cur)
            cur, key = child, None
        elif tok == "}":
            if key is not None or not stack:
                raise VDFError("misplaced }")
            cur = stack.pop()
        elif key is None:
            key = tok
        else:
            cur.append((key, tok))
            key = None
    if stack or key is not None:
        raise VDFError("truncated")
    return root


def dump(nodes, depth=0):
    """Serializes the way Steam writes its own .vdf files."""
    out, pad = [], "\t" * depth
    for key, value in nodes:
        if isinstance(value, list):
            out.append("%s%s\n%s{\n%s%s}\n" % (pad, key, pad, dump(value, depth + 1), pad))
        else:
            out.append("%s%s\t\t%s\n" % (pad, key, value))
    return "".join(out)


def block(nodes, key):
    """Returns the block named key (VDF keys ignore case), appending it if absent."""
    for k, v in nodes:
        if k.lower() == key.lower() and isinstance(v, list):
            return v
    child = []
    nodes.append((key, child))
    return child


def disable(apps, appid):
    """Sets UseSteamControllerConfig "0" in apps' block for appid; True if changed."""
    game = block(apps, '"%s"' % appid)
    for i, (k, v) in enumerate(game):
        if k.lower() == KEY.lower():
            if v == '"0"':
                return False
            game[i] = (k, '"0"')
            return True
    game.append((KEY, '"0"'))
    return True


def read(path):
    with open(path, encoding="utf-8") as f:
        text = f.read(MAX_SIZE + 1)
    if len(text) > MAX_SIZE:
        raise VDFError("larger than %d bytes" % MAX_SIZE)
    return text


def installed(steam):
    """App IDs listed in libraryfolders.vdf (every library folder)."""
    ids = set()
    for top_key, top in parse(read(os.path.join(steam, "steamapps", "libraryfolders.vdf"))):
        if top_key.lower() != '"libraryfolders"' or not isinstance(top, list):
            continue
        for _, folder in top:
            if not isinstance(folder, list):
                continue
            for k, apps in folder:
                if k.lower() == '"apps"' and isinstance(apps, list):
                    ids.update(appid.strip('"') for appid, _ in apps)
    # The IDs go back into the file as keys: never write anything but digits.
    return sorted(i for i in ids if i.isdigit())


def update(path, ids):
    """Disables Steam Input for ids in one localconfig.vdf; True if it changed."""
    tree = parse(read(path))
    store = None
    for k, v in tree:
        if k.lower() == '"userlocalconfigstore"' and isinstance(v, list):
            store = v
    if store is None:
        raise VDFError("no UserLocalConfigStore")
    # Steam's per-game Steam Input setting writes both: the top-level "apps"
    # (controller config) and Software/Valve/Steam/apps (launch settings).
    steam_apps = block(block(block(block(store, '"Software"'), '"Valve"'), '"Steam"'), '"apps"')
    changed = False
    for appid in ids:
        changed |= disable(block(store, '"apps"'), appid)
        changed |= disable(steam_apps, appid)
    if not changed:
        return False
    fd, tmp = tempfile.mkstemp(dir=os.path.dirname(path), prefix=".localconfig.")
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as f:
            f.write(dump(tree))
        os.chmod(tmp, os.stat(path).st_mode & 0o777)
        os.replace(tmp, path)
    except BaseException:
        os.unlink(tmp)
        raise
    return True


def main(steam):
    if not os.path.isdir(os.path.join(steam, "userdata")):
        return 0  # Steam has never logged in here: nothing to set.
    try:
        ids = installed(steam)
    except (OSError, UnicodeError, VDFError) as e:
        print("libraryfolders.vdf unreadable, Steam Input left as is: %s" % type(e).__name__, file=sys.stderr)
        return 1
    status = 0
    for path in sorted(glob.glob(os.path.join(steam, "userdata", "*", "config", "localconfig.vdf"))):
        try:
            if update(path, ids):
                print("Steam Input disabled for %d apps in %s" % (len(ids), path))
        except (OSError, UnicodeError, VDFError) as e:
            # Never quote the file: it is user-writable.
            print("left %s unchanged: %s" % (path, type(e).__name__), file=sys.stderr)
            status = 1
    return status


if __name__ == "__main__":
    sys.exit(main(sys.argv[1]))
