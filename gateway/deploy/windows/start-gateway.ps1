# -----------------------------------------------------------------------------
# start-gateway.ps1
#
# Точка входа Windows-службы шлюза.
# При каждом старте (в том числе после перезагрузки сервера):
#   1. Обновляет бинарник и конфиг из GitLab.
#   2. Запускает Go-шлюз на переднем плане.
#
# Параметры задаются через переменные окружения или параметры.
# -----------------------------------------------------------------------------

param(
    [Parameter(Mandatory = $true)]
    [string] $IBName,

    [string] $DeployRepoUrl = $env:AI_GATE_DEPLOY_REPO,
    [string] $SourceRepoUrl = $env:AI_GATE_SOURCE_REPO,
    [string] $BaseDir = "D:\AI_Gate"
)

$ErrorActionPreference = "Stop"

$ibDir = Join-Path $BaseDir $IBName
$updateScript = Join-Path $PSScriptRoot "update-repo.ps1"

# При старте всегда тянем свежие файлы из GitLab
& $updateScript -IBName $IBName -DeployRepoUrl $DeployRepoUrl -SourceRepoUrl $SourceRepoUrl -BaseDir $BaseDir

Set-Location -LiteralPath $ibDir

$configPath = Join-Path $ibDir "config.json"
$stdoutLog = Join-Path $ibDir "gateway.stdout.log"
$stderrLog = Join-Path $ibDir "gateway.stderr.log"

# Запускаем шлюз. stdout/stderr пишем в файлы, чтобы NSSM корректно отслеживал процесс.
& "$ibDir\ai-assistant-go.exe" -config="$configPath" > "$stdoutLog" 2> "$stderrLog"
