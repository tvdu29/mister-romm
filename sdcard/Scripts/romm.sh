#!/bin/bash
# mister-romm launcher. The MiSTer Scripts menu only lists .sh files; everything else (pairing,
# sync, downloads, settings, diagnostics) is done by the program.
# Same pattern as the official Scripts/update.sh: copy to /tmp, chmod +x, run.
BIN=/media/fat/Scripts/.config/romm/bin/romm-tui
if [ ! -s "$BIN" ]; then
    echo "romm-tui not found: $BIN"
    exit 1
fi
RUN=$(mktemp /tmp/romm-tui.XXXXXX) || exit 1
trap 'rm -f "$RUN"' EXIT
cp "$BIN" "$RUN" && chmod 700 "$RUN" || exit 1
"$RUN" "$@"
