param(
    [string] $IcoPath = (Join-Path (Split-Path -Parent $PSScriptRoot) 'windows/app.ico'),
    [string] $PreviewPath = (Join-Path (Split-Path -Parent $PSScriptRoot) 'windows/app-icon-256.png')
)

$ErrorActionPreference = 'Stop'

Add-Type -AssemblyName System.Drawing

function New-RectF {
    param([double] $X, [double] $Y, [double] $Width, [double] $Height)
    return New-Object System.Drawing.RectangleF -ArgumentList @([single] $X, [single] $Y, [single] $Width, [single] $Height)
}

function New-PointF {
    param([double] $X, [double] $Y)
    return New-Object System.Drawing.PointF -ArgumentList @([single] $X, [single] $Y)
}

function New-RoundedRectPath {
    param([double] $X, [double] $Y, [double] $Width, [double] $Height, [double] $Radius)

    $path = New-Object System.Drawing.Drawing2D.GraphicsPath
    $diameter = [single] ($Radius * 2)
    $path.AddArc([single] $X, [single] $Y, $diameter, $diameter, 180, 90)
    $path.AddArc([single] ($X + $Width - $diameter), [single] $Y, $diameter, $diameter, 270, 90)
    $path.AddArc([single] ($X + $Width - $diameter), [single] ($Y + $Height - $diameter), $diameter, $diameter, 0, 90)
    $path.AddArc([single] $X, [single] ($Y + $Height - $diameter), $diameter, $diameter, 90, 90)
    $path.CloseFigure()
    return $path
}

function New-Argb {
    param([int] $Alpha, [int] $Red, [int] $Green, [int] $Blue)
    return [System.Drawing.Color]::FromArgb($Alpha, $Red, $Green, $Blue)
}

