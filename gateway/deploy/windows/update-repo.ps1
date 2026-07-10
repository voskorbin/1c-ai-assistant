# -----------------------------------------------------------------------------
# update-repo.ps1
#
# Обновляет файлы шлюза для указанного IB из GitLab-репозитория деплоя
# и обновляет/клонирует рабочую копию MAIN_XML.
#
# Параметры:
#   -IBName        имя информационной базы (IB1, IB2, ...)
#   -DeployRepoUrl URL GitLab-репозитория с конфигами и бинарником
#   -SourceRepoUrl URL GitLab-репозитория с MAIN_XML
#   -BaseDir       корневая папка на сервере (по умолчанию D:\AI_Gate)
# -----------------------------------------------------------------------------

param(
    [Parameter(Mandatory = $true)]
    [string] $IBName,

    [Parameter(Mandatory = $true)]
    [string] $DeployRepoUrl,

    [Parameter(Mandatory = $true)]
    [string] $SourceRepoUrl,

    [string] $BaseDir = "D:\AI_Gate"
)

$ErrorActionPreference = "Stop"

$ibDir = Join-Path $BaseDir $IBName
$deployDir = Join-Path $ibDir ".deploy"
$repoDir = Join-Path $ibDir "repo"
$binTarget = Join-Path $ibDir "ai-assistant-go.exe"
$configTarget = Join-Path $ibDir "config.json"

# Убедимся, что целевые папки существуют
New-Item -ItemType Directory -Path $ibDir -Force | Out-Null

# -----------------------------------------------------------------------------
# 1. Обновляем репозиторий деплоя (конфиги + бинарник)
# -----------------------------------------------------------------------------
if (Test-Path -Path (Join-Path $deployDir ".git")) {
    Write-Host "[$IBName] Updating deploy repo..."
    Set-Location -LiteralPath $deployDir
    git pull
}
else {
    Write-Host "[$IBName] Cloning deploy repo..."
    if (Test-Path -Path $deployDir) {
        Remove-Item -LiteralPath $deployDir -Recurse -Force
    }
    git clone $DeployRepoUrl $deployDir
}

# -----------------------------------------------------------------------------
# 2. Копируем свежий бинарник и конфиг
# -----------------------------------------------------------------------------
$binSource = Join-Path $deployDir "bin\ai-assistant-go.exe"
$configSource = Join-Path $deployDir "configs\$IBName.config.json"

if (-not (Test-Path -Path $binSource)) {
    throw "Binary not found: $binSource"
}
if (-not (Test-Path -Path $configSource)) {
    throw "Config not found: $configSource"
}

Copy-Item -Path $binSource -Destination $binTarget -Force
Copy-Item -Path $configSource -Destination $configTarget -Force

Write-Host "[$IBName] Binary and config updated."

# -----------------------------------------------------------------------------
# 3. Обновляем рабочую копию MAIN_XML
# -----------------------------------------------------------------------------
if (Test-Path -Path (Join-Path $repoDir ".git")) {
    Write-Host "[$IBName] Updating source repo..."
    Set-Location -LiteralPath $repoDir
    git pull
}
else {
    Write-Host "[$IBName] Cloning source repo..."
    if (Test-Path -Path $repoDir) {
        Remove-Item -LiteralPath $repoDir -Recurse -Force
    }
    New-Item -ItemType Directory -Path $repoDir -Force | Out-Null
    git clone $SourceRepoUrl $repoDir
}

Write-Host "[$IBName] Done."
