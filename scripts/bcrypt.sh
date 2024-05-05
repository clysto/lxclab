#!/bin/bash

# Usage: bcrypt.sh cost password

htpasswd -bnBC $1 "" $2 | tr -d ':\n' | sed 's/$2y/$2a/'
echo ""