function Render-ClientIcon {
    param([int] $Size)

    $bitmap = New-Object System.Drawing.Bitmap -ArgumentList @($Size, $Size, [System.Drawing.Imaging.PixelFormat]::Format32bppArgb)
    $graphics = [System.Drawing.Graphics]::FromImage($bitmap)
    $graphics.SmoothingMode = [System.Drawing.Drawing2D.SmoothingMode]::AntiAlias
    $graphics.InterpolationMode = [System.Drawing.Drawing2D.InterpolationMode]::HighQualityBicubic
    $graphics.PixelOffsetMode = [System.Drawing.Drawing2D.PixelOffsetMode]::HighQuality
    $graphics.Clear([System.Drawing.Color]::Transparent)

    $scale = [double] $Size
    $outer = New-RoundedRectPath ($scale * 0.055) ($scale * 0.055) ($scale * 0.89) ($scale * 0.89) ($scale * 0.20)
    $bgRect = New-RectF ($scale * 0.055) ($scale * 0.055) ($scale * 0.89) ($scale * 0.89)
    $bgBrush = New-Object System.Drawing.Drawing2D.LinearGradientBrush -ArgumentList @(
        $bgRect,
        (New-Argb 255 19 50 77),
        (New-Argb 255 64 181 174),
        [System.Drawing.Drawing2D.LinearGradientMode]::ForwardDiagonal
    )
    $graphics.FillPath($bgBrush, $outer)

    $glowPen = New-Object System.Drawing.Pen -ArgumentList @((New-Argb 90 255 255 255), [single] ([Math]::Max(1.0, $scale * 0.018)))
    $graphics.DrawPath($glowPen, $outer)

    $shadowBrush = New-Object System.Drawing.SolidBrush -ArgumentList @((New-Argb 52 3 23 39))
    $whiteBrush = New-Object System.Drawing.SolidBrush -ArgumentList @((New-Argb 252 255 255 255))
    $screenBrush = New-Object System.Drawing.SolidBrush -ArgumentList @((New-Argb 255 23 50 77))
    $hullBrush = New-Object System.Drawing.SolidBrush -ArgumentList @((New-Argb 255 230 109 79))
    $waveBrush = New-Object System.Drawing.SolidBrush -ArgumentList @((New-Argb 235 105 210 199))
    $screenShineBrush = New-Object System.Drawing.SolidBrush -ArgumentList @((New-Argb 34 255 255 255))
    $skySheenBrush = New-Object System.Drawing.SolidBrush -ArgumentList @((New-Argb 22 255 255 255))

    $shadowOffset = [single] ([Math]::Max(1.0, $scale * 0.022))

    $currentPen = New-Object System.Drawing.Pen -ArgumentList @((New-Argb 48 255 255 255), [single] ([Math]::Max(1.0, $scale * 0.010)))
    $currentPen.StartCap = [System.Drawing.Drawing2D.LineCap]::Round
    $currentPen.EndCap = [System.Drawing.Drawing2D.LineCap]::Round
    $softCurrentPen = New-Object System.Drawing.Pen -ArgumentList @((New-Argb 36 255 255 255), [single] ([Math]::Max(1.0, $scale * 0.006)))
    $softCurrentPen.StartCap = [System.Drawing.Drawing2D.LineCap]::Round
    $softCurrentPen.EndCap = [System.Drawing.Drawing2D.LineCap]::Round
    $screenRimPen = New-Object System.Drawing.Pen -ArgumentList @((New-Argb 52 255 255 255), [single] ([Math]::Max(1.0, $scale * 0.005)))
    $screenRimPen.StartCap = [System.Drawing.Drawing2D.LineCap]::Round
    $screenRimPen.EndCap = [System.Drawing.Drawing2D.LineCap]::Round
    $keelPen = New-Object System.Drawing.Pen -ArgumentList @((New-Argb 58 113 42 46), [single] ([Math]::Max(1.0, $scale * 0.008)))
    $keelPen.StartCap = [System.Drawing.Drawing2D.LineCap]::Round
    $keelPen.EndCap = [System.Drawing.Drawing2D.LineCap]::Round
    $glintPen = New-Object System.Drawing.Pen -ArgumentList @((New-Argb 92 255 255 255), [single] ([Math]::Max(1.0, $scale * 0.006)))
    $glintPen.StartCap = [System.Drawing.Drawing2D.LineCap]::Round
    $glintPen.EndCap = [System.Drawing.Drawing2D.LineCap]::Round
    $skySheen = New-Object System.Drawing.Drawing2D.GraphicsPath
    $skySheen.AddBezier((New-PointF ($scale * 0.06) ($scale * 0.31)), (New-PointF ($scale * 0.26) ($scale * 0.17)), (New-PointF ($scale * 0.62) ($scale * 0.17)), (New-PointF ($scale * 0.95) ($scale * 0.08)))
    $skySheen.AddLine((New-PointF ($scale * 0.98) ($scale * 0.20)), (New-PointF ($scale * 0.98) ($scale * 0.20)))
    $skySheen.AddBezier((New-PointF ($scale * 0.98) ($scale * 0.20)), (New-PointF ($scale * 0.66) ($scale * 0.31)), (New-PointF ($scale * 0.29) ($scale * 0.30)), (New-PointF ($scale * 0.07) ($scale * 0.43)))
    $skySheen.CloseFigure()
    $graphics.SetClip($outer)
    $graphics.FillPath($skySheenBrush, $skySheen)
    $graphics.DrawBezier($currentPen, (New-PointF ($scale * 0.11) ($scale * 0.32)), (New-PointF ($scale * 0.28) ($scale * 0.20)), (New-PointF ($scale * 0.48) ($scale * 0.26)), (New-PointF ($scale * 0.63) ($scale * 0.16)))
    $graphics.DrawBezier($softCurrentPen, (New-PointF ($scale * 0.65) ($scale * 0.29)), (New-PointF ($scale * 0.76) ($scale * 0.22)), (New-PointF ($scale * 0.87) ($scale * 0.25)), (New-PointF ($scale * 0.96) ($scale * 0.18)))
    $graphics.DrawBezier($currentPen, (New-PointF ($scale * 0.39) ($scale * 0.90)), (New-PointF ($scale * 0.54) ($scale * 0.80)), (New-PointF ($scale * 0.75) ($scale * 0.86)), (New-PointF ($scale * 0.95) ($scale * 0.73)))
    $graphics.ResetClip()

    $monitorShadow = New-RoundedRectPath ($scale * 0.25) ($scale * 0.18) ($scale * 0.50) ($scale * 0.37) ($scale * 0.055)
    $graphics.TranslateTransform(0, $shadowOffset)
    $graphics.FillPath($shadowBrush, $monitorShadow)
    $graphics.ResetTransform()

    $monitor = New-RoundedRectPath ($scale * 0.25) ($scale * 0.16) ($scale * 0.50) ($scale * 0.37) ($scale * 0.055)
    $graphics.FillPath($whiteBrush, $monitor)
    $screen = New-RoundedRectPath ($scale * 0.31) ($scale * 0.23) ($scale * 0.38) ($scale * 0.22) ($scale * 0.025)
    $graphics.FillPath($screenBrush, $screen)
    $screenShine = New-Object System.Drawing.Drawing2D.GraphicsPath
    $screenShine.AddPolygon([System.Drawing.PointF[]] @(
        (New-PointF ($scale * 0.35) ($scale * 0.23)),
        (New-PointF ($scale * 0.47) ($scale * 0.23)),
        (New-PointF ($scale * 0.38) ($scale * 0.45)),
        (New-PointF ($scale * 0.31) ($scale * 0.45))
    ))
    $graphics.SetClip($screen)
    $graphics.FillPath($screenShineBrush, $screenShine)
    $graphics.DrawLine($screenRimPen, (New-PointF ($scale * 0.38) ($scale * 0.27)), (New-PointF ($scale * 0.61) ($scale * 0.27)))
    $graphics.ResetClip()

    $stand = [System.Drawing.PointF[]] @(
        (New-PointF ($scale * 0.46) ($scale * 0.53)),
        (New-PointF ($scale * 0.54) ($scale * 0.53)),
        (New-PointF ($scale * 0.58) ($scale * 0.64)),
        (New-PointF ($scale * 0.42) ($scale * 0.64))
    )
    $graphics.FillPolygon($whiteBrush, $stand)
    $base = New-RoundedRectPath ($scale * 0.36) ($scale * 0.62) ($scale * 0.28) ($scale * 0.055) ($scale * 0.025)
    $graphics.FillPath($whiteBrush, $base)

    $hullShadow = New-Object System.Drawing.Drawing2D.GraphicsPath
    $hullShadow.AddLine((New-PointF ($scale * 0.18) ($scale * 0.65)), (New-PointF ($scale * 0.84) ($scale * 0.65)))
    $hullShadow.AddLine((New-PointF ($scale * 0.84) ($scale * 0.65)), (New-PointF ($scale * 0.77) ($scale * 0.75)))
    $hullShadow.AddBezier((New-PointF ($scale * 0.77) ($scale * 0.75)), (New-PointF ($scale * 0.69) ($scale * 0.82)), (New-PointF ($scale * 0.37) ($scale * 0.82)), (New-PointF ($scale * 0.29) ($scale * 0.77)))
    $hullShadow.AddLine((New-PointF ($scale * 0.29) ($scale * 0.77)), (New-PointF ($scale * 0.18) ($scale * 0.65)))
    $hullShadow.CloseFigure()
    $graphics.TranslateTransform(0, $shadowOffset)
    $graphics.FillPath($shadowBrush, $hullShadow)
    $graphics.ResetTransform()

    $hull = New-Object System.Drawing.Drawing2D.GraphicsPath
    $hull.AddLine((New-PointF ($scale * 0.18) ($scale * 0.62)), (New-PointF ($scale * 0.84) ($scale * 0.62)))
    $hull.AddLine((New-PointF ($scale * 0.84) ($scale * 0.62)), (New-PointF ($scale * 0.77) ($scale * 0.72)))
    $hull.AddBezier((New-PointF ($scale * 0.77) ($scale * 0.72)), (New-PointF ($scale * 0.68) ($scale * 0.78)), (New-PointF ($scale * 0.38) ($scale * 0.78)), (New-PointF ($scale * 0.29) ($scale * 0.74)))
    $hull.AddLine((New-PointF ($scale * 0.29) ($scale * 0.74)), (New-PointF ($scale * 0.18) ($scale * 0.62)))
    $hull.CloseFigure()
    $graphics.FillPath($hullBrush, $hull)
    $graphics.DrawBezier($keelPen, (New-PointF ($scale * 0.35) ($scale * 0.74)), (New-PointF ($scale * 0.48) ($scale * 0.78)), (New-PointF ($scale * 0.64) ($scale * 0.77)), (New-PointF ($scale * 0.77) ($scale * 0.72)))

    $railPen = New-Object System.Drawing.Pen -ArgumentList @((New-Argb 215 255 255 255), [single] ([Math]::Max(1.2, $scale * 0.018)))
    $railPen.StartCap = [System.Drawing.Drawing2D.LineCap]::Round
    $railPen.EndCap = [System.Drawing.Drawing2D.LineCap]::Round
    $graphics.DrawLine($railPen, (New-PointF ($scale * 0.29) ($scale * 0.65)), (New-PointF ($scale * 0.72) ($scale * 0.65)))

    $wave1 = New-Object System.Drawing.Drawing2D.GraphicsPath
    $wave1.AddLine((New-PointF 0 ($scale * 0.75)), (New-PointF 0 ($scale * 0.75)))
    $wave1.AddBezier((New-PointF 0 ($scale * 0.75)), (New-PointF ($scale * 0.12) ($scale * 0.70)), (New-PointF ($scale * 0.28) ($scale * 0.80)), (New-PointF ($scale * 0.44) ($scale * 0.75)))
    $wave1.AddBezier((New-PointF ($scale * 0.44) ($scale * 0.75)), (New-PointF ($scale * 0.53) ($scale * 0.70)), (New-PointF ($scale * 0.62) ($scale * 0.80)), (New-PointF ($scale * 0.72) ($scale * 0.75)))
    $wave1.AddBezier((New-PointF ($scale * 0.72) ($scale * 0.75)), (New-PointF ($scale * 0.84) ($scale * 0.70)), (New-PointF ($scale * 0.93) ($scale * 0.76)), (New-PointF $scale ($scale * 0.72)))
    $wave1.AddLine((New-PointF $scale $scale), (New-PointF 0 $scale))
    $wave1.CloseFigure()
    $graphics.SetClip($outer)
    $graphics.FillPath($waveBrush, $wave1)

    $wavePen = New-Object System.Drawing.Pen -ArgumentList @((New-Argb 150 255 255 255), [single] ([Math]::Max(1.0, $scale * 0.012)))
    $wavePen.StartCap = [System.Drawing.Drawing2D.LineCap]::Round
    $wavePen.EndCap = [System.Drawing.Drawing2D.LineCap]::Round
    $graphics.DrawBezier($wavePen, (New-PointF ($scale * 0.03) ($scale * 0.80)), (New-PointF ($scale * 0.24) ($scale * 0.73)), (New-PointF ($scale * 0.37) ($scale * 0.87)), (New-PointF ($scale * 0.55) ($scale * 0.80)))
    $graphics.DrawBezier($wavePen, (New-PointF ($scale * 0.55) ($scale * 0.80)), (New-PointF ($scale * 0.68) ($scale * 0.74)), (New-PointF ($scale * 0.83) ($scale * 0.83)), (New-PointF ($scale * 0.98) ($scale * 0.78)))
    $fineWavePen = New-Object System.Drawing.Pen -ArgumentList @((New-Argb 82 255 255 255), [single] ([Math]::Max(1.0, $scale * 0.007)))
    $fineWavePen.StartCap = [System.Drawing.Drawing2D.LineCap]::Round
    $fineWavePen.EndCap = [System.Drawing.Drawing2D.LineCap]::Round
    $graphics.DrawBezier($glintPen, (New-PointF ($scale * 0.16) ($scale * 0.85)), (New-PointF ($scale * 0.21) ($scale * 0.83)), (New-PointF ($scale * 0.27) ($scale * 0.83)), (New-PointF ($scale * 0.32) ($scale * 0.85)))
    $graphics.DrawBezier($glintPen, (New-PointF ($scale * 0.69) ($scale * 0.89)), (New-PointF ($scale * 0.75) ($scale * 0.87)), (New-PointF ($scale * 0.82) ($scale * 0.88)), (New-PointF ($scale * 0.87) ($scale * 0.86)))
    $graphics.DrawBezier($fineWavePen, (New-PointF ($scale * 0.13) ($scale * 0.90)), (New-PointF ($scale * 0.31) ($scale * 0.86)), (New-PointF ($scale * 0.42) ($scale * 0.93)), (New-PointF ($scale * 0.61) ($scale * 0.89)))
    $graphics.ResetClip()

    foreach ($item in @($bgBrush, $glowPen, $shadowBrush, $whiteBrush, $screenBrush, $hullBrush, $waveBrush, $screenShineBrush, $skySheenBrush, $outer, $currentPen, $softCurrentPen, $screenRimPen, $keelPen, $glintPen, $skySheen, $monitorShadow, $monitor, $screen, $screenShine, $base, $hullShadow, $hull, $railPen, $wave1, $wavePen, $fineWavePen)) {
        if ($null -ne $item) {
            $item.Dispose()
        }
    }
    $graphics.Dispose()
    return $bitmap
}

