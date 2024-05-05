#!/bin/bash

# Usage: adduser.sh db username password

password=$(htpasswd -bnBC 10 "" $3 | tr -d ':\n' | sed 's/$2y/$2a/')

# check if user exists
if [ $(sqlite3 $1 "SELECT COUNT(*) FROM users WHERE username='$2';") -ne 0 ]; then
    echo "User already exists"
    exit 1
fi

# add user to database
sqlite3 $1 "INSERT INTO users (username, password, instance_limit) VALUES ('$2', '$password', 3);"
echo "User added successfully"
