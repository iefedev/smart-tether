#if DEBUG
using System;
using System.Collections.Generic;
using System.Windows.Media;
using System.Windows.Media.Imaging;

namespace SmartTether;

/// <summary>
/// Debug builds only: made-up data for --snapshot, so README screenshots never show a real
/// phone name, real traffic or a real pairing code.
/// </summary>
static class DemoData
{
    /// <summary>A status like the service's, for one of connected:usb, connected:wifi, reconnecting, searching, paused.</summary>
    public static Status Status(string state)
    {
        var parts = state.Split(':');
        var s = new Status
        {
            State = parts[0],
            Transport = parts.Length > 1 ? parts[1] : parts[0] == "reconnecting" ? "usb" : null,
            Phone = "Demo Phone",
            SessionSeconds = parts[0] is "connected" or "reconnecting" ? 2 * 3600 + 17 * 60 : 0,
            SessionDown = 1_620_000_000,
            SessionUp = 214_000_000,
            Settings = new Settings { Preference = "auto", Quic = false, Paused = parts[0] == "paused" },
            StartWithWindows = true,
            Tcp = 37,
            Udp = 5,
        };
        if (parts[0] != "connected")
        {
            s.SessionDown = s.SessionUp = 0;
        }
        // Two minutes of traffic: a steady download with a burst, light upload.
        var rnd = new Random(42);
        for (int i = 0; i < 120; i++)
        {
            double down = parts[0] == "connected" ? 6e6 + 3e6 * Math.Sin(i / 9.0) + rnd.NextDouble() * 1.5e6 : 0;
            if (parts[0] == "connected" && i is > 70 and < 95) down += 5e6;
            double up = parts[0] == "connected" ? 0.6e6 + rnd.NextDouble() * 0.9e6 : 0;
            s.History.Add(new Sample { Down = (ulong)down, Up = (ulong)up });
        }
        var now = DateTime.Now;
        s.Events = new List<TunnelEvent>
        {
            new() { Time = now.AddMinutes(-140), Text = "Started; looking for the phone" },
            new() { Time = now.AddMinutes(-137), Text = "Connected over USB" },
        };
        return s;
    }

    /// <summary>
    /// A QR-looking pattern (finder and timing patterns plus random modules) that encodes
    /// nothing; scanning it does not pair anything.
    /// </summary>
    public static BitmapSource FakeQr()
    {
        const int n = 29, quiet = 4, size = n + 2 * quiet;
        var dark = new bool[n, n];
        var rnd = new Random(7);
        for (int y = 0; y < n; y++)
            for (int x = 0; x < n; x++)
                dark[y, x] = rnd.Next(2) == 0;
        void Finder(int top, int left)
        {
            for (int y = -1; y <= 7; y++)
                for (int x = -1; x <= 7; x++)
                {
                    int yy = top + y, xx = left + x;
                    if (yy < 0 || xx < 0 || yy >= n || xx >= n) continue;
                    bool ring = y is 0 or 6 || x is 0 or 6;
                    bool core = y is >= 2 and <= 4 && x is >= 2 and <= 4;
                    bool inside = y is >= 0 and <= 6 && x is >= 0 and <= 6;
                    dark[yy, xx] = inside && (ring || core);
                }
        }
        Finder(0, 0);
        Finder(0, n - 7);
        Finder(n - 7, 0);
        for (int i = 8; i < n - 8; i++)
        {
            dark[6, i] = i % 2 == 0;
            dark[i, 6] = i % 2 == 0;
        }
        var px = new byte[size * size];
        for (int y = 0; y < size; y++)
            for (int x = 0; x < size; x++)
            {
                int my = y - quiet, mx = x - quiet;
                bool on = my >= 0 && mx >= 0 && my < n && mx < n && dark[my, mx];
                px[y * size + x] = on ? (byte)0 : (byte)255;
            }
        return BitmapSource.Create(size, size, 96, 96, PixelFormats.Gray8, null, px, size);
    }
}
#endif
