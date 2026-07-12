#!/usr/bin/env python3
"""Convert script(1) --log-out/--log-timing (classic format) to asciicast v2."""
import json, sys
out_f, tim_f, cast_f, cols, rows = sys.argv[1], sys.argv[2], sys.argv[3], int(sys.argv[4]), int(sys.argv[5])
data = open(out_f, "rb").read()
ev, t, pos = [], 0.0, 0
for line in open(tim_f):
    parts = line.split()
    if len(parts) != 2:
        continue
    try:
        delay, n = float(parts[0]), int(parts[1])
    except ValueError:
        continue
    t += delay
    chunk = data[pos:pos+n]; pos += n
    if chunk:
        ev.append([round(t, 3), "o", chunk.decode("utf-8", "replace")])
with open(cast_f, "w") as f:
    f.write(json.dumps({"version": 2, "width": cols, "height": rows,
                        "env": {"TERM": "xterm-256color"}}) + "\n")
    for e in ev:
        f.write(json.dumps(e) + "\n")
print(f"events={len(ev)}")