function Get-PngBytes {
    param([System.Drawing.Bitmap] $Bitmap)
    $stream = New-Object System.IO.MemoryStream
    $Bitmap.Save($stream, [System.Drawing.Imaging.ImageFormat]::Png)
    return $stream.ToArray()
}

function Write-Ico {
    param(
        [string] $Path,
        [object[]] $Images
    )

    $stream = [System.IO.File]::Create($Path)
    $writer = New-Object System.IO.BinaryWriter -ArgumentList @($stream)
    try {
        $writer.Write([uint16] 0)
        $writer.Write([uint16] 1)
        $writer.Write([uint16] $Images.Count)

        $offset = 6 + (16 * $Images.Count)
        foreach ($image in $Images) {
            $sizeByte = if ($image.Size -ge 256) { 0 } else { [byte] $image.Size }
            $writer.Write([byte] $sizeByte)
            $writer.Write([byte] $sizeByte)
            $writer.Write([byte] 0)
            $writer.Write([byte] 0)
            $writer.Write([uint16] 1)
            $writer.Write([uint16] 32)
            $writer.Write([uint32] $image.Bytes.Length)
            $writer.Write([uint32] $offset)
            $offset += $image.Bytes.Length
        }

        foreach ($image in $Images) {
            $writer.Write([byte[]] $image.Bytes)
        }
    }
    finally {
        $writer.Dispose()
        $stream.Dispose()
    }
}

New-Item -ItemType Directory -Force -Path (Split-Path -Parent $IcoPath) | Out-Null
New-Item -ItemType Directory -Force -Path (Split-Path -Parent $PreviewPath) | Out-Null

$sizes = @(16, 24, 32, 48, 64, 128, 256)
$images = @()
foreach ($size in $sizes) {
    $bitmap = Render-ClientIcon -Size $size
    try {
        if ($size -eq 256) {
            $bitmap.Save($PreviewPath, [System.Drawing.Imaging.ImageFormat]::Png)
        }
        $images += [pscustomobject] @{
            Size = $size
            Bytes = Get-PngBytes -Bitmap $bitmap
        }
    }
    finally {
        $bitmap.Dispose()
    }
}

Write-Ico -Path $IcoPath -Images $images
Write-Host "generated $IcoPath"
