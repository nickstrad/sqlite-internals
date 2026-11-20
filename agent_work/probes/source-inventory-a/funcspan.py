#!/usr/bin/env python3
"""funcspan.py FILE NAME... -> "NAME FILE START-END" for definitions in the pinned SQLite tree.

Read-only helper for the source inventory (research topic source-inventory-a).
  * plain NAME: a C function DEFINITION whose name starts at column 0 or follows a return
    type on a column-0 line ("static int foo(" / "int foo("); multi-line prototypes ending
    in ';' before any '{' are rejected. END = brace-counted close; also cross-checked
    against the first following column-0 '}' and flagged [CHECK ...] when they differ.
  * "OP_Xxx": the "case OP_Xxx:" in vdbe.c; END = first following column-0 '}' (cases
    such as OP_OpenRead/OP_Prev have no brace of their own and fall into a shared body).
  * "struct Foo": "struct Foo {" at column 0; END = first following '};'.
Caveat: sqlite3VdbeExec contains column-0 '}' closing opcode cases, so it is always flagged;
#if/#else blocks that each open a brace can fool brace counting -- read flagged spans by hand.
Prints NOTFOUND if nothing matches. Run with python3 -I.
"""
import re, sys, os
path, names = sys.argv[1], sys.argv[2:]
lines = open(path, encoding='utf-8', errors='replace').read().split('\n')
def strip(t):
    t = re.sub(r'"(?:\\.|[^"\\])*"', '""', t)
    t = re.sub(r"'(?:\\.|[^'\\])'", "''", t)
    return t
def end_braces(i):
    """End line by brace counting (comments and literals stripped). None if never closes."""
    depth, seen, incom = 0, False, False
    for j in range(i, len(lines)):
        t = lines[j]
        if incom:
            if '*/' not in t: continue
            t = t.split('*/', 1)[1]; incom = False
        t = re.sub(r'/\*.*?\*/', '', strip(t))
        if '/*' in t: t, incom = t.split('/*', 1)[0], True
        t = t.split('//', 1)[0]
        for ch in t:
            if ch == '{': depth += 1; seen = True
            elif ch == '}': depth -= 1
        if seen and depth <= 0:
            return j + 1
    return None
def end_from(i, pat):
    for j in range(i + 1, len(lines)):
        if re.match(pat, lines[j]):
            return j + 1
    return None
for n in names:
    start = None
    if n.startswith('OP_'):
        for i, l in enumerate(lines):
            if re.match(r'^case %s:' % re.escape(n), l):
                start, endpat = i, r'^}'; break
    elif n.startswith('struct '):
        for i, l in enumerate(lines):
            if re.match(r'^%s \{' % re.escape(n), l):
                start, endpat = i, r'^};'; break
    else:
        rx = re.compile(r'^(?:[A-Za-z_][A-Za-z0-9_ \*]*[ \*])?%s\(' % re.escape(n))
        for i, l in enumerate(lines):
            if rx.match(l) and not l.startswith(('return', 'if', 'while', 'for')):
                # scan forward: definition iff '{' appears before a ';'
                ok = False
                for j in range(i, min(i + 30, len(lines))):
                    t = re.sub(r'/\*.*?\*/', '', lines[j])
                    if '{' in t: ok = True; break
                    if ';' in t: break
                if ok:
                    start, endpat = i, r'^}'; break
    if start is None:
        print(f'{n} {os.path.basename(path)} NOTFOUND'); continue
    e1 = end_from(start, endpat); e2 = end_braces(start)
    flag = '' if e1 == e2 else f'  [CHECK: col0-close={e1} brace-count={e2}]'
    end = e1 if n.startswith('OP_') else e2   # opcode cases may have no braces of their own
    print(f'{n} {os.path.basename(path)} {start+1}-{end}{flag}')
