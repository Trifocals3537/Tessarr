# Measure the public CI runner's enabled XSTATE and required exception context
# buffer. This does not inspect a Tessarr process or any user machine.
$ErrorActionPreference = 'Stop'
Add-Type -TypeDefinition @'
using System;
using System.Runtime.InteropServices;

public static class WindowsExceptionContext {
    [DllImport("kernel32.dll", SetLastError = true)]
    public static extern bool InitializeContext(
        IntPtr buffer, uint flags, out IntPtr context, ref uint length);

    [DllImport("kernel32.dll")]
    public static extern ulong GetEnabledXStateFeatures();
}
'@

# AMD64 CONTEXT_ALL | CONTEXT_XSTATE. A null buffer returns the required length.
$contextLength = [uint32]0
$contextPointer = [IntPtr]::Zero
$initialized = [WindowsExceptionContext]::InitializeContext(
    [IntPtr]::Zero, [uint32]0x10005F, [ref]$contextPointer, [ref]$contextLength)
$win32Error = [Runtime.InteropServices.Marshal]::GetLastWin32Error()
if ($initialized -or $win32Error -ne 122 -or $contextLength -eq 0) {
    throw "InitializeContext length query failed: result=$initialized error=$win32Error length=$contextLength"
}

[pscustomobject]@{
    processor = (Get-CimInstance Win32_Processor | Select-Object -First 1 -ExpandProperty Name).Trim()
    enabled_xstate_mask = ('0x{0:x}' -f [WindowsExceptionContext]::GetEnabledXStateFeatures())
    context_size_bytes = $contextLength
} | ConvertTo-Json -Compress
