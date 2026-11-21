# Descoberta — sistema-alvo do madeploy

- **Data:** 2026-11-18
- **Alvo:** `~/Projetos/sisvita_2_0/legacy/` (SisVita, MadBuilder 4 / Adianti). Caminhos abaixo são relativos a essa pasta.
- **Fontes:** leitura do código (sem executar nada) + documentação já produzida em `~/Projetos/sisvita_2_0/docs/` (citada como `sv:docs/...`). Não houve log de deploy do MadBuilder nem acesso ao servidor.
- **Segredos:** nenhum valor de `app/config/*` foi copiado; só nomes de seções e chaves.

---

## 0. Antes de tudo: duas premissas do prompt não se confirmam

1. **O código PHP não está em nenhum repositório Git.** `legacy/` está no `.gitignore` do monorepo (`sisvita_2_0/.gitignore`) e não tem `.git` próprio. O `git diff <commit implantado>..HEAD` da §5.2 só funciona depois de criar esse repositório, e o commit inicial precisa ser **o que está em produção hoje**, não o snapshot local (que pode divergir).
2. **O plano do projeto é congelar o legado, não continuar evoluindo.** O ADR `sv:docs/adr/0017-virada-unica.md` diz: "O legado permanece intocado até o desligamento", e a substituição pelo NestJS + Next.js é "um evento único". O README do monorepo reforça: "`legacy/` é somente leitura". A ferramenta pressupõe o contrário: agentes de IA editando o PHP e publicando com frequência.

As duas premissas mudam o tamanho certo do madeploy. Ver Q1 em `open-questions.md` e a recomendação em `plan.md` §1.

---

## 1. Árvore de diretórios e papel

| Caminho | Papel | Deploy? |
|---|---|---|
| `index.php`, `engine.php`, `init.php` | Entrada web; `engine.php:39-67` decide permissão | sim |
| `rest.php`, `mobile.php`, `MadRestServer.php`, `soap.php.dist` | Entradas REST/mobile/API | sim |
| `cmd.php` | CLI (`PHP_SAPI !== 'cli'` → die) | sim |
| `download.php` | Entrega arquivos para usuário logado | sim |
| `db-mad-manager.php` | Gerenciador de banco do MadBuilder (só login `admin`) | sim (ver risco R4 do plano) |
| `install.php`, `install.html` | Instalador; bloqueado se já instalado | sim |
| `ClicksignWebhook.php` | Webhook sem autenticação | sim |
| `.htaccess` | 95 `RewriteRule` geradas por página + rotas REST/API | sim |
| `menu.xml`, `menu-*.xml`, `top_menu.xml` | Menus (117 `<action>` em `menu.xml`) | sim |
| `manifest.json` | Manifesto PWA ("Adianti Template") | sim |
| `app/control/**` | 370 arquivos, 365 classes de tela (`extends TPage/TWindow/...`) | sim |
| `app/model`, `app/service`, `app/lib`, `app/view`, `app/controller`, `app/middleware`, `app/routes` | Código | sim |
| `app/templates`, `app/resources`, `app/documents/*.html`, `app/reports`, `app/forms`, `app/drawings`, `app/template_evolucao` | Templates e recursos lidos em runtime | sim |
| `app/images/` | Imagens estáticas; `photos/` recebe upload | sim, menos `photos/` |
| `app/config/` | Conexões e configuração (ver §3) | **nunca**, exceto `framework_hashes.php` |
| `app/database/` | Scripts SQL de referência do template + `*.db` SQLite do template | ver §3 |
| `app/output/` | PDFs/planilhas gerados | **nunca** |
| `tmp/` | Uploads temporários, barcode/QR, `update_lib` | **nunca** |
| `files/documents/{id}/` | Anexos de usuário | **nunca** |
| `lib/` (19 MB) | Adianti, MadBuilder (`lib/mad`), bootstrap, JS/CSS | sim (código do framework) |
| `vendor/` (16 MB) | Composer | ver §5 |
| `composer.json`, `composer.lock` | Dependências | sim |

## 2. Conexões de banco

Quatro arquivos `app/config/*.php`, todos `'type' => "mysql"`:

| Conexão | Arquivo | Banco | Fonte |
|---|---|---|---|
| `sisvita` | `app/config/sisvita.php` | `sisvita` (127 tabelas, 15 views, 2 triggers, 321 índices) | `sv:docs/legacy/production-schema.md` §1 |
| `permission` | `app/config/permission.php` | configurado como `sisvita_permission`, mas as tabelas `system_*` com dados estão no dump de `sisvita` | `sv:docs/legacy/local-environment.md` §3 — **não resolvido** (Q4) |
| `log` | `app/config/log.php` | `sisvita_log` (banco separado, 5 tabelas) | `production-schema.md` §7 |
| `communication` | `app/config/communication.php` | não confirmado | `production-schema.md` §10 |

