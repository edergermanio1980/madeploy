# Plano — madeploy

- **Data:** 2026-11-18 · **Status:** todas as fases (F1–F6) implementadas (§7)
- **Base:** `docs/discovery.md`, `docs/open-questions.md`.
- **Valores confirmados:** projeto `~/Projetos/sisvita_2_0/legacy`, MySQL 8 (8.0.45), `deploy:www-data`, PHP-FPM 8.2. **Pendentes:** caminho no servidor, domínio, nº de releases (padrão 5), `vendor/` (proposta em ADR-06).

---

## 1. Recomendação antes do plano

A descoberta encontrou um conflito de escopo (discovery §0): o SisVita legado está **congelado** até a virada para o NestJS (ADR `0017` do sisvita_2_0), e o código PHP **nem está em Git**. A ferramenta completa (F0–F6) tem tamanho de produto. Se o legado só vai receber correções pontuais até ser desligado, a maior parte do valor está em três coisas:

1. **Deploy de arquivos in-place com backup, lock, drift e log** (F1). Isso sozinho substitui o deploy do MadBuilder.
2. **Migrações de banco versionadas e auditáveis** (F3). Servem também ao sistema novo, que usa o mesmo banco e já tem uma migração esperando janela (`ops/schema/001-auto-increment-system.sql`).
3. **`sync programs`** (F4), porque é o que o MadBuilder fazia escondido.

**Proposta:** aprovar F1 → F3 → F4, nesta ordem, e decidir F2 (releases atômicas) e F5 (TUI) quando a F1 estiver rodando contra o container. As duas são caras e não ampliam o que dá para fazer; só melhoram o conforto e o tempo de reversão. Se o madeploy também for servir a outros projetos MadBuilder (ex.: `~/Projetos/buona`), F2 e F5 se pagam e entram. É a pergunta Q1.

O resto do documento cobre o escopo completo do prompt e marca o que é corte opcional.

---

## 2. Arquitetura

```
cmd/madeploy/            main + árvore Cobra (só parsing de flags e saída)
internal/
  config/                madeploy.yaml → struct validada (schema JSON embutido)
  remote/                cliente SSH/SFTP, exec com argv escapado, túnel TCP, lock
  files/                 git diff, manifesto sha256, ignore, pacote tar.gz
  plan/                  monta o Plan (arquivos, schema, dados, programas, perms, hooks)
  deploy/                executa o Plan em etapas; emite eventos (Step, Log, Done, Err)
  db/                    SchemaDiffer (sqldef), classificação de risco, migrações, backup
  programs/              varre app/control, compara com system_program, gera SQL
  perms/                 calcula e aplica dono/modo/ACL
  ui/                    saída texto / JSON / TUI — todos consomem os mesmos eventos
```

Princípio: **`plan` é puro e testável** (entrada: estado local + estado remoto já lido; saída: `Plan`). `deploy` é o único pacote que altera produção, e só executa um `Plan` que já foi mostrado. A TUI e o `--json` consomem o canal de eventos de `deploy` e não têm lógica própria.

Estado remoto em `<base>/.madeploy/`: `state.json`, `manifest.json`, `deploy.lock`, `logs/`, `backups/`, `staging/`.

---

## 3. ADRs curtas

**ADR-01 — Go + Cobra; Bubble Tea v2 só na F5.** Conforme o prompt. Até a F5, a saída é texto + `--json`. A versão do Bubble Tea fica fixada no `go.mod` quando a F5 começar.

**ADR-02 — SSH nativo em Go.** `golang.org/x/crypto/ssh` + `ssh/agent` + `ssh/knownhosts` (host key obrigatório, nunca `InsecureIgnoreHostKey`), `github.com/pkg/sftp` e `github.com/kevinburke/ssh_config` para ler `~/.ssh/config` (`HostName`, `User`, `Port`, `IdentityFile`, `ProxyJump`). O jump host é feito discando pelo cliente do bastião. *Alternativa rejeitada:* chamar o `ssh` do sistema. Ganharia ControlMaster e todo o `ssh_config` de graça, mas perderia o controle do túnel e dos erros e quebraria o "binário único". Todo comando remoto é montado como `[]string`, com cada argumento escapado; nenhuma concatenação de entrada.

