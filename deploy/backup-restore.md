# Backup and restore

All persistent state is in the Compose `pasta-data` volume: `pasta.sqlite`, its SQLite WAL files, and `objects/`. Back up and restore this volume as one unit. The Caddy volumes contain only TLS state and are outside the application data backup.

## Backup

Run these commands from the directory containing `compose.yaml`. Set `VOLUME` to the volume mounted at `/data`; discover its exact name with `docker volume ls`.

```sh
export VOLUME=<pasta-data-volume>
mkdir -p backups
# Stop writes before copying SQLite and its WAL files.
docker compose stop pasta
docker run --rm -v "$VOLUME:/data:ro" -v "$PWD/backups:/backup" busybox:1.37 \
  tar -C /data -czf /backup/pasta-$(date +%Y%m%dT%H%M%SZ).tar.gz pasta.sqlite pasta.sqlite-wal pasta.sqlite-shm objects
# Resume the service only after tar exits successfully.
docker compose start pasta
```

If no WAL or SHM file exists, omit that file name from the `tar` command. Store the archive encrypted and outside the VPS. Test every backup with the restore procedure below.

## Restore on a clean instance

```sh
export VOLUME=<new-pasta-data-volume>
export BACKUP=/absolute/path/pasta-YYYYMMDDTHHMMSSZ.tar.gz
docker compose down
docker volume create "$VOLUME"
docker run --rm -v "$VOLUME:/data" -v "$(dirname "$BACKUP"):/backup:ro" busybox:1.37 \
  sh -c 'rm -rf /data/* && tar -C /data -xzf "/backup/$(basename "$BACKUP")" && chown -R 65532:65532 /data'
docker compose up -d
curl --fail --silent --show-error https://${PASTA_DOMAIN:-localhost}/readyz
```

Verify a known publication can be retrieved with its original full URL, then verify the same archive on a second clean volume before treating a release as recoverable.
