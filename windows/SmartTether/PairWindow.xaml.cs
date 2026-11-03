using System;
using System.IO;
using System.Threading.Tasks;
using System.Windows;
using System.Windows.Media.Imaging;

namespace SmartTether;

/// <summary>Popup with the pairing QR code (token and hotspot passphrase, from the service).</summary>
public partial class PairWindow : Window
{
    public PairWindow() => InitializeComponent();

    public async Task LoadAsync(Api api)
    {
        try
        {
            var dataUrl = await api.PairingQrAsync() ?? throw new InvalidOperationException("The service sent no code.");
            var png = Convert.FromBase64String(dataUrl[(dataUrl.IndexOf(',') + 1)..]);
            var img = new BitmapImage();
            img.BeginInit();
            img.StreamSource = new MemoryStream(png);
            img.CacheOption = BitmapCacheOption.OnLoad;
            img.EndInit();
            SetQr(img);
        }
        catch (Exception ex)
        {
            Message.Text = "Couldn't get the pairing code: " + ex.Message;
            Message.Visibility = Visibility.Visible;
        }
    }

    internal void SetQr(BitmapSource qr) => QrImage.Source = qr;

    void Close_Click(object sender, RoutedEventArgs e) => Close();
}
