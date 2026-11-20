#!/bin/bash
# Print the remaining OpenRouter budget (USD) on the key Neon runs through.
set -a; source /root/Software/.secrets/yt-transcribe.env; set +a
curl -s https://openrouter.ai/api/v1/key -H "Authorization: Bearer $OPENROUTER_API_KEY" | python3 -c "import json,sys; d=json.load(sys.stdin)['data']; print('remaining_usd', round(d['limit_remaining'],2), 'spent_usd', round(d['limit']-d['limit_remaining'],2))"
