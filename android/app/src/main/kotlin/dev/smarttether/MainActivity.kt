package dev.smarttether

import android.Manifest
import android.app.AppOpsManager
import android.content.ComponentName
import android.content.Context
import android.content.Intent
import android.content.pm.PackageManager
import android.net.Uri
import android.os.Build
import android.os.Bundle
import android.os.PowerManager
import android.os.Process
import android.provider.Settings
import android.text.format.DateUtils
import android.widget.Toast
import androidx.activity.ComponentActivity
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.compose.setContent
import androidx.activity.enableEdgeToEdge
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.foundation.Canvas
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.outlined.BatteryChargingFull
import androidx.compose.material.icons.outlined.BatterySaver
import androidx.compose.material.icons.outlined.CheckCircle
import androidx.compose.material.icons.outlined.Laptop
import androidx.compose.material.icons.outlined.Link
import androidx.compose.material.icons.outlined.Notifications
import androidx.compose.material.icons.outlined.QrCodeScanner
import androidx.compose.material.icons.outlined.RestartAlt
import androidx.compose.material3.Button
import androidx.compose.material3.Card
import androidx.compose.material3.CardDefaults
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.FilledTonalButton
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.ListItem
import androidx.compose.material3.ListItemDefaults
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.material3.TopAppBar
import androidx.compose.material3.TopAppBarDefaults
import androidx.compose.runtime.Composable
import androidx.compose.runtime.DisposableEffect
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableIntStateOf
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.StrokeCap
import androidx.compose.ui.graphics.drawscope.Stroke
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.platform.LocalLifecycleOwner
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.unit.dp
import androidx.core.content.ContextCompat
import androidx.lifecycle.Lifecycle
import androidx.lifecycle.LifecycleEventObserver
import com.journeyapps.barcodescanner.ScanContract
import com.journeyapps.barcodescanner.ScanOptions
import dev.smarttether.core.mobile.Mobile
import kotlinx.coroutines.delay
import org.json.JSONObject
import java.util.Locale

class MainActivity : ComponentActivity() {
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        // Intent extras are ignored here: any app can start the launcher activity.
        // USB pairing from the PC goes through UsbPairingActivity.
        enableEdgeToEdge()
        setContent { SmartTetherTheme { MainScreen() } }
    }
}

/** What the screen shows, refreshed every second from the Go relay. */
internal data class Ui(
    val running: Boolean = false,
    val connected: Boolean = false,
    val via: String = "",
    val sessionSeconds: Long = 0,
    val downRate: Double = 0.0, // Mbit/s
    val upRate: Double = 0.0,
    val sessionBytes: Long = 0,
)

