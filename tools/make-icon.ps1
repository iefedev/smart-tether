# Draws the Smart Tether app icon (green rounded tile, white Segoe Fluent phone glyph, like the
# Android launcher icon) into windows\SmartTether\SmartTether.ico. Run once; the .ico is committed.
$ErrorActionPreference = 'Stop'
Add-Type -AssemblyName System.Drawing
$root = Split-Path $PSScriptRoot -Parent
$out = Join-Path $root 'windows\SmartTether\SmartTether.ico'

$sizes = 16, 20, 24, 32, 40, 48, 64, 256
$pngs = foreach ($n in $sizes) {
    $bmp = New-Object System.Drawing.Bitmap $n, $n
    $g = [System.Drawing.Graphics]::FromImage($bmp)
    $g.SmoothingMode = 'AntiAlias'
    $g.TextRenderingHint = 'AntiAliasGridFit'
    $r = [Math]::Max(2, $n * 0.22)
    $path = New-Object System.Drawing.Drawing2D.GraphicsPath
    $w = $n - 1
    $path.AddArc(0, 0, 2 * $r, 2 * $r, 180, 90)
    $path.AddArc($w - 2 * $r, 0, 2 * $r, 2 * $r, 270, 90)
    $path.AddArc($w - 2 * $r, $w - 2 * $r, 2 * $r, 2 * $r, 0, 90)
    $path.AddArc(0, $w - 2 * $r, 2 * $r, 2 * $r, 90, 90)
    $path.CloseFigure()
    $g.FillPath((New-Object System.Drawing.SolidBrush ([System.Drawing.Color]::FromArgb(0x0F, 0x7B, 0x0F))), $path)
    $font = New-Object System.Drawing.Font 'Segoe Fluent Icons', ($n * 0.62), ([System.Drawing.GraphicsUnit]::Pixel)
    $fmt = New-Object System.Drawing.StringFormat
    $fmt.Alignment = 'Center'; $fmt.LineAlignment = 'Center'
    $g.DrawString([string][char]0xE8EA, $font, [System.Drawing.Brushes]::White, (New-Object System.Drawing.RectangleF 0, ($n * 0.04), $n, $n), $fmt)
    $g.Dispose()
    $ms = New-Object System.IO.MemoryStream
    if ($n -ge 256) {
        $bmp.Save($ms, [System.Drawing.Imaging.ImageFormat]::Png) # PNG only for 256: older readers take BMP below that
    } else {
        # 32-bit DIB: BITMAPINFOHEADER (height doubled for the mask), BGRA rows bottom-up, then the AND mask.
        $bw = New-Object System.IO.BinaryWriter $ms
        $bw.Write([uint32]40); $bw.Write([int32]$n); $bw.Write([int32](2 * $n))
        $bw.Write([uint16]1); $bw.Write([uint16]32); $bw.Write([uint32]0)
        $bw.Write([uint32]0); $bw.Write([int32]0); $bw.Write([int32]0); $bw.Write([uint32]0); $bw.Write([uint32]0)
        for ($y = $n - 1; $y -ge 0; $y--) {
            for ($x = 0; $x -lt $n; $x++) {
                $c = $bmp.GetPixel($x, $y)
                $bw.Write([byte]$c.B); $bw.Write([byte]$c.G); $bw.Write([byte]$c.R); $bw.Write([byte]$c.A)
            }
        }
        $maskRow = [int]([Math]::Ceiling($n / 32) * 4)
        $bw.Write((New-Object byte[] ($maskRow * $n)))
        $bw.Flush()
    }
    $bmp.Dispose()
    , $ms.ToArray()
}

# ICO: header, one directory entry per size, then the PNG images.
$f = New-Object System.IO.MemoryStream
$wr = New-Object System.IO.BinaryWriter $f
$wr.Write([uint16]0); $wr.Write([uint16]1); $wr.Write([uint16]$sizes.Count)
$offset = 6 + 16 * $sizes.Count
for ($i = 0; $i -lt $sizes.Count; $i++) {
    $n = $sizes[$i]; $b = if ($n -ge 256) { 0 } else { $n }
    $wr.Write([byte]$b); $wr.Write([byte]$b); $wr.Write([byte]0); $wr.Write([byte]0)
    $wr.Write([uint16]1); $wr.Write([uint16]32)
    $wr.Write([uint32]$pngs[$i].Length); $wr.Write([uint32]$offset)
    $offset += $pngs[$i].Length
}
foreach ($p in $pngs) { $wr.Write($p) }
$wr.Flush()
[System.IO.File]::WriteAllBytes($out, $f.ToArray())
"wrote $out"
