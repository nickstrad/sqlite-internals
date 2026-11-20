#!/usr/bin/env python3
"""Locate C function/struct definitions in the pinned SQLite tree.
Usage: locate.py ROOT file:sym [file:sym ...]
  sym = function name, or 'struct Name', or '#define NAME'.
Prints: sym | file | start-end | first line. Start = line of the definition header
(column-0 line containing 'sym(' whose header reaches '{' before ';'); end = next
column-0 '}' (functions) or '};' (structs). Reports NOTFOUND otherwise.
"""
import re, sys
root = sys.argv[1]
cache = {}
def lines(f):
    if f not in cache:
        cache[f] = open(f"{root}/{f}", encoding="latin-1").read().split("\n")
    return cache[f]
def find(f, sym):
    L = lines(f)
    if sym.startswith("#define "):
        n = sym.split()[1]
        for i, l in enumerate(L):
            if re.match(r"\s*#\s*define\s+%s\b" % re.escape(n), l):
                return i+1, i+1, l.strip()
        return None
    if sym.startswith("struct "):
        n = sym.split()[1]
        for i, l in enumerate(L):
            if re.match(r"(typedef\s+)?struct\s+%s\s*\{" % re.escape(n), l):
                for j in range(i, len(L)):
                    if re.match(r"\}.*;", L[j]):
                        return i+1, j+1, l.strip()
        return None
    pat = re.compile(r"\b%s\s*\(" % re.escape(sym))
    for i, l in enumerate(L):
        if not l or l[0] in " \t#/*" or not pat.search(l):
            continue
        if l.lstrip().startswith(("return", "if", "}")): continue
        # header: scan forward to first '{' or ';'
        hdr = ""
        for j in range(i, min(i+15, len(L))):
            hdr += L[j]
            if "{" in L[j] or ";" in L[j]: break
        if ";" in hdr.split("{")[0]:
            continue
        # include preceding return-type line(s) if the name starts the line
        start = i
        if re.match(r"\s*%s\s*\(" % re.escape(sym), l) or re.match(r"\*?%s\s*\(" % re.escape(sym), l):
            start = i-1
        for j in range(i, len(L)):
            if L[j].startswith("}"):
                return start+1, j+1, L[start].strip()[:70]
    return None
for arg in sys.argv[2:]:
    f, sym = arg.split(":", 1)
    r = find(f, sym)
    if r: print(f"{sym} | {f} | {r[0]}-{r[1]} | {r[2]}")
    else: print(f"{sym} | {f} | NOTFOUND")
