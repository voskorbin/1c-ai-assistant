@echo off
chcp 65001 >nul

:: Пример стартового скрипта для production-шлюза.
:: Скопируйте этот файл в вашу локальную папку (например, <local-secrets-folder>),
:: переименуйте в start.local.bat и задайте реальные значения переменных окружения.
::
:: Рекомендуется хранить секреты в файле .env в той же папке, что и start.local.bat.
:: См. пример в README.md проекта.

set LLM_API_KEY=%LLM_API_KEY%
set JIRA_URL=%JIRA_URL%
set JIRA_PERSONAL_TOKEN=%JIRA_PERSONAL_TOKEN%
set CONFLUENCE_URL=%CONFLUENCE_URL%
set CONFLUENCE_PERSONAL_TOKEN=%CONFLUENCE_PERSONAL_TOKEN%
set MCP_ONEC_USERNAME=%MCP_ONEC_USERNAME%
set MCP_ONEC_PASSWORD=%MCP_ONEC_PASSWORD%

set GATEWAY_CONFIG=%~dp0config.example.json

echo Starting AI gateway...
cd /d %~dp0
ai-assistant-go.exe -config=%GATEWAY_CONFIG% > gateway.stdout.log 2> gateway.stderr.log
