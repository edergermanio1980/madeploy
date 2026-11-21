# madeploy.yaml

Chave desconhecida é erro (pega erro de digitação). Todos os problemas aparecem de uma vez. **Nenhum segredo vai neste arquivo.**

```yaml
project: sisvita                  # só informativo

extra: [vendor]                   # diretórios NÃO versionados que vão no pacote (vendor montado de composer.lock)
exclude: ["*.bak", "/storage/"]   # somados ao padrão abaixo; sintaxe gitignore (também lê .madeployignore)

lint:
  local: php                      # php local para o lint antes do envio; ausente = só o lint remoto (obrigatório)

permissions:
  owner: deploy                   # só usado com sudo (chown owner:group); sem sudo, chgrp group
  group: www-data
  default: { dir: "0755", file: "0644" }
  writable: [tmp, app/output, files, app/images/photos]   # ACL g:<group>:rwX (ou chgrp + 2775)
  restricted: { "app/config/*": "0640" }                  # glob → modo
  executable: ["bin/*.sh"]                                 # glob → 0755

databases:                        # nome = conexão Adianti
  sisvita:
    config: app/config/sisvita.php     # padrão: app/config/<nome>.php, lido NO SERVIDOR
    backup: tables                     # tables (só as afetadas) | full | none
    large_table_rows: 100000           # ALTER acima disso exige --allow-large
    dump_options: [--skip-ssl-verify-server-cert]   # flags extras do mysqldump do servidor
  permission: { config: app/config/permission.php }
  log:        { config: app/config/log.php, backup: none }

programs:
  connection: permission          # onde estão system_program / system_group
  groups: [Admin]                 # grupos que recebem a tela nova
  names: { AgendaForm: "Agenda" } # nome amigável em system_program.name (padrão: nome da classe)
  exclude: [BaseScreen]           # classes de app/control que não são telas
  control_dir: app/control

environments:
  production:
    ssh: sisvita-prod             # alias do ~/.ssh/config, host, user@host ou user@host:porta
    path: /var/www/sisvita        # absoluto
    branch: main                  # deploy só desta branch (vazio = qualquer)
    strategy: inplace             # inplace | releases (depois de init --migrate-layout)
    shared: [storage]             # releases: caminhos extras em shared/ (tmp, files, app/output,
                                  # app/images/photos e app/config/* já são compartilhados)
    php: php8.2                   # binário do PHP remoto, usado no lint
    keep: 5                       # backups de deploy mantidos
    sudo: false                   # true = chown/setfacl com "sudo -n" (regra NOPASSWD pré-autorizada)
    migrate: before               # before | after (em relação à troca dos arquivos)
    hooks:
      before_switch: []           # roda em <path>; {{path}} {{staging}} {{id}} disponíveis
      after_switch:
        - sudo -n systemctl reload php8.2-fpm   # obrigatório com OPcache (ver runbook)
    healthcheck:
      url: https://dominio/index.php?class=LoginForm
      expect_status: 200
      expect_body: LoginForm      # o Adianti responde 200 mesmo renderizando erro: use um marcador
      timeout: 20s
```

## Exclusões padrão

Nunca empacotados, nunca contados como drift, nunca removidos:

```
.git/ .madeploy/ .env .env.* node_modules/ /docs/ /tests/ .idea/ .vscode/ .DS_Store *.swp
/app/config/*  (exceto /app/config/framework_hashes.php, que é código)
/tmp/ /files/ /app/output/ /app/images/photos/ /app/database/.cache.db
/madeploy.yaml /.madeployignore /database/ /.gitignore /.gitattributes
```

## Credenciais

Lidas do `app/config/<conexão>.php` **do servidor**, em memória, sem executar PHP. Para sobrescrever: `MADEPLOY_<CONEXAO>_{HOST,PORT,NAME,USER,PASS}` (ex.: `MADEPLOY_SISVITA_PASS`). `localhost` vira `127.0.0.1` visto do servidor. O banco só é acessado pelo túnel SSH. Para o `mysqldump` remoto, a senha vai num arquivo `0600` temporário (`--defaults-extra-file`), apagado mesmo em caso de erro.

## Layout

```
repositório/
  madeploy.yaml  .madeployignore
  database/<conexão>/schema/{table,view,trigger}_<nome>.sql   # db pull; fonte da verdade do esquema
  database/<conexão>/migrations/<AAAAMMDDHHMMSS>_<slug>.sql   # db diff
  database/<conexão>/data/<AAAAMMDDHHMMSS>_<slug>.sql         # dados de sistema (sync programs ou à mão)
  .madeploy/logs/<id>.jsonl                                   # log local (não versionar)

servidor <path>/.madeploy/
  state.json  manifest.json  deploy.lock/
  backups/<id>/{files.tar.gz, files.list, added.list, manifest.json, state.json, meta.json, db-<conn>.sql.gz}
  db-backups/<id>/db-<conn>.sql.gz     # db apply / sync programs --apply
  logs/<id>.jsonl
  shared.json                          # releases: caminhos em shared/
  releases/<id>/{meta,manifest,state}.json

servidor <path>/ (strategy: releases)
  releases/<id>/   shared/   current -> releases/<id>
```

Com `releases`, `backups/<id>/` só guarda os dumps do banco: o backup dos arquivos é a release anterior.

Migrações (de esquema e de dados) são aplicadas na ordem do nome do arquivo, uma vez, e registradas em `madeploy_migrations` em cada banco. `DELIMITER` e corpos `BEGIN…END` são suportados.
