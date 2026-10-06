#!/bin/bash
# mister-romm : lanceur. Le menu Scripts de la MiSTer ne liste que les fichiers .sh ; tout le travail
# (appairage, synchronisation, téléchargements, réglages, diagnostics) est fait par le programme.
# Même schéma que le Scripts/update.sh officiel : copie dans /tmp, chmod +x, exécution.
BIN=/media/fat/Scripts/.config/romm/bin/romm-tui
RUN=/tmp/romm-tui
if [ ! -s "$BIN" ]; then
    echo "romm-tui introuvable : $BIN"
    exit 1
fi
cp "$BIN" "$RUN" && chmod +x "$RUN" || exit 1
exec "$RUN" "$@"
