# Pasta

Pasta is a self-hosted service for encrypted text and file sharing. It encrypts
content in the browser before upload, then stores ciphertext, an encrypted
manifest, and metadata using SQLite and the filesystem.

## Features

- Browser-side encryption for text and files.
- Configurable expiry and delete links.
- Optional one-time reads.
- SQLite metadata and filesystem object storage.

## Security model

The decryption key is kept in the fragment of the sharing URL, so it is not
sent in HTTP requests. Anyone with the complete sharing URL can decrypt the
publication.

Pasta is not an anonymity service. The hosting provider and reverse proxy can
observe network metadata and object sizes.

## Requirements

- Go 1.26 or later for a local run.
- A modern browser with the Web Crypto API.
- Docker Compose for the container deployment.

## Local run

```sh
go test ./...
go run .
```

The server listens on `:8080` and writes state to `./data` by default.

## Docker deployment

```sh
export PASTA_DOMAIN=paste.example.com
docker compose -f deploy/compose.yaml up --build -d
curl --fail https://"$PASTA_DOMAIN"/readyz
```

Persistent data is kept in the Compose `pasta-data` volume. Do not commit
volumes, Caddy state, certificates, SQLite databases, or user ciphertext to
Git or release artifacts. See [backup and restore instructions](deploy/backup-restore.md).

## Configuration

| Variable | Default | Purpose |
| --- | --- | --- |
| `PASTA_ADDR` | `:8080` | HTTP listen address. |
| `PASTA_DATA_DIR` | `./data` | Directory for SQLite metadata and encrypted objects. |
| `PASTA_MAX_FILES` | `10` | Maximum files in one publication. |
| `PASTA_MAX_FILE_BYTES` | `52428800` (50 MiB) | Maximum size of an individual file. |
| `PASTA_MAX_PASTE_BYTES` | `104857600` (100 MiB) | Maximum total encrypted upload size. |
| `PASTA_MAX_TEXT_BYTES` | `1048576` (1 MiB) | Maximum plaintext text length. |
| `PASTA_TTL_HOURS` | `1,24,168,720` | Allowed expiry durations, in hours. |
| `PASTA_UPLOAD_TTL` | `1h` | Time allowed to finish an upload. |
| `PASTA_READ_TIMEOUT` | `2m` | Maximum request read duration. |
| `PASTA_WRITE_TIMEOUT` | `2m` | Maximum request write duration. |
