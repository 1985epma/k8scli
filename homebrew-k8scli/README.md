# homebrew-k8scli

Este diretorio contem o esqueleto inicial do tap Homebrew para o projeto `k8scli`.

Repositorio oficial do app:

```text
https://github.com/1985epma/k8scli
```

Repositorio esperado do tap:

```text
https://github.com/1985epma/homebrew-k8scli
```

## Estrutura

```text
homebrew-k8scli/
  Formula/
    k8scli.rb
```

## Como publicar o tap

1. Crie o repositorio `1985epma/homebrew-k8scli` no GitHub.
2. Copie o conteudo deste diretorio para a raiz do repositorio do tap.
3. Faça o primeiro push no branch `main`.
4. Depois disso, o comando abaixo passa a ter um destino valido:

```bash
brew tap 1985epma/k8scli
```

## Como testar localmente antes de publicar

```bash
./scripts/test-homebrew-tap.sh
```

## Publicacao automatica via GoReleaser

O arquivo [/.goreleaser.yaml](../.goreleaser.yaml) ja foi preparado para abrir commits no tap
`1985epma/homebrew-k8scli` quando uma release for executada com o token certo no ambiente.

Variavel esperada:

```bash
export HOMEBREW_TAP_GITHUB_TOKEN=seu_token
```

Esse token precisa ter permissao de escrita no repositorio do tap.

## Estado atual

- O repositorio oficial `1985epma/k8scli` ainda nao contem o codigo Go publicado nem releases versionadas; hoje ele mostra apenas `README.md`.
- Por isso, a formula incluida em `Formula/k8scli.rb` deve ser tratada como scaffold do tap e sera realmente utilizavel quando o codigo ou a primeira release estiverem publicados no repositorio oficial.
- Depois da primeira release versionada, o GoReleaser deve assumir a atualizacao da formula automaticamente.