#!/bin/bash

# Usage: listuser.sh db

sqlite3 $1 "SELECT username, password, instance_limit FROM users;"
