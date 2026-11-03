# Patch Wires-X.exe (Ver-2.010A) so RDP connects and disconnects don't end the
# WIRES-X room. See docs/wires-x-rdp-patch.md.
#
#   wires-x-rdp-patch.ps1            patch, keeping the original as Wires-X.orig.exe
#   wires-x-rdp-patch.ps1 -Restore   put the original back
#
# Run elevated, with WIRES-X closed. Refuses any build other than 2.010A: the
# offsets have to be found again for another version.
param(
    [string]$Path = 'C:\Program Files (x86)\YAESUMUSEN\WIRES-X\Wires-X.exe',
    [switch]$Restore
)
$ErrorActionPreference = 'Stop'

$originalHash = '8F05DC26794144438566B880CE775F5178D62EB3C4DEB8C053089216342BC671'
$patchedHash  = '7DD91B5B974427FA894D84F2C25F7DF2A756C60C1AA488901A4FE21E6F395467'

# File offset, original bytes, patched bytes.
$patches = @(
    # 0x4A2C7C jne -> jmp: WM_DEVICECHANGE / DBT_DEVNODES_CHANGED skips the
    # audio endpoint check.
    @{ Offset = 0xA207C; Old = [byte[]](0x75);       New = [byte[]](0xEB) },
    # 0x4D216D jl rel32 -> nop; jmp rel32: the capture watchdog never resets
    # the input device.
    @{ Offset = 0xD156D; Old = [byte[]](0x0F, 0x8C); New = [byte[]](0x90, 0xE9) }
)

$backup = Join-Path (Split-Path $Path) 'Wires-X.orig.exe'

$full = (Resolve-Path $Path).Path
if (Get-Process Wires-X -ErrorAction SilentlyContinue | Where-Object Path -eq $full) {
    throw 'WIRES-X is running from this file: close it first'
}

if ($Restore) {
    if ((Get-FileHash $backup).Hash -ne $originalHash) { throw "$backup is not the original 2.010A" }
    Copy-Item $backup $Path -Force
    "restored the original: $((Get-FileHash $Path).Hash)"
    return
}

$hash = (Get-FileHash $Path).Hash
if ($hash -eq $patchedHash) {
    'already patched'
    return
}
if ($hash -ne $originalHash) {
    throw "unknown Wires-X.exe ${hash}: not 2.010A, so the offsets need finding again"
}

if (Test-Path $backup) {
    if ((Get-FileHash $backup).Hash -ne $originalHash) { throw "$backup exists and is not the original" }
} else {
    Copy-Item $Path $backup
    "kept the original as $backup"
}

$bytes = [IO.File]::ReadAllBytes($Path)
foreach ($p in $patches) {
    for ($i = 0; $i -lt $p.Old.Length; $i++) {
        if ($bytes[$p.Offset + $i] -ne $p.Old[$i]) {
            throw ('unexpected byte at 0x{0:X}' -f ($p.Offset + $i))
        }
        $bytes[$p.Offset + $i] = $p.New[$i]
    }
}
[IO.File]::WriteAllBytes($Path, $bytes)

$hash = (Get-FileHash $Path).Hash
if ($hash -ne $patchedHash) { throw "patched file has hash $hash, expected $patchedHash" }
'patched'