/** The main screen. [demo] replaces the live relay state with fixed values (debug demo mode). */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
internal fun MainScreen(demo: Ui? = null) {
    val ctx = LocalContext.current
    var ui by remember { mutableStateOf(demo ?: Ui()) }
    var checks by remember { mutableIntStateOf(0) } // bump to re-read the setup checklist

    LaunchedEffect(Unit) {
        if (demo != null) return@LaunchedEffect
        var lastDown = -1L
        var lastUp = -1L
        while (true) {
            val j = runCatching { JSONObject(Mobile.stats()) }.getOrDefault(JSONObject())
            val down = j.optLong("down")
            val up = j.optLong("up")
            val secs = j.optLong("sessionSeconds")
            ui = Ui(
                running = RelayService.isRunning,
                connected = j.optBoolean("connected"),
                via = j.optString("via"),
                sessionSeconds = secs,
                downRate = if (lastDown >= 0) (down - lastDown) * 8 / 1e6 else 0.0,
                upRate = if (lastUp >= 0) (up - lastUp) * 8 / 1e6 else 0.0,
                // Counted by the relay since the session began, so reopening the screen doesn't reset it.
                sessionBytes = j.optLong("sessionBytes"),
            )
            lastDown = down
            lastUp = up
            delay(1000)
        }
    }

    // Re-check permissions and settings when coming back from a settings screen.
    val owner = LocalLifecycleOwner.current
    DisposableEffect(owner) {
        val obs = LifecycleEventObserver { _, e -> if (e == Lifecycle.Event.ON_RESUME) checks++ }
        owner.lifecycle.addObserver(obs)
        onDispose { owner.lifecycle.removeObserver(obs) }
    }

    val scan = rememberLauncherForActivityResult(ScanContract()) { r ->
        val text = r.contents ?: return@rememberLauncherForActivityResult
        if (Prefs.applyPairing(ctx, text)) {
            RelayService.start(ctx) // applies the new pairing if it's already running
            Toast.makeText(ctx, "Paired with your PC", Toast.LENGTH_SHORT).show()
            checks++
        } else {
            Toast.makeText(ctx, "That isn't a Smart Tether code", Toast.LENGTH_SHORT).show()
        }
    }
    val startScan = {
        scan.launch(
            ScanOptions().setDesiredBarcodeFormats(ScanOptions.QR_CODE)
                .setPrompt("Scan the code shown on your PC").setBeepEnabled(false).setOrientationLocked(true)
        )
    }
    val notifications = rememberLauncherForActivityResult(ActivityResultContracts.RequestPermission()) { checks++ }

    Scaffold(
        topBar = {
            TopAppBar(
                title = { Text("Smart Tether") },
                actions = {
                    IconButton(onClick = startScan) { Icon(Icons.Outlined.QrCodeScanner, contentDescription = "Scan pairing code") }
                },
                colors = TopAppBarDefaults.topAppBarColors(containerColor = MaterialTheme.colorScheme.surface),
            )
        },
    ) { pad ->
        Column(
            Modifier.fillMaxSize().padding(pad).verticalScroll(rememberScrollState()).padding(horizontal = 20.dp),
            horizontalAlignment = Alignment.CenterHorizontally,
        ) {
            Spacer(Modifier.height(12.dp))
            StatusRing(ui)
            Spacer(Modifier.height(16.dp))
            Text(
                when {
                    !ui.running -> "Stopped"
                    ui.connected -> "Connected to your PC"
                    ui.sessionSeconds > 0 -> "Reconnecting…"
                    else -> "Waiting for your PC"
                },
                style = MaterialTheme.typography.headlineSmall, fontWeight = FontWeight.SemiBold,
            )
            Text(
                when {
                    !ui.running -> "Start to share mobile data with your PC."
                    ui.connected -> "Over ${if (ui.via == "usb") "USB" else "Wi-Fi"} · ${age(ui.sessionSeconds)}"
                    ui.sessionSeconds > 0 -> "Open connections are kept."
                    else -> "Plug in USB or stay near your PC."
                },
                style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
            Spacer(Modifier.height(20.dp))
            Row(Modifier.fillMaxWidth(), horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                Stat("Download", rate(ui.downRate), "Mbit/s", Modifier.weight(1f))
                Stat("Upload", rate(ui.upRate), "Mbit/s", Modifier.weight(1f))
                val (v, unit) = bytes(ui.sessionBytes)
                Stat("This session", v, unit, Modifier.weight(1f))
            }
            Spacer(Modifier.height(16.dp))
            if (ui.running) {
                FilledTonalButton(onClick = { RelayService.stop(ctx) }, modifier = Modifier.fillMaxWidth().height(52.dp)) { Text("Stop") }
            } else {
                Button(onClick = {
                    if (!Prefs.paired(ctx)) startScan() else RelayService.start(ctx)
                }, modifier = Modifier.fillMaxWidth().height(52.dp)) { Text(if (Prefs.paired(ctx)) "Start" else "Pair and start") }
            }
            Spacer(Modifier.height(24.dp))
            Setup(checks, startScan, requestNotifications = { notifications.launch(Manifest.permission.POST_NOTIFICATIONS) })
            Spacer(Modifier.height(24.dp))
        }
    }
}

@Composable
private fun StatusRing(ui: Ui) {
    val color = when {
        !ui.running -> MaterialTheme.colorScheme.outlineVariant
        ui.connected -> StatusOk
        else -> StatusBad
    }
    val track = MaterialTheme.colorScheme.surfaceContainerHighest
    Box(Modifier.size(180.dp), contentAlignment = Alignment.Center) {
        Canvas(Modifier.fillMaxSize()) {
            val w = 14.dp.toPx()
            drawCircle(track, style = Stroke(w))
            drawArc(color, -90f, 360f, false, style = Stroke(w, cap = StrokeCap.Round))
        }
        Icon(Icons.Outlined.Laptop, contentDescription = null, tint = if (ui.running) color else MaterialTheme.colorScheme.onSurfaceVariant,
            modifier = Modifier.size(64.dp))
    }
}

@Composable
private fun Stat(label: String, value: String, unit: String, modifier: Modifier) {
    Card(modifier, colors = CardDefaults.cardColors(containerColor = MaterialTheme.colorScheme.surfaceContainerHigh)) {
        Column(Modifier.padding(horizontal = 12.dp, vertical = 10.dp)) {
            Text(label, style = MaterialTheme.typography.labelMedium, color = MaterialTheme.colorScheme.onSurfaceVariant, maxLines = 1)
            Text(value, style = MaterialTheme.typography.titleLarge, fontWeight = FontWeight.SemiBold, maxLines = 1)
            Text(unit, style = MaterialTheme.typography.labelSmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
        }
    }
}

/** One-time setup the phone needs so the relay keeps running in the background. */
@Composable
private fun Setup(checks: Int, scan: () -> Unit, requestNotifications: () -> Unit) {
    val ctx = LocalContext.current
    @Suppress("UNUSED_EXPRESSION") checks // re-read below whenever checks changes
    val paired = Prefs.paired(ctx)
    val battery = ctx.getSystemService(PowerManager::class.java).isIgnoringBatteryOptimizations(ctx.packageName)
    val notify = ContextCompat.checkSelfPermission(ctx, Manifest.permission.POST_NOTIFICATIONS) == PackageManager.PERMISSION_GRANTED

    Text("Setup", style = MaterialTheme.typography.titleMedium, modifier = Modifier.fillMaxWidth().padding(start = 4.dp, bottom = 8.dp))
    Card(colors = CardDefaults.cardColors(containerColor = MaterialTheme.colorScheme.surfaceContainer)) {
        SetupRow(Icons.Outlined.Link, "Paired with your PC", "Scan the code or plug in USB.", paired, "Scan", scan)
        HorizontalDivider()
        SetupRow(Icons.Outlined.BatteryChargingFull, "Run in background", "Battery saving can stop the relay.", battery, "Allow") {
            ctx.startActivity(Intent(Settings.ACTION_REQUEST_IGNORE_BATTERY_OPTIMIZATIONS, Uri.parse("package:${ctx.packageName}")))
        }
        HorizontalDivider()
        if (isXiaomi()) {
            SetupRow(Icons.Outlined.RestartAlt, "Autostart", "Needed on HyperOS / MIUI to keep running.", autostartAllowed(ctx), "Open") { openAutostart(ctx) }
            HorizontalDivider()
            // HyperOS doesn't let apps read this setting, so the row never shows as done;
            // it names the last kill instead, when there was one.
            val kill = remember(checks) { LastExit.previous(ctx)?.takeIf { LastExit.unexpected(it) } }
            val hint = kill?.let { "Stopped ${DateUtils.getRelativeTimeSpanString(it.timestamp)}: ${LastExit.label(it)}" }
                ?: "Set to No restrictions, or calls drop."
            SetupRow(Icons.Outlined.BatterySaver, "Battery saver", hint, null, "Open") { openBatterySaver(ctx) }
            HorizontalDivider()
        }
        SetupRow(Icons.Outlined.Notifications, "Notifications", "Shows when the relay is running.", notify, "Allow", requestNotifications)
    }
}

@Composable
private fun SetupRow(
    icon: androidx.compose.ui.graphics.vector.ImageVector, title: String, hint: String,
    done: Boolean?, action: String, onAction: () -> Unit,
) {
    ListItem(
        headlineContent = { Text(title) },
        supportingContent = { Text(hint, maxLines = 1) },
        leadingContent = { Icon(icon, contentDescription = null) },
        trailingContent = {
            if (done == true) Icon(Icons.Outlined.CheckCircle, contentDescription = "Done", tint = StatusOk)
            else TextButton(onClick = onAction) { Text(action) }
        },
        colors = ListItemDefaults.colors(containerColor = Color.Transparent),
    )
}

/** Xiaomi, Redmi and POCO phones (HyperOS / MIUI) all report Xiaomi as the manufacturer. */
private fun isXiaomi() = Build.MANUFACTURER.equals("Xiaomi", ignoreCase = true)

/**
 * HyperOS/MIUI's autostart switch is the hidden app op 10008 (OP_AUTO_START).
 * Null when it can't be read (blocked by the system).
 */
private fun autostartAllowed(ctx: Context): Boolean? = runCatching {
    val ops = ctx.getSystemService(AppOpsManager::class.java)
    val check = AppOpsManager::class.java.getMethod(
        "checkOpNoThrow", Int::class.javaPrimitiveType, Int::class.javaPrimitiveType, String::class.java,
    )
    val mode = check.invoke(ops, 10008, Process.myUid(), ctx.packageName) as Int
    mode == AppOpsManager.MODE_ALLOWED
}.getOrNull()

/** HyperOS keeps its autostart switch in the Security app; fall back to the app's settings page. */
private fun openAutostart(ctx: Context) {
    val miui = Intent().setComponent(ComponentName("com.miui.securitycenter", "com.miui.permcenter.autostart.AutoStartManagementActivity"))
    val fallback = Intent(Settings.ACTION_APPLICATION_DETAILS_SETTINGS, Uri.parse("package:${ctx.packageName}"))
    runCatching { ctx.startActivity(miui) }.onFailure { ctx.startActivity(fallback) }
}

/**
 * HyperOS's per-app battery saver, where "No restrictions" stops the screen-off
 * kills; fall back to the app's settings page.
 */
private fun openBatterySaver(ctx: Context) {
    val miui = Intent("miui.intent.action.HIDDEN_APPS_CONFIG_ACTIVITY")
        .putExtra("package_name", ctx.packageName)
        .putExtra("package_label", ctx.applicationInfo.loadLabel(ctx.packageManager).toString())
    val fallback = Intent(Settings.ACTION_APPLICATION_DETAILS_SETTINGS, Uri.parse("package:${ctx.packageName}"))
    runCatching { ctx.startActivity(miui) }.onFailure { ctx.startActivity(fallback) }
}

// The UI is English only, so numbers use one format on every phone.
private fun fmt(pattern: String, v: Double) = String.format(Locale.US, pattern, v)

private fun rate(v: Double) = when {
    v >= 100 -> fmt("%.0f", v)
    v >= 10 -> fmt("%.1f", v)
    else -> fmt("%.2f", v)
}

private fun bytes(b: Long): Pair<String, String> = when {
    b >= 1_000_000_000 -> fmt("%.2f", b / 1e9) to "GB"
    b >= 1_000_000 -> fmt(if (b >= 100_000_000) "%.0f" else "%.1f", b / 1e6) to "MB"
    else -> fmt("%.0f", b / 1e3) to "KB"
}

private fun age(s: Long) = when {
    s < 60 -> "$s s"
    s < 3600 -> "${s / 60} min"
    else -> "${s / 3600} h ${s % 3600 / 60} min"
}
