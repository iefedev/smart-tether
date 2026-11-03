using System;
using System.Diagnostics;
using System.Globalization;
using System.IO;
using System.Linq;
using System.Net.Sockets;
using System.Threading;
using System.Threading.Tasks;
using System.Windows;
using System.Windows.Controls;
using System.Windows.Controls.Primitives;
using System.Windows.Threading;
using Forms = System.Windows.Forms;

namespace SmartTether;

/// <summary>
/// Tray icon plus main window. Started at logon with --hidden (tray only);
/// starting it again shows the window of the running instance.
/// </summary>
public partial class App : Application
{
    const string MutexName = @"Local\SmartTetherUI";
    const string ShowEventName = @"Local\SmartTetherUI.Show";

    Mutex? single;
    EventWaitHandle? showEvent;
    Forms.NotifyIcon? tray;
    MainWindow? window;
    Window? menuOwner;
    ContextMenu? menu;
    MenuItem? miStatus, miAuto, miUsb, miWifi, miPause;
    string lastIcon = "";

    public Api Api { get; private set; } = null!;
    public Status? Last { get; private set; }
    public bool Quitting { get; private set; }

    protected override void OnStartup(StartupEventArgs e)
    {
        base.OnStartup(e);
        // The UI is English only; format numbers the same way on every system.
        CultureInfo.DefaultThreadCurrentCulture = CultureInfo.DefaultThreadCurrentUICulture = CultureInfo.InvariantCulture;
        CultureInfo.CurrentCulture = CultureInfo.CurrentUICulture = CultureInfo.InvariantCulture;
        // A bad reply from the service must not take the tray icon down with it.
        DispatcherUnhandledException += (_, ev) =>
        {
            LogError(ev.Exception);
            ev.Handled = true;
        };
        TaskScheduler.UnobservedTaskException += (_, ev) => { LogError(ev.Exception); ev.SetObserved(); };
#if DEBUG
        int snap = Array.IndexOf(e.Args, "--snapshot");
        if (snap >= 0 && snap + 1 < e.Args.Length)
        {
            _ = SnapshotAndExit(e.Args, e.Args[snap + 1]);
            return;
        }
#endif
        single = new Mutex(true, MutexName, out bool first);
        if (!first)
        {
            // Already running: ask that instance to show its window.
            try { EventWaitHandle.OpenExisting(ShowEventName).Set(); } catch { }
            Shutdown();
            return;
        }
        showEvent = new EventWaitHandle(false, EventResetMode.AutoReset, ShowEventName);
        new Thread(() => { while (showEvent.WaitOne()) Dispatcher.Invoke(ShowWindow); }) { IsBackground = true }.Start();

        try { Api = new Api(Api.ReadToken()); }
        catch (Exception ex)
        {
            MessageBox.Show(ex.Message, "Smart Tether",MessageBoxButton.OK, MessageBoxImage.Error);
            Shutdown();
            return;
        }

        window = new MainWindow(this);
        BuildTray();
        if (!e.Args.Contains("--hidden")) ShowWindow();

        var timer = new DispatcherTimer { Interval = TimeSpan.FromSeconds(1) };
        timer.Tick += async (_, _) => await Refresh();
        timer.Start();
        _ = Refresh();
        _ = KeepAdbAsync();
    }

#if DEBUG
    /// <summary>
    /// Debug builds only: renders a window with made-up demo data (DemoData.cs) to a PNG
    /// without showing it on screen and without contacting the service, for README screenshots.
    ///   SmartTether.exe --snapshot out.png [--theme light|dark]
    ///                   [--state connected:usb|connected:wifi|reconnecting|searching|paused] [--pair]
    /// </summary>
    async Task SnapshotAndExit(string[] args, string path)
    {
        string? Arg(string name) { int i = Array.IndexOf(args, name); return i >= 0 && i + 1 < args.Length ? args[i + 1] : null; }
        if (Arg("--theme") is string theme)
            ThemeMode = theme == "light" ? ThemeMode.Light : ThemeMode.Dark;
        try
        {
            if (args.Contains("--pair")) await SnapshotPair(path);
            else await Snapshot(path, Arg("--state") ?? "connected:usb");
        }
        catch (Exception ex) { LogError(ex); }
        Shutdown();
    }

