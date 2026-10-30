# homebrew-k8scli

O tap Homebrew do `k8scli` vive em um repositorio proprio:

```text
https://github.com/1985epma/homebrew-k8scli
```

Repositorio oficial do app:

```text
https://github.com/1985epma/k8scli
```

## Como o tap e atualizado

A formula **nao** e mantida a mao: o GoReleaser a gera e faz commit em
`Formula/k8scli.rb` do tap a cada release (veja a secao `brews` em
[.goreleaser.yaml](../.goreleaser.yaml)). Por isso este repositorio nao guarda
mais uma copia da formula, que ficava desatualizada (sha256 e versao fixos).

Para o primeiro uso basta que o repositorio `1985epma/homebrew-k8scli` exista
com o branch `main`. Depois disso:

```bash
brew install 1985epma/k8scli/k8scli
```

## Token

O workflow de release precisa do secret `HOMEBREW_TAP_GITHUB_TOKEN`, com
permissao de escrita no repositorio do tap. Se o secret nao existir, o
workflow falha logo no inicio, antes de publicar qualquer artefato.

## Como testar localmente

```bash
./scripts/test-homebrew-tap.sh
```

O script monta uma formula temporaria a partir do workspace atual, instala em um
tap local descartavel, roda `brew test` e remove tudo no final.
