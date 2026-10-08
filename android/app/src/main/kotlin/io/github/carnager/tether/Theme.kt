package io.github.carnager.tether

import android.os.Build
import androidx.compose.foundation.isSystemInDarkTheme
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.darkColorScheme
import androidx.compose.material3.dynamicDarkColorScheme
import androidx.compose.material3.dynamicLightColorScheme
import androidx.compose.material3.lightColorScheme
import androidx.compose.runtime.Composable
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.platform.LocalContext

// Fallback palette from the launcher icon's green, for Android < 12.
private val Light = lightColorScheme(
    primary = Color(0xFF2E5E4E),
    onPrimary = Color.White,
    primaryContainer = Color(0xFFB4EFD8),
    onPrimaryContainer = Color(0xFF002117),
    secondaryContainer = Color(0xFFCDE9DD),
)

private val Dark = darkColorScheme(
    primary = Color(0xFF98D3BC),
    onPrimary = Color(0xFF003828),
    primaryContainer = Color(0xFF15503D),
    onPrimaryContainer = Color(0xFFB4EFD8),
    secondaryContainer = Color(0xFF334B42),
)

/** Material You colors from the wallpaper where available. */
@Composable
fun TetherTheme(content: @Composable () -> Unit) {
    val dark = isSystemInDarkTheme()
    val ctx = LocalContext.current
    val colors = when {
        Build.VERSION.SDK_INT >= 31 && dark -> dynamicDarkColorScheme(ctx)
        Build.VERSION.SDK_INT >= 31 -> dynamicLightColorScheme(ctx)
        dark -> Dark
        else -> Light
    }
    MaterialTheme(colorScheme = colors, content = content)
}