    async Task Snapshot(string path, string state)
    {
        window = new MainWindow(this) { ShowActivated = false, ShowInTaskbar = false, Left = -20000, Top = -20000 };
        window.Show();
        Last = DemoData.Status(state);
        window.Show(Last, null);
        await Task.Delay(800);
        Render((FrameworkElement)window.Content, path);
        Quitting = true;
        window.Close();
    }

    async Task SnapshotPair(string path)
    {
        var pw = new PairWindow { ShowActivated = false, ShowInTaskbar = false, WindowStartupLocation = WindowStartupLocation.Manual, Left = -20000, Top = -20000 };
        pw.Show();
        pw.SetQr(DemoData.FakeQr());
        await Task.Delay(500);
        Render((FrameworkElement)pw.Content, path);
        pw.Close();
    }

    static void Render(FrameworkElement root, string path)
    {
        // The element draws at its margin offset, so the bitmap includes the margins.
        double w = root.ActualWidth + root.Margin.Left + root.Margin.Right;
        double h = root.ActualHeight + root.Margin.Top + root.Margin.Bottom;
        var bmp = new System.Windows.Media.Imaging.RenderTargetBitmap((int)w, (int)h, 96, 96, System.Windows.Media.PixelFormats.Pbgra32);
        var bg = new System.Windows.Shapes.Rectangle { Width = w, Height = h };
        bg.SetResourceReference(System.Windows.Shapes.Shape.FillProperty, "ApplicationBackgroundBrush");
        bg.Measure(new Size(w, h));
        bg.Arrange(new Rect(0, 0, w, h));
        bmp.Render(bg);
        bmp.Render(root);
        var enc = new System.Windows.Media.Imaging.PngBitmapEncoder();
        enc.Frames.Add(System.Windows.Media.Imaging.BitmapFrame.Create(bmp));
        using var f = File.Create(path);
        enc.Save(f);
    }
#endif

    void BuildTray()
    {
        tray = new Forms.NotifyIcon { Icon = TrayIcons.Make(TrayIcons.Off), Text = "Smart Tether", Visible = true };
        tray.MouseUp += (_, e) =>
        {
            if (e.Button == Forms.MouseButtons.Left) ToggleWindow();
            else if (e.Button == Forms.MouseButtons.Right) ShowMenu();
        };

        // The tray menu is a WPF ContextMenu so it gets the Windows 11 Fluent look.
        menuOwner = new Window { WindowStyle = WindowStyle.None, ShowInTaskbar = false, Width = 0, Height = 0, Left = -10000, Top = -10000, AllowsTransparency = true, Opacity = 0 };
        menu = new ContextMenu { Placement = PlacementMode.MousePoint };
        miStatus = new MenuItem { Header = "Starting…", IsEnabled = false };
        var miOpen = new MenuItem { Header = "Open Smart Tether", FontWeight = FontWeights.SemiBold };
        miOpen.Click += (_, _) => ShowWindow();
        miAuto = new MenuItem { Header = "Automatic", IsCheckable = true };
        miUsb = new MenuItem { Header = "USB only", IsCheckable = true };
        miWifi = new MenuItem { Header = "Wi-Fi only", IsCheckable = true };
        miAuto.Click += (_, _) => Change(new { preference = "auto" });
        miUsb.Click += (_, _) => Change(new { preference = "usb" });
        miWifi.Click += (_, _) => Change(new { preference = "wifi" });
        miPause = new MenuItem { Header = "Pause" };
        miPause.Click += (_, _) => Change(new { paused = !(Last?.Settings.Paused ?? false) });
        var miQuit = new MenuItem { Header = "Quit (the tunnel keeps running)" };
        miQuit.Click += (_, _) => Quit();
        foreach (var item in new Control[] { miStatus, miOpen, new Separator(), miAuto, miUsb, miWifi, new Separator(), miPause, new Separator(), miQuit })
            menu.Items.Add(item);
        menu.Closed += (_, _) => menuOwner.Hide();
    }

    void ShowMenu()
    {
        if (menu == null || menuOwner == null) return;
        menuOwner.Show();
        menuOwner.Activate(); // so a click elsewhere closes the menu
        menu.PlacementTarget = menuOwner;
        menu.IsOpen = true;
    }

    void ShowWindow()
    {
        if (window == null) return;
        window.Show();
        if (window.WindowState == WindowState.Minimized) window.WindowState = WindowState.Normal;
        window.Activate();
        window.Show(Last, null);
    }

    void ToggleWindow()
    {
        if (window == null) return;
        if (window.IsVisible && window.IsActive) window.Hide();
        else ShowWindow();
    }

