#!/bin/sh
set -e

# -----------------------------------------------------------------------------
# Точка входа контейнера шлюза ИИ-ассистента.
# Запускает code-index daemon в фоне, затем стартует Go-шлюз.
# -----------------------------------------------------------------------------

CONFIG_PATH="${CONFIG_PATH:-/app/config.json}"
REPO_PATH="${REPO_PATH:-/repo}"

export CODE_INDEX_HOME="${REPO_PATH}"

# Убедимся, что директория репозитория существует
mkdir -p "${REPO_PATH}"
cd "${REPO_PATH}"

# Запускаем code-index daemon в фоне
# Он создаст daemon.json в CODE_INDEX_HOME и будет координировать индексацию
npx -y @regsorm/code-index-mcp@0.42.1 daemon run &
DAEMON_PID=$!

# При завершении шлюза убиваем daemon
_cleanup() {
    if kill -0 "$DAEMON_PID" 2>/dev/null; then
        kill "$DAEMON_PID" 2>/dev/null || true
        wait "$DAEMON_PID" 2>/dev/null || true
    fi
}
trap _cleanup INT TERM

# Даём daemon время подняться
sleep 3

# Запускаем шлюз на переднем плане
# exec заменяет shell, чтобы шлюз получал сигналы Docker (SIGTERM)
exec /app/ai-assistant-go -config="${CONFIG_PATH}"