Produção: **MySQL 8.0.45**, InnoDB, `utf8mb3_unicode_ci` em todas as tabelas.

Fatos do esquema que afetam o diff:

- Nomes de FK em produção têm sufixo hex gerado (`fk_..._2911462e0430bdceea`). O script de referência `app/database/sisvita-mysql.sql` tem outros nomes, nenhum índice secundário (produção tem 321) e uma constraint duplicada que o torna não executável (`production-schema.md` §2, §9). **O script de referência não serve como fonte da verdade; o `db pull` de produção é obrigatório.**
- Views dependem umas das outras fora da ordem alfabética (`vw_faturamento` → `vw_locacao_equipamento`); o dump padrão não restaura (`production-schema.md` §12).
- 2 triggers `BEFORE INSERT/UPDATE` em `tb_atendimento` (`production-schema.md` §3).
- `system_*` **sem** AUTO_INCREMENT em produção. Existe migração pronta, `sv:ops/schema/001-auto-increment-system.sql`, aguardando janela (ADR `0015`).
- Volume: `tb_atendimento` tem 590.564 linhas; o resto é pequeno (`production-schema.md` §11). É o único candidato real ao aviso de "tabela grande".
- Dinheiro em `double` (73 colunas). Qualquer `ALTER` de tipo aqui é perda de precisão.

## 3. Graváveis, sensíveis e gerados em runtime

**Graváveis pelo servidor web** (referências em `app/**/*.php`):

| Diretório | Evidência | Observação |
|---|---|---|
| `app/output/` | 112 arquivos, ~460 referências | nunca é limpo (`sv:docs/000-resumo-executivo.md`, gargalo 8) |
| `tmp/` | 24 arquivos | inclui `tmp/update_lib.tar.gz` (atualizador do framework) |
| `files/documents/{id}/` | 3 arquivos | anexos |
| `app/images/photos/` | `app/control/admin/SystemProfileForm.php:139` | foto de perfil `<login>.jpg` |
| `app/database/` | `app/control/builder/BuilderPageBatchUpdate.php:94` cria `app/database/.cache.db` (SQLite) | só as ferramentas do MadBuilder usam (§4.3) |
| raiz do projeto | `BuilderMenuUpdate.php:55-60` grava `menu*.xml` na raiz; `BuilderPageBatchUpdate.php:457-463` grava qualquer arquivo e cria diretórios `0777` | **só se as ferramentas do MadBuilder continuarem ativas** |

`app/database/{communication,log,permission}.db` existem no snapshot, mas as 4 conexões são MySQL; não há indício de SQLite em produção (Q6).

**Sensíveis (nunca sobrescrever, nunca logar):**

- `app/config/application.ini`: seções `[builder]` (`manager_url` + token do MadBuilder), `[general]` (`rest_key`, `token`, `seed`, `debug`, `application_version`), `[permission]`, `[user_public_pages]`.
- `app/config/{sisvita,permission,log,communication}.php`: credenciais de banco.
- `app/config/sisvita.json`: lido por `app/service/ConfigService.php` (chaves Clicksign, segundo `sv:docs/adr/0012-segredos.md`).
- `app/config/install.ini` (senha do instalador), `app/config/installed.ini` (trava de reinstalação).
- `.env`: não existe no legado.

`app/config/framework_hashes.php` **não** é configuração: é a lista de sha512 dos arquivos do framework, usada por `SystemFrameworkUpdate`. Vai com o código.

**Gerados em runtime:** sessões PHP (handler nativo em `session_save_path()`, fora do projeto — `sv:docs/legacy/auth-permissions.md` §2.4), `app/output/*`, `tmp/*`, `app/database/.cache.db`, fotos.

## 4. Como uma tela nova entra no controle de acesso e no menu

### 4.1 Decisão de acesso

`engine.php:39-67` só executa a classe se `class` estiver em `$_SESSION['sisvita']['programs']`, nas 33 permissões padrão (`engine.php:74-110`) ou nas classes públicas (`application.ini:65-81`). `programs` é calculado **no login** a partir de `system_group_program` + `system_user_program` (`app/model/admin/SystemUsers.php:694-712`). Por isso **uma tela nova só aparece depois de novo login**, ou de `LoginForm::reloadPermissions()`.

O menu é filtrado por outra regra, `SystemPermission::checkPermission` (só olha classe): item cujo controller não está em `programs` some do menu.

### 4.2 Tabelas

