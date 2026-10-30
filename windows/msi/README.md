# Windows MSI

Este diretorio contem a definicao WiX do instalador MSI do `k8scli`.
O instalador copia `k8scli.exe` para `Program Files\k8scli` e adiciona a pasta ao `PATH` do sistema.

## Requisitos

- Windows
- PowerShell 7 (`pwsh`)
- WiX Toolset no `PATH` (o CI usa a versao 4.0.5, que nao exige aceite de EULA)

```powershell
dotnet tool install --global wix --version 4.0.5
```

Com WiX 7 ou superior o script aceita o EULA do OSMF automaticamente (`wix eula accept wix7`).

## Entrada esperada

Por padrao o script usa o executavel gerado pelo GoReleaser (`make package`):

```text
dist/k8scli_windows_amd64_v1/k8scli.exe
```

## Como gerar o MSI

```powershell
pwsh -File scripts/build-msi.ps1 -Version 0.1.0
```

Saida:

```text
dist/k8scli_0.1.0_windows_amd64.msi
```

A versao do MSI e sempre `major.minor.patch`; sufixos como `-rc1` sao removidos do numero
interno, mas mantidos no nome do arquivo.

## Parametros opcionais

```powershell
pwsh -File scripts/build-msi.ps1 `
  -Version 0.1.0 `
  -SourceExe C:\path\to\k8scli.exe `
  -OutputDir C:\artifacts
```

## CI e release

- `ci.yml` (job `msi`) compila o executavel e gera um MSI a cada PR, para quebrar cedo.
- `release.yml` (job `msi`) baixa o `.exe` ja publicado na release, gera o MSI, cria o attestation de proveniencia e anexa o arquivo na release.

## Observacao

O WiX avisa explicitamente que o build suportado e somente em Windows. Por isso o script falha cedo quando executado em macOS ou Linux.
