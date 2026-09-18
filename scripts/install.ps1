#Requires -Version 5.1
#
# Install NanoASR on Windows as a service.
#
#   irm https://github.com/usunrise88/NanoASR/releases/latest/download/install.ps1 | iex
#
# The one-liner is the whole interface: it works out the latest release, checks
# that this machine can run it, unpacks the archive into Program Files, writes a
# configuration with two API keys under ProgramData, downloads the models and
# hands the registration to the binary itself: `nanoasr.exe --install` is what
# creates the service, so the two can never disagree about how it is registered.
#
# Because a piped script cannot take parameters, every option is also an
# environment variable:
#
#   $env:NANOASR_ADDR = "0.0.0.0:8080"
#   irm https://github.com/usunrise88/NanoASR/releases/latest/download/install.ps1 | iex
#
# With the script on disk the parameters work as parameters:
#
#   .\install.ps1 -Addr 0.0.0.0:8080 -NoDownload
#   .\install.ps1 -Uninstall -Purge
#
# It elevates itself: without administrative rights it reopens in a window that
# has them, because nothing below (Program Files, the service control manager,
# the machine PATH, the firewall) is possible without.

[CmdletBinding()]
param(
    [string] $Version     = $env:NANOASR_VERSION,
    [string] $Prefix      = $(if ($env:NANOASR_PREFIX)   { $env:NANOASR_PREFIX }   else { Join-Path $env:ProgramFiles 'NanoASR' }),
    [string] $DataDir     = $(if ($env:NANOASR_DATA_DIR) { $env:NANOASR_DATA_DIR } else { Join-Path $env:ProgramData 'NanoASR' }),
    [string] $Addr        = $(if ($env:NANOASR_ADDR)     { $env:NANOASR_ADDR }     else { '127.0.0.1:8080' }),
    [string] $ServiceName = $(if ($env:NANOASR_SERVICE)  { $env:NANOASR_SERVICE }  else { 'NanoASR' }),
    [string] $Repo        = $(if ($env:NANOASR_REPO)     { $env:NANOASR_REPO }     else { 'usunrise88/NanoASR' }),
    [switch] $NoUI        = [bool]($env:NANOASR_UI       -eq '0'),
    [switch] $NoDownload  = [bool]($env:NANOASR_DOWNLOAD -eq '0'),
    [switch] $NoFfmpeg    = [bool]($env:NANOASR_FFMPEG   -eq '0'),
    [switch] $NoStart     = [bool]($env:NANOASR_START    -eq '0'),
    [switch] $NoPath      = [bool]($env:NANOASR_PATH     -eq '0'),
    [switch] $Uninstall,
    [switch] $Purge,
    [switch] $Help
)

