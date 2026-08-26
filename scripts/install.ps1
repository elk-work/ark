[CmdletBinding()]
param(
    [string]$Version,
    [string]$InstallDir,
    [switch]$NoPathUpdate
)

# Install or upgrade Ark from a published Windows release. This script does
# not require Go or administrator privileges.
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

# Windows PowerShell 5.1 can otherwise negotiate an obsolete TLS version on
# older machines, while GitHub requires TLS 1.2 or newer.
if ($PSVersionTable.PSEdition -eq 'Desktop') {
    [Net.ServicePointManager]::SecurityProtocol =
        [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12
}

$repository = 'elk-work/ark'

if (-not $Version) {
    $headers = @{ 'User-Agent' = 'ark-windows-installer' }
    $release = Invoke-RestMethod `
        -Uri "https://api.github.com/repos/$repository/releases/latest" `
        -Headers $headers
    $Version = $release.tag_name
}
$Version = $Version.Trim()
if (-not $Version.StartsWith('v')) {
    $Version = "v$Version"
}

$architecture = $env:PROCESSOR_ARCHITECTURE
if ($env:PROCESSOR_ARCHITEW6432) {
    $architecture = $env:PROCESSOR_ARCHITEW6432
}
switch -Regex ($architecture) {
    '^(AMD64|x86_64)$' { $arch = 'amd64'; break }
    '^(ARM64|aarch64)$' { $arch = 'arm64'; break }
    default { throw "Unsupported Windows architecture: $architecture" }
}

$asset = "ark_${Version}_windows_${arch}.zip"
$baseUrl = "https://github.com/$repository/releases/download/$Version"

if (-not $InstallDir) {
    $existing = Get-Command ark -CommandType Application -ErrorAction SilentlyContinue |
        Select-Object -First 1
    if ($existing -and $existing.Source) {
        $InstallDir = Split-Path -Parent $existing.Source
    } else {
        $InstallDir = Join-Path $HOME '.local\bin'
    }
}
$InstallDir = [System.IO.Path]::GetFullPath($InstallDir)
New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null

function ConvertTo-NormalizedPath([string]$PathEntry) {
    if (-not $PathEntry) { return $null }
    $expanded = [Environment]::ExpandEnvironmentVariables($PathEntry.Trim().Trim('"'))
    try {
        return [System.IO.Path]::GetFullPath($expanded).TrimEnd('\')
    } catch {
        return $null
    }
}

$tempDir = Join-Path ([System.IO.Path]::GetTempPath()) ("ark-install-" + [guid]::NewGuid())
New-Item -ItemType Directory -Path $tempDir | Out-Null
try {
    $archive = Join-Path $tempDir $asset
    $checksumFile = "$archive.sha256"
    Write-Host "Downloading Ark $Version for Windows $arch..."
    Invoke-WebRequest -Uri "$baseUrl/$asset" -OutFile $archive -UseBasicParsing
    Invoke-WebRequest -Uri "$baseUrl/$asset.sha256" -OutFile $checksumFile -UseBasicParsing

    $expected = ((Get-Content -Raw $checksumFile).Trim() -split '\s+')[0].ToLowerInvariant()
    if ($expected -notmatch '^[0-9a-f]{64}$') {
        throw "Release checksum is invalid: $expected"
    }
    $actual = (Get-FileHash -Algorithm SHA256 $archive).Hash.ToLowerInvariant()
    if ($actual -ne $expected) {
        throw "Checksum mismatch for ${asset}: expected $expected, got $actual"
    }

    $expanded = Join-Path $tempDir 'expanded'
    Expand-Archive -Path $archive -DestinationPath $expanded
    $source = Get-ChildItem -Path $expanded -Filter 'ark.exe' -File -Recurse |
        Select-Object -First 1
    if (-not $source) {
        throw "The release archive did not contain ark.exe"
    }

    $destination = Join-Path $InstallDir 'ark.exe'
    Copy-Item -LiteralPath $source.FullName -Destination $destination -Force

    if (-not $NoPathUpdate) {
        $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
        $entries = @($userPath -split ';' | Where-Object { $_ })
        $normalizedInstallDir = ConvertTo-NormalizedPath $InstallDir
        $alreadyPresent = $entries | Where-Object {
            [string]::Equals(
                (ConvertTo-NormalizedPath $_),
                $normalizedInstallDir,
                [System.StringComparison]::OrdinalIgnoreCase)
        }
        if (-not $alreadyPresent) {
            $newUserPath = (@($entries) + $InstallDir) -join ';'
            [Environment]::SetEnvironmentVariable('Path', $newUserPath, 'User')
            Write-Host "Added $InstallDir to your user PATH."
        }
        if (-not (($env:Path -split ';') -contains $InstallDir)) {
            $env:Path = "$InstallDir;$env:Path"
        }
    }

    $reported = & $destination --version | Select-Object -First 1
    Write-Host "Installed $reported at $destination"
    if (-not $NoPathUpdate) {
        Write-Host 'Open a new terminal, then run: ark --version'
    }
} finally {
    Remove-Item -LiteralPath $tempDir -Recurse -Force -ErrorAction SilentlyContinue
}
