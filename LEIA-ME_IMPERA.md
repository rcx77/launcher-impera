# Launcher Impera Genesis — guia de configuração e build

Este projeto é o **Slender Launcher** (https://github.com/luan/slender-launcher),
escolhido como base pronta (Go + Wails v2 + Svelte, com splash screen, auto-update
do próprio launcher e do client) e já personalizado com a identidade visual do
Impera Global e adaptado para falar com o backend que você já usa
(**KrayAccOpenTibia**).

## O que já foi feito

1. **Identidade visual** — troquei os assets pelas duas imagens que você enviou:
   - `frontend/src/assets/images/logo-universal.png` → seu emblema/crest (usado na
     tela principal, 148x148px).
   - `frontend/src/assets/images/background-artwork.jpg` → o banner "IMPERA
     GENESIS" (fundo da janela, com `background-size: cover`, então ele preenche
     bem a janela mesmo sendo bem "largo").
   - `build/appicon.png` e `build/windows/icon.ico` (ícone multi-resolução,
     16 a 256px) → gerados a partir do mesmo emblema, para o ícone do .exe e da
     barra de tarefas.
2. **Nome do launcher** — `wails.json` renomeado de "Slender" para
   **"ImperaGenesis"** (`name`/`outputfilename`), com `author`/`info` ajustados
   para "Impera Global". O launcher deriva o título da janela
   ("ImperaGenesis Launcher") e a pasta de instalação (`%appdata%/ImperaGenesis`)
   automaticamente a partir do nome do .exe compilado — então se algum dia você
   renomear o .exe final, tudo isso muda junto.
3. **Integração com o seu manifest (ponto 3 do seu pedido — fluxo de
   atualização)** — o Slender original foi feito para o formato do client
   oficial da CIP (dois manifests por sistema operacional, arquivos comprimidos
   em LZMA, hash "packed" e "unpacked" separados). Isso **não combinava** com o
   manifest simples que o KrayAccOpenTibia já gera (`{app, version, base_url,
   files:[{path,size,sha256}]}`, servido em `GET /client/manifest`, arquivos
   simples sem compressão). Por isso eu **reescrevi a lógica de download do
   launcher em Go** (`app.go`) para consumir diretamente o formato do
   KrayAccOpenTibia — não precisa gerar nada extra nem rodar o client-editor da
   CIP. Resumo do fluxo agora:
   - O launcher busca `manifest_url` (você configura, é a URL do
     `GET /client/manifest` do seu KrayAccOpenTibia).
   - Compara o `sha256` de cada arquivo do manifest com o arquivo local; só
     baixa o que estiver faltando ou diferente.
   - Baixa cada arquivo de `base_url` (campo que o próprio manifest já traz,
     apontando para `/launcher_client` no KrayAccOpenTibia) + o caminho relativo
     do arquivo.
   - Depois de atualizado, abre o executável do client (nome configurável, veja
     abaixo).

## Antes de compilar: 3 coisas para configurar

Abra `main.go` e ajuste as duas URLs no começo da função `main()`:

```go
launcherBaseURL := "http://SEU_SERVIDOR:PORTA/launcher/"   // onde fica o próprio launcher (auto-update dele)
manifestURL := "http://SEU_SERVIDOR:PORTA/client/manifest" // endpoint do KrayAccOpenTibia
```

- `launcherBaseURL`: uma pasta qualquer no seu servidor web com
  `ImperaGenesis.exe` + `ImperaGenesis.exe.sha256` (para o launcher se
  autoatualizar). Gere o `.sha256` com `certutil -hashfile ImperaGenesis.exe
  SHA256` (Windows) toda vez que publicar uma nova versão do launcher.
- `manifestURL`: a URL onde o KrayAccOpenTibia está servindo o manifest (o
  `GET /client/manifest` do backend Go que você já roda).

Essas duas URLs também ficam salvas em `config.toml` (em
`%appdata%/ImperaGenesis/config.toml`) depois da primeira execução — então, se
preferir, pode compilar com placeholders e só editar esse `config.toml` depois,
sem precisar recompilar.

