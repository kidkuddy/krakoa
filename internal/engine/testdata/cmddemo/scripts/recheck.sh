#!/bin/sh
# Prints "red" until the marker file exists, then "green" — a gate premise that
# expires, which is the whole point of the recheck.
if [ -f "$1" ]; then
  echo '{"outcome":"green"}'
else
  echo '{"outcome":"red"}'
fi
