# Варианты развёртывания шлюза ИИ-ассистента

Принцип: **одна информационная база 1С = один экземпляр шлюза** со своим портом, конфигом и локальной копией `MAIN_XML`.

## Содержимое папки

- `Dockerfile` — образ шлюза (Go + Node + Python/uvx).
- `start.sh` — точка входа: запускает `code-index daemon` и сам шлюз.
- `docker-compose.single.yml` — один IB на одном хосте.
- `docker-compose.multi.yml` — несколько IB + Traefik.
- `README.md` — этот файл.

---

## Вариант 1. Один контейнер на один IB (самый простой)

**Когда подходит:** тестовый стенд, первый боевой IB, небольшое число баз.

**Суть:**
- Один Docker-образ содержит Go-шлюз, Node.js, Python/uvx.
- Для каждого IB поднимается отдельный контейнер.
- Каждый контейнер пробрасывает свой порт наружу (`8001`, `8002`…).
- Внутри контейнера работает `code-index daemon` и сам шлюз.

**Плюсы:**
- Просто понять и отладить.
- Изоляция IB друг от друга.
- Можно обновлять IB по одному.

**Минусы:**
- На каждый IB тратится свой образ (но он общий) и порт.
- Много портов при росте числа IB.

**Использование:**

```bash
# Подготовить конфиг и .env
cp config.example.json deploy/config.json
cp .env.example deploy/ib1.env

# Подготовить/склонировать репозиторий
mkdir -p deploy/ib1-repo
git clone https://gitlab.example.com/your-group/your-repo.git deploy/ib1-repo

# Запустить
cd deploy
docker compose -f docker-compose.single.yml --env-file ib1.env up -d
```

---

## Вариант 2. Несколько IB через Docker Compose + Traefik

**Когда подходит:** 5–50+ IB на одном сервере.

**Суть:**
- Один стек `docker-compose.multi.yml`.
- Каждый IB — отдельный сервис с собственным volume и конфигом.
- Traefik слушает 80/443 порт и маршрутизирует по поддоменам:
  - `ib1.example.com` → `ib1-gateway:8000`
  - `ib2.example.com` → `ib2-gateway:8000`
- В 1С в константе `ИИ_АдресШлюза` указывается поддомен, а не порт.

**Плюсы:**
- Не нужно открывать десятки портов.
- Можно навесить HTTPS (Let's Encrypt) централизованно.
- Удобно масштабировать: добавить сервис = добавить IB.

**Минусы:**
- Нужен DNS/hosts для поддоменов.
- Traefik — дополнительная точка отказа.

**Использование:**

```bash
cd deploy
docker compose -f docker-compose.multi.yml up -d
```

---

## Вариант 3. Kubernetes (Helm-чарт) — в планах

> Этот вариант пока не реализован. Если вам нужен Helm-чарт — создайте issue или pull request.

**Когда подходит:** десятки/сотни IB, есть команда K8s.

**Суть:**
- Один Deployment на IB или один Deployment с несколькими репликами.
- ConfigMap/Secret для конфигурации и секретов.
- PersistentVolume для `MAIN_XML`.
- Ingress (nginx/traefik) для маршрутизации.

**Плюсы:**
- Автомасштабирование, self-healing, rolling update.
- Централизованное управление секретами.

**Минусы:**
- Сложнее в эксплуатации.
- Нужна инфраструктура K8s.

---

## Вариант 4. Без Docker — Windows services / NSSM

**Когда подходит:** Windows Server, Docker запрещён или нестабилен.

**Суть:**
- На сервере ставим Go-бинарник, Node.js, Python/uvx.
- Для каждого IB создаём папку с `config.json`, `start-gateway.ps1`.
- Регистрируем шлюз и `code-index daemon` как Windows-службы через [NSSM](https://nssm.cc/).

**Плюсы:**
- Нативно для Windows-инфраструктуры 1С.
- Не нужно учить Docker.

**Минусы:**
- Сложнее обновлять много IB.
- Нет изоляции.
- Ручное управление портами.

**Пример службы через NSSM:**

```bat
nssm install AI_Gate_IB1 "<AI_Gate_Dir>\IB1\ai-assistant-go.exe"
nssm set AI_Gate_IB1 AppDirectory "<AI_Gate_Dir>\IB1"
nssm set AI_Gate_IB1 AppParameters "-config=<AI_Gate_Dir>\IB1\config.json"
nssm start AI_Gate_IB1
```

---

## Рекомендация

Для старта выбрать **Вариант 1** (один контейнер на IB) или **Вариант 2** (Compose + Traefik), если IB уже несколько. Оба варианта покрывают текущую архитектуру 1 IB = 1 шлюз и не требуют переписывания кода.
