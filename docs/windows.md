# Windows

Ark supports Windows amd64 and arm64 as a native command-line application.
It does not require WSL, a C compiler, or administrator privileges.

## Prerequisites

- Windows PowerShell 5.1 or PowerShell 7
- [Git for Windows](https://git-scm.com/download/win), with `git` on PATH
- Go 1.26.5 or newer only when developing Ark itself or installing with
  `go install`

## Install or upgrade

From PowerShell:

```powershell
$installer = Join-Path $env:TEMP 'install-ark.ps1'
Invoke-WebRequest https://raw.githubusercontent.com/elk-work/ark/main/scripts/install.ps1 -OutFile $installer
Unblock-File $installer
& $installer
ark --version
```

The installer:

1. resolves the latest Ark release unless `-Version vX.Y.Z` is supplied;
2. selects the Windows amd64 or arm64 archive;
3. verifies it against the release's published SHA-256 file;
4. installs `ark.exe` in the directory of an existing Ark installation, or
   `%USERPROFILE%\.local\bin` for a first install; and
5. adds that directory to the user PATH if needed.

The current PowerShell session sees the new PATH immediately. Other terminals
and desktop applications must be restarted to inherit it. Examples:

```powershell
# Install a specific release.
& $installer -Version v0.5.2

# Choose a destination. It is added to the user PATH.
& $installer -InstallDir "$env:LOCALAPPDATA\Programs\Ark"

# For CI or a disposable test, leave the user PATH unchanged.
& $installer -InstallDir "$env:TEMP\ark-bin" -NoPathUpdate
```

### Manual installation

Download the matching `ark_<version>_windows_<arch>.zip` and adjacent
`.zip.sha256` from [Ark releases](https://github.com/elk-work/ark/releases).
Verify and extract it in PowerShell:

```powershell
(Get-FileHash .\ark_v0.5.2_windows_amd64.zip -Algorithm SHA256).Hash.ToLower()
Get-Content .\ark_v0.5.2_windows_amd64.zip.sha256
Expand-Archive .\ark_v0.5.2_windows_amd64.zip -DestinationPath .\ark-release
```

The two hashes must match. Move `ark.exe` to a permanent directory and add
that directory—not the executable itself—to your user PATH.

## Use Ark in a project

Ark state is local and `.ark/` is intentionally not committed. A fresh clone
therefore joins the project's existing Ark repository using the repository ID,
sync-service URL, and token supplied by the project owner:

```powershell
git clone https://github.com/example/project.git
Set-Location project

ark init --repository <repository-id>
ark remote set https://ark.example.com
ark login
ark sync
ark status
```

`ark login` accepts one token line on standard input. It stores a credential
once per sync-service host, not once per project, so that login covers every
Ark project using the same service. On Windows the fallback credential file is
`%USERPROFILE%\.ark\credentials.toml`, protected by a current-user-only ACL;
tokens are never stored inside the project. For CI and coding agents, set
`ARK_TOKEN` in the process environment instead:

```powershell
$env:ARK_TOKEN = '<token from your secret store>'
ark sync
```

Do not commit a token, put it in `.ark/config.toml`, or leave it in a checked-in
PowerShell script.

### Git worktrees

Git does not copy untracked files such as `.ark/` into a new worktree. Join
each worktree to the same Ark repository before using it:

```powershell
Set-Location path\to\project-worktree
ark init --repository <repository-id>
ark remote set https://ark.example.com
ark sync
```

Credentials are stored per Windows user and service host, so a prior login in
the main checkout also covers its worktrees. Do not copy an active `ark.db`
between worktrees; each client keeps its own local mutation log and syncs it.

## Develop Ark itself

Install Go 1.26.5 or newer and Git, then run the same checks used by Windows CI:

```powershell
git clone https://github.com/elk-work/ark.git
Set-Location ark

go build ./...
go test ./...
gofmt -l .    # must print nothing
go vet ./...
```

Ark's tests create temporary Git repositories and SQLite databases and do not
need a cloud account or a C toolchain.

## Run a local sync service

To test multiple clients entirely on one Windows machine:

```powershell
$env:ARK_API_TOKEN = 'local-development-token'
$env:DATA_DIR = Join-Path $env:TEMP 'ark-server-data'
$env:BASE_URL = 'http://127.0.0.1:8080'
$env:PORT = '8080'
go run ./cmd/ark-server
```

Keep that terminal open. In a second PowerShell terminal, initialize a Git
repository, set the same URL as its Ark remote, set `ARK_TOKEN` to the same
token, and run `ark sync`. The full server configuration and production
security requirements are in [self-hosting.md](self-hosting.md).

## Troubleshooting

- **`ark` is not recognized:** run the installer again, open a new terminal,
  and check `Get-Command ark`. The install directory must appear in `$env:Path`.
- **`git` is not recognized:** install Git for Windows and reopen the terminal.
- **No `.ark` directory:** run `ark init` for a new Ark repository, or
  `ark init --repository <id>` to join an existing one.
- **No credentials:** run `ark login` after `ark remote set`, or provide
  `ARK_TOKEN` from a secret store.
- **PowerShell blocks the downloaded installer:** run `Unblock-File` on the
  downloaded script. Do not lower the machine-wide execution policy.
