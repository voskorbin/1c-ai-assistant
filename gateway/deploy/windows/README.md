# Развёртывание на Windows Server

> **Важно:** среда будет Windows Server. Docker на Windows Server возможен, но часто избыточен и сложнее в поддержке, чем нативные службы. Поэтому базовый вариант — **Windows services + NSSM**.

## Идея

- Конфигурационные файлы и скрипты хранятся в **приватном репозитории**.
- На сервере для каждого IB создаётся отдельная папка, например:
  ```
  <AI_Gate_Dir>\
    IB1\
      ai-assistant-go.exe
      config.json          <- из приватного репозитория
      start-gateway.ps1
      update-repo.ps1
    IB2\
      ...
  ```
- Шлюз и `code-index daemon` запускаются как **Windows-службы** через [NSSM](https://nssm.cc/).
- Локальная копия `MAIN_XML` обновляется по расписанию через `git pull`.

## Что нужно на сервере

1. **Windows Server** (версия уточняется).
2. **Go-runtime не нужен** — используем скомпилированный `ai-assistant-go.exe`.
3. **Node.js LTS** — для `npx` и `code-index`.
4. **Python + uv/uvx** — для `mcp-atlassian`.
   ```powershell
   # Вариант установки uv на Windows
   powershell -ExecutionPolicy ByPass -c "irm https://astral.sh/uv/install.ps1 | iex"
   ```
5. **Git** — для `git clone` / `git pull`.
6. **NSSM** — для регистрации служб.

## Структура репозитория развёртывания

```
ai-gate-deployment/
  configs/
    IB1.config.json
    IB2.config.json
  scripts/
    start-gateway.ps1
    update-repo.ps1
  bin/
    ai-assistant-go.exe   <- артефакт сборки
```

> Секреты (`LLM_API_KEY`, `MCP_ONEC_PASSWORD` и т.д.) **не хранятся** в репозитории в открытом виде. В конфигах используются переменные окружения (`$LLM_API_KEY`), а реальные значения задаются в переменных окружения сервера или в CI/CD.
>
> Для автономной работы без интернета установите `code-index-mcp` и `mcp-atlassian` заранее и укажите прямые пути к бинарникам вместо `npx`/`uvx` (см. `config.example.offline.json` в корне `gateway/`).

## Пример конфига для Windows (`configs/IB1.config.json`)

```json
{
  "server": {
    "host": "0.0.0.0",
    "port": 8001,
    "stream_store_ttl_seconds": 3600
  },
  "llm": {
    "url": "https://your-llm-api.example.com/v1/chat/completions",
    "model": "your-model-name",
    "api_key_env": "LLM_API_KEY",
    "timeout_seconds": 300,
    "max_tokens": 4096,
    "temperature": 0.3,
    "reasoning": false,
    "max_concurrent_requests": 10
  },
  "mcp_servers": [
    {
      "name": "atlassian",
      "enabled": true,
      "transport": "stdio",
      "command": "uvx",
      "args": ["mcp-atlassian"],
      "read_only": true,
      "env": {
        "JIRA_URL": "https://jira.example.com",
        "JIRA_API_VERSION": "2",
        "JIRA_PERSONAL_TOKEN": "$JIRA_PERSONAL_TOKEN",
        "CONFLUENCE_URL": "https://confluence.example.com",
        "CONFLUENCE_PERSONAL_TOKEN": "$CONFLUENCE_PERSONAL_TOKEN"
      }
    },
    {
      "name": "mcp_1c",
      "enabled": true,
      "transport": "http",
      "url": "https://your-1c-server/your-base/hs/mcp/rpc",
      "username": "$MCP_ONEC_USERNAME",
      "password": "$MCP_ONEC_PASSWORD",
      "insecure_skip_verify": true,
      "read_only": true
    },
    {
      "name": "code_index",
      "enabled": true,
      "transport": "stdio",
      "command": "npx",
      "args": ["-y", "@regsorm/code-index-mcp@0.42.1", "serve", "--path", "<AI_Gate_Dir>\\IB1\\repo\\MAIN_XML"],
      "read_only": true,
      "env": {
        "CODE_INDEX_HOME": "<AI_Gate_Dir>\\IB1\\repo"
      }
    }
  ],
  "config": {
    "model": "your-model-name",
    "temperature": 0.3,
    "vision_supported": true,
    "confluence_spaces_filter": "YOUR_SPACE",
    "confluence_pages": [
      "https://confluence.example.com/pages/viewpage.action?pageId=123456789"
    ]
  }
}
```

## Установка одного IB

```powershell
# 1. Задать переменные окружения (глобально или в скрипте)
[Environment]::SetEnvironmentVariable("LLM_API_KEY", "your-llm-api-key", "Machine")
[Environment]::SetEnvironmentVariable("MCP_ONEC_PASSWORD", "your-mcp-password", "Machine")
# и т.д.

# 2. Установить службы через NSSM
nssm install AI_Gate_IB1 "<AI_Gate_Dir>\IB1\ai-assistant-go.exe"
nssm set AI_Gate_IB1 AppDirectory "<AI_Gate_Dir>\IB1"
nssm set AI_Gate_IB1 AppParameters "-config=<AI_Gate_Dir>\IB1\config.json"
nssm start AI_Gate_IB1

# 3. Склонировать/обновить репозиторий
.\scripts\update-repo.ps1 -IBName "IB1" -GitUrl "https://gitlab.example.com/your-group/your-repo.git"
```

## Обновление

1. CI/CD собирает новый `ai-assistant-go.exe`.
2. CI/CD кладёт его в `bin/ai-assistant-go.exe`.
3. На сервере запускается `update-repo.ps1`, который:
   - делает `git pull`;
   - копирует свежий бинарник и конфиг в папку IB.

После обновления файлов перезапустите службу шлюза вручную или через NSSM.

## Логи на Windows

- `gateway.log` — внутренний лог шлюза.
- `gateway.stdout.log` / `gateway.stderr.log` — stdout/stderr.
- `code-index-daemon.log` — лог демона.
- Для просмотра в реальном времени: `Get-Content gateway.log -Tail 50 -Wait`.

## Альтернатива: Docker на Windows

Если админы настаивают на Docker, можно использовать **Docker Desktop / Docker EE на Windows Server** с Linux-контейнерами через WSL2. Но для production-сервера 1С это редкий выбор.
