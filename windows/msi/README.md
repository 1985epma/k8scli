# Windows MSI

Este diretorio monta o passo 2 para gerar um instalador MSI do `k8scli`.

## Requisitos

- Windows
- PowerShell (`pwsh` ou Windows PowerShell)
- WiX Toolset 7 no `PATH`

Instalacao recomendada do WiX:

```powershell
dotnet tool install --global wix
```

## Entrada esperada

O script usa por padrao o executavel gerado em:

```text
dist-release-0.1.0/k8scli_windows_amd64_v1/k8scli.exe
```

## Como gerar o MSI

```powershell
pwsh -File scripts/build-msi.ps1 -Version 0.1.0
```

Saida esperada:

```text
dist-release-0.1.0/k8scli_0.1.0_windows_amd64.msi
```

## Parametros opcionais

```powershell
pwsh -File scripts/build-msi.ps1 \
  -Version 0.1.0 \
  -SourceExe C:\path\to\k8scli.exe \
  -OutputDir C:\artifacts
```

## Observacao

O WiX 7 avisa explicitamente que o build suportado e somente em Windows. Por isso o script falha cedo quando executado em macOS ou Linux.