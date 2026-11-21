# Perguntas em aberto

Cada pergunta traz o padrão seguro adotado enquanto não houver resposta.

| # | Pergunta | Por que importa | Padrão enquanto não houver resposta | Bloqueia |
|---|---|---|---|---|
| Q1 | O madeploy é só para o SisVita até a virada para o NestJS (ADR `0017`: "o legado permanece intocado"), ou para vários projetos MadBuilder? | Define se F2 (releases) e F5 (TUI) valem o custo | Fazer F1 → F3 → F4; F2 e F5 ficam para depois | ordem das fases |
| Q2 | Existe um log do deploy SSH do MadBuilder? O que ele faz exatamente (backup de quê, onde, como preserva o `.env`, o que exclui)? | O prompt exige "no mínimo igualar", e esse comportamento não está no código do app | Backup dos arquivos tocados + 5 retidos + log ao vivo | não |
| Q3 | **Respondida: Apache, `/var/www`.** Servidor web: Apache ou nginx? Qual o caminho da aplicação e do DocumentRoot? Há acesso ao vhost? | O `.htaccess` (95 regras) só vale no Apache; trocar o DocumentRoot é pré-requisito do modo `releases` | `inplace`; container de teste com Apache | **F1** (container e healthcheck) |
| Q4 | A conexão `permission` aponta para `sisvita_permission` ou para `sisvita`? Onde estão as tabelas `system_*` vivas? (pergunta #20 do sisvita_2_0) | O `sync programs` pode escrever no banco errado | Ler o `permission.php` de produção e mostrar banco + host no plano; recusar se `system_program`/`system_group` não existirem lá | F4 |
| Q5 | `vendor/` vai no pacote (montado localmente) ou é instalado no servidor? Há `composer` e acesso de rede no servidor? | 7 dependências `dev-master` | Vai no pacote (ADR-06) | não |
| Q6 | Algum SQLite (`app/database/*.db`) é usado em produção? | Decide se `app/database` é gravável/compartilhado | Não; `app/database` é tratado como código, exceto `.cache.db` | não |
| Q7 | O menu XML deve ser gerado automaticamente para telas novas, ou basta validar? | Gerar exige inventar rótulo, ícone e posição | Só validar (ADR-10) | não |
| Q8 | **Respondida: `sistema.buonavitahomecare.com.br`, marcador `LoginForm`.** Domínio e marcador do healthcheck | O Adianti responde 200 mesmo quando renderiza um erro | Exigir `healthcheck.url` e `expect_body` no config | **F1** |
| Q9 | A migração `001-auto-increment-system.sql` (ADR `0015`) já foi aplicada em produção? | Muda o SQL do `sync programs` | Detectar em runtime (`information_schema.COLUMNS.EXTRA`) | não |
| Q10 | O token `[builder]` pode ser removido de produção depois de adotar o madeploy? | Sem isso, a nuvem do MadBuilder continua gerando drift | O `doctor` alerta e não altera nada | não |
| Q11 | Quais grupos recebem tela nova automaticamente: só `Admin`? | São 15 grupos em produção | Só `Admin`, configurável | não |
| Q12 | Quantas releases/backups manter? Onde guardar os dumps do banco (o servidor tem disco para um dump com `tb_atendimento`)? | Retenção e espaço | 5; `df` checado antes do backup | não |
| Q13 | O `sudo` para `systemctl reload php8.2-fpm` já está liberado para o usuário `deploy`? | O reload é necessário para limpar o OPcache (confirmado no teste: sem ele, a versão antiga continua sendo servida por até `revalidate_freq` segundos) | O `init` gera o trecho de `sudoers`; sem ele, o hook falha e o deploy é revertido | não |
| Q14 | O `mysqldump` do servidor é MySQL ou MariaDB? A conexão usa TLS? | O cliente MariaDB recusa o certificado autoassinado do MySQL 8 (erro 2026) | `databases.<conn>.dump_options: [--skip-ssl-verify-server-cert]` só se o `init` mostrar esse erro | não |
| Q15 | Há `php` local na máquina de quem faz o deploy? | Sem ele, só o lint remoto roda (é o obrigatório) | Avisa e segue | não |
