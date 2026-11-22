#!/bin/bash
# Серия сквозных запросов через мост: проверка, что капча проходит стабильно.
TOK=$(grep '^AUTH_TOKEN=' /root/GLM-ZAI-2API/.env | cut -d= -f2)
OK=0; FAIL=0
for i in $(seq 1 "${1:-6}"); do
  t0=$(date +%s%N)
  out=$(curl -sS -m 200 -X POST http://127.0.0.1:5083/v1/chat/completions \
    -H "Authorization: Bearer $TOK" -H 'content-type: application/json' \
    -d '{"model":"glm-5.3","messages":[{"role":"user","content":"ответь одним словом: работает"}],"stream":false}' 2>&1)
  ms=$(( ($(date +%s%N)-t0)/1000000 ))
  content=$(printf '%s' "$out" | python3 -c "
import sys,json
try:
    d=json.loads(sys.stdin.read()); print(d.get('choices',[{}])[0].get('message',{}).get('content','').strip()[:60])
except Exception: print('')
")
  if [ -n "$content" ]; then
    OK=$((OK+1)); echo "  $i: OK ${ms}ms | $content"
  else
    FAIL=$((FAIL+1)); echo "  $i: FAIL ${ms}ms | $(printf '%s' "$out" | head -c 200)"
  fi
done
echo "ИТОГ: OK=$OK FAIL=$FAIL"
