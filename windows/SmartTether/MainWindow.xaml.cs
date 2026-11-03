using System;
using System.ComponentModel;
using System.Linq;
using System.Windows;
using System.Windows.Controls;
using System.Windows.Media;
using System.Windows.Media.Imaging;
using System.Windows.Shapes;

namespace SmartTether;

public partial class MainWindow : Window
{
    readonly App app;
    bool updating; // true while Show() sets controls, so their events don't post back

    public MainWindow(App app)
    {
        this.app = app;
        InitializeComponent();
        MaxHeight = SystemParameters.WorkArea.Height;
    }

    /// <summary>Closing only hides the window; the tray icon stays.</summary>
    protected override void OnClosing(CancelEventArgs e)
    {
        if (!app.Quitting)
        {
            e.Cancel = true;
            Hide();
        }
        base.OnClosing(e);
    }

    public void Show(Status? s, string? error)
    {
        Error.Text = error ?? "";
        Error.Visibility = string.IsNullOrEmpty(error) ? Visibility.Collapsed : Visibility.Visible;
        if (s == null)
        {
            StatusTile.SetResourceReference(Border.BackgroundProperty, "StatusBadBrush");
            return;
        }
        updating = true;
        try
        {
            PhoneName.Text = string.IsNullOrEmpty(s.Phone) ? "Your phone" : s.Phone;
            // The tile's background is the status (green when connected, red otherwise);
            // the glyph keeps one color.
            StatusTile.SetResourceReference(Border.BackgroundProperty, s.Connected ? "StatusOkBrush" : "StatusBadBrush");
            StatusGlyph.Text = s.Connected ? (s.Usb ? "" : "") : "";
            Detail.Text = s.State switch
            {
                "connected" => $"Sharing its mobile data over {(s.Usb ? "USB" : "Wi-Fi")} · {Age(s.SessionSeconds)}",
                "reconnecting" => "Reconnecting; open connections are kept.",
                "paused" => "Paused; Windows uses other connections.",
                _ => "Plug in the phone or open its app.",
            };

            var cur = s.History.Count > 0 ? s.History[^1] : new Sample();
            DownRate.Text = Rate(cur.Down);
            UpRate.Text = Rate(cur.Up);
            (Used.Text, UsedUnit.Text) = Bytes(s.SessionDown + s.SessionUp);
            Chart.Update(s.History);
            Peak.Text = $"Peak {Rate(Chart.Peak)} Mbit/s · last 60 s";

            Preference.SelectedItem = Preference.Items.Cast<ComboBoxItem>().FirstOrDefault(i => (string)i.Tag == s.Settings.Preference);
            AutoStart.IsChecked = s.StartWithWindows;
            Quic.IsChecked = s.Settings.Quic;
            bool paused = s.Settings.Paused;
            PauseButton.Content = paused ? "Resume" : "Pause";
            PauseGlyph.Text = paused ? "" : "";
            PauseTitle.Text = paused ? "The tunnel is paused" : "Pause the tunnel";
            PauseHint.Text = paused ? "Resume to use the phone's data again." : "Windows uses its other connections.";
        }
        finally
        {
            updating = false;
        }
    }

    static string Rate(ulong bytesPerSec)
    {
        double v = bytesPerSec * 8 / 1e6;
        return v >= 100 ? v.ToString("0") : v >= 10 ? v.ToString("0.0") : v.ToString("0.00");
    }

    static (string, string) Bytes(ulong b) =>
        b >= 1_000_000_000 ? ((b / 1e9).ToString("0.00"), "GB") :
        b >= 1_000_000 ? ((b / 1e6).ToString(b >= 100_000_000 ? "0" : "0.0"), "MB") :
        ((b / 1e3).ToString("0"), "KB");

    static string Age(long s) =>
        s < 60 ? $"{s} s" : s < 3600 ? $"{s / 60} min" : $"{s / 3600} h {s % 3600 / 60} min";

    void Preference_Changed(object sender, SelectionChangedEventArgs e)
    {
        if (updating || Preference.SelectedItem is not ComboBoxItem item) return;
        app.Change(new { preference = (string)item.Tag });
    }

    void Pause_Click(object sender, RoutedEventArgs e) =>
        app.Change(new { paused = !(app.Last?.Settings.Paused ?? false) });

    void AutoStart_Click(object sender, RoutedEventArgs e) =>
        app.Change(new { startWithWindows = AutoStart.IsChecked == true });

    void Quic_Click(object sender, RoutedEventArgs e) =>
        app.Change(new { quic = Quic.IsChecked == true });

    async void Pair_Click(object sender, RoutedEventArgs e)
    {
        var popup = new PairWindow { Owner = this };
        var load = popup.LoadAsync(app.Api);
        popup.ShowDialog();
        await load;
    }
}
