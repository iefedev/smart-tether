package dev.smarttether

import android.content.Context
import android.content.Intent
import org.json.JSONObject

/**
 * Link settings. Set by scanning the PC's pairing code, or by the PC over USB through
 * [UsbPairingActivity] (`am start -n dev.smarttether/.UsbPairingActivity --es token T --es appass P`).
 */
object Prefs {
    private const val FILE = "settings"
    private val USB_KEYS = listOf("token", "apssid", "appass")

    fun token(ctx: Context) = get(ctx, "token", "")
    fun apSsid(ctx: Context) = get(ctx, "apssid", "SmartTether")
    fun apPass(ctx: Context) = get(ctx, "appass", "")
    fun paired(ctx: Context) = token(ctx).isNotEmpty() && apPass(ctx).isNotEmpty()

    /** Stores the pairing extras the PC sends over adb. Only [UsbPairingActivity] calls this. */
    fun applyUsbPairing(ctx: Context, intent: Intent) {
        val e = prefs(ctx).edit()
        for (k in USB_KEYS) intent.getStringExtra(k)?.takeIf { it.isNotEmpty() }?.let { e.putString(k, it) }
        e.apply()
    }

    /** Applies the PC's QR payload: {"v":1,"token":…,"ssid":…,"pass":…}. Returns false if it isn't one. */
    fun applyPairing(ctx: Context, payload: String): Boolean {
        val j = runCatching { JSONObject(payload) }.getOrNull() ?: return false
        val token = j.optString("token")
        val pass = j.optString("pass")
        if (j.optInt("v") != 1 || token.isEmpty() || pass.isEmpty()) return false
        prefs(ctx).edit()
            .putString("token", token)
            .putString("appass", pass)
            .putString("apssid", j.optString("ssid", "SmartTether"))
            .apply()
        return true
    }

    private fun get(ctx: Context, key: String, def: String) = prefs(ctx).getString(key, def) ?: def
    private fun prefs(ctx: Context) = ctx.getSharedPreferences(FILE, Context.MODE_PRIVATE)
}
