# madeploy

Deploy por SSH de aplicações MadBuilder 4 / Adianti, para substituir o deploy do MadBuilder quando o código passa a viver num repositório Git. Envia só o que mudou, detecta arquivos alterados à mão em produção (drift), aplica migrações de esquema e de dados com backup e registro, registra telas novas no controle de acesso, ajusta permissões, roda hooks, confere a aplicação no ar e reverte sozinho se ela não responder.

**Guia completo de uso: [`docs/guia.md`](docs/guia.md).** Decisões, riscos e o que ficou para depois: `docs/plan.md`. Fatos sobre o sistema-alvo: `docs/discovery.md`. Perguntas abertas: `docs/open-questions.md`.

## Instalação

```sh
go install github.com/vitalsoft/madeploy/cmd/madeploy@latest      # ou os binários do GoReleaser
go install github.com/sqldef/sqldef/v3/cmd/mysqldef@v3.11.30       # só para db pull/diff
```

No servidor: `sh`, `tar`, `gzip`, `sha256sum`, `find`, o `php` de produção (para o lint) e `mysqldump` (para o backup do banco). `setfacl` é usado se existir. Nada é instalado lá.

## Primeiro uso (saindo do deploy do MadBuilder)

1. **SSH.** Conecte uma vez com `ssh` e confira a chave do servidor: o madeploy exige `~/.ssh/known_hosts` e nunca aceita host desconhecido. Alias, usuário, porta, chave e `ProxyJump` vêm do `~/.ssh/config`; chaves com senha precisam estar no `ssh-agent`.
2. **Config.** Num diretório vazio: `madeploy init` cria o `madeploy.yaml`. Ajuste `ssh`, `path` e `healthcheck` (referência em `docs/config.md`). Segredos nunca entram nele: as credenciais de banco são lidas do `app/config/*.php` **de produção**, ou de `MADEPLOY_<CONEXAO>_PASS`.
3. **Semente a partir de produção.** `madeploy init production --pull-to .` baixa o código que está no ar (sem `app/config`, `tmp`, `files`, `app/output` nem fotos), grava o *baseline* em `.madeploy/` no servidor e sugere o trecho de `sudoers`. Depois: `git init && git add -A && git commit -m "chore: estado de produção"`.
4. **`vendor/`.** Se não for versionado, rode `composer install --no-dev -o` localmente e deixe `extra: [vendor]` no config (ADR-06).
5. **Esquema.** `madeploy db pull production` grava `database/<conexão>/schema/*.sql` (um arquivo por tabela, view e trigger). Faça commit.
6. **Primeiro deploy.** `madeploy deploy production` precisa dizer *nada a fazer*. Se não disser, o repositório não é igual à produção: investigue antes de seguir.
7. **Desligue o atualizador do MadBuilder.** `madeploy doctor production` avisa se `application.ini [builder]` ainda tem token. Enquanto tiver, a nuvem do MadBuilder pode reescrever código e permissões e gerar drift (ADR-11).

## Releases atômicas (opcional)

Com acesso ao vhost, troque o deploy in-place pelo layout de releases. A troca de versão vira a mudança de um symlink, e o rollback fica instantâneo:

```
<path>/releases/<id>/    uma árvore por deploy (montada com cp -al da anterior + só o delta)
<path>/shared/           tmp, files, app/output, fotos e app/config de produção (links em cada release)
<path>/current -> releases/<id>
```

1. `madeploy init production --migrate-layout` copia a instalação atual para `releases/<id>` e os dados para `shared/`, e cria `current`. Nada do que está no ar é alterado.
2. **Você** aponta o DocumentRoot para `<path>/current` e recarrega o servidor web e o PHP-FPM. O madeploy só mostra as instruções.
3. Coloque `strategy: releases` no ambiente e rode `madeploy init production --migrate-layout --finish`, que copia os uploads gravados no meio tempo sem sobrescrever nada.

A troca usa `mv -T` (rename(2) atômico), com `ln -sfn` só onde `mv -T` não existe. Se o healthcheck falhar, `current` volta sozinho para a release anterior. `madeploy rollback` aponta `current` para a release anterior (ou `--to <id>`), e `madeploy releases` marca a atual com `*`.

## Interface interativa

`madeploy tui production` abre 6 telas: Painel, Arquivos (com diff local × produção e filtro `/`), Banco (instruções por risco e SQL completo), Permissões, Execução (etapas, barra de progresso e log ao vivo; `c` cancela entre etapas) e Releases (`enter` faz rollback). Funciona em 80×24, respeita `NO_COLOR` e tem ajuda no `?`. Deploy e rollback pedem para digitar o nome do ambiente. A TUI só chama as mesmas funções dos comandos: o plano é recalculado sob o lock e, se mudou desde a tela, o deploy recusa.

