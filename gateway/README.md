# AI Assistant Gateway

On-premise шлюз для ИИ-ассистента 1С. Обеспечивает HTTP API между расширением конфигурации 1С (CFE) и языковыми моделями (LLM), а также инструментами MCP.

## Возможности

- Потоковая и синхронная генерация ответов от LLM.
- Подключение MCP-серверов (1С, Atlassian, code-index и др.).
- Работа с изображениями (vision) — требуется поддержка со стороны модели.
- Контекст открытого объекта 1С.
- CORS-заголовки для работы в веб-клиенте 1С.
- Поддержка нескольких баз 1С через разные конфиги/порты.

## Требования

- Go 1.21+
- Windows / Linux
- Доступ к LLM API (например, `https://your-llm-api.example.com`)
- Доступ к MCP-серверам (например, HTTP-сервис 1С `hs/mcp/rpc`)
- Для MCP-серверов через `npx`/`uvx`: Node.js и Python с `uv` или `pip` (см. раздел «Автономная работа без интернета»)

## Сборка

```powershell
go build -o ai-assistant-go.exe .
```

## Конфигурация

Основной конфиг задаётся через параметр `-config`. Примеры:

- `config.example.json` — production-шаблон с плейсхолдерами и переменными окружения.
- `config.test.json` — конфиг для запуска тестов.

Реальные конфиги (с вашими URL, именами баз, моделями) храните вне репозитория, например в `<local-secrets-folder>\configs\`.

Доступные примеры конфигов:

- `config.example.json` — быстрый старт через `npx`/`uvx` (при первом запуске требуется интернет для скачивания пакетов).
- `config.example.offline.json` — автономный вариант с прямыми путями к заранее установленным бинарникам `code-index-mcp` и `mcp-atlassian`.

Переменные окружения (примеры):

```powershell
$env:LLM_API_KEY="your-llm-api-key"
$env:JIRA_PERSONAL_TOKEN="your-jira-token"
$env:CONFLUENCE_PERSONAL_TOKEN="your-confluence-token"
$env:MCP_ONEC_USERNAME="your-mcp-username"
$env:MCP_ONEC_PASSWORD="your-mcp-password"
```

Локальные секреты и скрипты запуска храните в отдельной папке (например, `1c-ai-assistant-project-local`), которая не попадает в Git.

## Запуск

```powershell
# через пример скрипта (секреты должны быть заданы в переменных окружения)
.\start.example.bat

# или напрямую
$env:LLM_API_KEY="your-llm-api-key"
.\ai-assistant-go.exe -config=config.example.json
```

## Безопасность

Доступ к шлюзу рекомендуется ограничить сетевой изоляцией:

- Разрешите входящий трафик на порт шлюза только с IP-адресов серверов 1С (firewall / security groups).
- Размещайте шлюз в том же защищённом сегменте сети, что и серверы 1С.
- Не выставляйте порт шлюза наружу (в интернет) без reverse proxy с аутентификацией.

Пример правила Windows Firewall (PowerShell):

```powershell
New-NetFirewallRule `
  -DisplayName "AI Assistant Gateway — 1C only" `
  -Direction Inbound `
  -LocalPort 8000 `
  -Protocol TCP `
  -RemoteAddress <1C_SERVER_IP>/32 `
  -Action Allow
```

## Автономная работа без интернета

Шлюз сам по себе не требует интернета. Однако `npx` и `uvx` при первом запуске качают пакеты из сети. Чтобы работать полностью автономно:

1. Установите MCP-серверы заранее:

   ```powershell
   # code-index (требуется Node.js)
   npm install -g @regsorm/code-index-mcp@0.42.1

   # mcp-atlassian (требуется Python + uv/pip)
   uv tool install mcp-atlassian
   # или
   pip install mcp-atlassian
   ```

2. Найдите путь к установленным бинарникам:

   ```powershell
   Get-Command code-index-mcp
   Get-Command mcp-atlassian
   ```

3. Используйте `config.example.offline.json` как шаблон: замените `<user>` и пути на реальные.

4. LLM должен быть доступен по локальному адресу (например, `http://localhost:11434/v1/chat/completions` для Ollama).

После этого шлюз можно запускать без выхода в интернет.

## Развёртывание как службы Windows (WinSW / NSSM)

Шлюз можно зарегистрировать как Windows-службу любым удобным способом, например через **WinSW** или **NSSM**.

### WinSW

1. Скопируйте файлы на сервер.
2. Создайте `ai-assistant-gateway.xml` (пример ниже).
3. Установите и запустите службу:

```powershell
winsw install ai-assistant-gateway.xml
winsw start ai-assistant-gateway.xml
```

Пример `ai-assistant-gateway.xml`:

```xml
<service>
  <id>ai-assistant-gateway</id>
  <name>AI Assistant Gateway</name>
  <description>On-premise шлюз к LLM и MCP для ИИ-ассистента 1С</description>
  <executable>ai-assistant-go.exe</executable>
  <arguments>-config=config.json</arguments>
  <workingdirectory>%BASE%</workingdirectory>
  <log mode="roll-by-size">
    <sizeThreshold>10240</sizeThreshold>
    <keepFiles>8</keepFiles>
  </log>
  <env name="LLM_API_KEY" value="%LLM_API_KEY%" />
  <startmode>Automatic</startmode>
</service>
```

## HTTP API

- `POST /chat` — синхронный чат.
- `POST /chat/stream` — запуск потоковой генерации.
- `GET /chat/status/{request_id}` — статус потокового запроса.
- `POST /chat/stop` — остановка генерации.
- `GET /tools` — список доступных MCP-инструментов.
- `GET /health` — проверка работоспособности.

## Связанные проекты

- [1c_mcp](https://github.com/vladimir-kharin/1c_mcp/) — готовый MCP-сервер для публикации в 1С (`hs/mcp/rpc`).
