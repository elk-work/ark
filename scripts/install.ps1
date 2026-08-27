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
# The tag is interpolated into a download URL and into a file name under the
# temp directory, so keep it to the shape a release tag actually has. Without
# this, "-Version ..\..\x" would both retarget the download and write outside
# the temp directory.
if ($Version -notmatch '^v[0-9A-Za-z][0-9A-Za-z.+_-]*$') {
    throw "Invalid release version: $Version"
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

# Writing PATH straight to the registry does not tell running programs that
# the environment changed; [Environment]::SetEnvironmentVariable used to do
# that for us. Broadcast it so a terminal opened from Explorer afterwards
# inherits the new PATH instead of waiting for a sign-out. Best effort: a
# machine that will not let us P/Invoke still gets a correct install.
function Publish-EnvironmentChange {
    try {
        if (-not ('ArkInstaller.NativeMethods' -as [type])) {
            Add-Type -Namespace 'ArkInstaller' -Name 'NativeMethods' -MemberDefinition @'
[System.Runtime.InteropServices.DllImport("user32.dll", SetLastError = true, CharSet = System.Runtime.InteropServices.CharSet.Auto)]
public static extern System.IntPtr SendMessageTimeout(
    System.IntPtr hWnd, uint Msg, System.IntPtr wParam, string lParam,
    uint fuFlags, uint uTimeout, out System.UIntPtr lpdwResult);
'@
        }
        $result = [System.UIntPtr]::Zero
        # HWND_BROADCAST, WM_SETTINGCHANGE, SMTO_ABORTIFHUNG, 5s.
        [void][ArkInstaller.NativeMethods]::SendMessageTimeout(
            [System.IntPtr]0xffff, 0x001A, [System.IntPtr]::Zero, 'Environment',
            0x0002, 5000, [ref]$result)
    } catch {
        Write-Verbose "Could not broadcast the environment change: $_"
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
    # Windows refuses to overwrite a running executable but will happily
    # rename one, so move the old binary aside first: an upgrade started while
    # another shell still has ark open otherwise dies on a raw IOException.
    # Sweep any copy an earlier upgrade could not delete because it was still
    # mapped at the time.
    Get-ChildItem -Path $InstallDir -Filter 'ark.exe.old-*' -File -ErrorAction SilentlyContinue |
        ForEach-Object { Remove-Item -LiteralPath $_.FullName -Force -ErrorAction SilentlyContinue }
    $displaced = $null
    if (Test-Path -LiteralPath $destination) {
        $candidate = "$destination.old-" + [guid]::NewGuid().ToString('N').Substring(0, 8)
        try {
            Move-Item -LiteralPath $destination -Destination $candidate -Force -ErrorAction Stop
            $displaced = $candidate
        } catch {
            # Not renameable either; let Copy-Item report the real problem.
            $displaced = $null
        }
    }
    try {
        Copy-Item -LiteralPath $source.FullName -Destination $destination -Force -ErrorAction Stop
    } catch {
        if ($displaced) {
            # Never leave the user without the ark they already had.
            Move-Item -LiteralPath $displaced -Destination $destination -Force -ErrorAction SilentlyContinue
        }
        throw
    }
    if ($displaced) {
        Remove-Item -LiteralPath $displaced -Force -ErrorAction SilentlyContinue
    }

    if (-not $NoPathUpdate) {
        # Read and write the user PATH through the registry, preserving its
        # value kind. [Environment]::GetEnvironmentVariable(..., 'User')
        # EXPANDS a REG_EXPAND_SZ PATH and SetEnvironmentVariable writes the
        # result back as REG_SZ, so an entry like "%JAVA_HOME%\bin" would be
        # frozen to today's value permanently -- a silent, unrecoverable
        # change to the user's environment that this installer has no
        # business making.
        $environmentKey =
            [Microsoft.Win32.Registry]::CurrentUser.OpenSubKey('Environment', $true)
        try {
            $userPath = [string]$environmentKey.GetValue(
                'Path', '',
                [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames)
            $valueKind = [Microsoft.Win32.RegistryValueKind]::ExpandString
            if (@($environmentKey.GetValueNames()) -contains 'Path') {
                $valueKind = $environmentKey.GetValueKind('Path')
            }
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
                $environmentKey.SetValue('Path', $newUserPath, $valueKind)
                Publish-EnvironmentChange
                Write-Host "Added $InstallDir to your user PATH."
            }
        } finally {
            $environmentKey.Close()
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