**ADR-03 — Diff de esquema com sqldef (`mysqldef`), atrás de `SchemaDiffer`.**
- sqldef: MIT; gerencia tabelas, colunas, índices, FKs, CHECK, **views e triggers** (o SisVita tem 15 views e 2 triggers); **não gera DROP por padrão** (`--enable-drop` libera e mapeia para `--allow-destructive`); tem `--export` (= `db pull`), `--dry-run` e `--check` (detecção de drift). Procedures não são suportadas, mas o SisVita não tem nenhuma.
- Atlas community: exige dev database (Docker) para normalizar; peso extra sem ganho aqui.
- Skeema: views e triggers só na versão paga → descartado.
- *Riscos a provar no spike da F3:* (a) a API Go do sqldef não é documentada como biblioteca; se não for estável, `SchemaDiffer` chama o binário `mysqldef` com versão e checksum fixados; (b) dependência entre views fora de ordem (discovery §2); (c) FKs com nome hex: como o `db pull` exporta de produção, os nomes batem e o diff não deve gerar ruído; (d) FKs compostas ("may not work for now", segundo a documentação do sqldef).
- O sqldef roda **na máquina local**, conectado ao MySQL de produção pelo túnel SSH (ADR-07). Nada é instalado no servidor.

**ADR-04 — Começar com a estratégia `inplace`; `releases` fica para a F2.** Mudar o DocumentRoot exige acesso ao vhost, que ainda não conhecemos (Q3). No `inplace`: backup `tar.gz` só dos arquivos que serão tocados (não da árvore inteira), extração do pacote e remoções confirmadas. No `releases`, os caminhos relativos do Adianti (`app/output` e `tmp` são relativos ao cwd) funcionam por symlink para `shared/`, mas o reload do PHP-FPM depois da troca é obrigatório, por causa do cache de realpath/OPcache.

**ADR-05 — Fonte da verdade do código: repositório Git novo, semeado a partir de produção.** `madeploy init` baixa a árvore de produção (sem `shared` e ignorados), o operador cria o commit inicial, e esse commit vira o `state.json` inicial junto com o manifesto. Assim o primeiro deploy é um no-op verificável. O snapshot local em `sisvita_2_0/legacy` não serve de base (pode divergir de produção).

**ADR-06 — `vendor/` vai no pacote, montado localmente a partir do `composer.lock`.** Motivo: 7 dependências `dev-master` e nenhuma garantia de `composer` + rede no servidor. `composer install --no-dev -o` roda na máquina do operador (ou no CI), e o `vendor/` resultante entra no manifesto como qualquer arquivo; só o que mudou é enviado. O hook remoto `composer install` continua disponível, mas desligado por padrão. Confirmar em Q5.

**ADR-07 — Banco só pelo túnel SSH.** `ssh.Client.Dial("tcp", "127.0.0.1:3306")` exposto numa porta local efêmera. As credenciais vêm do `app/config/<conn>.php` **de produção**, lido via SSH e interpretado em Go (o array `return [...]`, sem executar PHP local), ou de variável de ambiente. Senha nunca aparece em log, plano ou JSON.

**ADR-08 — Migrações em `madeploy_migrations`, uma instrução por vez.** Uma tabela por conexão: `id, file, checksum, applied_at, duration_ms, operator, release, statements_done`. DDL no MySQL não é transacional, então cada instrução é registrada logo depois de aplicada. Se o processo cair no meio, o `status` mostra `statements_done/total` e o próximo `apply` recusa até uma decisão manual (`db resolve`). Migração já aplicada com checksum diferente = erro.

**ADR-09 — `sync programs` reproduz o MadBuilder, mas não apaga nada.** Igual a `BuilderPermissionUpdate.php:111-127`: cria `system_program(name, controller)` com `actions = NULL` e vincula aos grupos configurados (padrão `Admin`). Diferença deliberada: **nunca** faz o `DELETE` de vínculos de `:102-105`. Id: `INSERT ... SELECT COALESCE(MAX(id),0)+1 ... WHERE NOT EXISTS` dentro de transação com `SELECT ... FOR UPDATE`, porque `system_*` não tem AUTO_INCREMENT em produção; se a migração `001-auto-increment-system` já tiver sido aplicada, o SQL omite o id. O `name` padrão é o nome da classe, e o `madeploy.yaml` aceita um mapa `controller → name`.

**ADR-10 — Menu XML: validar, não gerar.** O `menu.xml` é código versionado, editado por agente ou humano. O madeploy **não** inventa rótulo, ícone e posição. O `sync programs` reporta: (a) `<action>` do menu sem classe, (b) `<action>` sem `system_program`, (c) tela nova fora de qualquer menu (aviso, não erro). Isso diverge do prompt §5.4 ("atualização do menu XML") porque não dá para inferir rótulo, ícone e posição sem errar. Confirmar em Q7.

**ADR-11 — Desligar o atualizador da nuvem do MadBuilder em produção.** Enquanto `application.ini [builder]` tiver token, qualquer `admin` pode reescrever código, menu e permissões pela nuvem (discovery §4.3), e o madeploy vai acusar drift depois de cada uso. O `doctor` alerta quando o token estiver presente; a remoção é manual e fica documentada no runbook.

