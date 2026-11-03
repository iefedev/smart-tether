package dev.smarttether

import androidx.compose.foundation.isSystemInDarkTheme
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.dynamicDarkColorScheme
import androidx.compose.material3.dynamicLightColorScheme
import androidx.compose.runtime.Composable
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.platform.LocalContext

// Status colors shared with the Windows app (its tile uses the same green and red).
val StatusOk = Color(0xFF0F7B0F)
val StatusBad = Color(0xFFC42B1C)

/** Material You: colors from the wallpaper, light or dark like the system. */
@Composable
fun SmartTetherTheme(content: @Composable () -> Unit) {
    val ctx = LocalContext.current
    val scheme = if (isSystemInDarkTheme()) dynamicDarkColorScheme(ctx) else dynamicLightColorScheme(ctx)
    MaterialTheme(colorScheme = scheme, content = content)
}
