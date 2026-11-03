package dev.smarttether

import android.app.Activity
import android.os.Bundle

/**
 * Entry point for the PC's USB pairing: stores the token and hotspot passphrase from the
 * intent extras, starts the relay and closes again without showing anything.
 *
 * The manifest protects it with android.permission.DUMP. The adb shell (and root) hold that
 * permission and ordinary apps cannot get it, so only a PC with USB debugging access to the
 * phone can pair it this way. The launcher activity ignores intent extras.
 */
class UsbPairingActivity : Activity() {
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        Prefs.applyUsbPairing(this, intent)
        RelayService.start(this)
        finish()
    }
}
