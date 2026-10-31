# Instalacao do k8scli

Este guia mostra como instalar o `k8scli` no macOS, Linux e por build local.

Repositorio oficial:

```text
https://github.com/1985epma/k8scli
```

## Requisitos

- Go instalado, se voce for compilar localmente
- Acesso a um arquivo kubeconfig valido para usar comandos que consultam o cluster

## Opcao 1: instalar localmente no macOS

```bash
brew install 1985epma/k8scli/k8scli
```

Depois valide:

```bash
k8scli version
```

Se preferir instalar a partir do codigo local do repositorio:

```bash
make install-local
```

O tap oficial do Homebrew fica em:

```text
https://github.com/1985epma/homebrew-k8scli
```

Se quiser testar a formula Homebrew localmente antes de publicar o tap:

```bash
./scripts/test-homebrew-tap.sh
```

Observacao: esse teste gera uma formula temporaria a partir de um tarball do workspace local atual. A formula publicada no tap e gerada pelo GoReleaser a cada release.

## Opcao 2: instalar a partir do codigo-fonte

Dentro da pasta do projeto:

```bash
make build
make install
```

Isso instala o binario no diretorio padrao do Go, normalmente:

```bash
$GOPATH/bin
```

Se o comando nao abrir, adicione o binario ao `PATH`:

```bash
export PATH="$PATH:$(go env GOPATH)/bin"
```

Depois valide:

```bash
k8scli version
```

## Opcao 3: instalar com completion de shell

O projeto possui um instalador local que copia o binario e instala o arquivo de completion.

### macOS e Linux com zsh

```bash
make install-local
```

Por padrao, o binario vai para:

```bash
/usr/local/bin/k8scli
```

E a completion do zsh vai para:

```bash
/usr/local/share/zsh/site-functions/_k8scli
```

Se quiser instalar em outro prefixo:

```bash
make install-local PREFIX="$HOME/.local" SHELL_NAME=zsh
```

## Opcao 4: instalar manualmente a partir de release

Os artefatos publicados ficam em:

```text
https://github.com/1985epma/k8scli/releases
```

### Linux (download com verificacao de checksum)

```bash
VERSION=0.1.1   # troque pela release desejada
ARCH=amd64      # ou arm64
BASE="https://github.com/1985epma/k8scli/releases/download/v${VERSION}"

curl -fsSLO "${BASE}/k8scli_${VERSION}_linux_${ARCH}.deb"
curl -fsSLO "${BASE}/checksums.txt"
sha256sum --ignore-missing -c checksums.txt
```

Debian ou Ubuntu:

```bash
sudo dpkg -i "k8scli_${VERSION}_linux_${ARCH}.deb"
```

RHEL, Fedora ou derivados (baixe o `.rpm` da mesma forma):

```bash
sudo rpm -i "k8scli_${VERSION}_linux_${ARCH}.rpm"
```

Nas releases publicadas com o pipeline de supply chain, o `checksums.txt` tambem
tem assinatura cosign (`checksums.txt.sigstore.json`), SBOMs e attestation de
proveniencia. Veja o passo a passo de verificacao no [README](README.md#verify-a-release).

### Docker

```bash
docker run --rm -v ~/.kube:/home/nonroot/.kube:ro ghcr.io/1985epma/k8scli:latest pods
```

### Windows

Use o instalador MSI (ele adiciona o `k8scli` ao `PATH`) ou o binario portable
`k8scli_<versao>_windows_amd64.exe` publicado nas releases.

Se quiser montar o MSI manualmente no ambiente de build Windows (apos `make package`):

```powershell
pwsh -File scripts/build-msi.ps1 -Version 0.1.0
```

Guia do passo 2:

```text
windows/msi/README.md
```

## Console web e assistentes de configuracao

Depois de instalar, `k8scli ui` (ou `k8scli-ui`) abre o console web no navegador. `k8scli serve`
faz o mesmo e so imprime o link. O console escuta somente em `127.0.0.1` e o link impresso traz um
token de sessao. A interface e em ingles e traz assistentes passo a passo: **Connect a cluster**,
**Install the CLI** (para outras maquinas) e **Shell completion**.

```bash
k8scli ui
```

O `k8scli-ui` e o mesmo binario com outro nome (um link para `k8scli`): o Homebrew, os pacotes
`.deb`/`.rpm`, `make install` e `make install-local` o instalam. No Windows, o zip e o MSI trazem um `k8scli-ui.cmd` que executa `k8scli.exe ui`; com o `.exe` portatil avulso, use `k8scli ui`.
A pagina **Apps (Helm)** instala aplicativos no cluster a partir de um catalogo de 24 apps revisados
(por exemplo Metrics Server, cert-manager, Traefik, Argo CD e CloudNativePG) e tambem permite buscar charts
no Artifact Hub. O Helm ja vem embutido no binario, entao nao e preciso instalar o `helm`. Toda
instalacao mostra antes o que sera criado (**Review changes**); para um chart vindo da busca, voce ainda
digita o nome do chart para confirmar. `--catalog-only` desliga a busca. O console tambem tem a pagina
**Resilience** (recuperacao de desastre, veja [docs/dr.md](docs/dr.md)), em modo somente leitura a menos que
voce inicie com `--allow-dr-runs`.
Veja o [guia do console web](docs/web-console.md) e a secao "Web console" do [README](README.md#web-console).

## Como verificar se instalou corretamente

```bash
k8scli version
k8scli contexts
```

Se voce ja tiver um kubeconfig valido:

```bash
k8scli cluster
```

## Completion de shell manual

Se quiser gerar a completion manualmente:

```bash
k8scli completion zsh
k8scli completion bash
k8scli completion fish
```

Exemplo para zsh:

```bash
mkdir -p ~/.zsh/completion
k8scli completion zsh > ~/.zsh/completion/_k8scli
```

## Problemas comuns

### erro no Homebrew: Repository not found

Verifique se voce esta usando exatamente este comando:

```bash
brew install 1985epma/k8scli/k8scli
```

Se o tap ainda nao tiver sido atualizado na sua maquina, rode:

```bash
brew tap 1985epma/k8scli
brew update
```

### comando nao encontrado

Verifique se o diretorio do binario esta no `PATH`.

### erro de kubeconfig

Os comandos `pods`, `pods-all`, `logs`, `set`, `scale` e `cluster` precisam de um kubeconfig valido.

Voce pode informar um arquivo especifico assim:

```bash
k8scli --kubeconfig ~/.kube/config contexts
```

## Resumo rapido

```bash
# build local
make build

# instalar no GOPATH/bin
make install

# instalar binario + completion
make install-local

# testar
k8scli version
```