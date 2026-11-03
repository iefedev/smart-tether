using System;
using System.Collections.Generic;
using System.Linq;
using System.Windows;
using System.Windows.Media;

namespace SmartTether;

/// <summary>Bars for the last 60 seconds of download (accent) and upload (secondary) traffic.</summary>
public sealed class RateChart : FrameworkElement
{
    IReadOnlyList<Sample> samples = Array.Empty<Sample>();

    public static readonly DependencyProperty DownBrushProperty = DependencyProperty.Register(
        nameof(DownBrush), typeof(Brush), typeof(RateChart), new FrameworkPropertyMetadata(Brushes.DodgerBlue, FrameworkPropertyMetadataOptions.AffectsRender));
    public static readonly DependencyProperty UpBrushProperty = DependencyProperty.Register(
        nameof(UpBrush), typeof(Brush), typeof(RateChart), new FrameworkPropertyMetadata(Brushes.MediumPurple, FrameworkPropertyMetadataOptions.AffectsRender));
    public static readonly DependencyProperty GridBrushProperty = DependencyProperty.Register(
        nameof(GridBrush), typeof(Brush), typeof(RateChart), new FrameworkPropertyMetadata(Brushes.Gray, FrameworkPropertyMetadataOptions.AffectsRender));

    public Brush DownBrush { get => (Brush)GetValue(DownBrushProperty); set => SetValue(DownBrushProperty, value); }
    public Brush UpBrush { get => (Brush)GetValue(UpBrushProperty); set => SetValue(UpBrushProperty, value); }
    public Brush GridBrush { get => (Brush)GetValue(GridBrushProperty); set => SetValue(GridBrushProperty, value); }

    /// <summary>Peak of the visible window in bytes per second.</summary>
    public ulong Peak { get; private set; }

    public void Update(IReadOnlyList<Sample> s)
    {
        samples = s.Count > 60 ? s.Skip(s.Count - 60).ToList() : s;
        Peak = Math.Max(1, samples.Count == 0 ? 1 : samples.Max(x => Math.Max(x.Down, x.Up)));
        InvalidateVisual();
    }

    protected override void OnRender(DrawingContext dc)
    {
        double w = ActualWidth, h = ActualHeight;
        var grid = new Pen(GridBrush, 1);
        grid.Freeze();
        foreach (var f in new[] { 0.5, 1.0 })
        {
            double y = Math.Round(h - h * f) + 0.5;
            dc.DrawLine(grid, new Point(0, y), new Point(w, y));
        }
        const int n = 60;
        double bw = w / n;
        for (int i = 0; i < samples.Count; i++)
        {
            var s = samples[i];
            double x = (n - samples.Count + i) * bw;
            double dh = Math.Max(1, h * s.Down / Peak);
            double uh = s.Up == 0 ? 0 : Math.Max(1, h * s.Up / Peak);
            var bar = Math.Max(1, bw - 2);
            dc.DrawRoundedRectangle(DownBrush, null, new Rect(x + 1, h - dh, bar, dh), 1, 1);
            if (uh > 0) dc.DrawRoundedRectangle(UpBrush, null, new Rect(x + 1, h - uh, bar, uh), 1, 1);
        }
    }
}
