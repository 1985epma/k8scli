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
k8scli help
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

Observacao: esse teste usa um tarball gerado do workspace local atual para validar a formula antes ou depois da publicacao.

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
k8scli help
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

### Linux Debian ou Ubuntu

```bash
sudo dpkg -i k8scli_*.deb
```

### Linux RHEL, Fedora ou derivados

```bash
sudo rpm -i k8scli_*.rpm
```

### Windows

Use o instalador MSI ou o binario portable publicado nas releases.

Se quiser montar o MSI manualmente no ambiente de build Windows:

```powershell
pwsh -File scripts/build-msi.ps1 -Version 0.1.0
```

Guia do passo 2:

```text
windows/msi/README.md
```

## Como verificar se instalou corretamente

```bash
k8scli help
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
k8scli help
```