**ADR-12 — `php -l` duas vezes.** Local (rápido, aborta antes de enviar) e **remoto, no PHP 8.2 de produção**, nos arquivos alterados já extraídos no staging, antes de qualquer troca. O ambiente local roda 8.1, então só o lint remoto garante compatibilidade com o 8.2.

---

## 4. Fluxo de `deploy` (estratégia `inplace`)

1. Pré-checagens: árvore Git limpa, branch permitida, SSH + host key, `df` remoto, `php -v` remoto igual ao esperado, lock livre.
2. Lock remoto: `mkdir .madeploy/deploy.lock` (atômico), com operador, host, pid e data.
3. Plano: `git diff --name-status <state.commit>..HEAD` ∪ manifesto local × remoto → A/M/D/R + **drift** (sha remoto ≠ sha do commit implantado). Drift bloqueia até `--on-drift=overwrite|ignore` ou `madeploy drift pull` (traz o arquivo para o repositório).
4. Mostra o plano; `--dry-run` para aqui; sem TTY, exige `--yes`.
5. `php -l` local → pacote `tar.gz` → upload SFTP para `.madeploy/staging/` → conferência do sha256 remoto.
6. Backup: `tar.gz` dos arquivos M/D/R atuais em `.madeploy/backups/<ts>/files.tar.gz` (+ dump do banco, se houver migração).
7. `php -l` remoto no staging → migrações (padrão: antes da troca; expand/contract documentado no runbook) → extração sobre a árvore → remoção dos D (com confirmação) → permissões só no delta + `writable`.
8. Hooks `after_switch` (reload do PHP-FPM via `sudo` pré-autorizado) → healthcheck.
9. Se o healthcheck falhar: restaura `files.tar.gz`, roda os hooks de novo, avisa que **o banco não foi revertido** e mostra o comando de restauração do dump.
10. Grava `state.json` + manifesto + log JSON, rotaciona backups (`keep: 5`) e libera o lock. Erro em qualquer etapa: libera o lock e imprime o que já foi feito e o comando de reversão.

Healthcheck padrão: `GET <url>/index.php?class=LoginForm` → 200 **e** corpo contendo um marcador configurável (o Adianti responde 200 mesmo quando renderiza um erro de PHP). Q8.

---

## 5. Riscos

| # | Risco | Mitigação |
|---|---|---|
| R1 | Repositório semeado a partir do snapshot local, e não de produção; o primeiro deploy sobrescreve correções feitas lá | ADR-05: semente obrigatória a partir de produção; o primeiro deploy precisa ser no-op |
| R2 | As ferramentas do MadBuilder em produção reescrevem arquivos e permissões (`mkdir 0777`, `DELETE` de vínculos) | ADR-11 + detecção de drift + `doctor` |
| R3 | `INSERT` em `system_program` sem id grava `id=0` | ADR-09 |
| R4 | Rotas abertas que o deploy não deve "consertar" sozinho (`db-mad-manager.php`, `/api/internal/database-manager`, webhook sem HMAC) | Fora do escopo; o `doctor` lista como aviso e não altera nada |
| R5 | `ALTER` em `tb_atendimento` (590 mil linhas) trava a agenda | Limite de linhas configurável → bloqueia e sugere `gh-ost`/`pt-osc`; usa `ALGORITHM=INSTANT` quando o MySQL 8 aceitar |
| R6 | Views com dependência fora de ordem quebram a aplicação do diff | Spike na F3; aplicar views em passes, como o `ops/restore.sh` do sisvita_2_0 |
| R7 | DDL parcial (não transacional) | ADR-08 |
| R8 | A conexão `permission` aponta para `sisvita_permission`, e não para onde estão os dados | Q4; o `sync programs` usa a conexão configurada e mostra banco e host no plano |
| R9 | `.htaccess` não vale em nginx; o healthcheck dá falso positivo | Q3, Q8 |
| R10 | Segredos vazarem em log (já aconteceu: ADR `0012` do sisvita_2_0) | `app/config/**` nunca entra no plano; o parser de credenciais roda só em memória; o logger tem redator para as chaves `pass`, `password`, `token`, `key`, `secret` |

---

## 6. Fases

