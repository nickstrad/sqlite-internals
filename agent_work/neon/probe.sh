#!/bin/bash
# Is any Neon route usable right now? Prints "NEON-AVAILABLE <route>" (exit 0) or
# "NEON-UNAVAILABLE <reason>" (exit 1). Costs one tiny call on the direct route at most.
set -a; source /root/Software/.secrets/yt-transcribe.env; source /root/Software/.secrets/gemini-neon.env; set +a
left=$(curl -s -m 20 https://openrouter.ai/api/v1/key -H "Authorization: Bearer $OPENROUTER_API_KEY" | python3 -c "import json,sys; print(json.load(sys.stdin)['data']['limit_remaining'])" 2>/dev/null || echo 0)
python3 -c "import sys; sys.exit(0 if float('$left' or 0) >= 1.5 else 1)" && { echo "NEON-AVAILABLE openrouter (\$$left left)"; exit 0; }
out=$(cd /tmp && timeout 90 opencode run -m google/gemini-4-neon "Reply with exactly: OK" 2>&1 | tail -2)
echo "$out" | grep -q "^OK" && { echo "NEON-AVAILABLE direct"; exit 0; }
echo "NEON-UNAVAILABLE openrouter \$$left left; direct: $(echo "$out" | tr '\n' ' ' | cut -c1-120)"; exit 1
