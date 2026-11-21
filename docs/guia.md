# Guia de uso do madeploy

Guia completo para quem vai **usar** o madeploy no dia a dia. Referências mais específicas:

- [`config.md`](config.md): todas as chaves do `madeploy.yaml`.
- [`runbook.md`](runbook.md): como reverter à mão, sem o madeploy.
- [`plan.md`](plan.md): decisões de projeto (ADRs) e riscos.

---

## Sumário

1. [O que é e como funciona](#1-o-que-é-e-como-funciona)
2. [Instalação](#2-instalação)
3. [Preparar o servidor](#3-preparar-o-servidor)
4. [Primeiro uso: trazer o código de produção](#4-primeiro-uso-trazer-o-código-de-produção)
5. [Configuração essencial](#5-configuração-essencial)
6. [Dia a dia](#6-dia-a-dia)
7. [Referência de comandos](#7-referência-de-comandos)
8. [Interface interativa (TUI)](#8-interface-interativa-tui)
9. [Banco de dados e migrações](#9-banco-de-dados-e-migrações)
10. [Telas novas e controle de acesso](#10-telas-novas-e-controle-de-acesso)
11. [Drift: alterações feitas direto no servidor](#11-drift-alterações-feitas-direto-no-servidor)
12. [Rollback e recuperação](#12-rollback-e-recuperação)
13. [Releases atômicas (opcional)](#13-releases-atômicas-opcional)
14. [Automação, CI e códigos de saída](#14-automação-ci-e-códigos-de-saída)
15. [Segurança](#15-segurança)
16. [Solução de problemas](#16-solução-de-problemas)
17. [Perguntas frequentes](#17-perguntas-frequentes)

---

## 1. O que é e como funciona

O madeploy substitui o deploy por SSH do MadBuilder para uma aplicação MadBuilder 4 / Adianti cujo código passa a viver num repositório Git. Ele roda **na sua máquina** e conversa com o servidor só por SSH. Nada é instalado no servidor.

Em cada deploy, o madeploy:

1. Calcula o que mudou desde o último deploy (Git + hash sha256 de cada arquivo).
2. Confere se alguém alterou arquivos direto no servidor (**drift**) e para se houver.
3. Mostra o **plano** completo: arquivos, migrações de banco, permissões e hooks.
4. Pede confirmação: você digita o nome do ambiente.
5. Faz backup do que vai ser tocado, envia **um único pacote**, valida a sintaxe PHP **no PHP do servidor** e aplica.
6. Confere se o sistema responde (**healthcheck**). Se não responder, **desfaz sozinho**.

### Conceitos

| Termo | O que significa |
|---|---|
| **Ambiente** | Um servidor de destino declarado em `environments:` no `madeploy.yaml` (ex.: `production`). Quase todo comando recebe o nome dele. |
| **Baseline** | A fotografia inicial do servidor (lista de arquivos + sha256), gravada pelo `init`. É o ponto de partida para detectar mudanças e drift. |
| **Manifesto** | A lista de arquivos e hashes do último deploy, em `<path>/.madeploy/manifest.json` no servidor. |
| **Drift** | Arquivo alterado, criado ou apagado no servidor **fora** do madeploy (à mão, por FTP, pelo atualizador do MadBuilder). |
| **Plano** | Tudo o que um deploy faria. É mostrado antes de qualquer alteração e recalculado sob o lock. |
| **Lock** | Trava no servidor (`.madeploy/deploy.lock/`) que impede dois deploys ao mesmo tempo. |
| **In-place** | Estratégia padrão: os arquivos são trocados dentro da pasta da aplicação. |
| **Releases** | Estratégia opcional: cada deploy é uma pasta nova, e um symlink `current` aponta para a versão no ar. O rollback é instantâneo. |

### O que o madeploy **nunca** faz

- Conectar num servidor que não está no seu `~/.ssh/known_hosts`.
- Guardar senha em arquivo.
- Reverter o banco sozinho.
- Apagar ou alterar dados de negócio.
- Mexer em `app/config`, uploads excluídos, `tmp`, `files`, `app/output` e fotos.

---

## 2. Instalação

Requisito: **Go 1.26+** (`go version`).

```sh
cd ~/Projetos/madeploy
go install ./cmd/madeploy                                     # instala em ~/go/bin/madeploy
go install github.com/sqldef/sqldef/v3/cmd/mysqldef@v3.11.30  # usado por db pull / db diff
```

Se aparecer `command not found: madeploy`, a pasta `~/go/bin` não está no `PATH`:

```sh
echo 'export PATH="$PATH:$HOME/go/bin"' >> ~/.zshrc && source ~/.zshrc
```

**Atualizar** depois de uma correção no código: rode `go install ./cmd/madeploy` de novo na pasta do madeploy.

**Conferir:** `madeploy --version` e `madeploy --help`.

**No servidor**, o madeploy usa `sh`, `tar`, `gzip`, `sha256sum`, `find`, o `php` de linha de comando (lint) e o `mysqldump` (backup do banco). `setfacl` é usado se existir. A primeira linha de cada comando mostra o que foi encontrado, por exemplo: `servidor sisvita-prod: sem-acl mysqldump php-7.4.3`.

---

## 3. Preparar o servidor

Faça isto **uma vez**, como `root` no servidor.

### 3.1 Usuário de deploy

O ideal é um usuário próprio, no grupo do servidor web:

```sh
adduser deploy
usermod -aG www-data deploy
chown -R deploy:www-data /var/www/html        # o deploy passa a ser dono do código
```

> Antes do `chown`, liste no `permissions.writable` todas as pastas onde o sistema **grava** (uploads, `tmp`, relatórios). O madeploy deixa só essas pastas graváveis pelo `www-data`.

Usar `root` também funciona, mas um erro de configuração rodando como root tem alcance total no servidor.

### 3.2 Chave SSH (o madeploy não usa senha)

O madeploy autentica **só por chave**, pelo `ssh-agent` ou pelo `IdentityFile`. Se hoje você entra com usuário e senha, use a senha **uma última vez** para instalar a chave:

```sh
# na sua máquina
ssh-keygen -t ed25519 -f ~/.ssh/sisvita -C sisvita-deploy
ssh-copy-id -i ~/.ssh/sisvita.pub deploy@IP_DO_SERVIDOR
```

No `~/.ssh/config`:

```
Host sisvita-prod
    HostName IP_DO_SERVIDOR
    User deploy
    IdentityFile ~/.ssh/sisvita
```

Conecte uma vez com `ssh sisvita-prod` e **confira a impressão digital** da chave do servidor. Isso grava o servidor no `~/.ssh/known_hosts`, e sem esse registro o madeploy recusa a conexão.

Chave com senha? Rode `ssh-add --apple-use-keychain ~/.ssh/sisvita` uma vez (macOS).

### 3.3 Limpar o cache do PHP (OPcache) depois do deploy

Sem isso, o PHP continua servindo a versão antiga por alguns segundos depois do deploy. O comando depende de como o PHP roda:

```sh
apache2ctl -M | grep -Ei 'php|proxy_fcgi'
```

| Saída | Modo | Hook `after_switch` | Linha do sudoers |
|---|---|---|---|
| `php7_module` / `php_module` | mod_php | `sudo -n apache2ctl graceful` | `deploy ALL=(root) NOPASSWD: /usr/sbin/apache2ctl graceful` |
| `proxy_fcgi_module` | PHP-FPM | `sudo -n systemctl reload php8.2-fpm` | `deploy ALL=(root) NOPASSWD: /usr/bin/systemctl reload php8.2-fpm` |

Libere **só esse comando** no sudoers:

```sh
echo 'deploy ALL=(root) NOPASSWD: /usr/sbin/apache2ctl graceful' > /etc/sudoers.d/madeploy
chmod 440 /etc/sudoers.d/madeploy && visudo -c
```

### 3.4 Versão do PHP do lint

O `php -l` roda no PHP de linha de comando do servidor, que precisa ser **a mesma versão** que o Apache/FPM usa. Confira com `php -v` e `ls /etc/apache2/mods-enabled/ | grep php`. Se forem diferentes, aponte `php:` no config para o binário certo (ex.: `php7.4`).

---

## 4. Primeiro uso: trazer o código de produção

```sh
mkdir ~/Projetos/sisvita && cd ~/Projetos/sisvita

# 1. cria o madeploy.yaml (modelo comentado)
madeploy init
#    → ajuste ssh, path, php, hooks, healthcheck, exclude e writable (seção 5)

# 2. confere conexão, ferramentas e permissões (só leitura)
madeploy doctor production

# 3. baixa o código que está no ar e grava o baseline no servidor
madeploy init production --pull-to .
```

O passo 3 mostra o andamento ao vivo:

```
· pull          contando arquivos em /var/www/html (sem config, uploads e excluídos)…
· pull          5678 arquivo(s), 80.0 MB a baixar para .
⇣ 1203/5678 arquivos · 20.1 MB/80.0 MB · 25% · 1.3 MB/s · restam ~46s · app/control/PacienteList.php
✓ pull          5678 arquivo(s), 80.0 MB baixados (1m02s)
· baseline      calculando o sha256 de cada arquivo no servidor (só leitura)…
✓ baseline      5678 arquivo(s) registrados em /var/www/html/.madeploy (18s)
```

Depois:

```sh
# 4. confira se veio algum dado que não é código (PDFs, fotos, .db) e exclua (seção 5.2)
find . -type f \( -iname '*.pdf' -o -iname '*.jp*g' -o -name '*.db' \) -not -path './vendor/*' | head

# 5. crie o .gitignore (seção 5.3) e o repositório
git init -b main && git add -A && git commit -m "chore: estado de produção"

# 6. traga o esquema do banco
madeploy db pull production
git add -A && git commit -m "chore: esquema de produção"

# 7. valide: tem de dizer "nada a fazer"
madeploy deploy production --dry-run
```

Se o passo 7 listar arquivos, a sua pasta **não** está igual à produção. Descubra o motivo antes de qualquer deploy real.

Por fim, desligue o atualizador do MadBuilder. Se o `doctor` avisar que `application.ini [builder]` ainda tem token, remova o token. Senão a nuvem do MadBuilder pode reescrever arquivos e gerar drift.

---

## 5. Configuração essencial

Referência completa: [`config.md`](config.md). Chave desconhecida é erro, e todos os erros aparecem de uma vez.

### 5.1 Exemplo (Apache + mod_php, usuário deploy)

```yaml
project: sisvita
extra: [vendor]                       # vendor não versionado vai no pacote

exclude: [/arquivos_cpr/, /arquivos_hitorico_paciente/, /atendimento/,
          /contratos_profissional/, /documentos/, /tmpe/, /app/anexos/, /app/database/*.db]

permissions:
  owner: deploy
  group: www-data
  default: { dir: "0755", file: "0644" }
  writable: [tmp, app/output, files, app/images/photos, arquivos_cpr, atendimento,
             contratos_profissional, docs, documentos, arquivos_hitorico_paciente, tmpe, app/anexos]
  restricted: { "app/config/*": "0640" }

databases:
  sisvita:    { config: app/config/sisvita.php }
  permission: { config: app/config/permission.php }
  log:        { config: app/config/log.php, backup: none }

programs:
  connection: permission
  groups: [Admin]

environments:
  production:
    ssh: sisvita-prod                 # alias do ~/.ssh/config
    path: /var/www/html
    branch: main                      # só faz deploy desta branch
    php: php                          # mesma versão do PHP do Apache
    keep: 5                           # backups mantidos
    sudo: false                       # true = chown/setfacl com sudo -n
    migrate: before
    hooks:
      after_switch:
        - sudo -n apache2ctl graceful # limpa o OPcache (mod_php)
    healthcheck:
      url: https://sistema.exemplo.com.br/index.php?class=LoginForm
      expect_status: 200
      expect_body: LoginForm          # texto que só aparece quando a página está OK
      timeout: 20s
```

### 5.2 `exclude` × `writable`: a regra mais importante

| | Para que serve |
|---|---|
| `exclude` | O madeploy **nunca** baixa, envia, apaga ou compara esses caminhos. Use para **dados**: uploads, documentos de pacientes, SQLite de runtime. |
| `writable` | O madeploy deixa essas pastas graváveis pelo grupo do servidor web (ACL ou `chgrp` + `2775`). |

Uma pasta de upload normalmente vai **nas duas** listas.

Já são excluídos por padrão: `app/config/*` (menos `framework_hashes.php`), `tmp/`, `files/`, `app/output/`, `app/images/photos/`, `.git/`, `.madeploy/`, `.env*`, `docs/`, `tests/`, `database/`, `node_modules/` e arquivos de editor/SO.

### 5.3 `.gitignore` e `exclude` andam juntos

> **Toda pasta de dados que estiver no `.gitignore` e existir no servidor também precisa estar no `exclude`.**

O motivo: o baseline registra tudo o que existe no servidor. Se uma pasta some do repositório (porque o Git a ignora) mas não está no `exclude`, o madeploy entende como "código removido" e **planeja apagá-la em produção**. O plano mostra a remoção antes, mas é fácil não notar numa lista longa.

`.gitignore` recomendado:

```gitignore
/app/config/*
!/app/config/framework_hashes.php
.env
.env.*
!.env.example

# dados (espelhar no exclude do madeploy.yaml)
/arquivos_cpr/
/arquivos_hitorico_paciente/
/atendimento/
/contratos_profissional/
/documentos/
/tmpe/
/app/anexos/
/files/
/app/images/photos/
/app/output/
/tmp/
/app/database/*.db

/vendor/
/.madeploy/

*.log
*.sql.gz
*.bak
*.orig
*.swp
*~
.DS_Store
._*
Thumbs.db
.idea/
.vscode/
```

Use `/app/config/*` com exceção, e não uma lista de arquivos. Assim uma config nova com senha nunca entra no Git por engano.

### 5.4 Credenciais do banco

O madeploy lê as credenciais do `app/config/<conexão>.php` **do servidor**, em memória, sem executar PHP. Para sobrescrever, use variáveis de ambiente: `MADEPLOY_<CONEXAO>_{HOST,PORT,NAME,USER,PASS}` (ex.: `MADEPLOY_SISVITA_PASS`). Nenhuma senha vai para o `madeploy.yaml`.

---

## 6. Dia a dia

### 6.1 Alterar código

```sh
# edite, teste localmente, faça commit
git add -A && git commit -m "fix: cálculo da agenda"

madeploy status production            # implantado × HEAD, pendências, drift, lock
madeploy diff production              # o que vai subir (A/M/D/R) e drift
madeploy deploy production --dry-run  # o plano completo, sem alterar nada
madeploy deploy production            # mostra o plano e pede para digitar "production"
```

O deploy exige **árvore Git limpa** (tudo commitado) e a **branch** configurada em `branch:`.

### 6.2 Criar uma tela nova

```sh
# crie app/control/AgendaForm.php e o item no menu.xml
madeploy sync programs production     # gera database/permission/data/<ts>_sync_programs.sql
git add -A && git commit -m "feat: agenda"
madeploy deploy production            # envia a tela e registra em system_program + grupos
```

### 6.3 Alterar o banco

```sh
# edite database/sisvita/schema/table_paciente.sql (ex.: nova coluna)
madeploy db diff production --name add_apelido
#   → gera database/sisvita/migrations/<ts>_add_apelido.sql; revise o SQL!
git add -A && git commit -m "feat: apelido do paciente"
madeploy deploy production            # backup das tabelas afetadas → migração → arquivos
```

### 6.4 Pela interface

```sh
madeploy tui production
```

Veja a [seção 8](#8-interface-interativa-tui).

### 6.5 Fluxo interno de um deploy

```
árvore Git limpa e branch permitida
→ pré-checagem remota (ferramentas, disco, versão do PHP)
→ lock
→ plano (Git + manifesto + drift + migrações + permissões + hooks)
→ confirmação (digitar o ambiente)
→ php -l local (se houver php na sua máquina)
→ pacote tar.gz único, sha256 conferido no servidor
→ php -l no PHP de produção
→ backup dos arquivos tocados (e do banco, se houver migração)
→ before_switch → migrações (migrate: before) → troca dos arquivos
→ permissões só no que mudou + pastas graváveis
→ after_switch (reload do PHP)
→ healthcheck  ──falhou──► restaura os arquivos sozinho
→ estado, manifesto e log → retenção de backups → libera o lock
```

`Ctrl-C` só cancela **entre** etapas. Nenhuma etapa é interrompida no meio.

---

## 7. Referência de comandos

Forma geral: `madeploy <comando> <ambiente> [flags]`. O **ambiente é obrigatório** em todos, menos no `init` sem argumentos. `madeploy status` sem o ambiente dá `accepts 1 arg(s), received 0`.

### Flags globais

| Flag | Efeito |
|---|---|
| `-c, --config ARQ` | Outro arquivo de configuração (padrão `madeploy.yaml`). |
| `--json` | Saída estável em JSON no stdout; eventos em JSON no stderr. |
| `-y, --yes` | Não pergunta nada. Obrigatório sem terminal (CI). |
| `-v, --version` | Versão. |

### `init`: preparar

| Uso | O que faz |
|---|---|
| `madeploy init` | Cria o `madeploy.yaml` modelo na pasta atual. |
| `madeploy init <amb>` | Checa o servidor, grava o baseline e sugere o sudoers. |
| `madeploy init <amb> --pull-to DIR` | Também baixa o código de produção para `DIR` (com progresso). |
| `--force` | Refaz o baseline mesmo se já existir. Use com cuidado: o que estiver no servidor passa a ser "o implantado". |
| `--php-service NOME` | Serviço usado no sudoers sugerido (padrão `php8.2-fpm`). |
| `--migrate-layout [--finish]` | Converte in-place → releases ([seção 13](#13-releases-atômicas-opcional)). |

### `status <amb>`

Commit implantado × HEAD local, arquivos e migrações pendentes, drift e lock. Só leitura.

### `diff <amb>`

Lista os arquivos que subiriam (`A` novo, `M` modificado, `D` removido, `R` renomeado) e o drift. Só leitura.

### `deploy <amb>`

| Flag | Efeito |
|---|---|
| `--dry-run` | Só mostra o plano. |
| `--only files,db,perms` | Faz só parte do deploy. `perms` sozinho reaplica permissões sem enviar arquivos. |
| `--on-drift overwrite` | Sobrescreve as alterações manuais (o backup guarda a versão manual). |
| `--on-drift ignore` | Não toca nos arquivos com drift (eles continuam aparecendo como drift). |
| `--allow-destructive` | Permite migrações destrutivas ([9.2](#92-classificação-de-risco)). |
| `--allow-large` | Permite `ALTER` em tabela acima de `large_table_rows`. |

Um deploy sem mudanças é **no-op**: diz "nada a fazer" e só atualiza o ponteiro de commit.

### `rollback <amb> [--to ID]`

Reverte o **código** do último deploy, ou de todos até `ID`. **O banco não é alterado.** Os IDs saem de `madeploy releases`.

### `releases <amb>`

Lista os deploys com backup disponível. Com `strategy: releases`, marca a versão no ar com `*`.

### `logs <amb> [--id ID] [-f]`

Mostra o log JSON do último deploy (ou de `ID`). `--follow` acompanha ao vivo um deploy em andamento, inclusive um disparado por outra pessoa. Os logs ficam no servidor (`.madeploy/logs/`) e na sua máquina (`.madeploy/logs/`).

### `doctor <amb>`

Só leitura. Mostra:
- permissões erradas e config legível por todos;
- token do atualizador do MadBuilder ainda ativo;
- lock esquecido;
- rotas expostas.

### `unlock <amb>`

Remove o lock de um deploy interrompido (rede caiu, terminal fechado). Antes, confira que **não há** deploy rodando: `madeploy logs <amb> --follow`.

### `drift pull <amb> [arquivo...]`

Traz do servidor para o repositório os arquivos com drift (ou só os indicados). Depois revise com `git diff` e faça commit.

### `db`: banco ([seção 9](#9-banco-de-dados-e-migrações))

`--conn NOME` limita a uma conexão (padrão: todas de `databases:`).

| Comando | O que faz |
|---|---|
| `db pull <amb>` | Grava o esquema de produção em `database/<conn>/schema/` (um arquivo por tabela, view e trigger). |
| `db diff <amb> [--name X] [--allow-destructive]` | Compara o `schema/` local com produção e gera `migrations/<ts>_<X>.sql`. Sem `--allow-destructive`, os `DROP`s ficam fora. |
| `db apply <amb> [--allow-destructive] [--allow-large] [--resume]` | Aplica as migrações pendentes agora, com backup das tabelas afetadas. |
| `db status <amb>` | Migrações aplicadas, pendentes e parciais. |
| `db resolve <amb> <migração>` | Marca como aplicada uma migração que você terminou à mão. |

### `sync programs <amb>`

Registra telas novas em `system_program` e nos grupos de `programs.groups`, e valida os menus XML.

| Flag | Efeito |
|---|---|
| (nenhuma) | Gera `database/<permission>/data/<ts>_sync_programs.sql`, que vai no próximo deploy. |
| `--apply` | Aplica agora, com backup. |
| `--allow-broken-menu` | Não falha quando o menu aponta para uma classe inexistente. |

### `tui <amb>`

Interface interativa ([seção 8](#8-interface-interativa-tui)).

### `completion`

Gera o autocompletar do shell: `madeploy completion zsh > "${fpath[1]}/_madeploy"`.

---

## 8. Interface interativa (TUI)

```sh
madeploy tui production
```

O argumento é o **ambiente**, não a tela. As telas se abrem pelas teclas, já dentro da TUI.

| Tecla | Tela | O que mostra |
|---|---|---|
| `1` | Painel | Commit no ar × local, pendências, drift, lock. |
| `2` | Arquivos | Lista A/M/D/R. `enter` abre o diff local × produção; `/` filtra. |
| `3` | Banco | Migrações pendentes por risco e o SQL completo. |
| `4` | Permissões | O que será ajustado. |
| `5` | Execução | Etapas, barra de progresso e log ao vivo. |
| `6` | Releases | Deploys anteriores; `enter` faz rollback. |

| Tecla | Ação |
|---|---|
| `tab` / `→` / `l` · `shift+tab` / `←` / `h` | Próxima / tela anterior. |
| `↑` `↓` / `k` `j` | Navegar na lista. |
| `enter` | Abrir item (diff, rollback). |
| `/` | Filtrar arquivos. |
| `d` | Deploy (pede para digitar o nome do ambiente). |
| `c` | Cancelar o deploy em andamento (entre etapas). |
| `r` | Recarregar do servidor. |
| `?` | Ajuda. |
| `q` / `esc` | Sair / fechar. |

A TUI usa as mesmas funções dos comandos. O plano é recalculado sob o lock, e se mudou desde que você o viu na tela, o deploy recusa. Funciona em 80×24 e respeita `NO_COLOR`.

---

## 9. Banco de dados e migrações

### 9.1 Organização

```
database/<conexão>/schema/table_<nome>.sql     # fonte da verdade do esquema (db pull)
database/<conexão>/migrations/<ts>_<slug>.sql  # mudanças de esquema (db diff)
database/<conexão>/data/<ts>_<slug>.sql        # dados de sistema (sync programs ou à mão)
```

As migrações rodam **em ordem de nome**, **uma vez**, e ficam registradas na tabela `madeploy_migrations` de cada banco (checksum, operador, data, progresso). `DELIMITER` e corpos `BEGIN…END` (procedures, triggers) são suportados.

### 9.2 Classificação de risco

Cada instrução é classificada antes de rodar, e o plano mostra o nível e o motivo:

| Nível | Exemplos | Comportamento |
|---|---|---|
| **seguro** | `CREATE TABLE`, `ADD COLUMN` com `NULL`/`DEFAULT`, `CREATE INDEX` | Roda. |
| **atenção** | `MODIFY`/`CHANGE` de tipo, `RENAME TABLE`, `DROP VIEW/TRIGGER/PROCEDURE`, `DROP INDEX` | Roda; o motivo aparece no plano para você revisar. |
| **destrutivo** | `DROP TABLE`, `DROP COLUMN`, `TRUNCATE`, `DELETE`/`UPDATE` sem `WHERE`, remover índice único, coluna `NOT NULL` sem `DEFAULT` | Bloqueia (código 2) até `--allow-destructive`. |
| **tabela grande** | `ALTER` em tabela acima de `large_table_rows` (padrão 100 mil linhas) | Bloqueia até `--allow-large`. Prefira `gh-ost`/`pt-online-schema-change` + `db resolve`. |

### 9.3 Backups

Antes de migrar, o madeploy roda `mysqldump --single-transaction` **no servidor**:
- só das tabelas afetadas (`backup: tables`, o padrão), ou do banco inteiro (`backup: full`);
- no deploy, o dump fica em `.madeploy/backups/<id>/db-<conn>.sql.gz`;
- no `db apply`, fica em `.madeploy/db-backups/<id>/`;
- a senha vai num arquivo `0600` temporário, apagado mesmo em caso de erro.

Se o `mysqldump` do servidor for MariaDB e o MySQL usar certificado autoassinado (erro 2026), use `dump_options: [--skip-ssl-verify-server-cert]` na conexão.

### 9.4 Migração que parou no meio

DDL do MySQL não é transacional, então cada instrução é registrada ao terminar. Se falhar no meio:

```sh
madeploy db status production                    # mostra "partial", N de M
# corrija a causa e continue de onde parou:
madeploy db apply production --resume
# ou, se terminou à mão:
madeploy db resolve production --conn sisvita migrations/<arquivo>.sql
```

Nunca edite uma migração já aplicada: o checksum muda e o deploy recusa. Crie outra.

### 9.5 Expand/contract

Com `migrate: before` (padrão), as migrações rodam **antes** da troca do código, então por alguns segundos o código antigo roda sobre o esquema novo. Por isso:

1. **Expand**: só adições compatíveis (coluna nova `NULL`/`DEFAULT`, tabela nova, índice).
2. O código novo passa a usar o que foi adicionado.
3. **Contract**, num deploy posterior: `DROP`, `NOT NULL`, renomeações, com `--allow-destructive`.

---

## 10. Telas novas e controle de acesso

No Adianti, uma tela só aparece para o usuário se estiver em `system_program` e ligada a um grupo. O `sync programs`:

- Procura classes novas em `app/control` (configurável em `programs.control_dir`, com `programs.exclude` para classes que não são telas).
- Gera SQL **idempotente**: `INSERT … WHERE NOT EXISTS`, com `MAX(id)+1` ou AUTO_INCREMENT quando detectado.
- Liga a tela aos grupos de `programs.groups` (padrão `Admin`).
- **Nunca apaga** registros.
- Valida os menus XML: um item apontando para uma classe inexistente é erro.

O `deploy` avisa quando há tela nova sem registro. Nomes amigáveis: `programs.names: { AgendaForm: "Agenda" }`.

---

## 11. Drift: alterações feitas direto no servidor

Se alguém alterou, criou ou apagou um arquivo no servidor fora do madeploy, o deploy **para com código 2** e lista os arquivos. Você decide:

| Quero… | Comando |
|---|---|
| **manter** a alteração manual | `madeploy drift pull production` → `git diff` → commit → deploy |
| **descartar** a alteração manual | `madeploy deploy production --on-drift=overwrite` (o backup guarda a versão manual) |
| deixar como está por enquanto | `madeploy deploy production --on-drift=ignore` |

Causa mais comum: o atualizador do MadBuilder ainda ativo (token em `application.ini [builder]`). O `doctor` avisa.

---

## 12. Rollback e recuperação

```sh
madeploy releases production          # lista os deploys
madeploy rollback production          # volta o último
madeploy rollback production --to 20261120-153012.345   # volta até esse
```

- O rollback reverte **só o código**. Para o banco, use os dumps em `.madeploy/backups/<id>/` ([`runbook.md`](runbook.md), seção 2).
- Se o healthcheck falha, o madeploy restaura os arquivos **sozinho**. O backup do deploy que falhou vira `reverted-<id>` e não é aplicado de novo.
- Tudo o que o madeploy faz pode ser revertido **sem ele**, só com SSH. Os passos estão em [`runbook.md`](runbook.md).

---

## 13. Releases atômicas (opcional)

Com acesso à configuração do Apache (vhost), dá para trocar a estratégia in-place por releases:

```
<path>/releases/<id>/    uma árvore por deploy (cp -al da anterior + só o delta)
<path>/shared/           tmp, files, app/output, fotos e app/config (links em cada release)
<path>/current -> releases/<id>
```

Vantagens: a troca é um rename atômico do symlink, e o rollback é instantâneo. Passos:

1. `madeploy init production --migrate-layout`: copia a instalação atual para `releases/<id>` e os dados para `shared/`, e cria `current`. Nada do que está no ar muda.
2. **Você** aponta o `DocumentRoot` do Apache para `<path>/current` e recarrega o Apache.
3. Coloque `strategy: releases` no ambiente e rode `madeploy init production --migrate-layout --finish`, que copia os uploads gravados no meio tempo.

Pastas de upload próprias do sistema (ex.: `app/anexos`) também precisam ser compartilhadas: liste-as em `shared:` no ambiente, senão cada release teria uma cópia separada.

---

## 14. Automação, CI e códigos de saída

Sem terminal, toda ação que altera produção exige `--yes`:

```sh
madeploy deploy production --yes --json > plano.json 2> eventos.jsonl
```

- `--json`: resultado estável no **stdout** e um evento JSON por linha no **stderr** (`time`, `step`, `level`, `msg`, `duration`).
- Fora de um terminal, o progresso do pull vira uma linha a cada 5 s. Com `--json`, o progresso não aparece.

| Código | Significado |
|---|---|
| 0 | ok (inclui "nada a fazer") |
| 1 | erro (lint, healthcheck, SSH, migração que falhou…) |
| 2 | bloqueado até você decidir: drift, migração destrutiva, `ALTER` em tabela grande |
| 3 | cancelado pelo operador |
| 4 | `madeploy.yaml` ou flag inválida |
| 5 | outro deploy em andamento (lock) |

---

## 15. Segurança

- **known_hosts obrigatório**: o madeploy nunca aceita servidor desconhecido. Se a chave do servidor mudar, a conexão é recusada até você conferir e atualizar o `known_hosts`.
- **Só chave SSH**, nunca senha em arquivo.
- **Sem segredos no `madeploy.yaml`**: credenciais vêm do servidor ou de variáveis de ambiente.
- **Comandos remotos escapados**: nenhum caminho ou nome vira comando de shell sem aspas.
- **sudo mínimo**: só o reload do PHP/Apache, com `sudo -n` (nunca pede senha).
- **Dados de pacientes**: mantenha as pastas de upload no `exclude` e no `.gitignore` ([5.3](#53-gitignore-e-exclude-andam-juntos)). Se algum dado foi baixado para a sua máquina antes do `exclude`, apague a cópia local.
- **Scripts expostos**: arquivos como `phpinfo()` ou scripts de manutenção na raiz pública ficam acessíveis pelo navegador. O `doctor` aponta rotas expostas. Remova o que não deveria estar no ar.
- **Pasta `.madeploy/` no servidor**: se ela ficar dentro do DocumentRoot, bloqueie o acesso web a ela no `.htaccess` (`RedirectMatch 404 /\.madeploy`).

---

## 16. Solução de problemas

| Sintoma | Causa / solução |
|---|---|
| `command not found: madeploy` | `~/go/bin` fora do `PATH` ([seção 2](#2-instalação)). |
| `accepts 1 arg(s), received 0` | Faltou o ambiente: `madeploy status production`. |
| `ambiente "x" não existe (disponíveis: production)` | O argumento é o **ambiente**. Telas da TUI se abrem com as teclas 1–6. |
| erro de `known_hosts` / host desconhecido | Conecte uma vez com `ssh <alias>` e confira a impressão digital. |
| `unable to authenticate` | Chave não instalada (`ssh-copy-id`) ou com senha fora do agente (`ssh-add`). |
| `… já tem manifesto; use --force` | O baseline já existe. Só use `--force` se tiver certeza de que o servidor está no estado certo. |
| deploy bloqueado com código 2 | Drift ou migração arriscada; leia a lista e veja as seções [11](#11-drift-alterações-feitas-direto-no-servidor) e [9.2](#92-classificação-de-risco). |
| código 5 / "lock" | Outro deploy rodando ou um interrompido. Confira com `logs --follow` e use `unlock`. |
| `php -l` falha no servidor | Erro de sintaxe, ou código que exige uma versão de PHP mais nova que a do servidor. |
| healthcheck falha e o deploy é revertido | Veja o log; confira `url` e `expect_body` abrindo a URL no navegador. |
| a página continua mostrando a versão antiga | Falta o hook de OPcache certo ([3.3](#33-limpar-o-cache-do-php-opcache-depois-do-deploy)). |
| hook com `sudo -n` falha | Linha do sudoers ausente ou com caminho errado; `visudo -c` e `sudo -n -l` como deploy. |
| `Permission denied` ao enviar arquivos | O usuário do SSH não é dono dos arquivos ([3.1](#31-usuário-de-deploy)). |
| `--dry-run` do primeiro deploy lista arquivos | O repositório difere da produção; veja se `.gitignore` e `exclude` estão alinhados ([5.3](#53-gitignore-e-exclude-andam-juntos)). |
| mysqldump erro 2026 (TLS) | `dump_options: [--skip-ssl-verify-server-cert]` ([9.3](#93-backups)). |

Para investigar qualquer deploy: `madeploy logs production` (ou `--id <id>`).

---

## 17. Perguntas frequentes

**Posso continuar usando o MadBuilder para gerar telas?**
Sim, para gerar código **na sua máquina**, que depois você commita e implanta com o madeploy. Desligue só o atualizador remoto, que escreve direto no servidor.

**O madeploy apaga uploads ou dados?**
Não, se as pastas estiverem no `exclude`. Os padrões (`tmp`, `files`, `app/output`, fotos, `app/config`) já vêm excluídos. Uploads próprios do sistema você precisa listar ([5.2](#52-exclude--writable-a-regra-mais-importante)).

**E se a internet cair no meio do deploy?**
Cada etapa termina ou não começa. Rode `madeploy status`: se o lock ficou, use `unlock` e repita o deploy. O backup da tentativa está em `.madeploy/backups/`.

**Duas pessoas podem fazer deploy?**
Sim, mas não ao mesmo tempo: o lock impede, e o segundo recebe o código 5. `logs --follow` mostra o deploy do outro.

**Como faço deploy só das permissões?**
`madeploy deploy production --only perms`.

**Posso ter um ambiente de homologação?**
Sim: adicione outro bloco em `environments:` (ex.: `staging`) com o seu próprio `ssh`, `path` e `healthcheck`, e use `madeploy deploy staging`.
