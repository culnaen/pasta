# PRD: подготовка Pasta к публичной публикации

## Цель

Опубликовать Pasta как самостоятельное self-hosted приложение на GitHub без
секретов, эксплуатационных данных и привязки к текущей инсталляции.

## Контекст

Pasta — Go-приложение для обмена текстом и файлами. Шифрование выполняется в
браузере до загрузки: сервер хранит ciphertext, зашифрованный manifest и
метаданные. Ключ находится во фрагменте URL и не отправляется в HTTP-запросах.

Публичный репозиторий должен быть новым: содержимое экспортировано без `.git`,
а старая история и недостижимые Git-объекты не публикуются.

## Не входит в задачу

- Публикация существующей Git-истории.
- Миграция production-данных, Docker volumes или TLS-сертификатов.
- Изменение протокола шифрования или API без отдельной задачи.
- Обещания анонимности или защиты сетевых метаданных.

## Требования

### 1. Чистый публичный исходник

- [x] Создана рабочая копия без `.git`.
- [x] Убедиться, что новая копия не содержит `.git`, SQLite-баз, WAL/SHM,
  runtime-каталогов, архивов бэкапов, `.env`, TLS-материалов, `graft/`,
  `.claude/`, `.mcp.json` и инструкций, относящихся только к локальному
  агентскому окружению.
- [x] Выполнить scan экспортированного дерева:
  ```sh
  gitleaks detect --source . --redact --no-git
  ```
- [x] При находке секрета: не создавать remote, отозвать затронутый credential,
  удалить данные из экспорта и повторить scan. Секреты не обнаружены.
- [x] Инициализировать новый Git-репозиторий только после успешного scan.
- [x] Перед первым push включить GitHub secret scanning и push protection.

### 2. Переносимая конфигурация развёртывания

- [x] Удалить `deploy/compose.yml` с production-specific hostname.
- [x] Оставить единственный `deploy/compose.yaml`.
- [x] Использовать только `PASTA_DOMAIN`; документация и примеры должны
  использовать `paste.example.com`.
- [x] Проверить манифест:
  ```sh
  PASTA_DOMAIN=example.test docker compose -f deploy/compose.yaml config --quiet
  ```
- [x] Не включать Docker volumes, Caddy state, сертификаты, базу SQLite и
  пользовательские ciphertext-объекты в Git или release artifacts.

### 3. Защита от будущих утечек

- [x] Расширить `.gitignore`:
  ```gitignore
  /data/
  /backups/
  *.sqlite
  *.sqlite-shm
  *.sqlite-wal
  .env
  .env.*
  !.env.example
  /dist/
  graft/
  ```
- [x] Добавить `.env.example` только при необходимости; в нём допустимы имена
  переменных и безопасные примеры, но не настоящие домены, IP, email, токены и
  пути к бэкапам. Не требуется: конфигурация документирована в README.
- [x] Проверить staged-файлы перед каждым коммитом и не добавлять runtime data.

### 4. README

- [x] Создать `README.md` на английском языке без badges и рекламных заявлений.
- [x] Описать назначение: self-hosted encrypted text and file sharing.
- [x] Кратко перечислить факты: browser-side encryption, text/files,
  expiry, delete links, optional one-time reads, SQLite/filesystem storage.
- [x] Явно описать security model:
  - ключ находится во фрагменте sharing URL;
  - любой, у кого есть полный URL, может расшифровать публикацию;
  - сервис не является средством анонимизации;
  - хостинг и reverse proxy видят сетевые метаданные и размер объектов.
- [x] Добавить локальный запуск:
  ```sh
  go test ./...
  go run .
  ```
- [x] Указать требования: Go 1.26+ и современный браузер с Web Crypto API.
- [x] Добавить Docker-запуск:
  ```sh
  export PASTA_DOMAIN=paste.example.com
  docker compose -f deploy/compose.yaml up --build -d
  curl --fail https://"$PASTA_DOMAIN"/readyz
  ```
- [x] Документировать `PASTA_ADDR`, `PASTA_DATA_DIR`, лимиты файлов и текста,
  TTL и ссылку на `deploy/backup-restore.md`.

### 5. Лицензирование и поддержка

- [x] Владелец выбирает лицензию до первого public push: MIT или Apache-2.0.
- [x] Добавить полный текст выбранной лицензии в `LICENSE`.
- [x] Указать лицензию в README и GitHub repository metadata.
- [x] Добавить `SECURITY.md` только при наличии обслуживаемого приватного канала
  для сообщений об уязвимостях. Приватный канал отсутствует.
- [ ] Назначить владельца repository и ответственного за security reports.

### 6. Качество кода и CI

- [x] Не добавлять абстракции, генераторы, шаблонные комментарии и тесты без
  наблюдаемого контракта.
- [x] Сохранять Go-код отформатированным и запускать:
  ```sh
  gofmt -w main.go main_test.go
  go test ./...
  go vet ./...
  ```
- [x] Добавить минимальный GitHub Actions workflow: `gofmt` check, `go test`,
  `go vet` и secret scan.
- [x] Запускать workflow с минимальными permissions (`contents: read`).
- [x] Подключить Dependabot для Go modules.
- [x] Обновления зависимостей, документации, деплоя и функциональности держать
  в независимых PR и Conventional Commits.

## Порядок реализации

- [x] `chore(repo): exclude runtime data and local configuration`
- [x] `chore(deploy): remove production-specific compose file`
- [x] `docs: add concise project README`
- [x] `docs: add project license`
- [x] `ci: verify Go tests and formatting`
- [ ] Выполнить полный pre-push scan и итоговые проверки.
- [x] Создать private GitHub repository, проверить настройки security scanning,
  затем сделать repository public.

## Критерии готовности

- [x] В новом Git-репозитории нет старой истории и `.git`-объектов источника.
- [x] Gitleaks не находит секретов в публичном дереве.
- [x] Нет production hostname, пользовательских данных, локальных конфигов и
  криптографических материалов.
- [x] Есть один переносимый Docker Compose путь.
- [x] Есть README, LICENSE и понятная процедура резервного копирования.
- [x] `go test ./...`, `go vet ./...` и Compose validation проходят.
- [x] GitHub secret scanning, push protection и CI включены.
