using System;
using System.Drawing;
using System.Drawing.Drawing2D;
using System.Drawing.Text;
using System.Runtime.InteropServices;
using Microsoft.Win32;

namespace SmartTether;

/// <summary>Tray icons: the Segoe Fluent phone glyph in the taskbar's text color, with a status dot.</summary>
static class TrayIcons
{
    public static readonly Color Usb = Color.FromArgb(0x10, 0x9e, 0x5b);
    public static readonly Color WiFi = Color.FromArgb(0x1e, 0x88, 0xe5);
    public static readonly Color Wait = Color.FromArgb(0xe0, 0x9b, 0x10);
    public static readonly Color Off = Color.FromArgb(0x8a, 0x8a, 0x8a);

    [DllImport("user32.dll")] static extern bool DestroyIcon(IntPtr h);

    static bool TaskbarIsLight()
    {
        using var k = Registry.CurrentUser.OpenSubKey(@"Software\Microsoft\Windows\CurrentVersion\Themes\Personalize");
        return k?.GetValue("SystemUsesLightTheme") is int v && v == 1;
    }

    public static Icon Make(Color dot)
    {
        const int n = 32;
        using var bmp = new Bitmap(n, n);
        using (var g = Graphics.FromImage(bmp))
        {
            g.SmoothingMode = SmoothingMode.AntiAlias;
            g.TextRenderingHint = TextRenderingHint.AntiAliasGridFit;
            var ink = TaskbarIsLight() ? Color.FromArgb(0x1b, 0x1b, 0x1b) : Color.White;
            using var font = new Font("Segoe Fluent Icons", 20, GraphicsUnit.Pixel);
            using var inkBrush = new SolidBrush(ink);
            g.DrawString("", font, inkBrush, -3, 2);
            using var ring = new SolidBrush(TaskbarIsLight() ? Color.White : Color.FromArgb(0x20, 0x20, 0x20));
            g.FillEllipse(ring, 15, 15, 17, 17);
            using var fill = new SolidBrush(dot);
            g.FillEllipse(fill, 17.5f, 17.5f, 12, 12);
        }
        var h = bmp.GetHicon();
        var icon = (Icon)Icon.FromHandle(h).Clone();
        DestroyIcon(h);
        return icon;
    }
}
