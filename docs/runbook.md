# Runbook — reverter à mão

Tudo o que o madeploy faz fica em `<path>/.madeploy/` no servidor. Os passos abaixo funcionam **sem** o madeploy, só com SSH.

## 1. Reverter o código

Automático: `madeploy rollback production` (último deploy) ou `--to <id>` (`madeploy releases production` lista os ids).

À mão, para o deploy `<id>` (do mais novo para o mais antigo, um por vez):

```sh
cd /var/www/sisvita
B=.madeploy/backups/<id>
xargs -r rm -f -- < $B/added.list              # remove o que o deploy criou
[ -f $B/files.tar.gz ] && tar -xzf $B/files.tar.gz --no-same-owner   # devolve o que ele alterou/removeu
cp -p $B/manifest.json $B/state.json .madeploy/ 2>/dev/null
mv $B .madeploy/backups/reverted-<id>           # não aplicar duas vezes
sudo -n systemctl reload php8.2-fpm             # limpa o OPcache — sem isto o PHP continua servindo a versão antiga
```

Com `strategy: releases`, o código anterior está inteiro em `releases/`:

```sh
cd /var/www/sisvita
ls releases/                                     # ou: madeploy releases production
ln -sfn releases/<id> current.tmp && mv -T current.tmp current
cp -p .madeploy/releases/<id>/manifest.json .madeploy/releases/<id>/state.json .madeploy/
sudo -n systemctl reload php8.2-fpm
```

## 2. Reverter o banco

**O madeploy nunca reverte o banco sozinho.** Antes de cada migração ele guarda o dump das tabelas afetadas (ou do banco inteiro, com `backup: full`) em `.madeploy/backups/<id>/db-<conexão>.sql.gz` (no deploy) ou `.madeploy/db-backups/<id>/` (no `db apply`).

```sh
# 1) pare a escrita: modo manutenção ou PHP-FPM parado
# 2) restaure (o dump tem DROP TABLE + CREATE + INSERT das tabelas salvas)
gunzip -c .madeploy/backups/<id>/db-sisvita.sql.gz | mysql --defaults-extra-file=~/.my.cnf sisvita
# 3) remova o registro da migração revertida, senão ela não roda de novo
mysql sisvita -e "DELETE FROM madeploy_migrations WHERE id = 'migrations/<arquivo>.sql'"
```

Restaurar um dump de tabela **apaga o que foi escrito nela depois do backup**. Em `tb_atendimento` (590 mil linhas, escrita contínua), prefira uma migração de correção (passo 4) a restaurar.

## 3. Migração que parou no meio

DDL do MySQL não é transacional: cada instrução é registrada ao terminar. `madeploy db status production` mostra `partial` com `N de M`. A mensagem de erro do deploy lista o que foi e o que não foi aplicado, e o SQL que falhou.

- Corrigiu o problema e quer continuar da instrução que falhou: `madeploy db apply production --resume`.
- Terminou à mão o que faltava: `madeploy db resolve production --conn sisvita migrations/<arquivo>.sql`.
- Nunca edite uma migração já aplicada (o checksum muda e o deploy recusa). Crie outra.

## 4. Expand/contract (padrão `migrate: before`)

As migrações rodam **antes** dos arquivos trocarem. Durante esses segundos, o código antigo roda sobre o esquema novo. Portanto:

1. **Expand** (deploy 1): só adições compatíveis com o código antigo: coluna nova `NULL` ou com `DEFAULT`, tabela nova, índice novo.
2. Código novo passa a usar o que foi adicionado (mesmo deploy ou o seguinte).
3. **Contract** (deploy posterior, quando nada mais usa o antigo): `DROP COLUMN`, `NOT NULL`, renomeações, com `--allow-destructive`.

`ALTER` em tabela acima de `large_table_rows` é bloqueado: rode com `gh-ost` ou `pt-online-schema-change` fora do madeploy e marque a migração com `db resolve`.

## 5. Lock preso

Um deploy morto (rede caiu, Ctrl-C duas vezes) deixa `.madeploy/deploy.lock/`. Confira se não há deploy rodando (`madeploy logs production --follow`, `ps` no servidor) e então `madeploy unlock production`.

## 6. Drift

Alguém alterou arquivo direto em produção (ou o atualizador do MadBuilder rodou). O deploy para com código 2 e lista os arquivos.

- A mudança deve ficar: `madeploy drift pull production`, revise com `git diff`, faça commit e faça o deploy.
- A mudança deve sumir: `madeploy deploy production --on-drift=overwrite` (o backup guarda a versão manual).
- Deixar como está por enquanto: `--on-drift=ignore` (o arquivo não é tocado e continua aparecendo como drift).
