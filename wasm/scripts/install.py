#!/usr/bin/env python3
"""Install the Postgres build into a staging tree.

usage: install.py <meson build dir> <destdir>

`meson install` refuses to run while some targets (tools that need fork())
fail to build, so copy whatever `meson introspect --installed` lists and
exists. Installed paths are absolute (/usr/local/pgsql/...) and land under
destdir.
"""
import json
import os
import shutil
import subprocess
import sys

build, dest = sys.argv[1], sys.argv[2]
installed = json.loads(subprocess.check_output(
    ["meson", "introspect", "--installed", build]))
copied = missing = 0
for src, dst in installed.items():
    if not os.path.exists(src):
        missing += 1
        continue
    target = os.path.join(dest, dst.lstrip("/"))
    os.makedirs(os.path.dirname(target), exist_ok=True)
    if os.path.isdir(src):
        shutil.copytree(src, target, dirs_exist_ok=True)
    else:
        shutil.copy2(src, target)
    copied += 1
print(f"installed {copied} files into {dest} ({missing} not built)")
