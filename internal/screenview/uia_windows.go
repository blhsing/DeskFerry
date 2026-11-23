//go:build windows

package screenview

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"

	"golang.org/x/sys/windows"
)

const uiAutomationTimeout = 20 * time.Second

// uiAutomationScript reads the UI Automation tree. Parameters arrive in
// environment variables, never in the script text, so remote input cannot
// change the code that runs.
const uiAutomationScript = `
$ErrorActionPreference = 'Stop'
Add-Type -TypeDefinition @'
using System;
using System.Runtime.InteropServices;
public static class DeskFerryNative {
  [DllImport("user32.dll")] public static extern IntPtr SetProcessDpiAwarenessContext(IntPtr value);
  [DllImport("user32.dll")] public static extern IntPtr GetForegroundWindow();
}
'@
[void][DeskFerryNative]::SetProcessDpiAwarenessContext([IntPtr](-4))
Add-Type -AssemblyName UIAutomationClient
Add-Type -AssemblyName UIAutomationTypes
$mode = $env:DESKFERRY_UIA_MODE
$depthMax = [int]$env:DESKFERRY_UIA_DEPTH
$max = [int]$env:DESKFERRY_UIA_MAX
$text = $env:DESKFERRY_UIA_TEXT
$scope = $env:DESKFERRY_UIA_SCOPE
$AE = [System.Windows.Automation.AutomationElement]
if ($scope -eq 'desktop') {
  $root = $AE::RootElement
} elseif ($scope -eq 'focused') {
  $root = $AE::FocusedElement
} else {
  $handle = [DeskFerryNative]::GetForegroundWindow()
  if ($handle -eq [IntPtr]::Zero) { $root = $AE::RootElement } else { $root = $AE::FromHandle($handle) }
}
$nodes = New-Object System.Collections.Generic.List[object]
function Describe($element, $depth) {
  $current = $element.Current
  $bounds = $current.BoundingRectangle
  if ($bounds.IsEmpty -or [double]::IsInfinity($bounds.X)) { $rect = @(0, 0, 0, 0) } else { $rect = @([int]$bounds.X, [int]$bounds.Y, [int]$bounds.Width, [int]$bounds.Height) }
  $value = ''
  try {
    $pattern = $null
    if ($element.TryGetCurrentPattern([System.Windows.Automation.ValuePattern]::Pattern, [ref]$pattern)) { $value = $pattern.Current.Value }
  } catch {}
  if ($value -and $value.Length -gt 2000) { $value = $value.Substring(0, 2000) }
  [ordered]@{
    name = $current.Name
    role = ($current.ControlType.ProgrammaticName -replace '^ControlType\.', '')
    value = $value
    automation_id = $current.AutomationId
    class = $current.ClassName
    enabled = $current.IsEnabled
    focused = $current.HasKeyboardFocus
    rect = $rect
    depth = $depth
  }
}
if ($mode -eq 'find') {
  $all = $root.FindAll([System.Windows.Automation.TreeScope]::Descendants, [System.Windows.Automation.Condition]::TrueCondition)
  foreach ($element in $all) {
    if ($nodes.Count -ge $max) { break }
    try { $name = $element.Current.Name; $id = $element.Current.AutomationId } catch { continue }
    $match = ($name -and $name.IndexOf($text, [StringComparison]::OrdinalIgnoreCase) -ge 0) -or ($id -and $id.IndexOf($text, [StringComparison]::OrdinalIgnoreCase) -ge 0)
    if ($match) { try { $nodes.Add((Describe $element 0)) } catch {} }
  }
} else {
  $walker = [System.Windows.Automation.TreeWalker]::ControlViewWalker
  $stack = New-Object System.Collections.Generic.Stack[object]
  $stack.Push([object[]]@($root, 0))
  while ($stack.Count -gt 0 -and $nodes.Count -lt $max) {
    $item = $stack.Pop()
    $element = $item[0]
    $depth = [int]$item[1]
    try { $nodes.Add((Describe $element $depth)) } catch { continue }
    if ($depth -ge $depthMax) { continue }
    $children = New-Object System.Collections.Generic.List[object]
    try {
      $child = $walker.GetFirstChild($element)
      while ($child -ne $null) { $children.Add($child); $child = $walker.GetNextSibling($child) }
    } catch {}
    for ($index = $children.Count - 1; $index -ge 0; $index--) { $stack.Push([object[]]@($children[$index], ($depth + 1))) }
  }
}
[Console]::OutputEncoding = New-Object System.Text.UTF8Encoding $false
[Console]::Out.Write((ConvertTo-Json -InputObject @($nodes.ToArray()) -Compress -Depth 4))
`

func runUIAutomation(mode string, command Command) ([]UINode, error) {
	depth := command.Depth
	if depth <= 0 {
		depth = 6
	}
	depth = min(depth, 30)
	maxNodes := command.MaxNodes
	if mode == "find" {
		maxNodes = command.Max
		if maxNodes <= 0 {
			maxNodes = 20
		}
	}
	if maxNodes <= 0 {
		maxNodes = 400
	}
	maxNodes = min(maxNodes, 5000)
	scope := command.Scope
	if scope != "desktop" && scope != "focused" {
		scope = "foreground"
	}
	systemRoot := os.Getenv("SystemRoot")
	if systemRoot == "" {
		systemRoot = `C:\Windows`
	}
	powershell := filepath.Join(systemRoot, "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
	ctx, cancel := context.WithTimeout(context.Background(), uiAutomationTimeout)
	defer cancel()
	encoded := base64.StdEncoding.EncodeToString(utf16LE(uiAutomationScript))
	process := exec.CommandContext(ctx, powershell, "-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-EncodedCommand", encoded)
	process.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	process.Env = append(os.Environ(),
		"DESKFERRY_UIA_MODE="+mode,
		"DESKFERRY_UIA_DEPTH="+strconv.Itoa(depth),
		"DESKFERRY_UIA_MAX="+strconv.Itoa(maxNodes),
		"DESKFERRY_UIA_TEXT="+command.Text,
		"DESKFERRY_UIA_SCOPE="+scope,
	)
	var stdout, stderr bytes.Buffer
	process.Stdout = &stdout
	process.Stderr = &stderr
	err := process.Run()
	if ctx.Err() != nil {
		return nil, commandError("timeout", "UI Automation did not answer within %s", uiAutomationTimeout)
	}
	if err != nil {
		message := strings.TrimSpace(stderr.String())
		if len(message) > 400 {
			message = message[:400]
		}
		return nil, commandError("failed", "UI Automation failed: %v %s", err, message)
	}
	output := bytes.TrimPrefix(bytes.TrimSpace(stdout.Bytes()), []byte("\xef\xbb\xbf"))
	var nodes []UINode
	if len(output) == 0 {
		return nodes, nil
	}
	if err := json.Unmarshal(output, &nodes); err != nil {
		return nil, commandError("failed", "UI Automation returned unreadable output: %v", err)
	}
	return nodes, nil
}

func utf16LE(text string) []byte {
	units := utf16.Encode([]rune(text))
	out := make([]byte, len(units)*2)
	for index, unit := range units {
		out[index*2] = byte(unit)
		out[index*2+1] = byte(unit >> 8)
	}
	return out
}