| Fase | Entrega | Pronto quando |
|---|---|---|
| **F1** | config + validação, SSH/known_hosts/jump, lock/unlock, manifesto, `init` (semente), `status`, `diff` (Git + drift), `deploy` inplace com backup, `php -l` local e remoto, permissões, hooks, healthcheck + restauração de arquivos, log JSON, `doctor` básico | deploy completo, drift e falha de healthcheck passando em container (SSH + PHP-FPM 8.2 + Apache + MySQL 8) |
| **F3** | túnel, `db pull/diff/apply/status`, classificação de risco + `--allow-destructive`, backup `mysqldump --single-transaction`, `madeploy_migrations`, migrações de dados | spike do sqldef verde contra o esquema real (dump de estrutura) + falha no meio de migração testada |
| **F4** | `sync programs` + validação do menu | relatório programa × classe × menu correto contra os dados reais de `system_*` |
| F2 *(decidir)* | releases + symlink `mv -T`, `rollback`, `releases`, `init --migrate-layout` (só instruções para o vhost) | rollback testado em container |
| F5 *(decidir)* | TUI sobre os eventos | 80×24, `NO_COLOR`, confirmação digitando o nome do ambiente |
| F6 | GoReleaser (darwin amd64/arm64, linux amd64/arm64), README, `docs/config.md`, `docs/runbook.md` | — |

Testes: unitários em `plan`, `files` (ignore, manifesto), `db` (classificação de risco), `perms` e `programs`; golden files para a saída do plano; integração com `testcontainers-go` desde a F1. Dados de produção **não** entram nos testes: o dump completo tem dados de paciente, e só o dump de estrutura pode virar fixture (ver `production-schema.md` §11 do sisvita_2_0).

Cada fase termina com testes verdes, `CHANGELOG.md` atualizado, demonstração em container e lista de pendências.

---

## 7. Estado da implementação (2026-11-18)

| Fase | Estado | Evidência |
|---|---|---|
| F1 | feito | `internal/deploy/deploy_test.go` (ciclo completo em diretórios temporários) + `test/integration` (SSH real) |
| F3 | feito | `internal/db/db_test.go` + `test/integration` (pull/diff/apply, destrutiva, falha no meio) |
| F4 | feito | `internal/programs/programs_test.go` + `test/integration` (id 2 no grupo Admin, sem duplicar) |
| F6 | feito | `.goreleaser.yaml`, README, `docs/config.md`, `docs/runbook.md`; build cruzado verificado |
| F2 | feito | `internal/deploy/releases_test.go` (fallbacks BSD) + `TestReleasesEndToEnd` (cp -al, mv -T, DocumentRoot trocado, volta automática, rollback) |
| F5 | feito | `internal/tui/tui_test.go` (80×24, NO_COLOR, confirmação, progresso) + TUI aberta num pseudo-terminal contra o container |

Desvios e achados durante a implementação:

- **ADR-03 resolvido:** o `SchemaDiffer` chama o binário `mysqldef` (v3.11.30) em vez da API Go, que não é documentada como biblioteca. O spike passou em MySQL 8.0 com view e trigger: `db pull` → `db diff` volta vazio. O dry-run vem envolto em `BEGIN/COMMIT`, então o diff usa `--disable-ddl-transaction`, e a tabela `madeploy_migrations` é filtrada (o sqldef tentaria criá-la ou removê-la).
- **Bug de segurança evitado pelo teste de integração:** instruções lidas de arquivo trazem o comentário `-- RISCO`, e a classificação ancorada em `^` não via o `DROP`. Agora os comentários são removidos antes da análise (`StripComments`), e há teste unitário cobrindo isso.
- **OPcache confirmado como requisito:** com `revalidate_freq=2`, restauração e rollback continuam servindo a versão antiga por até 2 s. O hook `after_switch` precisa recarregar o PHP-FPM (`docs/runbook.md`).
- **Testes de integração com o CLI do Docker, não `testcontainers-go`:** mesma cobertura, sem a dependência. Trocar é mecânico, se for exigido.
- **`dump_options`:** o cliente MariaDB recusa o certificado autoassinado do MySQL 8 por padrão (Q14).
- **Cobertura:** a unitária é ≥ 66% em `config`, `perms`, `programs` e `deploy`. `db`, `remote` e `cmd` (SSH, túnel, sqldef, CLI) são cobertos pelo teste de integração, que roda o binário e por isso não entra no `go test -cover`.

## 8. O que ainda preciso de você

1. Q1 continua valendo como decisão de uso: no SisVita congelado, `inplace` basta; `releases` exige mexer no vhost uma vez.
2. Q3 e Q8 respondidas (Apache, `/var/www`, `sistema.buonavitahomecare.com.br`) e já no template do `madeploy init`. Falta só confirmar se o PHP roda como FPM ou mod_php (hook de OPcache).
3. Os ADRs 04, 06, 09, 10 e 11 foram implementados como propostos; diga se algum precisa mudar.
