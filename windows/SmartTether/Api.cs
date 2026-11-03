using System;
using System.Collections.Generic;
using System.IO;
using System.Net.Http;
using System.Net.Http.Json;
using System.Text.Json.Serialization;
using System.Threading.Tasks;

namespace SmartTether;

public sealed class Sample
{
    [JsonPropertyName("d")] public ulong Down { get; set; }
    [JsonPropertyName("u")] public ulong Up { get; set; }
}

public sealed class TunnelEvent
{
    [JsonPropertyName("t")] public DateTime Time { get; set; }
    [JsonPropertyName("text")] public string Text { get; set; } = "";
}

public sealed class Settings
{
    [JsonPropertyName("preference")] public string Preference { get; set; } = "auto";
    [JsonPropertyName("quic")] public bool Quic { get; set; }
    [JsonPropertyName("paused")] public bool Paused { get; set; }
}

public sealed class Status
{
    [JsonPropertyName("state")] public string State { get; set; } = "searching";
    [JsonPropertyName("transport")] public string? Transport { get; set; }
    [JsonPropertyName("phone")] public string? Phone { get; set; }
    [JsonPropertyName("sessionSeconds")] public long SessionSeconds { get; set; }
    [JsonPropertyName("sessionDown")] public ulong SessionDown { get; set; }
    [JsonPropertyName("sessionUp")] public ulong SessionUp { get; set; }
    [JsonPropertyName("history")] public List<Sample> History { get; set; } = new();
    [JsonPropertyName("events")] public List<TunnelEvent> Events { get; set; } = new();
    [JsonPropertyName("settings")] public Settings Settings { get; set; } = new();
    [JsonPropertyName("startWithWindows")] public bool StartWithWindows { get; set; }
    [JsonPropertyName("tcp")] public long Tcp { get; set; }
    [JsonPropertyName("udp")] public long Udp { get; set; }

    public bool Connected => State == "connected";
    public bool Usb => Transport == "usb";
}

/// <summary>Client for the Smart Tether service's loopback API (internal/pc/api.go).</summary>
public sealed class Api
{
    readonly HttpClient http;

    public Api(string token)
    {
        http = new HttpClient { BaseAddress = new Uri("http://127.0.0.1:7780/"), Timeout = TimeSpan.FromSeconds(3) };
        http.DefaultRequestHeaders.Add("X-PT-Key", token);
    }

    /// <summary>The service's installer writes the token to %ProgramData%\Smart Tether\token.</summary>
    public static string ReadToken()
    {
        var p = Path.Combine(Environment.GetFolderPath(Environment.SpecialFolder.CommonApplicationData), "Smart Tether", "token");
        if (File.Exists(p)) return File.ReadAllText(p).Trim();
        throw new FileNotFoundException("Smart Tether can't find its key (" + p + "). Reinstall Smart Tether.");
    }

    public async Task<Status?> StatusAsync() => Normalize(await http.GetFromJsonAsync<Status>("api/status"));

    /// <summary>JSON null for a list or object replaces the C# default; put the defaults back.</summary>
    static Status? Normalize(Status? s)
    {
        if (s == null) return null;
        s.History ??= new();
        s.Events ??= new();
        s.Settings ??= new();
        return s;
    }

    public async Task<Status?> ChangeAsync(object change)
    {
        var r = await http.PostAsJsonAsync("api/settings", change);
        if (!r.IsSuccessStatusCode) throw new InvalidOperationException((await r.Content.ReadAsStringAsync()).Trim());
        return Normalize(await r.Content.ReadFromJsonAsync<Status>());
    }

    public async Task<string?> PairingQrAsync()
    {
        var r = await http.GetFromJsonAsync<Dictionary<string, string>>("api/pairing");
        return r != null && r.TryGetValue("qr", out var qr) ? qr : null;
    }
}
