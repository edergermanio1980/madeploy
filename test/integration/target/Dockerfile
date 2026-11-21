# Simulated production: SSH + Apache + PHP 8.2 (same major.minor as SisVita), mysql client, ACL tools.
FROM php:8.2-apache
RUN apt-get update \
 && apt-get install -y --no-install-recommends openssh-server acl default-mysql-client sudo \
 && rm -rf /var/lib/apt/lists/* \
 && docker-php-ext-install pdo_mysql \
 && useradd -m -s /bin/bash -G www-data deploy \
 && echo 'deploy ALL=(root) NOPASSWD: /usr/sbin/apache2ctl graceful' > /etc/sudoers.d/madeploy \
 && chmod 0440 /etc/sudoers.d/madeploy \
 && mkdir -p /run/sshd \
 && printf 'PasswordAuthentication no\nAllowTcpForwarding yes\n' >> /etc/ssh/sshd_config
COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh
CMD ["/entrypoint.sh"]