    async Task Refresh()
    {
        string? error = null;
        try { Last = await Api.StatusAsync(); }
        catch { Last = null; error = "Can't reach the Smart Tether service. Is it running?"; }
        UpdateTray(Last);
        if (window is { IsVisible: true }) window.Show(Last, error);
    }

    public async void Change(object change)
    {
        try
        {
            Last = await Api.ChangeAsync(change);
            UpdateTray(Last);
            window?.Show(Last, null);
        }
        catch (Exception ex)
        {
            window?.Show(Last, "Couldn't change that setting: " + ex.Message);
        }
    }

    void UpdateTray(Status? s)
    {
        if (tray == null) return;
        string line, icon;
        if (s == null) { icon = "off"; line = "Service not running"; }
        else
        {
            switch (s.State)
            {
                case "connected":
                    double d = s.History.Count > 0 ? s.History[^1].Down * 8 / 1e6 : 0;
                    icon = s.Usb ? "usb" : "wifi";
                    line = $"Connected over {(s.Usb ? "USB" : "Wi-Fi")} · {d:0} Mbit/s";
                    break;
                case "reconnecting": icon = "wait"; line = "Reconnecting…"; break;
                case "paused": icon = "off"; line = "Paused"; break;
                default: icon = "wait"; line = "Looking for your phone…"; break;
            }
        }
        if (icon != lastIcon)
        {
            var old = tray.Icon;
            tray.Icon = TrayIcons.Make(icon switch { "usb" => TrayIcons.Usb, "wifi" => TrayIcons.WiFi, "wait" => TrayIcons.Wait, _ => TrayIcons.Off });
            old?.Dispose();
            lastIcon = icon;
        }
        var tip = "Smart Tether: " + line;
        tray.Text = tip.Length > 63 ? tip[..63] : tip;
        if (miStatus != null) miStatus.Header = line;
        if (s != null && miAuto != null && miUsb != null && miWifi != null && miPause != null)
        {
            miAuto.IsChecked = s.Settings.Preference == "auto";
            miUsb.IsChecked = s.Settings.Preference == "usb";
            miWifi.IsChecked = s.Settings.Preference == "wifi";
            miPause.Header = s.Settings.Paused ? "Resume" : "Pause";
        }
    }

    static void LogError(Exception ex)
    {
        try
        {
            var dir = Path.Combine(Environment.GetFolderPath(Environment.SpecialFolder.LocalApplicationData), "Smart Tether");
            Directory.CreateDirectory(dir);
            File.AppendAllText(Path.Combine(dir, "ui-errors.log"), $"{DateTime.Now:o} {ex}\n");
        }
        catch { }
    }

    void Quit()
    {
        Quitting = true;
        if (tray != null) { tray.Visible = false; tray.Dispose(); }
        window?.Close();
        menuOwner?.Close();
        Shutdown();
    }

    /// <summary>
    /// Starts the adb server in this session whenever it is not running: the
    /// service only talks to it (an adb server started by the service would run in session 0, where it fails).
    /// </summary>
    static async Task KeepAdbAsync()
    {
        // The installer puts adb in bin\ next to this exe; an Android SDK's copy is the fallback.
        var sdk = Environment.GetEnvironmentVariable("ANDROID_HOME") ?? Environment.GetEnvironmentVariable("ANDROID_SDK_ROOT")
            ?? Path.Combine(Environment.GetFolderPath(Environment.SpecialFolder.LocalApplicationData), "Android", "Sdk");
        var adb = new[]
        {
            Path.Combine(AppContext.BaseDirectory, "bin", "adb.exe"),
            Path.Combine(sdk, "platform-tools", "adb.exe"),
        }.FirstOrDefault(File.Exists) ?? "";
        while (true)
        {
            try
            {
                using var c = new TcpClient();
                var connect = c.ConnectAsync("127.0.0.1", 5037);
                if (await Task.WhenAny(connect, Task.Delay(1000)) != connect || !c.Connected)
                    throw new IOException("adb server not running");
            }
            catch when (adb != "")
            {
                try
                {
                    using var p = Process.Start(new ProcessStartInfo(adb, "start-server") { CreateNoWindow = true, UseShellExecute = false });
                    if (p != null) await p.WaitForExitAsync();
                }
                catch { }
            }
            catch { }
            await Task.Delay(TimeSpan.FromSeconds(10));
        }
    }
}
