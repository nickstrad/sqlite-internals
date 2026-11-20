#!/usr/bin/env python3
"""torn_copy.py SRC DST [delay_s]: copy SRC to DST one 4 KiB block at a time with a sleep
between blocks, to imitate a slow backup tool copying a live file (no locks taken)."""
import sys, time
src, dst = sys.argv[1], sys.argv[2]; delay = float(sys.argv[3]) if len(sys.argv) > 3 else 0.0005
with open(src, 'rb') as f, open(dst, 'wb') as g:
    while True:
        b = f.read(4096)
        if not b: break
        g.write(b); time.sleep(delay)
