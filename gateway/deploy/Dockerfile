# syntax=docker/dockerfile:1

# -----------------------------------------------------------------------------
# Этап 1: сборка Go-бинарника
# -----------------------------------------------------------------------------
FROM golang:1.21-alpine AS builder

WORKDIR /build

# Сначала копируем зависимости, чтобы кэшировать слой go mod download
COPY go.mod go.sum ./
RUN go mod download

# Копируем исходники и собираем бинарник
COPY . ./
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o ai-assistant-go .

# -----------------------------------------------------------------------------
# Этап 2: runtime-образ
# -----------------------------------------------------------------------------
# node:20-slim — Debian, в нём удобно ставить Python и uv
FROM node:20-slim

# Устанавливаем Python, pip, git и сертификаты
RUN apt-get update && apt-get install -y --no-install-recommends \
    python3 \
    python3-pip \
    git \
    ca-certificates \
    wget \
    && rm -rf /var/lib/apt/lists/*

# Устанавливаем uv/uvx для запуска mcp-atlassian
RUN pip3 install --break-system-packages uv || pip3 install uv

# Рабочая директория приложения
WORKDIR /app

# Копируем бинарник и скрипт запуска
COPY --from=builder /build/ai-assistant-go /app/ai-assistant-go
COPY deploy/start.sh /app/start.sh
RUN chmod +x /app/start.sh /app/ai-assistant-go

# code-index ищет daemon.json относительно CODE_INDEX_HOME
ENV CODE_INDEX_HOME=/repo
ENV PATH="/app:${PATH}"

# Порт, который слушает шлюз (можно переопределить через config)
EXPOSE 8000

# Точка входа: запускает code-index daemon и шлюз
ENTRYPOINT ["/app/start.sh"]
