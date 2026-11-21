#!/bin/sh
# Test container only: install the test key, seed the app as the MadBuilder deploy left it, start sshd + Apache.
set -e
install -d -m 700 -o deploy -g deploy /home/deploy/.ssh
install -m 600 -o deploy -g deploy /keys/authorized_keys /home/deploy/.ssh/authorized_keys
if [ ! -f /var/www/html/index.php ]; then
  rm -rf /var/www/html/*
  cp -a /seed/. /var/www/html/
  chown -R deploy:www-data /var/www/html
  chmod 2775 /var/www/html
  chmod 0640 /var/www/html/app/config/*.php
fi
ssh-keygen -A >/dev/null
/usr/sbin/sshd
exec apache2-foreground