## Dia a dia

```sh
madeploy status production              # implantado × HEAD, pendências, drift, lock
madeploy diff production                # A/M/D/R e drift, sem enviar nada
madeploy db diff production --name add_apelido   # gera database/<conn>/migrations/<ts>_add_apelido.sql
madeploy sync programs production       # telas novas → database/<permission>/data/<ts>_sync_programs.sql
git add -A && git commit -m "feat: agenda"
madeploy deploy production --dry-run    # o plano completo
madeploy deploy production              # mostra o plano e pede o nome do ambiente
```

Fluxo do `deploy`: árvore Git limpa e branch permitida → pré-checagem remota (ferramentas, disco, versão do PHP) → lock → plano (Git + manifesto + drift + migrações + permissões + hooks) → confirmação → `php -l` local (se houver `php`) → pacote `tar.gz` único com conferência do sha256 → `php -l` **no PHP de produção** → backup dos arquivos tocados (e do banco, se houver migração) → `before_switch` → migrações (`migrate: before`, o padrão) → troca → permissões só no que mudou e nos diretórios graváveis → `after_switch` → healthcheck → estado, manifesto e log → retenção → lock liberado. Se o healthcheck falhar, os arquivos voltam sozinhos. **O banco nunca volta sozinho** (`docs/runbook.md`).

Ctrl-C só cancela entre etapas; nenhuma etapa é interrompida no meio.

## Comandos

| Comando | O que faz |
|---|---|
| `init [env] [--pull-to DIR] [--force]` | sem env: cria `madeploy.yaml`; com env: checa o servidor, grava o baseline, sugere sudoers |
| `init <env> --migrate-layout [--finish]` | converte in-place → releases sem downtime (o DocumentRoot fica com você) |
| `tui <env>` | interface interativa sobre os mesmos comandos |
| `status <env>` | commit implantado × local, pendências, drift, lock |
| `diff <env>` | arquivos A/M/D/R e drift |
| `deploy <env> [--dry-run] [--only files,db,perms] [--on-drift overwrite\|ignore] [--allow-destructive] [--allow-large]` | deploy completo |
| `rollback <env> [--to ID]` | reverte o código do último deploy (ou até ID); banco intocado |
| `releases <env>` | deploys com backup disponível |
| `drift pull <env> [arquivo...]` | traz do servidor os arquivos com drift para o repositório |
| `db pull\|diff\|apply\|status\|resolve <env> [--conn X]` | esquema e migrações |
| `sync programs <env> [--apply]` | registra telas novas em `system_program` e nos grupos; valida os menus XML |
| `logs <env> [--id ID] [--follow]` | log JSON do deploy (gravado no servidor e em `.madeploy/logs/` local) |
| `doctor <env>` | permissões erradas, config legível, token do MadBuilder, lock, rotas expostas |
| `unlock <env>` | remove o lock de um deploy interrompido |

Flags globais: `--config`, `--json` (saída estável em stdout; eventos em JSON no stderr), `--yes` (obrigatório sem terminal, para CI). Sem `--yes`, toda ação que altera produção pede para digitar o nome do ambiente.

### Códigos de saída

| Código | Significado |
|---|---|
| 0 | ok (inclui "nada a fazer") |
| 1 | erro (lint, healthcheck, SSH, migração que falhou...) |
| 2 | bloqueado até você decidir: drift, migração destrutiva, `ALTER` em tabela grande |
| 3 | cancelado pelo operador |
| 4 | `madeploy.yaml` ou flag inválida |
| 5 | outro deploy em andamento (lock) |

## Testes

```sh
go test -race ./...                                    # unitários + deploy completo em diretórios temporários
go test -tags integration -v ./test/integration/       # Docker: SSH + Apache/PHP 8.2 + MySQL 8.0 (precisa do mysqldef no PATH)
go test ./cmd/madeploy -run Golden -update             # regrava o golden do plano
```

O teste de integração dirige o binário real contra um "servidor de produção" em container, com o banco acessível só pelo túnel SSH. Cobre: semente e baseline, deploy, permissões, `db pull`/`diff` (views e triggers), migração aditiva e destrutiva, falha no meio de uma migração, `sync programs`, drift, falha de healthcheck com restauração, rollback, lock, `doctor` e `logs`. Cobre também o layout de releases (migração sem downtime, troca do DocumentRoot, `cp -al`, `mv -T`, volta automática, rollback) e a TUI aberta num pseudo-terminal. Nunca rode nada disto contra um servidor real.
