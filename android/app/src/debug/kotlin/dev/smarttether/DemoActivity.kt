package dev.smarttether

import android.os.Bundle
import androidx.activity.ComponentActivity
import androidx.activity.compose.setContent
import androidx.activity.enableEdgeToEdge

/**
 * Debug builds only: the main screen with made-up numbers, for README screenshots.
 *
 *     adb shell am start -n dev.smarttether/.DemoActivity
 */
class DemoActivity : ComponentActivity() {
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        enableEdgeToEdge()
        val demo = Ui(
            running = true,
            connected = true,
            via = "usb",
            sessionSeconds = 2 * 3600 + 17 * 60,
            downRate = 84.6,
            upRate = 12.3,
            sessionBytes = 1_840_000_000,
        )
        setContent { SmartTetherTheme { MainScreen(demo) } }
    }
}