O terceiro item é o **nome do executável do client** (`IMPERAHELPER.exe` ou
qual for o nome real do seu .exe): está na chave `executable` do mesmo
`config.toml`, com valor padrão `"client.exe"` — troque para o nome real depois
da primeira execução, ou já ajuste o padrão em `main.go` (variável
`clientExecutable`) antes de compilar.

## Como compilar (isso eu não consegui fazer aqui)

Eu escrevi e revisei o código Go/Svelte manualmente e validei a sintaxe (gofmt
não encontrou nenhum erro), mas **não consegui rodar o build completo dentro
desta sessão**: o ambiente aqui bloqueia o acesso a `proxy.golang.org` (onde o
Go baixa as dependências do módulo) e, além disso, compilar um app Wails
gera um binário nativo com webview - isso exige um Windows real (ou pelo menos
as ferramentas nativas dele) para linkar corretamente, o que este sandbox em
Linux não tem.

Para compilar de verdade, no seu Windows:

1. Instale [Go 1.18+](https://go.dev/dl/), [Node.js](https://nodejs.org/) e a
   CLI do Wails:
   ```
   go install github.com/wailsapp/wails/v2/cmd/wails@latest
   ```
2. Dentro da pasta do projeto:
   ```
   wails build
   ```
   O executável final sai em `build/bin/ImperaGenesis.exe`.

Se preferir não instalar nada localmente, também é possível compilar via
GitHub Actions (runner `windows-latest` rodando `wails build`) — me avise se
quiser que eu monte esse workflow.

## Resposta ao ponto 1 do seu pedido original: proteção dos arquivos do client

Isso ainda não tinha sido respondido, então aproveito para fechar aqui. Três
opções reais, da mais simples à mais forte (nenhuma delas exige tocar no
launcher, são todas do lado do client OTClient):

1. **Empacotar em `.otpkg`** (o próprio client já suporta, via
   `g_resources.searchAndAddPackages`): é só um `.zip` renomeado. Organiza os
   arquivos e tira da navegação casual do Explorer, mas qualquer jogador
   com um pouco de conhecimento abre com qualquer ferramenta de zip trocando a
   extensão de volta. Proteção fraca, zero esforço.
2. **Compilar os `.lua` para bytecode com `luac5.1`** (o mesmo `luac5.1` que
   usei a sessão inteira para validar sintaxe): o engine do client
   (`LuaInterface::loadBuffer`) carrega bytecode Lua sem nenhuma restrição, ou
   seja, funciona sem precisar recompilar o client.exe. É bem mais forte que o
   `.otpkg` sozinho — o jogador não consegue simplesmente abrir o `.lua` num
   editor de texto e ler/editar sua lógica. Ainda é reversível por alguém com
   um decompilador de Lua e conhecimento técnico avançado, mas isso já filtra
   a esmagadora maioria dos jogadores curiosos.
3. **Criptografia nativa do próprio fork** (`ResourceManager::encrypt`/
   `runEncryption`, crédito a @Mrpox/@TheMaoci no README do projeto): existe no
   código-fonte, mas está **totalmente desligada** no seu `config.h`
   atual (`ENABLE_ENCRYPTION 0`, senha/header ainda como placeholder). O
   próprio README do projeto avisa que essa cifra é "unsafe" (é uma cifra de
   stream simples, não é criptografia forte de verdade) — mas ainda assim é
   mais um obstáculo que o `.otpkg` puro. Ativar isso exige: mudar 2 flags +
   colocar senha/header reais em `config.h`, e recompilar o client inteiro em
   Visual Studio/CMake — algo que esta sessão não tem como fazer sem acesso ao
   ambiente de build do seu client (só tenho a ponte de arquivos, sem shell no
   seu PC).

**Minha recomendação prática**: combine as opções 1 e 2 (empacotar em
`.otpkg` os `.lua` já compilados em bytecode) — dá o melhor resultado sem
precisar recompilar o client.exe. A opção 3 fica como reforço futuro, se um dia
vocês forem recompilar o client por outro motivo.