$ErrorActionPreference = 'Stop'
# Invoke-WebRequest spends most of a download drawing the progress bar, and this
# one is twenty megabytes.
$ProgressPreference = 'SilentlyContinue'
# Windows PowerShell 5.1 still defaults to TLS 1.0 on older builds, which GitHub
# closes the connection on.
[Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12

$SelfUrl = "https://github.com/$Repo/releases/latest/download/install.ps1"
$Config  = Join-Path $DataDir 'nanoasr.yaml'
$Exe     = Join-Path $Prefix 'nanoasr.exe'

function Say  ($m) { Write-Host "==> $m" -ForegroundColor Cyan }
function Warn ($m) { Write-Host "    warning: $m" -ForegroundColor Yellow }
function Note ($m) { Write-Host $m }

function Show-Usage {
    @"
Install NanoASR as a Windows service.

  -Version TAG      release to install                   (default: the latest)
  -Prefix DIR       binary and libraries                 ($Prefix)
  -DataDir DIR      configuration, models, database      ($DataDir)
  -Addr HOST:PORT   listen address                       ($Addr)
  -ServiceName NAME name in the service control manager  ($ServiceName)
  -NoUI             install the build without the web interface
  -NoDownload       write the configuration but fetch no models
  -NoFfmpeg         do not install ffmpeg
  -NoStart          register the service but leave it stopped
  -NoPath           do not add the installation to the machine PATH
  -Uninstall        stop and remove the service and the installation
  -Purge            with -Uninstall: also delete the models and the database

Each has an environment variable, for the piped form where parameters cannot be
passed: NANOASR_VERSION, NANOASR_PREFIX, NANOASR_DATA_DIR, NANOASR_ADDR,
NANOASR_SERVICE, NANOASR_UI=0, NANOASR_DOWNLOAD=0, NANOASR_FFMPEG=0,
NANOASR_START=0, NANOASR_PATH=0, NANOASR_UNINSTALL=1, NANOASR_PURGE=1.
"@
}

function Test-Administrator {
    $id = [Security.Principal.WindowsIdentity]::GetCurrent()
    (New-Object Security.Principal.WindowsPrincipal $id).IsInRole(
        [Security.Principal.WindowsBuiltInRole]::Administrator)
}

# Invoke-Elevated reopens this script in a window that has administrative
# rights.
#
# Every option is passed explicitly rather than relied on to carry over: an
# elevated process is started through the UAC broker and does not inherit the
# environment of the window that asked for it, so the NANOASR_* variables that
# configure a piped run would quietly be lost. And when the script arrived
# through a pipe there is no file to reopen, so it is fetched first.
function Invoke-Elevated {
    $script = $PSCommandPath
    if (-not $script -or -not (Test-Path -LiteralPath $script)) {
        $script = Join-Path $env:TEMP 'nanoasr-install.ps1'
        Say "downloading the installer so it can be reopened as administrator"
        Invoke-WebRequest -UseBasicParsing -Uri $SelfUrl -OutFile $script
    }

    $a = @('-NoProfile', '-ExecutionPolicy', 'Bypass', '-NoExit', '-File', "`"$script`"",
           '-Prefix', "`"$Prefix`"", '-DataDir', "`"$DataDir`"", '-Addr', "`"$Addr`"",
           '-ServiceName', "`"$ServiceName`"", '-Repo', "`"$Repo`"")
    if ($Version)    { $a += @('-Version', "`"$Version`"") }
    foreach ($s in 'NoUI', 'NoDownload', 'NoFfmpeg', 'NoStart', 'NoPath', 'Uninstall', 'Purge') {
        if ((Get-Variable -Name $s -ValueOnly)) { $a += "-$s" }
    }

    Say "asking for administrative rights"
    try {
        Start-Process -FilePath (Get-Process -Id $PID).Path -Verb RunAs -ArgumentList $a
    } catch {
        throw "administrative rights are required, and the request was refused. " +
              "Open PowerShell with `"Run as administrator`" and run this again."
    }
    # -NoExit above keeps that window open, because the API keys are printed
    # there and a window that closes on its own takes them with it.
    Note "Continuing in the elevated window; the API keys are printed there."
}

# Invoke-Native runs the binary, shows what it printed and keeps a copy.
#
# The error preference is relaxed for the call because a native program writing
# to stderr, which nanoasr does for every note that is not the result, is
# turned into a terminating error by "Stop", and the exit code is the thing that
# actually says whether it worked.
function Invoke-Native {
    param([string] $File, [string[]] $Arguments)

    $previous = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    try {
        $out = & $File @Arguments 2>&1 | ForEach-Object { $line = "$_"; Write-Host $line; $line }
    } finally {
        $ErrorActionPreference = $previous
    }
    if ($LASTEXITCODE -ne 0) {
        throw "$(Split-Path -Leaf $File) $($Arguments -join ' ') failed with exit code $LASTEXITCODE"
    }
    return $out
}

function Test-ConfigHasKeys {
    if (-not (Test-Path -LiteralPath $Config) -or -not (Test-Path -LiteralPath $Exe)) { return $false }
    $previous = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    try   { $out = (& $Exe key list -config $Config 2>&1 | Out-String) }
    finally { $ErrorActionPreference = $previous }
    return ($LASTEXITCODE -eq 0 -and $out -notmatch 'no keys in')
}

# Get-LatestTag asks GitHub which release is current: the API first, and the
# redirect that /releases/latest performs when the API has had enough requests
# from this address today.
function Get-LatestTag {
    try {
        return (Invoke-RestMethod -UseBasicParsing -Uri "https://api.github.com/repos/$Repo/releases/latest").tag_name
    } catch {
        try {
            $req = [Net.WebRequest]::Create("https://github.com/$Repo/releases/latest")
            $req.AllowAutoRedirect = $false
            $req.Method = 'HEAD'
            $location = $req.GetResponse().Headers['Location']
            if ($location) { return $location.Split('/')[-1] }
        } catch { }
    }
    throw "cannot work out the latest release; pass -Version TAG"
}

# Test-Checksum checks the archive against the published checksums. HTTPS
# already says the bytes came from GitHub; this says that all of them arrived.
function Test-Checksum {
    param([string] $Dir, [string] $File, [string] $Base)

    $sums = Join-Path $Dir 'sha256sums.txt'
    try {
        Invoke-WebRequest -UseBasicParsing -Uri "$Base/sha256sums.txt" -OutFile $sums
    } catch {
        Warn "$Version publishes no sha256sums.txt, so $File was not verified"
        return
    }
    $pattern = "\s\*?$([regex]::Escape($File))$"
    $line = Get-Content -LiteralPath $sums | Where-Object { $_ -match $pattern } | Select-Object -First 1
    if (-not $line) { throw "sha256sums.txt does not mention $File" }

    $want = ($line -split '\s+')[0]
    $got  = (Get-FileHash -LiteralPath (Join-Path $Dir $File) -Algorithm SHA256).Hash
    if ($want -ine $got) { throw "$File failed its checksum: expected $want, got $got" }
    Say "checksum ok"
}

function Get-ProbeUrl {
    $parts = $Addr -split ':'
    $port  = $parts[-1]
    $host_ = ($parts[0..($parts.Length - 2)] -join ':')
    if ($host_ -in @('', '0.0.0.0', '*', '::', '[::]')) { $host_ = '127.0.0.1' }
    return "http://${host_}:${port}"
}

function Test-Loopback {
    $host_ = ($Addr -split ':')[0]
    return ($host_ -in @('127.0.0.1', 'localhost', '[::1]', '::1'))
}

# Install-Ffmpeg is a convenience, not a dependency: without ffmpeg NanoASR
# accepts WAV and raw PCM and answers 415 to everything else, which is a
# surprising way to discover that a package is missing.
function Install-Ffmpeg {
    if ($NoFfmpeg) { return }
    if (Get-Command ffmpeg -ErrorAction SilentlyContinue) { return }
    if (-not (Get-Command winget -ErrorAction SilentlyContinue)) {
        Warn "ffmpeg is not installed and winget is not here; without it only WAV and raw PCM are accepted"
        return
    }
    Say "installing ffmpeg with winget (mp3, m4a, ogg and the rest need it)"
    try {
        Invoke-Native 'winget' @('install', '--id', 'Gyan.FFmpeg', '-e', '--silent',
                                 '--accept-package-agreements', '--accept-source-agreements') | Out-Null
    } catch {
        Warn "ffmpeg could not be installed ($($_.Exception.Message)); WAV and raw PCM still work"
    }
}

function Add-ToMachinePath {
    if ($NoPath) { return }
    $current = [Environment]::GetEnvironmentVariable('Path', 'Machine')
    if (-not $current) { $current = '' }
    if (($current -split ';') -contains $Prefix) { return }
    Say "adding $Prefix to the machine PATH"
    [Environment]::SetEnvironmentVariable('Path', ($current.TrimEnd(';') + ";$Prefix"), 'Machine')
    $env:Path = "$env:Path;$Prefix"
}

function Remove-FromMachinePath {
    $current = [Environment]::GetEnvironmentVariable('Path', 'Machine')
    $kept = ($current -split ';') | Where-Object { $_ -and $_ -ne $Prefix }
    [Environment]::SetEnvironmentVariable('Path', ($kept -join ';'), 'Machine')
}

# Open-Firewall is only reached for an address that is not the loopback, which
# is a deliberate decision by whoever passed it: a port nobody can reach is not
# what -Addr 0.0.0.0:8080 was asking for. It is announced rather than silent.
function Open-Firewall {
    $port = ($Addr -split ':')[-1]
    $rule = "NanoASR ($port/tcp)"
    try {
        if (Get-NetFirewallRule -DisplayName $rule -ErrorAction SilentlyContinue) { return }
        Say "opening TCP $port in the firewall, because $Addr is reachable from outside this machine"
        New-NetFirewallRule -DisplayName $rule -Direction Inbound -Action Allow `
            -Protocol TCP -LocalPort $port -Profile Any | Out-Null
    } catch {
        Warn "the firewall rule was not created: $($_.Exception.Message)"
    }
}

function Close-Firewall {
    try {
        Get-NetFirewallRule -DisplayName 'NanoASR (*' -ErrorAction SilentlyContinue |
            Remove-NetFirewallRule -ErrorAction SilentlyContinue
    } catch { }
}

function Wait-ForHealth {
    $url = Get-ProbeUrl
    for ($i = 0; $i -lt 60; $i++) {
        try {
            Invoke-WebRequest -UseBasicParsing -Uri "$url/healthz" -TimeoutSec 2 | Out-Null
            Say "$ServiceName answers on $url"
            return
        } catch { }
        $svc = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
        if ($svc -and $svc.Status -ne 'Running') {
            Warn "the service is $($svc.Status); the reason is in $DataDir\logs\nanoasr.log"
            return
        }
        Start-Sleep -Seconds 1
    }
    Warn "no answer from $url/healthz yet; loading the weights takes a while ($DataDir\logs\nanoasr.log)"
}

function Install-NanoASR {
    # PROCESSOR_ARCHITECTURE describes the PowerShell host, not the machine: a
    # 32-bit host on 64-bit Windows says x86 and puts the truth in the other
    # variable. Reading only the first would refuse to install on a machine that
    # is perfectly able to run this.
    $arch = $env:PROCESSOR_ARCHITEW6432
    if (-not $arch) { $arch = $env:PROCESSOR_ARCHITECTURE }
    if ($arch -eq 'ARM64') {
        Warn "this is an ARM64 machine; the x64 build runs under emulation and will be slow"
    } elseif ($arch -ne 'AMD64') {
        throw "the published builds are x86-64 and this is $arch"
    }

    if (-not $Version) {
        Say "asking GitHub for the latest release"
        $script:Version = Get-LatestTag
    }
    $suffix  = if ($NoUI) { '-noui' } else { '' }
    $archive = "nanoasr-$Version-windows-amd64$suffix.zip"
    $base    = "https://github.com/$Repo/releases/download/$Version"

    $tmp = Join-Path ([IO.Path]::GetTempPath()) ("nanoasr-" + [Guid]::NewGuid().ToString('N'))
    New-Item -ItemType Directory -Path $tmp | Out-Null
    try {
        Say "downloading $archive"
        try {
            Invoke-WebRequest -UseBasicParsing -Uri "$base/$archive" -OutFile (Join-Path $tmp $archive)
        } catch {
            throw "no such release asset: $base/$archive"
        }
        Test-Checksum -Dir $tmp -File $archive -Base $base

        # Stopped before anything is overwritten: Windows refuses to replace the
        # exe and the DLLs of a running process, so an upgrade that skipped this
        # would fail halfway through and leave a mixture behind.
        $existing = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
        if ($existing -and $existing.Status -ne 'Stopped') {
            Say "stopping the running $ServiceName service"
            Stop-Service -Name $ServiceName -Force
            $existing.WaitForStatus('Stopped', [TimeSpan]::FromSeconds(90))
        }

        New-Item -ItemType Directory -Path $Prefix, $DataDir -Force | Out-Null
        Say "unpacking into $Prefix"
        Expand-Archive -LiteralPath (Join-Path $tmp $archive) -DestinationPath $Prefix -Force
        if (-not (Test-Path -LiteralPath $Exe)) {
            throw "$archive did not contain nanoasr.exe"
        }
    } finally {
        Remove-Item -LiteralPath $tmp -Recurse -Force -ErrorAction SilentlyContinue
    }

    Install-Ffmpeg
    Add-ToMachinePath

    # The configuration lives beside the models rather than in Program Files:
    # everything that changes after an installation belongs in one place, and
    # that place is the one the binary looks in when it registers the service.
    $keys = @()
    if (Test-ConfigHasKeys) {
        Say "keeping the configuration in $Config"
    } else {
        Say "writing the configuration and fetching the models (this is gigabytes)"
        $initArgs = @('init', '-config', $Config, '-data-dir', $DataDir, '-addr', $Addr, '-force')
        if ($NoDownload) { $initArgs += '-no-download' }
        $out  = Invoke-Native $Exe $initArgs
        $keys = @($out | Where-Object { $_ -match '^(admin|user) key' })
    }

    Say "registering the $ServiceName service"
    $svcArgs = @('service', 'install', '-name', $ServiceName, '-config', $Config)
    if ($NoStart) { $svcArgs += '-no-start' }
    Invoke-Native $Exe $svcArgs | Out-Null

    if (-not (Test-Loopback)) { Open-Firewall }
    if (-not $NoStart) { Wait-ForHealth }

    $url = Get-ProbeUrl
    Note ""
    Note "NanoASR $Version is installed."
    Note ""
    Note "  service   $ServiceName          Get-Service $ServiceName"
    Note "  binary    $Exe"
    Note "  config    $Config"
    Note "  data      $DataDir"
    Note "  log       $DataDir\logs\nanoasr.log"
    Note "  url       $url    ($url/ui in a browser)"
    Note ""
    if ($keys.Count) {
        $keys | ForEach-Object { Note "  $_" }
    } else {
        Note "  The API keys are in $Config (nanoasr key list -config `"$Config`")."
    }
    Note ""
    Note "  restart    nanoasr --restart        (or Restart-Service $ServiceName)"
    Note "  status     nanoasr --status"
    Note "  update     irm https://github.com/$Repo/releases/latest/download/install.ps1 | iex"
    Note "  uninstall  `$env:NANOASR_UNINSTALL=1; irm https://github.com/$Repo/releases/latest/download/install.ps1 | iex"
}

function Uninstall-NanoASR {
    if (Test-Path -LiteralPath $Exe) {
        Say "removing the $ServiceName service"
        try   { Invoke-Native $Exe @('service', 'uninstall', '-name', $ServiceName) | Out-Null }
        catch { Warn $_.Exception.Message }
    } elseif (Get-Service -Name $ServiceName -ErrorAction SilentlyContinue) {
        # The binary is already gone, so ask the control manager directly.
        Say "removing the $ServiceName service with sc.exe"
        Stop-Service -Name $ServiceName -Force -ErrorAction SilentlyContinue
        & sc.exe delete $ServiceName | Out-Null
    }

    Close-Firewall
    Remove-FromMachinePath

    if (Test-Path -LiteralPath $Prefix) {
        Say "removing $Prefix"
        Remove-Item -LiteralPath $Prefix -Recurse -Force -ErrorAction SilentlyContinue
    }
    if ($Purge) {
        Say "removing $DataDir"
        Remove-Item -LiteralPath $DataDir -Recurse -Force -ErrorAction SilentlyContinue
    } else {
        Say "kept $DataDir - the configuration, the models and the job database; -Purge deletes it"
    }
    Say "done"
}

if ($Help) { Show-Usage; return }

# The piped form cannot pass -Uninstall either.
if ($env:NANOASR_UNINSTALL -eq '1') { $Uninstall = [switch]$true }
if ($env:NANOASR_PURGE -eq '1')     { $Purge     = [switch]$true }

if (-not (Test-Administrator)) {
    Invoke-Elevated
    return
}

try {
    if ($Uninstall) { Uninstall-NanoASR } else { Install-NanoASR }
} catch {
    Write-Host ""
    Write-Host "install.ps1: $($_.Exception.Message)" -ForegroundColor Red
    exit 1
}
