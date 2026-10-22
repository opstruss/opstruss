#!/usr/bin/env bash
# chat.sh <task>: what the Telegram, Slack, and Discord plugins would do,
# for the use cases, with files in the working directory for the chat
# services. Every call writes a line to chat.log, the task and the inputs
# it got as JSON, with the token as "set"; send and post number their
# messages for message_id and ts. ask waits for the answer that the test
# writes to answer.json, {"answer": …, "by": …}, and passes over an answer
# of someone not in allowed. The sources updates and interactions print
# the lines appended to updates.in and interactions.in that they have not
# printed before, as the services hand out each update once.
set -euo pipefail
case "$1" in
updates | interactions)
  echo "$1 ${INPUT_TOKEN:+token=set}${INPUT_LISTEN:+listen=$INPUT_LISTEN public_key=$INPUT_PUBLIC_KEY}" >> chat.log
  touch "$1.in"
  while :; do
    seen=$(cat "$1.seen" 2>/dev/null || echo 0)
    total=$(wc -l < "$1.in" | tr -d ' ')
    if [ "$total" -gt "$seen" ]; then
      sed -n "$((seen + 1)),${total}p" "$1.in"
      echo "$total" > "$1.seen"
    fi
    sleep 0.1
  done
  ;;
esac
echo "$1 $(jq -cS 'with_entries(select(.value != null)) | if has("token") then .token = "set" else . end' "$OPSTRUSS_INPUTS")" >> chat.log
n=$(grep -c "^$1 " chat.log)
case "$1" in
send)
  jq -n --argjson n "$n" '{message_id: $n}' > "$OPSTRUSS_OUTPUT"
  ;;
post)
  jq -n --arg ts "$(printf '1729278840.%06d' "$n")" '{ts: $ts}' > "$OPSTRUSS_OUTPUT"
  ;;
ask)
  while :; do
    if [ -f answer.json ]; then
      answer=$(cat answer.json)
      rm -f answer.json
      by=$(jq -r .by <<<"$answer")
      jq -e --arg by "$by" '.allowed == [] or any(.allowed[]; . == $by)' "$OPSTRUSS_INPUTS" >/dev/null && break
      echo "ignored $by" >> chat.log
    fi
    sleep 0.1
  done
  jq -n --argjson a "$answer" --slurpfile in "$OPSTRUSS_INPUTS" \
    '{approved: ($a.answer == $in[0].options[0]), answer: $a.answer, by: $a.by}' > "$OPSTRUSS_OUTPUT"
  ;;
esac
