# Changelog

## [Não lançado]
- `init --pull-to`: conta antes o que vai baixar (mesmas exclusões do download) e mostra progresso ao vivo: arquivos e bytes feitos/total, %, velocidade, tempo restante e arquivo atual. Fora de terminal, uma linha a cada 5s; `--json`, silencioso. O baseline informa o que está calculando e quanto levou.
- Corrigido: `init --pull-to .` (destino relativo) era recusado com "entrada fora do destino".

## [0.1.0] — 2026-11-18

### F1 — núcleo e deploy in-place
- `madeploy.yaml` validado (chave desconhecida é erro; todos os problemas de uma vez).
- SSH nativo com `known_hosts` obrigatório, `ssh-agent`, `~/.ssh/config` e `ProxyJump`; todo comando remoto montado com argumentos escapados.
- Detecção de mudanças por Git (`--name-status`, renomeações) + manifesto sha256; drift (alterado, removido ou criado à mão em produção) bloqueia o deploy até `--on-drift` ou `drift pull`.
- `deploy` in-place: lock remoto atômico, pacote `tar.gz` único com conferência do sha256, `php -l` local e no PHP de produção, backup dos arquivos tocados, hooks, permissões só no delta + diretórios graváveis (ACL ou setgid), healthcheck com restauração automática, retenção de backups, logs JSON local e remoto.
- `init` (semente a partir de produção + baseline + sudoers sugerido), `status`, `diff`, `rollback`, `releases`, `logs --follow`, `doctor`, `unlock`, `drift pull`.
- Deploy sem mudanças é no-op (só move o ponteiro de commit).

### F3 — banco
- `db pull` / `db diff` com sqldef (`mysqldef`) pelo túnel SSH; esquema versionado em um arquivo por tabela, view e trigger.
- Classificação de risco por instrução; destrutivas fora do plano sem `--allow-destructive`; `ALTER` em tabela grande bloqueado sem `--allow-large`.
- `db apply` / migrações no deploy, instrução a instrução, com `madeploy_migrations` (checksum, progresso, operador, release); `--resume` e `db resolve` para migração parcial.
- Backup com `mysqldump --single-transaction` no servidor, credenciais em arquivo `0600` temporário.

### F4 — dados de sistema
- `sync programs`: telas novas → `system_program` (+ grupos), SQL idempotente, `MAX(id)+1` ou AUTO_INCREMENT detectado; nada é apagado; validação dos menus XML; aviso no deploy quando há tela sem registro.

### F6 — distribuição e docs
- GoReleaser (darwin/linux × amd64/arm64), README, `docs/config.md`, `docs/runbook.md`.
- Teste de integração em Docker (SSH + Apache/PHP 8.2 + MySQL 8.0) dirigindo o binário real.

### F2 — releases atômicas
- `strategy: releases`: `releases/<id>` montada a partir da anterior com `cp -al` (com fallback para `cp -a`) e só o delta aplicado; caminhos alterados são desligados antes, para nunca reescrever inodes da release anterior.
- `shared/` com symlinks (runtime e `app/config` de produção); troca de `current` com `mv -T` (rename atômico), com `ln -sfn` só como fallback.
- Healthcheck falho volta `current` sozinho; `rollback` e `rollback --to` (também para frente); retenção que nunca remove a release atual.
- `init --migrate-layout [--finish]`: converte in-place → releases sem downtime e só instrui a troca do DocumentRoot.

### F5 — TUI
- `madeploy tui <env>` (Bubble Tea v2 + Bubbles + Lip Gloss): Painel, Arquivos (diff e filtro), Banco (risco e SQL), Permissões, Execução (progresso, log ao vivo, cancelamento entre etapas) e Releases (rollback).
- 80×24, `NO_COLOR`, ajuda no `?`, confirmação digitando o ambiente; o deploy recusa se o plano recalculado sob o lock diferir do mostrado.