| Tabela | Colunas | Uso |
|---|---|---|
| `system_program` | `id, name, controller, actions` | uma linha por classe; `actions` JSON `[{"name","action"}]` ou `NULL` (= todas liberadas) |
| `system_group_program` | `id, system_group_id, system_program_id, actions` | grupo ↔ programa |
| `system_user_program` | `id, system_user_id, system_program_id` | concessão direta |
| `system_group` | `id, name, uuid` | 15 grupos em produção |

Produção: 398 programas e 15 grupos (`local-environment.md` §3). Código: 365 classes de tela. A diferença (programa sem classe e classe sem programa) é o primeiro relatório útil do `sync programs`.

### 4.3 O que o MadBuilder faz hoje (lido no código do app, não no MadBuilder)

O app traz embutidas as ferramentas que o MadBuilder usa para atualizar o servidor. Todas exigem o login literal `admin` (`app/service/builder/BuilderPermissionService.php:5-11`) e **puxam conteúdo da nuvem do MadBuilder** via `[builder] manager_url` + token (`app/service/builder/BuilderPageService.php:31-60`, com `verify_peer => false`):

| Ferramenta | O que faz | Evidência |
|---|---|---|
| Atualizar permissões | `getPermissionsV1` → acha o grupo por `uuid` (senão por nome) e cria se faltar; **se `builder_permission_group`, apaga todos os vínculos do grupo**; para cada `controller => name`, cria `system_program` se faltar e vincula ao grupo; recarrega permissões da sessão | `app/control/builder/BuilderPermissionUpdate.php:51-139`; apagar em `:102-105`; criar em `:111-127` |
| Atualizar menu | `getMenus` → sobrescreve 6 arquivos XML na raiz | `app/control/builder/BuilderMenuUpdate.php:47-60` |
| Atualizar códigos | `getAllCodes` → compara cada arquivo pelo conteúdo, marca Novo/Alterado, detecta conflito por nome duplicado e grava com `mkdir 0777` | `app/control/builder/BuilderPageBatchUpdate.php:101-164`, `:406-470` |
| Database merge | compara tabelas, colunas e views do banco do projeto com as do builder, gera comandos, o usuário confirma e eles rodam **um a um** com `$conn->query($command)` | `app/control/builder/BuilderConfirmCommandsDiffForm.php:88-114`; `app/service/builder/database/BuilderDatabaseService.php` |

Consequências para a ferramenta:

- Em produção, `system_program.id` é gerado pelo Adianti (`TRecord::store` → `MAX(id)+1`), porque a tabela não tem AUTO_INCREMENT. Um `INSERT ... WHERE NOT EXISTS` sem `id` grava `id = 0` (`local-environment.md` §1). **O SQL do `sync programs` precisa calcular o id, ou depender da migração `001-auto-increment-system.sql`.**
- O MadBuilder **não** preenche `system_program.actions` ao registrar (`BuilderPermissionUpdate.php:117-120`): tela nova nasce com todas as ações liberadas. O madeploy deve fazer o mesmo.
- Esse caminho do MadBuilder **não** gera `RewriteRule` no `.htaccess`; as 95 regras existentes vêm do template. Telas novas funcionam por `index.php?class=X`.
- O deploy por SSH (backup datado, 5 últimos, log ao vivo, preservar `.env`) roda **no MadBuilder desktop/nuvem**, não neste código. Sem um log, não dá para confirmar o comportamento (Q2).
- Enquanto `[builder] token` estiver em produção, qualquer `admin` pode reescrever código, menu e permissões a partir da nuvem. Isso gera **drift** que o madeploy precisa detectar (risco R2 do plano).

## 5. Composer e `vendor/`

- 14 dependências, **7 delas `dev-master`** (`adianti/plugins`, `adianti/pdfdesigner`, `pablodalloglio/*` ×4, `madbuild3r/pquery`). `composer install` no servidor resolve pelo `composer.lock`, mas exige rede até esses repositórios e o binário `composer` lá.
- `knplabs/knp-snappy` depende do binário `wkhtmltopdf` no servidor.
- `vendor/` existe localmente (16 MB) e não está versionado em lugar nenhum (o legado inteiro não está).
- Nenhuma restrição `php` no `composer.json`. O ambiente local roda PHP 8.1; produção é PHP-FPM 8.2 (informado). `php -l` com 8.1 não pega tudo o que quebra em 8.2.

## 6. Servidor (informado, não verificado)

- MySQL 8, usuário `deploy`, grupo `www-data`, PHP-FPM 8.2.
- Apache ou nginx: desconhecido. O `.htaccess` com 95 regras só funciona em Apache (ou com tradução manual para nginx). Isso afeta o healthcheck e a troca de DocumentRoot (Q3).
- DocumentRoot, caminho da aplicação e domínio: não informados.
