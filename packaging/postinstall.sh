#!/bin/sh
cat <<'EOF'
@NAME@ is installed. As your own user (not root), run:
  @NAME@ serve                                  # first run: link WhatsApp in the browser
  systemctl --user enable --now @NAME@.service  # or: @NAME@ service install
On a server you reach over SSH, also run `loginctl enable-linger` once.
EOF
